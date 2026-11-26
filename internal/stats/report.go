package stats

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// Load reads the old copies of the stats file that reach since and the file
// itself, oldest first. Lines that are not valid records, including records
// without a readable time (a torn write of a crashed graft), are counted in
// malformed and left out.
func Load(path string, since time.Time) (recs []Record, malformed int, err error) {
	recs, malformed, _, err = load(path, since, "")
	return recs, malformed, err
}

// LoadProject reads only records whose project field can match project. The
// report still applies its normal exact project filter after decoding.
func LoadProject(path string, since time.Time, project string) (recs []Record, malformed int, err error) {
	recs, malformed, _, err = load(path, since, project)
	return recs, malformed, err
}

// LoadProjectWithStart also returns the earliest valid record time across all
// projects, for report headers that describe the whole journal.
func LoadProjectWithStart(path string, since time.Time, project string) (recs []Record, malformed int, first time.Time, err error) {
	return load(path, since, project)
}

func load(path string, since time.Time, project string) (recs []Record, malformed int, first time.Time, err error) {
	files, err := Files(path, since)
	if err != nil {
		return nil, 0, time.Time{}, err
	}
	sweepHistoryGZ(path)
	for _, p := range files {
		decode := decodeRecords
		if project != "" {
			decode = func(reader io.Reader) ([]Record, int, error) {
				r, bad, start, decodeErr := decodeRecordsProjectWithStart(reader, project)
				first = earlierTime(first, start)
				return r, bad, decodeErr
			}
		}
		r, m, err := readHistory(p, decode)
		if err != nil {
			return nil, 0, time.Time{}, err
		}
		recs = append(recs, r...)
		malformed += m
	}
	return recs, malformed, first, nil
}

// LoadTail reads at most maxBytes from the newest end of the stats journal,
// including rotated copies, oldest first. A partial first line is discarded.
func LoadTail(path string, maxBytes int64) (recs []Record, malformed int, err error) {
	if maxBytes <= 0 {
		return nil, 0, nil
	}
	files, err := Files(path, time.Time{})
	if err != nil {
		return nil, 0, err
	}
	remaining := maxBytes
	var newestFirst [][]Record
	for i := len(files) - 1; i >= 0 && remaining > 0; i-- {
		rows, bad, read, readErr := loadTailFile(files[i], remaining)
		if readErr != nil {
			return nil, 0, readErr
		}
		remaining -= read
		malformed += bad
		newestFirst = append(newestFirst, rows)
	}
	for i := len(newestFirst) - 1; i >= 0; i-- {
		recs = append(recs, newestFirst[i]...)
	}
	return recs, malformed, nil
}

// LoadTailMatching reads a bounded newest tail and decodes only lines
// containing needle. A partial first line is discarded.
func LoadTailMatching(path string, maxBytes int64, needle string) ([]Record, int, error) {
	encodedNeedle, err := stringPrefixNeedle(needle)
	if err != nil {
		return nil, 0, err
	}
	return loadBounded(path, time.Time{}, maxBytes, encodedNeedle)
}

// LoadWindow reads recent raw journal bytes, newest first, with a hard byte
// limit. Files are opened before reading so rotation cannot expose one inode
// under two names and make its records count twice.
func LoadWindow(path string, since time.Time, needle string, maxBytes int64) ([]Record, int, error) {
	encodedNeedle, err := stringPrefixNeedle(needle)
	if err != nil {
		return nil, 0, err
	}
	return loadBounded(path, since, maxBytes, encodedNeedle)
}

// RecordMatch selects a record by every nonempty field. Multiple matches are
// alternatives, so callers can scan one bounded window for a set of keys.
type RecordMatch struct {
	Profile string
	Step    string
	Project string
	Host    string
}

func LoadWindowMatching(path string, since time.Time, matches []RecordMatch, maxBytes int64) ([]Record, int, error) {
	encoded, err := encodeRecordMatches(matches)
	if err != nil {
		return nil, 0, err
	}
	return loadBoundedMatches(path, since, maxBytes, encoded)
}

type encodedRecordMatch [][]byte

func encodeRecordMatches(matches []RecordMatch) ([]encodedRecordMatch, error) {
	var out []encodedRecordMatch
	for _, match := range matches {
		var fields encodedRecordMatch
		for _, pair := range [][2]string{{"profile", match.Profile}, {"step", match.Step}, {"project", match.Project}, {"host", match.Host}} {
			if pair[1] == "" {
				continue
			}
			field, err := json.Marshal(pair[0])
			if err != nil {
				return nil, fmt.Errorf("encoding stats field: %w", err)
			}
			value, err := json.Marshal(pair[1])
			if err != nil {
				return nil, fmt.Errorf("encoding stats value: %w", err)
			}
			fragment := append(append(field, ':'), value...)
			fields = append(fields, fragment)
		}
		if len(fields) > 0 {
			out = append(out, fields)
		}
	}
	return out, nil
}

