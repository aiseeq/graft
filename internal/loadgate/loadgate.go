// Package loadgate holds a heavy task back while the machine is busy: the
// laptop is shared, and builds and test runs started at once by several
// sessions drive the load, the disk and the swap past what anyone can work
// with.
package loadgate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aiseeq/akit/pressure"
	"github.com/gofrs/flock"

	"github.com/aiseeq/graft/v2/internal/lock"
	"github.com/aiseeq/graft/v2/internal/userconfig"
)

// queueTimeout bounds the wait for the queue; Ctrl-C stops it sooner.
const queueTimeout = 24 * time.Hour

// Why a heavy task waited, the keys of Waits.
const (
	// WhyQueue is the wait behind other heavy tasks for the queue's lock.
	WhyQueue = "queue"
	// WhyLoad, WhyIO and WhySwap are the waits for the load, the IO
	// pressure and the swapping to fall below their limits.
	WhyLoad = "load"
	WhyIO   = "io"
	WhySwap = "swap"
	// WhyQuiet is the wait for the quiet mode akit's busy --quiet-until
	// sets to end.
	WhyQuiet = "quiet"
)

// Waits is how long a heavy task waited, by why.
type Waits map[string]time.Duration

// Admission is what a heavy task's admission saw: how long it waited, by
// why, and the profile the disk state source gave its kind at the last
// reading, nil without the source.
type Admission struct {
	Waits       Waits
	DiskProfile json.RawMessage
}

// Total is the whole wait.
func (w Waits) Total() time.Duration {
	var total time.Duration
	for _, d := range w {
		total += d
	}
	return total
}

// The environment of a priority: graft deploy sets it for itself and every
// graft its script starts.
const (
	// EnvPriority names what the heavy tasks run for.
	EnvPriority = "GRAFT_LOAD_PRIORITY"
	// EnvBudget is the file of what is left of the deploy's budget for
	// waiting (NewBudget).
	EnvBudget = "GRAFT_LOAD_BUDGET"
	// EnvThen is what a task does at the deadline: start or fail.
	EnvThen = "GRAFT_LOAD_THEN"
)

// Then values: at the deadline a priority task starts, or fails.
const (
	ThenStart = "start"
	ThenFail  = "fail"
)

// Priority is what a deploy gives its heavy tasks and those of the grafts
// its script starts: they skip the queue, a deploy not standing behind the
// test runs of every project, and wait for the machine out of one budget for
// the whole deploy, kept in the file Budget; when it is spent they start, or
// with Fail fail. Only waiting spends it: a task that finds the machine calm
// late in a long deploy takes nothing from it.
type Priority struct {
	For    string
	Budget string
	Fail   bool
}

// Env is the priority as the variables its grafts read.
func (p Priority) Env() map[string]string {
	then := ThenStart
	if p.Fail {
		then = ThenFail
	}
	return map[string]string{EnvPriority: p.For, EnvBudget: p.Budget, EnvThen: then}
}

// PriorityFromEnv reads the priority of this process; ok is false without
// one. A priority without its budget file is an error.
func PriorityFromEnv() (p Priority, ok bool, err error) {
	name := os.Getenv(EnvPriority)
	if name == "" {
		return Priority{}, false, nil
	}
	budget := os.Getenv(EnvBudget)
	if budget == "" {
		return Priority{}, false, fmt.Errorf("%s=%s needs %s, the file of its budget for waiting", EnvPriority, name, EnvBudget)
	}
	p = Priority{For: name, Budget: budget}
	switch then := os.Getenv(EnvThen); then {
	case ThenStart:
	case ThenFail:
		p.Fail = true
	default:
		return Priority{}, false, fmt.Errorf("%s: want %s or %s, got %q", EnvThen, ThenStart, ThenFail, then)
	}
	return p, true, nil
}

// NewBudget writes a budget of d for waiting into a new file in dir and
// returns its path; the deploy removes it when it ends.
func NewBudget(dir string, d time.Duration) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("the budget for waiting: %w", err)
	}
	f, err := os.CreateTemp(dir, "load-budget-*")
	if err != nil {
		return "", fmt.Errorf("the budget for waiting: %w", err)
	}
	_, err = f.WriteString(strconv.FormatInt(int64(d), 10))
	if err = errors.Join(err, f.Close()); err != nil {
		return "", fmt.Errorf("the budget for waiting: %w", err)
	}
	return f.Name(), nil
}

