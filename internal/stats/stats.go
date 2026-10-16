// Package stats records what the processes graft runs cost: wall time, CPU
// time and peak memory of every step, one JSON line each, in a file shared
// by every graft on the machine, so that heavy tests, linters and builds can
// be found from the accumulated runs instead of by measuring them by hand.
package stats

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// MaxSize is the size at which the file is moved to its one old copy
// (stats.jsonl.1) before the next line is written.
const MaxSize = 20 << 20

// MaxTestsSize is MaxSize of the tests file: a run of a large suite writes
// hundreds of kilobytes, and the flaky report needs weeks of them.
const MaxTestsSize = 64 << 20

// maxStepText is how much of a step's text a line keeps, in runes.
const maxStepText = 200

// Kinds of lines.
const (
	KindStep = "step" // one process
	KindTask = "task" // a task as a whole, everything it ran included
	// KindCacheHit is a task the cache skipped, with the costs of the run
	// whose pass it reused: what the skip saved.
	KindCacheHit = "cache-hit"
)

// Record is one line of the stats file.
type Record struct {
	// Time is when the step or task finished.
	Time time.Time `json:"time"`
	Kind string    `json:"kind"`
	// Project is the absolute root of the project.
	Project string `json:"project"`
	Task    string `json:"task"`
	// Top is what the task ran under: the graft command (commit, gate,
	// deploy, ...) or, for graft <task>, the outermost task.
	Top  string `json:"top"`
	Step string `json:"step,omitempty"`
	// Steps is, on a task line, how many processes it ran.
	Steps int     `json:"steps,omitempty"`
	WallS float64 `json:"wall_s"`
	UserS float64 `json:"user_s"`
	SysS  float64 `json:"sys_s"`
	// MaxRSSMB is the largest peak resident memory of a single process: the
	// step's own or that of a descendant it waited for, never their sum.
	// Absent where the OS does not report it.
	MaxRSSMB *float64 `json:"max_rss_mb,omitempty"`
	// TreeMemMB is the peak of the memory the process tree held at once:
	// the step's processes and all their descendants, sampled (Watch).
	// On a task line, its largest step's. Absent where it is not read.
	TreeMemMB *float64 `json:"tree_mem_mb,omitempty"`
	// LoadStart and LoadMax are the 1-minute load average when the step or
	// task started and the highest seen while it ran: a run on a loaded
	// machine is slower for that alone. Absent where it is not read.
	LoadStart *float64 `json:"load_start,omitempty"`
	LoadMax   *float64 `json:"load_max,omitempty"`
	// DBCPUS and DBMemMB are, on the line of a task with test_db, what the
	// test database's container used from the moment the database was ready
	// until the task ended: its CPU time and the most memory it held, page
	// cache and tmpfs data included. The container is shared, so a task
	// running beside it in another worktree counts too. Absent when graft
	// did not start the container or cannot see its cgroup.
	DBCPUS  *float64 `json:"db_cpu_s,omitempty"`
	DBMemMB *float64 `json:"db_mem_mb,omitempty"`
	// Exit is the exit status; -1 when there is none (killed by a signal, or
	// a task that failed outside its processes).
	Exit int `json:"exit"`
}

// Usage is what one process or a group of them cost.
type Usage struct {
	Wall, User, Sys time.Duration
	// MaxRSS is the peak resident memory in bytes; RSSKnown is false where
	// the OS does not report it.
	MaxRSS   int64
	RSSKnown bool
	// TreeMem is the peak of the process tree's memory in bytes, known when
	// TreeKnown.
	TreeMem   int64
	TreeKnown bool
	// LoadStart and LoadMax are known when LoadKnown.
	LoadStart, LoadMax float64
	LoadKnown          bool
	// DB is what the test database's container used, known when DBKnown.
	DB      DBSample
	DBKnown bool
}

// Watched adds what a Watch saw; errors other than an unsupported measure
// are returned for the caller to report.
func (s *Usage) Watched(w Sample) error {
	var errs []error
	if w.TreeErr == nil {
		s.TreeMem, s.TreeKnown = w.TreeMem, true
	} else if !Unsupported(w.TreeErr) {
		errs = append(errs, fmt.Errorf("reading the memory of the process tree: %w", w.TreeErr))
	}
	if w.LoadErr == nil {
		s.LoadStart, s.LoadMax, s.LoadKnown = w.LoadStart, w.LoadMax, true
	} else if !Unsupported(w.LoadErr) {
		errs = append(errs, fmt.Errorf("reading the load average: %w", w.LoadErr))
	}
	return errors.Join(errs...)
}

// FromProcess reads the usage of a finished process; wall is measured by the
// caller around the run.
func FromProcess(ps *os.ProcessState, wall time.Duration) Usage {
	rss, known := maxRSS(ps)
	return Usage{Wall: wall, User: ps.UserTime(), Sys: ps.SystemTime(), MaxRSS: rss, RSSKnown: known}
}

// Add adds the CPU time of u and keeps the larger peak memory; wall time is
// the caller's to measure.
func (s *Usage) Add(u Usage) {
	s.User += u.User
	s.Sys += u.Sys
	if u.RSSKnown {
		s.MaxRSS = max(s.MaxRSS, u.MaxRSS)
		s.RSSKnown = true
	}
	if u.TreeKnown {
		s.TreeMem = max(s.TreeMem, u.TreeMem)
		s.TreeKnown = true
	}
	if u.LoadKnown {
		s.LoadMax = max(s.LoadMax, u.LoadMax)
	}
}

