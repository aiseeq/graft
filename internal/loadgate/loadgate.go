// Package loadgate holds a heavy task back while the machine is busy: the
// laptop is shared, and builds and test runs started at once by several
// sessions drive the load, the disk and the swap past what anyone can work
// with.
package loadgate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/aiseeq/graft/v2/internal/loadavg"
	"github.com/aiseeq/graft/v2/internal/lock"
	"github.com/aiseeq/graft/v2/internal/userconfig"
)

// queueTimeout bounds the wait for the queue; Ctrl-C stops it sooner.
const queueTimeout = 24 * time.Hour

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

// Wait admits a heavy task and returns how long it waited. A calm machine
// (MachineLoad) takes it at once. Otherwise it queues behind the other
// heavy tasks of the machine, one at a time under a lock at lockPath, until
// the machine is calm with the load below WaitAbove; a task that waited, or
// starts above FreeBelow, then holds the lock for Settle, since the load
// average is a minute's and every waiter would otherwise see the same low
// figure and start together. A task with a priority skips the queue and the
// settling, and waits no longer than its budget allows.
func Wait(ctx context.Context, name string, p *Priority, l userconfig.MachineLoad, lockPath string, log io.Writer) (time.Duration, error) {
	m := &machine{l: l, log: log, name: name}
	busy, err := m.busy(ctx, l.FreeBelow, true)
	if err != nil || busy == "" {
		return 0, err
	}
	start := time.Now()
	if p != nil {
		fmt.Fprintf(log, "graft: %s: %s, not queueing behind other heavy tasks: it runs for %s\n", name, busy, p.For)
		err := m.admit(ctx, p)
		return time.Since(start), err
	}
	lk, err := lock.AcquireFIFO(ctx, lockPath, queueTimeout, "graft "+name+" (waiting for the machine)", log)
	if err != nil {
		return time.Since(start), err
	}
	err = m.admit(ctx, nil)
	return time.Since(start), errors.Join(err, lk.Release())
}

// swapPages reads the swap count; tests count on their own.
var swapPages = loadavg.SwapPages

// machine reads what makes the machine busy for one task.
type machine struct {
	l    userconfig.MachineLoad
	log  io.Writer
	name string
	load float64
	// unchecked holds the measures the system does not have, told once.
	unchecked map[error]bool
	// pages and at are the last swap count and when it was read.
	pages int64
	at    time.Time
}

// busy says why the machine cannot take a heavy task now, "" when it can:
// the load at or above maxLoad, the IO pressure or the swapping above their
// limits. fresh measures the swapping over SwapWindow; otherwise since the
// last call. A measure the system lacks is skipped and told once.
func (m *machine) busy(ctx context.Context, maxLoad float64, fresh bool) (string, error) {
	load, err := loadavg.Read()
	if m.skip(err, loadavg.ErrNone) {
		return "", nil // no load, and on such a system no pressure or swap either
	}
	if err != nil {
		return "", err
	}
	m.load = load
	if load >= maxLoad {
		return fmt.Sprintf("load %.2f is at or above %v (user config load.wait_above)", load, maxLoad), nil
	}
	ioFull, err := loadavg.IOFull()
	switch {
	case m.skip(err, loadavg.ErrNoPressure):
	case err != nil:
		return "", err
	case ioFull > m.l.MaxIOFull:
		return fmt.Sprintf("IO pressure %.1f%% is above %v%% (user config load.max_io_full)", ioFull, m.l.MaxIOFull), nil
	}
	kbs, err := m.swapping(ctx, fresh)
	switch {
	case m.skip(err, loadavg.ErrNoVMStat):
	case err != nil:
		return "", err
	case kbs > m.l.MaxSwapKBs:
		return fmt.Sprintf("swapping %.0f KB/s is above %v (user config load.max_swap_kbs)", kbs, m.l.MaxSwapKBs), nil
	}
	return "", nil
}

// skip tells whether err is none, the system lacking a measure, and says so
// the first time.
func (m *machine) skip(err, none error) bool {
	if !errors.Is(err, none) {
		return false
	}
	if !m.unchecked[none] {
		if m.unchecked == nil {
			m.unchecked = map[error]bool{}
		}
		m.unchecked[none] = true
		fmt.Fprintf(m.log, "graft: %s: %v, not checked\n", m.name, none)
	}
	return true
}

// swapping is the swap traffic in KB/s: over SwapWindow when fresh, else
// since the last reading.
func (m *machine) swapping(ctx context.Context, fresh bool) (float64, error) {
	if fresh || m.at.IsZero() {
		pages, err := swapPages()
		if err != nil {
			return 0, err
		}
		m.pages, m.at = pages, time.Now()
		if err := sleep(ctx, m.l.SwapWindow); err != nil {
			return 0, err
		}
	}
	pages, err := swapPages()
	if err != nil {
		return 0, err
	}
	elapsed := time.Since(m.at).Seconds()
	kbs := float64(pages-m.pages) * float64(os.Getpagesize()) / 1024 / elapsed
	m.pages, m.at = pages, time.Now()
	return kbs, nil
}

// admit waits for the machine to be calm with the load below WaitAbove,
// then settles; a priority task does not settle and waits no longer than
// its budget allows, spending it as it waits.
func (m *machine) admit(ctx context.Context, p *Priority) error {
	start := time.Now()
	spent := start
	var said time.Time
	waited := false
	for {
		busy, err := m.busy(ctx, m.l.WaitAbove, said.IsZero() && !waited)
		if err != nil {
			return err
		}
		if busy == "" {
			if waited {
				fmt.Fprintf(m.log, "graft: %s: load %.2f, starting after %s\n", m.name, m.load, time.Since(start).Round(time.Second))
			}
			if p != nil || (!waited && m.load < m.l.FreeBelow) {
				return nil
			}
			return sleep(ctx, m.l.Settle)
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
				return fmt.Errorf("%s: %s after %s, past the wait %s allows (deploy.load_wait)", m.name, busy, time.Since(start).Round(time.Second), p.For)
			}
			fmt.Fprintf(m.log, "graft: %s: %s, starting after %s, all the wait %s allows (deploy.load_wait)\n", m.name, busy, time.Since(start).Round(time.Second), p.For)
			return nil
		}
		if time.Since(said) >= time.Minute {
			fmt.Fprintf(m.log, "graft: %s: %s, waiting\n", m.name, busy)
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
