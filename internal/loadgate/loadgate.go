// Package loadgate holds a heavy task back while the machine is loaded: the
// laptop is shared, and builds and test runs started at once by several
// sessions drive the load past what anyone can work with.
package loadgate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aiseeq/graft/v2/internal/loadavg"
	"github.com/aiseeq/graft/v2/internal/lock"
	"github.com/aiseeq/graft/v2/internal/userconfig"
)

// queueTimeout bounds the wait for the queue; Ctrl-C stops it sooner.
const queueTimeout = 24 * time.Hour

// EnvPriority, set by graft deploy for itself and every graft its script
// starts, names what their heavy tasks run for: they skip the queue, a
// deploy not standing behind the test runs of every project, and wait for
// the load at most priorityWait.
const EnvPriority = "GRAFT_LOAD_PRIORITY"

// priorityWait bounds a priority task's wait for the load.
var priorityWait = 10 * time.Minute

// Wait admits a heavy task and returns how long it waited. Below FreeBelow it
// starts at once. Otherwise it queues behind the other heavy tasks of the
// machine, one at a time under a lock at lockPath, until the 1-minute load
// average falls below WaitAbove; above FreeBelow it then holds the lock for
// Settle, since the average is a minute's and every waiter would otherwise
// see the same low figure and start together. A task with a priority (what
// it runs for, EnvPriority) skips the queue and the settling, and starts
// after priorityWait whatever the load.
func Wait(ctx context.Context, name, priority string, l userconfig.MachineLoad, lockPath string, log io.Writer) (time.Duration, error) {
	load, err := loadavg.Read()
	if errors.Is(err, loadavg.ErrNone) {
		fmt.Fprintf(log, "graft: %s: %v, starting without waiting for the load\n", name, err)
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if load < l.FreeBelow {
		return 0, nil
	}
	start := time.Now()
	if priority != "" {
		fmt.Fprintf(log, "graft: %s: load %.2f, not queueing behind other heavy tasks: it runs for %s\n", name, load, priority)
		err := admit(ctx, name, l, time.Now().Add(priorityWait), log)
		return time.Since(start), err
	}
	lk, err := lock.Acquire(ctx, lockPath, queueTimeout, "graft "+name+" (waiting for the load)", log)
	if err != nil {
		return time.Since(start), err
	}
	err = admit(ctx, name, l, time.Time{}, log)
	return time.Since(start), errors.Join(err, lk.Release())
}

// admit waits for the load to fall below WaitAbove, then settles when it is
// above FreeBelow; with a deadline it does neither past it.
func admit(ctx context.Context, name string, l userconfig.MachineLoad, deadline time.Time, log io.Writer) error {
	start := time.Now()
	var said time.Time
	for {
		load, err := loadavg.Read()
		if err != nil {
			return err
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			fmt.Fprintf(log, "graft: %s: load %.2f, starting after %s, the most a priority task waits\n", name, load, time.Since(start).Round(time.Second))
			return nil
		}
		if load < l.WaitAbove {
			if !said.IsZero() {
				fmt.Fprintf(log, "graft: %s: load %.2f, starting after %s\n", name, load, time.Since(start).Round(time.Second))
			}
			if load < l.FreeBelow || !deadline.IsZero() {
				return nil
			}
			return sleep(ctx, l.Settle)
		}
		if time.Since(said) >= time.Minute {
			fmt.Fprintf(log, "graft: %s: load %.2f is above %v, waiting (user config load.wait_above)\n", name, load, l.WaitAbove)
			said = time.Now()
		}
		if err := sleep(ctx, l.Poll); err != nil {
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