func stringPrefixNeedle(value string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encoding stats needle: %w", err)
	}
	return encoded[:len(encoded)-1], nil
}

var loadBoundedAfterOpen func()

type openedWindowFile struct {
	f    *os.File
	size int64
}

func loadBounded(path string, since time.Time, maxBytes int64, needle []byte) ([]Record, int, error) {
	return loadBoundedWithReader(path, since, maxBytes, func(data []byte) ([]Record, int, error) {
		return readWindowRecords(data, needle)
	})
}

func loadBoundedMatches(path string, since time.Time, maxBytes int64, matches []encodedRecordMatch) ([]Record, int, error) {
	if maxBytes <= 0 || len(matches) == 0 {
		return nil, 0, nil
	}
	paths, err := Files(path, since)
	if err != nil {
		return nil, 0, err
	}
	files, err := openWindowFiles(paths)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		for _, x := range files {
			_ = x.f.Close()
		}
	}()
	if loadBoundedAfterOpen != nil {
		loadBoundedAfterOpen()
	}
	remaining := maxBytes
	var grouped [][]Record
	var malformed int
	seen := map[string]bool{}
	counts := make([]int, len(matches))
	for i := len(files) - 1; i >= 0 && remaining > 0; i-- {
		x := files[i]
		n := min(x.size, remaining)
		start := x.size - n
		rows, bad, readErr := readMatchedWindowFile(x.f, start, n, matches)
		if readErr != nil {
			return nil, 0, readErr
		}
		remaining -= n
		malformed += bad
		countMatchedWindowRows(rows, since, matches, seen, counts)
		grouped = append(grouped, rows)
		if enoughWindowMatches(counts) {
			break
		}
	}
	return uniqueWindowRecords(grouped, since), malformed, nil
}

func readMatchedWindowFile(file *os.File, start, n int64, matches []encodedRecordMatch) ([]Record, int, error) {
	data, err := readWindowBytes(file, start, n)
	if err != nil {
		return nil, 0, err
	}
	if start > 0 {
		boundary := bytes.IndexByte(data, '\n')
		if boundary < 0 {
			return nil, 0, nil
		}
		data = data[boundary+1:]
	}
	return readWindowMatchedRecords(data, matches)
}

func countMatchedWindowRows(rows []Record, since time.Time, matches []encodedRecordMatch, seen map[string]bool, counts []int) {
	for i := range rows {
		rec := &rows[i]
		if (!since.IsZero() && rec.Time.Before(since)) || rec.Kind != KindStep || rec.Exit != 0 || rec.WallS <= 0 {
			continue
		}
		key := rec.Time.UTC().Format(time.RFC3339Nano) + "\x00" + rec.Run + "\x00" + rec.Step
		if seen[key] {
			continue
		}
		seen[key] = true
		for j, match := range matches {
			if recordMatchesQuery(*rec, match) && queryAcceptsPriceRecord(*rec, match) {
				counts[j]++
			}
		}
	}
}

func queryAcceptsPriceRecord(rec Record, match encodedRecordMatch) bool {
	for _, fragment := range match {
		if bytes.HasPrefix(fragment, []byte(`"profile":`)) {
			return rec.Host == ""
		}
	}
	return true
}

func enoughWindowMatches(counts []int) bool {
	for _, count := range counts {
		if count < 5 {
			return false
		}
	}
	return true
}

func recordMatchesQuery(rec Record, match encodedRecordMatch) bool {
	for _, fragment := range match {
		var field, value string
		colon := bytes.IndexByte(fragment, ':')
		if colon < 0 || json.Unmarshal(fragment[:colon], &field) != nil || json.Unmarshal(fragment[colon+1:], &value) != nil {
			return false
		}
		switch field {
		case "profile":
			if rec.Profile != value {
				return false
			}
		case "step":
			if rec.Step != value {
				return false
			}
		case "project":
			if rec.Project != value {
				return false
			}
		case "host":
			if rec.Host != value {
				return false
			}
		}
	}
	return true
}

func loadBoundedWithReader(path string, since time.Time, maxBytes int64, read func([]byte) ([]Record, int, error)) ([]Record, int, error) {
	if maxBytes <= 0 {
		return nil, 0, nil
	}
	paths, err := Files(path, since)
	if err != nil {
		return nil, 0, err
	}
	files, err := openWindowFiles(paths)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		for _, x := range files {
			x.f.Close()
		}
	}()
	if loadBoundedAfterOpen != nil {
		loadBoundedAfterOpen()
	}
	grouped, malformed, err := readWindowTail(files, maxBytes, read)
	if err != nil {
		return nil, 0, err
	}
	return uniqueWindowRecords(grouped, since), malformed, nil
}

