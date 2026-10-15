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

// Wait admits a heavy task. Below FreeBelow it starts at once. Otherwise it
// queues behind the other heavy tasks of the machine, one at a time under a
// lock at lockPath, until the 1-minute load average falls below WaitAbove;
// above FreeBelow it then holds the lock for Settle, since the average is a
// minute's and every waiter would otherwise see the same low figure and start
// together.
func Wait(ctx context.Context, name string, l userconfig.MachineLoad, lockPath string, log io.Writer) error {
	load, err := loadavg.Read()
	if errors.Is(err, loadavg.ErrNone) {
		fmt.Fprintf(log, "graft: %s: %v, starting without waiting for the load\n", name, err)
		return nil
	}
	if err != nil {
		return err
	}
	if load < l.FreeBelow {
		return nil
	}
	lk, err := lock.Acquire(ctx, lockPath, queueTimeout, "graft "+name+" (waiting for the load)", log)
	if err != nil {
		return err
	}
	return errors.Join(admit(ctx, name, l, log), lk.Release())
}

func admit(ctx context.Context, name string, l userconfig.MachineLoad, log io.Writer) error {
	start := time.Now()
	var said time.Time
	for {
		load, err := loadavg.Read()
		if err != nil {
			return err
		}
		if load < l.WaitAbove {
			if !said.IsZero() {
				fmt.Fprintf(log, "graft: %s: load %.2f, starting after %s\n", name, load, time.Since(start).Round(time.Second))
			}
			if load < l.FreeBelow {
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