// spend takes d from the budget and returns what is left, under a lock: the
// heavy tasks of a deploy wait side by side, and their waits add up.
func (p *Priority) spend(d time.Duration) (left time.Duration, err error) {
	fl := flock.New(p.Budget + ".lock")
	if err := fl.Lock(); err != nil {
		return 0, fmt.Errorf("locking the budget for waiting %s: %w", p.Budget, err)
	}
	defer func() { err = errors.Join(err, fl.Unlock()) }()
	data, err := os.ReadFile(p.Budget)
	if err != nil {
		return 0, fmt.Errorf("the budget for waiting of %s: %w", p.For, err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("the budget for waiting %s: %q is not a number of nanoseconds", p.Budget, data)
	}
	left = time.Duration(n) - d
	if d > 0 {
		if err := os.WriteFile(p.Budget, []byte(strconv.FormatInt(int64(max(left, 0)), 10)), 0o644); err != nil {
			return 0, fmt.Errorf("the budget for waiting: %w", err)
		}
	}
	return left, nil
}

// Wait admits a heavy task and returns how long it waited and why, and the
// release the task calls when it ends. Every heavy task of the machine, on a
// calm machine too, queues under a lock at lockPath and starts once the
// machine is calm (MachineLoad), one at a time; it keeps the lock for Settle
// after it starts, or until its release if it ends sooner, so the next one
// measures the machine with this one's load on it: tasks that measure
// together all find the same calm machine and start together. A task with a
// priority skips the queue and the settling, and waits no longer than its
// budget allows.
//
// kind is the task's kind as the stats profile names it, which the disk state
// source keeps its profiles by.
func Wait(ctx context.Context, name, kind string, p *Priority, l userconfig.MachineLoad, lockPath string, log io.Writer) (Admission, func() error, error) {
	m := newMachine(l, log, name, kind)
	if p != nil {
		err := m.admit(ctx, p)
		return m.admission(), released, err
	}
	start := time.Now()
	lk, err := lock.AcquireFIFO(ctx, lockPath, queueTimeout, "graft "+name+" (starting a heavy task)", log)
	m.waits[WhyQueue] += time.Since(start)
	if err != nil {
		return m.admission(), released, err
	}
	if err := m.admit(ctx, nil); err != nil {
		return m.admission(), released, errors.Join(err, lk.Release())
	}
	return m.admission(), settle(lk, l.Settle), nil
}

// released is the release of a task that holds nothing.
func released() error { return nil }

// settle keeps the queue's lock for d and returns the release that gives it
// up sooner, when the task ends: its load ends with it. The lock's error
// comes back from the release, whichever of them gave it up.
func settle(lk *lock.Lock, d time.Duration) func() error {
	var once sync.Once
	var err error
	unlock := func() { err = lk.Release() }
	timer := time.AfterFunc(d, func() { once.Do(unlock) })
	return func() error {
		timer.Stop()
		once.Do(unlock)
		return err
	}
}

// ProcEnv, when set, is the directory read instead of /proc to measure the
// machine: for tests.
const ProcEnv = "GRAFT_PROC"

// machine reads what makes the machine busy for one task.
type machine struct {
	l          userconfig.MachineLoad
	log        io.Writer
	name, kind string
	meter      *pressure.Meter
	// disk is the disk state source's profile of the kind at the last
	// reading.
	disk json.RawMessage
	// runnable is the last count of the tasks ready to run.
	runnable float64
	// unchecked holds the measures the system does not have, told once.
	unchecked map[error]bool
	// waits is how long admit waited, by why.
	waits Waits
}

func newMachine(l userconfig.MachineLoad, log io.Writer, name, kind string) *machine {
	proc := "/proc"
	if dir := os.Getenv(ProcEnv); dir != "" {
		proc = dir
	}
	return &machine{l: l, log: log, name: name, kind: kind, meter: pressure.NewMeter(proc), waits: Waits{}}
}

func (m *machine) admission() Admission { return Admission{Waits: m.waits, DiskProfile: m.disk} }

// BusyNow tells whether Wait would wait now, without entering the queue: a
// task holds the queue's lock at lockPath (starting, or settling after its
// start), or the machine measures Wait uses, read once, say busy. A missing
// measure is skipped and reported as it is in Wait. An empty lockPath leaves
// the queue out, for a task with a priority, which Wait does not queue.
func BusyNow(ctx context.Context, name, kind string, l userconfig.MachineLoad, lockPath string, log io.Writer) (bool, error) {
	if lockPath != "" {
		holders, err := lock.Holders(lockPath)
		if err != nil || len(holders) > 0 {
			return err == nil, err
		}
	}
	b, err := newMachine(l, log, name, kind).busy(ctx, true)
	return b.why != "", err
}

// busyness is why the machine cannot take a heavy task, one of the Why
// constants, and the words that say it; a calm machine has neither.
type busyness struct{ why, text string }

// measures are the rule's measures as the waits name them, with what sets
// each limit.
var measures = map[string]struct{ why, setting string }{
	pressure.CPU:   {WhyLoad, "user config load.wait_above"},
	pressure.IO:    {WhyIO, "user config load.max_io_full"},
	pressure.Swap:  {WhySwap, "user config load.max_mem_some, load.max_swap_kbs"},
	pressure.Quiet: {WhyQuiet, "busy --quiet-off ends it"},
}

// busy says why the machine cannot take a heavy task now, nothing when it
// can, by the rule of a busy machine (akit's pressure): the tasks ready to
// run at or above WaitAbove, read once, a disk freeze going on or the IO
// pressure of the last 5 seconds at or above its limit, or the memory
// pressure above its limit while the machine swaps. fresh measures the
// swapping, and without the disk state source the IO pressure, over
// SwapWindow; otherwise since the last call. A measure the system lacks is
// skipped and told once.
func (m *machine) busy(ctx context.Context, fresh bool) (busyness, error) {
	v, err := m.meter.Check(ctx, pressure.Limits{Runnable: m.l.WaitAbove, MaxIOFull: m.l.MaxIOFull, MaxMemSome: m.l.MaxMemSome,
		MaxSwapKBs: m.l.MaxSwapKBs, SwapWindow: m.l.SwapWindow}, m.kind, fresh)
	if err != nil {
		return busyness{}, fmt.Errorf("measuring the machine: %w", err)
	}
	for _, none := range v.Skipped {
		m.skip(none, v.Load.DiskFault)
	}
	m.runnable = float64(v.Load.Runnable)
	m.disk = nil
	if v.Load.Disk != nil {
		m.disk = v.Load.Disk.Profile
	}
	var b busyness
	if v.Busy() {
		// A measure graft does not know yet still makes the machine busy.
		ms, known := measures[v.Measure]
		b = busyness{v.Measure, v.Why()}
		if known {
			b = busyness{ms.why, fmt.Sprintf("%s (%s)", v.Why(), ms.setting)}
		}
	}
	return b, nil
}

// skip says the first time that the system lacks a measure. Without the disk
// state source, which fault tells why, the disk is still measured by the IO
// pressure's total.
func (m *machine) skip(none, fault error) {
	if m.unchecked[none] {
		return
	}
	if m.unchecked == nil {
		m.unchecked = map[error]bool{}
	}
	m.unchecked[none] = true
	switch {
	case errors.Is(none, pressure.ErrNoIOSource):
		fmt.Fprintf(m.log, "graft: %s: %v, PSI total used\n", m.name, none)
	case errors.Is(none, pressure.ErrIOSourceFailed):
		fmt.Fprintf(m.log, "graft: %s: %v, PSI total used\n", m.name, fault)
	default:
		fmt.Fprintf(m.log, "graft: %s: %v, not checked\n", m.name, none)
	}
}

// admit waits for the machine to be calm; a priority task waits no longer
// than its budget allows, spending it as it waits. The time between two
// readings goes to what the first of them found busy, the first reading's
// own to what it found.
func (m *machine) admit(ctx context.Context, p *Priority) error {
	start := time.Now()
	spent, mark := start, start
	var said time.Time
	waited := false
	why := ""
	for {
		b, err := m.busy(ctx, said.IsZero() && !waited)
		now := time.Now()
		if why == "" {
			why = b.why
		}
		if why != "" {
			m.waits[why] += now.Sub(mark)
		}
		mark, why = now, b.why
		if err != nil {
			return err
		}
		if b.why == "" {
			if waited {
				fmt.Fprintf(m.log, "graft: %s: runnable %.0f, starting after %s\n", m.name, m.runnable, time.Since(start).Round(time.Second))
			}
			return nil
		}
		if p != nil && !waited {
			fmt.Fprintf(m.log, "graft: %s: %s, not queueing behind other heavy tasks: it runs for %s\n", m.name, b.text, p.For)
		}
		waited = true
		var left time.Duration
		if p != nil {
			now := time.Now()
			if left, err = p.spend(now.Sub(spent)); err != nil {
				return err
			}
			spent = now
		}
		if p != nil && left <= 0 {
			if p.Fail {
				return fmt.Errorf("%s: %s after %s, past the wait %s allows (deploy.load_wait)", m.name, b.text, time.Since(start).Round(time.Second), p.For)
			}
			fmt.Fprintf(m.log, "graft: %s: %s, starting after %s, all the wait %s allows (deploy.load_wait)\n", m.name, b.text, time.Since(start).Round(time.Second), p.For)
			return nil
		}
		if time.Since(said) >= time.Minute {
			fmt.Fprintf(m.log, "graft: %s: %s, waiting\n", m.name, b.text)
			said = time.Now()
		}
		pause := m.l.Poll
		if p != nil {
			pause = min(pause, left)
		}
		if err := sleep(ctx, pause); err != nil {
			return err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