func openWindowFiles(paths []string) ([]openedWindowFile, error) {
	files := make([]openedWindowFile, 0, len(paths))
	seenInodes := map[[2]uint64]bool{}
	for _, p := range paths {
		f, e := os.Open(p)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			for _, x := range files {
				x.f.Close()
			}
			return nil, fmt.Errorf("reading the stats: %w", e)
		}
		info, e := f.Stat()
		if e != nil {
			f.Close()
			for _, x := range files {
				x.f.Close()
			}
			return nil, fmt.Errorf("stating %s: %w", p, e)
		}
		st := reflect.ValueOf(info.Sys())
		if st.Kind() == reflect.Pointer {
			st = st.Elem()
		}
		if !st.IsValid() || st.Kind() != reflect.Struct {
			files = append(files, openedWindowFile{f, info.Size()})
			continue
		}
		dev, okD := statFieldUint(st, "Dev")
		ino, okI := statFieldUint(st, "Ino")
		if okD && okI {
			id := [2]uint64{dev, ino}
			if seenInodes[id] {
				f.Close()
				continue
			}
			seenInodes[id] = true
		}
		files = append(files, openedWindowFile{f, info.Size()})
	}
	return files, nil
}

func readWindowTail(files []openedWindowFile, maxBytes int64, read func([]byte) ([]Record, int, error)) ([][]Record, int, error) {
	remaining := maxBytes
	var grouped [][]Record
	var malformed int
	for i := len(files) - 1; i >= 0 && remaining > 0; i-- {
		x := files[i]
		n := min(x.size, remaining)
		start := x.size - n
		data, err := readWindowBytes(x.f, start, n)
		if err != nil {
			return nil, 0, err
		}
		if start > 0 {
			end := bytes.IndexByte(data, '\n')
			if end < 0 {
				remaining -= n
				grouped = append(grouped, nil)
				continue
			}
			data = data[end+1:]
		}
		rows, bad, err := read(data)
		if err != nil {
			return nil, 0, err
		}
		remaining -= n
		malformed += bad
		grouped = append(grouped, rows)
	}
	return grouped, malformed, nil
}

func readWindowRecords(data, needle []byte) ([]Record, int, error) {
	rows, bad, err := decodeRecordsMatching(bytes.NewReader(data), needle)
	if err != nil {
		return nil, 0, fmt.Errorf("decoding stats window: %w", err)
	}
	return rows, bad, nil
}

func readWindowMatchedRecords(data []byte, matches []encodedRecordMatch) ([]Record, int, error) {
	return decodeQueriedRecords(data, matches)
}

func readWindowBytes(f *os.File, start, n int64) ([]byte, error) {
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seeking in stats window: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, n))
	if err != nil {
		return nil, fmt.Errorf("reading stats window: %w", err)
	}
	if int64(len(data)) != n {
		return nil, fmt.Errorf("reading stats window: short read from %s: wanted %d bytes, read %d", f.Name(), n, len(data))
	}
	return data, nil
}

func decodeQueriedRecords(data []byte, matches []encodedRecordMatch) ([]Record, int, error) {
	var records []Record
	var malformed int
	start := 0
	for start < len(data) {
		end := bytes.IndexByte(data[start:], '\n')
		if end < 0 {
			end = len(data) - start
		}
		line := data[start : start+end]
		start += end + 1
		if lineMatchesRecord(line, matches) {
			var rec Record
			if json.Unmarshal(line, &rec) != nil || rec.Kind == "" || rec.Task == "" {
				malformed++
				continue
			}
			if rec.Time.IsZero() {
				malformed++
				continue
			}
			records = append(records, rec)
		} else {
			if _, valid := foreignRecordTime(line); !valid {
				malformed++
			}
		}
	}
	return records, malformed, nil
}

