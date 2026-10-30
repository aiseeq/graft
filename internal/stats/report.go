package stats

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// Load reads the old copies of the stats file that reach since and the file
// itself, oldest first. Lines that are not records (a torn write of a
// crashed graft) are counted in malformed and left out.
func Load(path string, since time.Time) (recs []Record, malformed int, err error) {
	files, err := Files(path, since)
	if err != nil {
		return nil, 0, err
	}
	for _, p := range files {
		r, m, err := loadFile(p)
		if err != nil {
			return nil, 0, err
		}
		recs = append(recs, r...)
		malformed += m
	}
	return recs, malformed, nil
}

func loadFile(path string) (recs []Record, malformed int, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("reading the stats: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.Kind == "" || r.Task == "" {
			malformed++
			continue
		}
		recs = append(recs, r)
	}
	if err := sc.Err(); err != nil {
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
	Label   string // the task, or the step of the broken down task
	Runs    int
	WallMed float64
	WallMax float64
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
	// DBCPUMed and DBMemMB are the median CPU time and the largest memory
	// of the test database over the runs that recorded it; nil when none
	// did.
	DBCPUMed *float64
	DBMemMB  *float64
	Failed   int
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

// Summarize groups the records f selects into rows, the heaviest in memory
// first; a broken down task leads with its whole-task row. Saved sums the
// cache hits, which also show on their task's row.
func Summarize(recs []Record, f Filter) ([]Row, Saved) {
	groups := map[rowKey][]Record{}
	hits := map[rowKey]Saved{}
	var saved Saved
	var order []rowKey
	runs := f.taskRuns(recs)
	for i := range recs {
		r := &recs[i]
		if !f.selects(r) {
			continue
		}
		if r.Kind == KindCacheHit {
			if f.Task == "" || r.Task == f.Task {
				k := f.rowKey(r, hitLabel(r, f))
				hits[k] = hits[k].add(r)
				saved = saved.add(r)
			}
			continue
		}
		label, ok := runLabel(r, f, runs)
		if !ok || !calm(r.LoadMax, f.MaxLoad) {
			continue
		}
		k := f.rowKey(r, label)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], *r)
	}
	rows := make([]Row, 0, len(order))
	for _, k := range order {
		row := summarize(k.project, k.label, groups[k])
		row.Hits, row.SavedS = hits[k].Hits, hits[k].WallS
		rows = append(rows, row)
	}
	sortRows(rows, f.Task != "")
	return rows, saved
}

// selects tells whether a record is of the period and the project.
func (f Filter) selects(r *Record) bool {
	return !r.Time.Before(f.Since) && (f.Project == "" || r.Project == f.Project)
}

