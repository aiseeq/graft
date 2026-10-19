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
	"time"

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
	// EnvDeadline is when the deploy's budget for waiting ends, RFC 3339.
	EnvDeadline = "GRAFT_LOAD_DEADLINE"
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
// test runs of every project, and wait for the machine until Deadline, one
// budget for the whole deploy; then they start, or with Fail fail.
type Priority struct {
	For      string
	Deadline time.Time
	Fail     bool
}

// Env is the priority as the variables its grafts read.
func (p Priority) Env() map[string]string {
	then := ThenStart
	if p.Fail {
		then = ThenFail
	}
	return map[string]string{EnvPriority: p.For, EnvDeadline: p.Deadline.Format(time.RFC3339Nano), EnvThen: then}
}

// PriorityFromEnv reads the priority of this process; nil without one. A
// priority with a missing or unreadable deadline is an error.
func PriorityFromEnv() (*Priority, error) {
	name := os.Getenv(EnvPriority)
	if name == "" {
		return nil, nil
	}
	deadline, err := time.Parse(time.RFC3339Nano, os.Getenv(EnvDeadline))
	if err != nil {
		return nil, fmt.Errorf("%s=%s needs %s, the end of its wait: %w", EnvPriority, name, EnvDeadline, err)
	}
	p := &Priority{For: name, Deadline: deadline}
	switch then := os.Getenv(EnvThen); then {
	case ThenStart:
	case ThenFail:
		p.Fail = true
	default:
		return nil, fmt.Errorf("%s: want %s or %s, got %q", EnvThen, ThenStart, ThenFail, then)
	}
	return p, nil
}

// Wait admits a heavy task and returns how long it waited. A calm machine
// (MachineLoad) takes it at once. Otherwise it queues behind the other
// heavy tasks of the machine, one at a time under a lock at lockPath, until
// the machine is calm with the load below WaitAbove; a task that waited, or
// starts above FreeBelow, then holds the lock for Settle, since the load
// average is a minute's and every waiter would otherwise see the same low
// figure and start together. A task with a priority skips the queue and the
// settling, and waits no longer than its deadline.
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
	lk, err := lock.Acquire(ctx, lockPath, queueTimeout, "graft "+name+" (waiting for the machine)", log)
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
// its deadline.
func (m *machine) admit(ctx context.Context, p *Priority) error {
	start := time.Now()
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
		if p != nil && !time.Now().Before(p.Deadline) {
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
			pause = min(pause, time.Until(p.Deadline))
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