func lineMatchesRecord(line []byte, matches []encodedRecordMatch) bool {
	for _, match := range matches {
		matched := true
		for _, fragment := range match {
			if !bytes.Contains(line, fragment) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func uniqueWindowRecords(grouped [][]Record, since time.Time) []Record {
	var out []Record
	seenRows := map[string]bool{}
	for i := len(grouped) - 1; i >= 0; i-- {
		for j := range grouped[i] {
			rec := &grouped[i][j]
			if !since.IsZero() && rec.Time.Before(since) {
				continue
			}
			key := rec.Time.UTC().Format(time.RFC3339Nano) + "\x00" + rec.Run + "\x00" + rec.Step
			if seenRows[key] {
				continue
			}
			seenRows[key] = true
			out = append(out, *rec)
		}
	}
	return out
}

func loadTailFile(path string, maxBytes int64) ([]Record, int, int64, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, 0, nil
	}
	if err != nil {
		return nil, 0, 0, fmt.Errorf("reading the stats: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("reading %s: %w", path, err)
	}
	start := max(int64(0), info.Size()-maxBytes)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, 0, 0, fmt.Errorf("reading %s: %w", path, err)
	}
	data, err := io.ReadAll(io.LimitReader(f, info.Size()-start))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("reading %s: %w", path, err)
	}
	read := int64(len(data))
	if start > 0 {
		lineEnd := bytes.IndexByte(data, '\n')
		if lineEnd < 0 {
			return nil, 0, read, nil
		}
		data = data[lineEnd+1:]
	}
	recs, malformed, err := decodeRecords(bytes.NewReader(data))
	return recs, malformed, read, err
}

func decodeRecords(reader io.Reader) (recs []Record, malformed int, err error) {
	return decodeRecordsMatching(reader, nil)
}

func decodeRecordsProject(reader io.Reader, project string) ([]Record, int, error) {
	encoded, err := json.Marshal(project)
	if err != nil {
		return nil, 0, fmt.Errorf("encoding project needle: %w", err)
	}
	return decodeRecordsMatching(reader, encoded)
}

func decodeRecordsMatching(reader io.Reader, needle []byte) (recs []Record, malformed int, err error) {
	recs, malformed, _, err = decodeRecordsMatchingWithStart(reader, needle)
	return recs, malformed, err
}

func decodeRecordsProjectWithStart(reader io.Reader, project string) ([]Record, int, time.Time, error) {
	encoded, err := json.Marshal(project)
	if err != nil {
		return nil, 0, time.Time{}, fmt.Errorf("encoding project needle: %w", err)
	}
	return decodeRecordsMatchingWithStart(reader, encoded)
}

func decodeRecordsMatchingWithStart(reader io.Reader, needle []byte) (recs []Record, malformed int, first time.Time, err error) {
	sc := bufio.NewScanner(reader)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if needle != nil && !bytes.Contains(line, needle) {
			start, valid := foreignRecordTime(line)
			if !valid {
				malformed++
				continue
			}
			first = earlierTime(first, start)
			continue
		}
		var r Record
		if json.Unmarshal(line, &r) != nil || r.Kind == "" || r.Task == "" {
			malformed++
			continue
		}
		// The record is decoded already: a missing time leaves it zero and an
		// unreadable one fails Unmarshal above. Same rule as foreignRecordTime
		// without three more decodes of the line.
		if r.Time.IsZero() {
			malformed++
			continue
		}
		first = earlierTime(first, r.Time)
		recs = append(recs, r)
	}
	if err := sc.Err(); err != nil {
		return recs, malformed, first, fmt.Errorf("scanning stats records: %w", err)
	}
	return recs, malformed, first, nil
}

// foreignRecordTime reads another project's line as the full reader reads a
// Record, so both count the same lines as malformed.
func foreignRecordTime(line []byte) (time.Time, bool) {
	var r Record
	if json.Unmarshal(line, &r) != nil || r.Kind == "" || r.Task == "" || r.Time.IsZero() {
		return time.Time{}, false
	}
	return r.Time, true
}

func earlierTime(current, candidate time.Time) time.Time {
	if candidate.IsZero() || !current.IsZero() && !candidate.Before(current) {
		return current
	}
	return candidate
}

func loadFile(path string) (recs []Record, malformed int, err error) {
	return loadFileProject(path, "")
}

func loadFileProject(path, project string) (recs []Record, malformed int, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("reading the stats: %w", err)
	}
	defer f.Close()
	if project == "" {
		recs, malformed, err = decodeRecords(f)
	} else {
		recs, malformed, err = decodeRecordsProject(f, project)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("reading %s: %w", path, err)
	}
	return recs, malformed, nil
}

// Filter selects the records a report covers.
type Filter struct {
	Since time.Time
	// Project is the project root; empty takes every project, one row
	// group each.
	Project string
	// Task, when set, breaks that task down into its steps.
	Task string
	// In selects task records run in this task.
	In string
	// MaxLoad, when set, takes only the runs whose highest load stayed
	// below it; runs without a load are left out.
	MaxLoad float64
}

// calm tells whether a run with peak load stays under maxLoad, 0 taking
// every run.
func calm(peak *float64, maxLoad float64) bool {
	return maxLoad == 0 || (peak != nil && *peak < maxLoad)
}

// TaskLabel is the label of the whole-task row of a task broken down into
// its steps.
const TaskLabel = "(task)"

// Row is one line of a report: the runs of a task, or of a step.
type Row struct {
	Project string // set when the report covers every project
	// LimitProject is the path used to read project limits after repo grouping.
	LimitProject string
	Label        string // the task, or the step of the broken down task
	Runs         int
	WallMed      float64
	WallMax      float64
	// WaitMed and WaitMax are of the waits before the steps, part of the
	// wall time.
	WaitMed, WaitMax float64
	// WaitWhy sums the waits of the runs by what they waited for, in
	// seconds (Record.WaitWhy).
	WaitWhy map[string]float64
	// OwnMed is the median wall time without the waits.
	OwnMed float64
	CPUMed float64
	// MaxRSSMB is the largest peak of a run; nil when none was reported.
	MaxRSSMB *float64
	// TreeMemMB is the largest peak of a run's process tree as a whole;
	// nil when none was reported.
	TreeMemMB *float64
	// CgroupPeakMB is the largest cgroup memory peak among the runs.
	CgroupPeakMB *float64
	// DBCPUMed and DBMemMB are the median CPU time and the largest memory
	// of the test database over the runs that recorded it; nil when none
	// did.
	DBCPUMed *float64
	DBMemMB  *float64
	// ReadMBMax, WriteMBMed and WriteMBMax summarize runs that recorded
	// storage use; nil when none did.
	ReadMBMax, WriteMBMed, WriteMBMax *float64
	Failed                            int
	// Hits are the runs the cache skipped; SavedS is the wall time they
	// saved.
	Hits   int
	SavedS float64
}