// taskRuns are the runs of graft that ran the broken down task: the
// parents of the grafts it started.
func (f Filter) taskRuns(recs []Record) map[string]bool {
	runs := map[string]bool{}
	if f.Task == "" {
		return runs
	}
	for i := range recs {
		if r := &recs[i]; r.Kind == KindTask && r.Task == f.Task && r.Run != "" && f.selects(r) {
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
		return r.Task, true
	case r.Kind == KindTask && runs[r.Parent] && r.Task == r.Top:
		return NestedPrefix + r.Task, true
	case r.Kind == KindTask && f.Task != "" && r.In == f.Task && r.Task != f.Task:
		return InnerPrefix + r.Task, true
	case f.Task == "" || r.Task != f.Task:
		return "", false
	case r.Kind == KindTask:
		return TaskLabel, true
	case r.Kind == KindStep:
		return r.Step, true
	}
	return "", false
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

func summarize(project, label string, recs []Record) Row {
	row := Row{Project: project, Label: label, Runs: len(recs)}
	walls := make([]float64, 0, len(recs))
	waits := make([]float64, 0, len(recs))
	owns := make([]float64, 0, len(recs))
	cpus := make([]float64, 0, len(recs))
	var dbCPUs []float64
	for i := range recs {
		r := &recs[i]
		walls = append(walls, r.WallS)
		waits = append(waits, r.WaitS)
		owns = append(owns, r.WallS-r.WaitS)
		cpus = append(cpus, r.UserS+r.SysS)
		row.WallMax = max(row.WallMax, r.WallS)
		row.WaitMax = max(row.WaitMax, r.WaitS)
		for why, s := range r.WaitWhy {
			if row.WaitWhy == nil {
				row.WaitWhy = map[string]float64{}
			}
			row.WaitWhy[why] += s
		}
		row.MaxRSSMB = larger(row.MaxRSSMB, r.MaxRSSMB)
		row.TreeMemMB = larger(row.TreeMemMB, r.TreeMemMB)
		row.DBMemMB = larger(row.DBMemMB, r.DBMemMB)
		if r.DBCPUS != nil {
			dbCPUs = append(dbCPUs, *r.DBCPUS)
		}
		if r.Exit != 0 {
			row.Failed++
		}
	}
	row.WallMed = median(walls)
	row.WaitMed = median(waits)
	row.OwnMed = median(owns)
	row.CPUMed = median(cpus)
	if len(dbCPUs) > 0 {
		m := median(dbCPUs)
		row.DBCPUMed = &m
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

func median(v []float64) float64 {
	slices.Sort(v)
	n := len(v)
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
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

// OverLimits are the task rows over their limits, the farthest over first:
// the gate's over gate, every other task's over task. Rows of a broken down
// task are steps and have none.
func OverLimits(rows []Row, gate, task time.Duration) []OverLimit {
	var out []OverLimit
	for i := range rows {
		r := &rows[i]
		limit, key := LimitOf(r.Label, gate, task)
		if r.OwnMed > limit.Seconds() {
			out = append(out, OverLimit{Project: r.Project, Task: r.Label, OwnMed: r.OwnMed, Limit: limit, Key: key})
		}
	}
	slices.SortStableFunc(out, func(a, b OverLimit) int {
		return cmpFloat(b.OwnMed/b.Limit.Seconds(), a.OwnMed/a.Limit.Seconds())
	})
	return out
}

// LimitOf is the limit of a task line and its user config key: gate for
// the gate, task for every other task.
func LimitOf(label string, gate, task time.Duration) (time.Duration, string) {
	if label == GateTask {
		return gate, "limits.gate"
	}
	return task, "limits.task"
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
	return fmt.Sprintf("over the limit: %s %s %s %swithout waits, above %s (user config %s)", task, verb, formatSeconds(own), measure, formatSeconds(limit.Seconds()), key)
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
	var withDB, withWait bool
	for i := range rows {
		withDB = withDB || rows[i].DBCPUMed != nil
		withWait = withWait || rows[i].WaitMax > 0
	}
	header := labelHeader + "\tRUNS\tWALL MED\tWALL MAX"
	if withWait {
		header += "\tWAIT MED\tWAIT MAX\tWAIT WHY"
	}
	header += "\tCPU MED\tTREE MEM\tMEM MAX\tFAILED\tSAVED"
	if withProject {
		header = "PROJECT\t" + header
	}
	if withDB {
		header += "\tDB CPU\tDB MEM"
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
		fmt.Fprintf(tw, "\t%s\t%s\t%s\t%d%%\t%s", formatSeconds(r.CPUMed),
			formatMB(r.TreeMemMB), formatMB(r.MaxRSSMB), (r.Failed*100+r.Runs/2)/r.Runs, saved)
		if withDB {
			dbCPU := "-"
			if r.DBCPUMed != nil {
				dbCPU = formatSeconds(*r.DBCPUMed)
			}
			fmt.Fprintf(tw, "\t%s\t%s", dbCPU, formatMB(r.DBMemMB))
		}
		fmt.Fprintln(tw)
	}
	return tw.Flush()
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