// Fill sets the usage fields of rec.
func (s Usage) Fill(rec *Record) {
	rec.WallS = seconds(s.Wall)
	rec.UserS = seconds(s.User)
	rec.SysS = seconds(s.Sys)
	rec.MaxRSSMB, rec.TreeMemMB = nil, nil
	if s.RSSKnown {
		rec.MaxRSSMB = megabytes(s.MaxRSS)
	}
	if s.TreeKnown {
		rec.TreeMemMB = megabytes(s.TreeMem)
	}
	rec.LoadStart, rec.LoadMax = s.Loads()
	rec.DBCPUS, rec.DBMemMB = nil, nil
	if s.DBKnown {
		cpu := seconds(s.DB.CPU)
		rec.DBCPUS, rec.DBMemMB = &cpu, megabytes(s.DB.Mem)
	}
}

// Loads are LoadStart and LoadMax for a line: nil when unknown.
func (s Usage) Loads() (start, peak *float64) {
	if !s.LoadKnown {
		return nil, nil
	}
	round := func(v float64) *float64 {
		v = math.Round(v*100) / 100
		return &v
	}
	return round(s.LoadStart), round(s.LoadMax)
}

func megabytes(b int64) *float64 {
	mb := math.Round(float64(b)/(1<<20)*10) / 10
	return &mb
}

func seconds(d time.Duration) float64 { return math.Round(d.Seconds()*1000) / 1000 }

// StepText is the text of a step as a line keeps it: on one line, cut to
// maxStepText runes.
func StepText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxStepText {
		s = string(r[:maxStepText-3]) + "..."
	}
	return s
}

// Path is the stats file under the user's cache directory (XDG_CACHE_HOME
// where set).
func Path() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locating the cache: %w", err)
	}
	return filepath.Join(dir, "graft", "stats.jsonl"), nil
}

// TestsPath is the tests file next to the stats file at path.
func TestsPath(path string) string {
	return filepath.Join(filepath.Dir(path), "tests.jsonl")
}

// Recorder appends records to the stats file.
//
// Recording is an explicit failover: the numbers serve a later audit, so a
// write that fails is reported once per run on the warning writer and the
// task goes on, rather than failing a build over its bookkeeping.
type Recorder struct {
	warn   io.Writer
	mu     sync.Mutex
	path   string
	err    error // locating the file failed
	warned bool
}

// NewRecorder prepares a recorder of the user's stats file; warn receives
// the one warning of a failed write.
func NewRecorder(warn io.Writer) *Recorder {
	path, err := Path()
	return &Recorder{warn: warn, path: path, err: err}
}

// Write appends rec as one line. Failures are reported, not returned: see
// Recorder.
func (r *Recorder) Write(rec Record) {
	r.append(r.path, rec, MaxSize)
}

// WriteTests appends a package run to the tests file, the same way.
func (r *Recorder) WriteTests(run PackageRun) {
	r.append(TestsPath(r.path), run, MaxTestsSize)
}

// Warn reports a problem of the records once per run, as a failed write is.
func (r *Recorder) Warn(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.warnLocked(err)
}

func (r *Recorder) append(path string, v any, maxSize int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	err := r.err
	if err == nil {
		err = Append(path, v, maxSize)
	}
	r.warnLocked(err)
}

func (r *Recorder) warnLocked(err error) {
	if err != nil && !r.warned {
		r.warned = true
		fmt.Fprintf(r.warn, "graft: resource stats not recorded (the task goes on): %v\n", err)
	}
}

// Append writes v to the file at path as one line, rotating the file first
// when it has reached maxSize. The line goes out in a single write to a file
// opened with O_APPEND, so lines of concurrent grafts never interleave.
func Append(path string, v any, maxSize int64) error {
	line, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding the stats line: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating the stats directory: %w", err)
	}
	if err := rotate(path, maxSize); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	_, err = f.Write(append(line, '\n'))
	if err != nil {
		err = fmt.Errorf("writing %s: %w", path, err)
	}
	if cerr := f.Close(); cerr != nil {
		err = errors.Join(err, fmt.Errorf("closing %s: %w", path, cerr))
	}
	return err
}

// rotate moves a full file to path.1, replacing the older copy. One graft
// rotates at a time, under a lock next to the file, and checks the size
// again under it: two grafts that both saw the file full would otherwise
// rotate twice and the second would throw the first copy away.
func rotate(path string, maxSize int64) error {
	full, err := isFull(path, maxSize)
	if err != nil || !full {
		return err
	}
	fl := flock.New(path + ".lock")
	locked, err := fl.TryLock()
	if err != nil {
		return fmt.Errorf("locking %s: %w", fl.Path(), err)
	}
	if !locked {
		return nil // another graft is rotating; append to whichever file is there
	}
	if full, err = isFull(path, maxSize); err == nil && full {
		if rerr := os.Rename(path, path+".1"); rerr != nil {
			err = fmt.Errorf("rotating %s: %w", path, rerr)
		}
	}
	if cerr := fl.Close(); cerr != nil {
		err = errors.Join(err, fmt.Errorf("unlocking %s: %w", fl.Path(), cerr))
	}
	return err
}

func isFull(path string, maxSize int64) (bool, error) {
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading the size of %s: %w", path, err)
	}
	return st.Size() >= maxSize, nil
}