// Saved is what the cache skipped in a report: runs, wall and CPU time.
type Saved struct {
	Hits        int
	WallS, CPUS float64
}

// rowKey names a row: the project is empty when the report covers one.
type rowKey struct{ project, label string }

func (f Filter) rowKey(r *Record, label string) rowKey {
	if f.Project != "" {
		return rowKey{label: label}
	}
	return rowKey{project: r.Project, label: label}
}

// HasHost reports whether the task report selects a record measured on a
// remote_heavy host.
func HasHost(recs []Record, f Filter) bool {
	runs := f.taskRuns(recs)
	for i := range recs {
		r := &recs[i]
		if r.Host != "" && f.selects(r) && f.ranIn(r, runs) && calm(r.LoadMax, f.MaxLoad) {
			if _, ok := runLabel(r, f, runs); ok {
				return true
			}
		}
	}
	return false
}

// Summarize groups the records f selects into rows, the heaviest in memory
// first; a broken down task leads with its whole-task row. Saved sums the
// cache hits, which also show on their task's row.
func Summarize(recs []Record, f Filter) ([]Row, Saved) {
	return summarizeFiltered(recs, f, nil)
}

// SummarizeByProject groups records by projectKey while still filtering and
// summarizing the original records. projectKey is called only when f.Project
// is empty.
func SummarizeByProject(recs []Record, f Filter, projectKey func(*Record) string) ([]Row, Saved) {
	return summarizeFiltered(recs, f, projectKey)
}

func summarizeFiltered(recs []Record, f Filter, projectKey func(*Record) string) ([]Row, Saved) {
	rowKeyFor := func(r *Record, label string) rowKey {
		if f.Project != "" {
			return rowKey{label: label}
		}
		if projectKey != nil {
			return rowKey{project: projectKey(r), label: label}
		}
		return f.rowKey(r, label)
	}
	groups := map[rowKey][]int{}
	hits := map[rowKey]Saved{}
	var saved Saved
	var order []rowKey
	var selectedTasks []int
	runs := f.taskRuns(recs)
	for i := range recs {
		r := &recs[i]
		if !f.selects(r) || !f.ranIn(r, runs) {
			continue
		}
		if r.Kind == KindCacheHit {
			if f.Task == "" || r.Task == f.Task {
				k := rowKeyFor(r, hitLabel(r, f))
				hits[k] = hits[k].add(r)
				saved = saved.add(r)
			}
			continue
		}
		label, ok := runLabel(r, f, runs)
		if !ok || !calm(r.LoadMax, f.MaxLoad) {
			continue
		}
		k := rowKeyFor(r, label)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], i)
		if r.Kind == KindTask {
			selectedTasks = append(selectedTasks, i)
		}
	}
	nestedWaits := NestedWaits(recs, selectedTasks)
	rows := make([]Row, 0, len(order))
	for _, k := range order {
		row := summarize(k.project, k.label, recs, groups[k], nestedWaits)
		row.Hits, row.SavedS = hits[k].Hits, hits[k].WallS
		rows = append(rows, row)
	}
	sortRows(rows, f.Task != "")
	return rows, saved
}

// NestedWaits returns the selected task records whose waits are already
// represented by a parent in the same filtered report selection. Parent is
// the parent graft process id, so it matches a task record's Run only when
// that task's [Time-WallS, Time] interval contains the child's interval. In
// names the task running the child; it matches Task in the same Run, also
// subject to interval containment. When both fields are present, Parent
// candidates must additionally have Task == In. Callers pass indices after
// applying their report filters, so a parent omitted by time, project, task,
// load or journal rotation does not hide its child's wait.
func NestedWaits(recs []Record, selected []int) map[int]bool {
	byRun := map[string][]int{}
	byRunTask := map[struct{ run, task string }][]int{}
	for _, i := range selected {
		r := &recs[i]
		if r.Kind != KindTask || r.Run == "" {
			continue
		}
		byRun[r.Run] = append(byRun[r.Run], i)
		key := struct{ run, task string }{r.Run, r.Task}
		byRunTask[key] = append(byRunTask[key], i)
	}
	nested := make(map[int]bool)
	for _, i := range selected {
		r := &recs[i]
		if r.Kind != KindTask {
			continue
		}
		parentFound := false
		for _, parent := range byRun[r.Parent] {
			p := &recs[parent]
			if r.Parent != "" && parent != i && (r.In == "" || p.Task == r.In) && containsInterval(p, r) {
				parentFound = true
				break
			}
		}
		inParentFound := false
		if !parentFound && r.In != "" {
			key := struct{ run, task string }{r.Run, r.In}
			for _, parent := range byRunTask[key] {
				if parent != i && containsInterval(&recs[parent], r) {
					inParentFound = true
					break
				}
			}
		}
		if parentFound || inParentFound {
			nested[i] = true
		}
	}
	return nested
}

