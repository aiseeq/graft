package stats

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"

	"github.com/aiseeq/graft/v2/internal/atomicfile"
)

const execHistoryMaxSize = 1 << 20

// ExecHistoryRecord is the compact history used only by the cheap exec
// shadow decision.
type ExecHistoryRecord struct {
	Time      time.Time `json:"time"`
	CmdKey    string    `json:"cmd_key"`
	CPUS      float64   `json:"cpu_s"`
	TreeMemMB *float64  `json:"tree_mem_mb"`
	Exit      int       `json:"exit"`
	Cached    bool      `json:"cached"`
	Host      bool      `json:"host"`
}

// ExecHistoryFromUsage converts the complete process sample to its compact
// history representation.
func ExecHistoryFromUsage(at time.Time, key string, use Usage, exit int, cached, host bool) ExecHistoryRecord {
	rec := Record{}
	use.Fill(&rec)
	return ExecHistoryRecord{Time: at, CmdKey: key, CPUS: seconds(use.User + use.Sys), TreeMemMB: rec.TreeMemMB,
		Exit: exit, Cached: cached, Host: host}
}

// ExecHistoryPath is next to stats.jsonl in the same graft cache directory.
func ExecHistoryPath(statsPath string) string {
	return filepath.Join(filepath.Dir(statsPath), "exec-history.jsonl")
}

// LoadExecHistory reads only the compact exec history journal.
func LoadExecHistory(path string, since time.Time) ([]ExecHistoryRecord, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	defer f.Close()
	var records []ExecHistoryRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 1<<20)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		var record ExecHistoryRecord
		if err := decodeExecHistoryRecord(sc.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("reading %s: invalid exec history line %d: %w", path, lineNo, err)
		}
		if !record.Time.Before(since) {
			records = append(records, record)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: scanning exec history: %w", path, err)
	}
	return records, nil
}

// AppendExecHistory appends a compact record under the stats journal lock.
// When the file grows past 1 MiB, it atomically drops records older than a week.
func AppendExecHistory(statsPath string, record ExecHistoryRecord) error {
	path := ExecHistoryPath(statsPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating the stats directory: %w", err)
	}
	fl := flock.New(statsPath + ".lock")
	if err := fl.Lock(); err != nil {
		return fmt.Errorf("locking %s: %w", fl.Path(), err)
	}
	defer fl.Unlock()
	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encoding exec history: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	_, writeErr := f.Write(append(line, '\n'))
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Size() > execHistoryMaxSize {
		return pruneExecHistory(path, time.Now().Add(-CheapHistoryWindow))
	}
	return nil
}

func pruneExecHistory(path string, since time.Time) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("reading %s for pruning: %w", path, err)
	}
	var kept []byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 1<<20)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		var record ExecHistoryRecord
		valid := decodeExecHistoryRecord(line, &record) == nil
		if valid && !record.Time.IsZero() && record.Time.Before(since) {
			continue
		}
		if !valid {
			at, readable, ageErr := execHistoryLineTime(line)
			if ageErr == nil && readable && at.Before(since) {
				continue
			}
			// A damaged row with an unreadable time must remain a bar in the
			// next decision, so pruning preserves it.
		}
		if !valid || !record.Time.Before(since) || record.Time.IsZero() {
			kept = append(kept, line...)
			kept = append(kept, '\n')
		}
	}
	scanErr := sc.Err()
	closeErr := f.Close()
	if err := errors.Join(scanErr, closeErr); err != nil {
		return fmt.Errorf("reading %s for pruning: %w", path, err)
	}
	if err := atomicfile.Write(path, kept, 0o644); err != nil {
		return fmt.Errorf("pruning %s: %w", path, err)
	}
	return nil
}

func decodeExecHistoryRecord(line []byte, record *ExecHistoryRecord) error {
	var fields struct {
		Time      *time.Time `json:"time"`
		CmdKey    *string    `json:"cmd_key"`
		CPUS      *float64   `json:"cpu_s"`
		TreeMemMB *float64   `json:"tree_mem_mb"`
		Exit      *int       `json:"exit"`
		Cached    *bool      `json:"cached"`
		Host      *bool      `json:"host"`
	}
	if err := json.Unmarshal(line, &fields); err != nil {
		return fmt.Errorf("decode exec history fields: %w", err)
	}
	for name, present := range map[string]bool{
		"time": fields.Time != nil, "cmd_key": fields.CmdKey != nil, "cpu_s": fields.CPUS != nil,
		"exit": fields.Exit != nil, "cached": fields.Cached != nil, "host": fields.Host != nil,
	} {
		if !present {
			return fmt.Errorf("required field %q is missing or null", name)
		}
	}
	if fields.Time.IsZero() {
		return errors.New("time is zero")
	}
	if *fields.CmdKey == "" {
		return errors.New("cmd_key is empty")
	}
	*record = ExecHistoryRecord{Time: *fields.Time, CmdKey: *fields.CmdKey, CPUS: *fields.CPUS,
		TreeMemMB: fields.TreeMemMB, Exit: *fields.Exit, Cached: *fields.Cached, Host: *fields.Host}
	return nil
}

// execHistoryLineTime reads the leading time field even if a later field on
// the JSON line is malformed; lines without a readable timestamp are kept.
func execHistoryLineTime(line []byte) (time.Time, bool, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	opening, err := dec.Token()
	if err != nil {
		return time.Time{}, false, fmt.Errorf("reading object start: %w", err)
	}
	if opening != json.Delim('{') {
		return time.Time{}, false, errors.New("line is not a JSON object")
	}
	for dec.More() {
		name, err := dec.Token()
		if err != nil {
			return time.Time{}, false, fmt.Errorf("reading field name: %w", err)
		}
		if name == "time" {
			var at time.Time
			if err := dec.Decode(&at); err != nil {
				return time.Time{}, false, fmt.Errorf("reading time value: %w", err)
			}
			if at.IsZero() {
				return time.Time{}, false, nil
			}
			return at, true, nil
		}
		var ignored json.RawMessage
		if err := dec.Decode(&ignored); err != nil {
			return time.Time{}, false, fmt.Errorf("reading %q value before time: %w", name, err)
		}
	}
	return time.Time{}, false, errors.New("time field is missing")
}

// WriteExecHistory records one completed local exec task; like stats writes,
// a journal failure is warned about and does not fail the command.
func (r *Recorder) WriteExecHistory(record ExecHistoryRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	err := r.err
	if err == nil {
		err = AppendExecHistory(r.path, record)
	}
	r.warnLocked(err)
}
