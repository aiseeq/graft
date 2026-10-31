//go:build !windows

package proc

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var callerGoneInterval = time.Second

func CallerGoneInterval() (time.Duration, error) {
	const name = "GRAFT_TEST_CALLER_GONE_INTERVAL"
	if raw := os.Getenv(name); raw != "" {
		interval, err := time.ParseDuration(raw)
		if err != nil || interval <= 0 {
			if err == nil {
				err = fmt.Errorf("duration must be positive")
			}
			return 0, fmt.Errorf("%s=%q: %w", name, raw, err)
		}
		return interval, nil
	}
	return callerGoneInterval, nil
}

// WatchCaller cancels when both output streams have lost their reader.
func WatchCaller(parent context.Context, interval time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	files := []*os.File{os.Stdout, os.Stderr}
	var polls []unix.PollFd
	var identities []fileIdentity
	for _, f := range files {
		var st syscall.Stat_t
		if f == nil || syscall.Fstat(int(f.Fd()), &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFIFO && st.Mode&syscall.S_IFMT != syscall.S_IFSOCK {
			return ctx, func() { cancel(context.Canceled) }
		}
		id := fileIdentity{dev: uint64(st.Dev), ino: st.Ino}
		duplicate := false
		for _, old := range identities {
			if old.dev == id.dev && old.ino == id.ino {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		identities = append(identities, id)
		polls = append(polls, unix.PollFd{Fd: int32(f.Fd()), Events: unix.POLLERR | unix.POLLHUP})
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			allGone, err := pollCallerGone(polls)
			if err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "graft: caller watcher poll: %v\n", err)
				return
			}
			if allGone {
				cancel(errorsCallerGone)
				return
			}
		}
	}()
	return ctx, func() { cancel(context.Canceled) }
}

func pollCallerGone(polls []unix.PollFd) (bool, error) {
	for i := range polls {
		polls[i].Revents = 0
		if _, err := unix.Poll(polls[i:i+1], 0); err != nil {
			return false, fmt.Errorf("fd %d: %w", polls[i].Fd, err)
		}
		if polls[i].Revents&(unix.POLLERR|unix.POLLHUP) == 0 {
			return false, nil
		}
	}
	return true, nil
}

type fileIdentity struct {
	dev uint64
	ino uint64
}

var errorsCallerGone = callerGoneError("caller gone")

type callerGoneError string

func (e callerGoneError) Error() string { return string(e) }