func containsInterval(parent, child *Record) bool {
	parentStart := parent.Time.Add(-time.Duration(parent.WallS * float64(time.Second)))
	childStart := child.Time.Add(-time.Duration(child.WallS * float64(time.Second)))
	return !parentStart.After(childStart) && !parent.Time.Before(child.Time)
}

// selects tells whether a record is of the period and the project.
func (f Filter) selects(r *Record) bool {
	return !r.Time.Before(f.Since) && (f.Project == "" || r.Project == f.Project)
}

// ranIn tells, with --in, whether a record belongs to a run of the broken
// down task in that parent: the task line itself by its in field, its steps,
// inner tasks and cache hits by the graft that ran it, and a graft it started
// by its parent. A graft that ran the task in two parents keeps the steps of
// both: step lines do not name the parent.
func (f Filter) ranIn(r *Record, runs map[string]bool) bool {
	if f.In == "" {
		return true
	}
	if r.Kind == KindTask && r.Task == f.Task {
		return matchesIn(r, f.In)
	}
	return (r.Run != "" && runs[r.Run]) || (r.Parent != "" && runs[r.Parent])
}

// taskRuns are the runs of graft that ran the broken down task: the
// parents of the grafts it started.
func (f Filter) taskRuns(recs []Record) map[string]bool {
	runs := map[string]bool{}
	if f.Task == "" {
		return runs
	}
	for i := range recs {
		if r := &recs[i]; r.Kind == KindTask && r.Task == f.Task && r.Run != "" && f.selects(r) && matchesIn(r, f.In) {
			runs[r.Run] = true
		}
	}
	return runs
}

// NestedPrefix labels, in a broken down task, a graft it started (a deploy
// script or a step that runs graft <task>): its outermost task as a whole.
const NestedPrefix = "graft "

// InnerPrefix labels, in a broken down task, a task it ran in the same
// graft, as a dep or a step: that task as a whole.
const InnerPrefix = "task "

// runLabel is the row a task or step record belongs to; false leaves it out
// of the report. runs are the grafts that ran the broken down task.
func runLabel(r *Record, f Filter, runs map[string]bool) (string, bool) {
	switch {
	case f.Task == "" && r.Kind == KindTask:
		return markedTaskLabel(r, r.Task), true
	case r.Kind == KindTask && runs[r.Parent] && r.Task == r.Top:
		return NestedPrefix + r.Task, true
	case r.Kind == KindTask && f.Task != "" && r.In == f.Task && r.Task != f.Task:
		return InnerPrefix + r.Task, true
	case f.Task == "" || r.Task != f.Task:
		return "", false
	case r.Kind == KindTask:
		return markedTaskLabel(r, TaskLabel), true
	case r.Kind == KindStep:
		return r.Step, true
	}
	return "", false
}

func markedTaskLabel(r *Record, label string) string {
	if r.Mark != "" {
		return label + " [" + r.Mark + "]"
	}
	return label
}

// sortRows puts the heaviest in memory first; broken down, the whole-task
// row leads.
func sortRows(rows []Row, brokenDown bool) {
	// By index: a Row is too large to copy per comparison.
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := &rows[i], &rows[j]
		if (a.Label == TaskLabel) != (b.Label == TaskLabel) && brokenDown {
			return a.Label == TaskLabel
		}
		if c := compareRSS(b.TreeMemMB, a.TreeMemMB); c != 0 {
			return c < 0
		}
		if c := compareRSS(b.MaxRSSMB, a.MaxRSSMB); c != 0 {
			return c < 0
		}
		return a.Project+"\x00"+a.Label < b.Project+"\x00"+b.Label
	})
}

func compareRSS(a, b *float64) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	case *a < *b:
		return -1
	case *a > *b:
		return 1
	}
	return 0
}

func summarize(project, label string, all []Record, indices []int, nestedWaits map[int]bool) Row {
	row := Row{Project: project, Label: label, Runs: len(indices)}
	walls := make([]float64, 0, len(indices))
	waits := make([]float64, 0, len(indices))
	owns := make([]float64, 0, len(indices))
	cpus := make([]float64, 0, len(indices))
	var dbCPUs []float64
	var writes []float64
	for _, index := range indices {
		r := &all[index]
		walls = append(walls, r.WallS)
		if !nestedWaits[index] {
			waits = append(waits, r.WaitS)
			row.WaitMax = max(row.WaitMax, r.WaitS)
			for why, s := range r.WaitWhy {
				if row.WaitWhy == nil {
					row.WaitWhy = map[string]float64{}
				}
				row.WaitWhy[why] += s
			}
		}
		owns = append(owns, r.WallS-r.WaitS)
		cpus = append(cpus, r.UserS+r.SysS)
		row.WallMax = max(row.WallMax, r.WallS)
		row.MaxRSSMB = larger(row.MaxRSSMB, r.MaxRSSMB)
		row.TreeMemMB = larger(row.TreeMemMB, r.TreeMemMB)
		row.CgroupPeakMB = larger(row.CgroupPeakMB, r.CgroupPeakMB)
		row.DBMemMB = larger(row.DBMemMB, r.DBMemMB)
		if r.ReadMB != nil {
			row.ReadMBMax = larger(row.ReadMBMax, r.ReadMB)
		}
		if r.WriteMB != nil {
			row.WriteMBMax = larger(row.WriteMBMax, r.WriteMB)
			writes = append(writes, *r.WriteMB)
		}
		if r.DBCPUS != nil {
			dbCPUs = append(dbCPUs, *r.DBCPUS)
		}
		if r.Exit != 0 {
			row.Failed++
		}
	}
	row.WallMed = Median(walls)
	if len(waits) > 0 {
		row.WaitMed = Median(waits)
	}
	row.OwnMed = Median(owns)
	row.CPUMed = Median(cpus)
	if len(dbCPUs) > 0 {
		m := Median(dbCPUs)
		row.DBCPUMed = &m
	}
	if len(writes) > 0 {
		m := Median(writes)
		row.WriteMBMed = &m
	}
	return row
}

// larger is the larger of two optional values, a copy.
func larger(a, b *float64) *float64 {
	if b == nil || (a != nil && *a >= *b) {
		return a
	}
	v := *b
	return &v
}

// Median returns the median of v, sorting v in place. The caller must pass a
// non-empty slice.
func Median(v []float64) float64 {
	slices.Sort(v)
	n := len(v)
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}

// Percentile returns the nearest-rank percentile of v. An empty slice has no
// percentile and returns zero.
func Percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sorted := slices.Clone(v)
	slices.Sort(sorted)
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(0, min(rank, len(sorted)-1))]
}

// hitLabel is the row a cache hit belongs to: its task's, or the whole-task
// row of a broken down task.
func hitLabel(r *Record, f Filter) string {
	if f.Task != "" {
		return TaskLabel
	}
	return r.Task
}

func (s Saved) add(r *Record) Saved {
	s.Hits++
	s.WallS += r.WallS
	s.CPUS += r.UserS + r.SysS
	return s
}

// GateTask is the task line of the gate as a whole.
const GateTask = "gate"

// OverLimit is a row whose median run without the waits is longer than its
// limit.
type OverLimit struct {
	Project, Task string
	OwnMed        float64
	// Limit is the limit and Key its user config key.
	Limit time.Duration
	Key   string
}

// OverLimitsUsing compares each row with its project-specific effective limit.
func OverLimitsUsing(rows []Row, limitFor func(*Row) (time.Duration, string)) []OverLimit {
	var out []OverLimit
	for i := range rows {
		r := &rows[i]
		limit, source := limitFor(r)
		if r.OwnMed > limit.Seconds() {
			out = append(out, OverLimit{Project: r.Project, Task: r.Label, OwnMed: r.OwnMed, Limit: limit, Key: source})
		}
	}
	slices.SortStableFunc(out, func(a, b OverLimit) int {
		return cmpFloat(b.OwnMed/b.Limit.Seconds(), a.OwnMed/a.Limit.Seconds())
	})
	return out
}

// FormatOverLimit is the line that marks a row over its limit.
func FormatOverLimit(o OverLimit) string {
	task := o.Task
	if o.Project != "" {
		task = o.Project + " " + task
	}
	return overLimit(task, "takes", "by median ", o.OwnMed, o.Limit, o.Key)
}

// FormatRunOverLimit is the line a run of a task ends with when it took
// longer than its limit without its waits.
func FormatRunOverLimit(task string, own, limit time.Duration, key string) string {
	return overLimit(task, "took", "", own.Seconds(), limit, key)
}

func overLimit(task, verb, measure string, own float64, limit time.Duration, key string) string {
	return fmt.Sprintf("over the limit: %s %s %s %swithout waits, above %s (%s)", task, verb, formatSeconds(own), measure, formatSeconds(limit.Seconds()), key)
}

// FormatSaved is the line a report ends with when the cache skipped runs.
func FormatSaved(s Saved) string {
	runs := "runs"
	if s.Hits == 1 {
		runs = "run"
	}
	return fmt.Sprintf("%d %s skipped by the cache, saving %s of wall time and %s of CPU time", s.Hits, runs, formatSeconds(s.WallS), formatSeconds(s.CPUS))
}

// Render prints rows as a table; labelHeader names the first column after
// the project, which is shown when withProject is set. The waits follow the
// wall time and the test database's columns come last, when a row has them.
func Render(w io.Writer, rows []Row, labelHeader string, withProject bool) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	var withDB, withWait, withRead, withWrite bool
	withCgroupPeak := hasCgroupPeak(rows)
	for i := range rows {
		withDB = withDB || rows[i].DBCPUMed != nil
		withWait = withWait || rows[i].WaitMax > 0
		withRead = withRead || rows[i].ReadMBMax != nil
		withWrite = withWrite || rows[i].WriteMBMed != nil
	}
	header := labelHeader + "\tRUNS\tWALL MED\tWALL MAX"
	if withWait {
		header += "\tWAIT MED\tWAIT MAX\tWAIT WHY"
	}
	header += "\tCPU MED\tTREE MEM"
	header += cgroupPeakHeader(withCgroupPeak)
	header += "\tMEM MAX\tFAILED\tSAVED"
	if withProject {
		header = "PROJECT\t" + header
	}
	if withDB {
		header += "\tDB CPU\tDB MEM"
	}
	if withRead {
		header += "\tREAD MAX"
	}
	if withWrite {
		header += "\tWRITE MED\tWRITE MAX"
	}
	fmt.Fprintln(tw, header)
	for i := range rows {
		r := &rows[i]
		if withProject {
			fmt.Fprintf(tw, "%s\t", r.Project)
		}
		saved := "-"
		if r.Hits > 0 {
			saved = fmt.Sprintf("%s in %d", formatSeconds(r.SavedS), r.Hits)
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s", r.Label, r.Runs, formatSeconds(r.WallMed), formatSeconds(r.WallMax))
		if withWait {
			fmt.Fprintf(tw, "\t%s\t%s\t%s", formatSeconds(r.WaitMed), formatSeconds(r.WaitMax), formatWhy(r.WaitWhy))
		}
		fmt.Fprintf(tw, "\t%s\t%s", formatSeconds(r.CPUMed), formatMB(r.TreeMemMB))
		writeCgroupPeak(tw, r, withCgroupPeak)
		fmt.Fprintf(tw, "\t%s\t%d%%\t%s", formatMB(r.MaxRSSMB), (r.Failed*100+r.Runs/2)/r.Runs, saved)
		if withDB {
			dbCPU := "-"
			if r.DBCPUMed != nil {
				dbCPU = formatSeconds(*r.DBCPUMed)
			}
			fmt.Fprintf(tw, "\t%s\t%s", dbCPU, formatMB(r.DBMemMB))
		}
		if withRead {
			fmt.Fprintf(tw, "\t%s", formatMB(r.ReadMBMax))
		}
		if withWrite {
			fmt.Fprintf(tw, "\t%s\t%s", formatMB(r.WriteMBMed), formatMB(r.WriteMBMax))
		}
		fmt.Fprintln(tw)
	}
	return tw.Flush()
}

func hasCgroupPeak(rows []Row) bool {
	for i := range rows {
		if rows[i].CgroupPeakMB != nil {
			return true
		}
	}
	return false
}

func cgroupPeakHeader(present bool) string {
	if present {
		return "\tCGROUP PEAK"
	}
	return ""
}

func writeCgroupPeak(w *tabwriter.Writer, row *Row, present bool) {
	if present {
		fmt.Fprintf(w, "\t%s", formatMB(row.CgroupPeakMB))
	}
}

// ReserveSummary reports how admitted task reservations ended. Empty is
// returned when no task record contains a reservation.
func ReserveSummary(recs []Record) string {
	total, cpu, cap, done := 0, 0, 0, 0
	for i := range recs {
		r := &recs[i]
		if r.Kind != KindTask || r.ReserveCores <= 0 {
			continue
		}
		total++
		switch r.ReserveEnd {
		case "cpu":
			cpu++
		case "cap":
			cap++
		case "done":
			done++
		}
	}
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("reserve: %d tasks, released by CPU %d, at the time limit %d (%d%%), ended first %d", total, cpu, cap, (cap*100+total/2)/total, done)
}

func formatSeconds(s float64) string {
	d := time.Duration(s * float64(time.Second))
	switch {
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// formatWhy names what the runs waited for, the longest first, with the
// total of each: "swap 8m12s, load 40s"; "-" when no run recorded it.
func formatWhy(why map[string]float64) string {
	if len(why) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(why))
	for k := range why {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if why[keys[i]] != why[keys[j]] {
			return why[keys[i]] > why[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + " " + formatSeconds(why[k])
	}
	return strings.Join(parts, ", ")
}

func formatMB(mb *float64) string {
	switch {
	case mb == nil:
		return "-"
	case *mb < 1024:
		return fmt.Sprintf("%.0f MB", *mb)
	}
	return fmt.Sprintf("%.1f GB", *mb/1024)
}
