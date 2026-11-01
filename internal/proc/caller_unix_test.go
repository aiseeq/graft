//go:build !windows

package proc

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCallerWatcherPollPanicIsLogged(t *testing.T) {
	// WatchCaller watches only when both stdout and stderr are pipes or
	// sockets: give it pipes whatever go test was started with.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdoutR.Close()
	defer stdoutW.Close()
	go func() { _, _ = io.Copy(io.Discard, stdoutR) }()
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stderrR.Close()
	defer stderrW.Close()
	oldStdout, oldStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = stdoutW, stderrW
	defer func() { os.Stdout, os.Stderr = oldStdout, oldStderr }()

	oldPoll := callerGonePoll
	callerGonePoll = func([]unix.PollFd) (bool, error) {
		panic("synthetic caller poll panic")
	}
	defer func() { callerGonePoll = oldPoll }()

	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(stderrR)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	_, cancel := WatchCaller(context.Background(), time.Millisecond)
	defer cancel()
	const want = "graft: caller watcher: panic in poll: synthetic caller poll panic"
	deadline := time.After(10 * time.Second)
	var seen []string
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("stderr closed without %q; saw %q", want, seen)
			}
			if strings.Contains(line, want) {
				return
			}
			seen = append(seen, line)
		case <-deadline:
			t.Fatalf("no %q within 10s; saw %q", want, seen)
		}
	}
}

// TestCallerGonePollLeavesAnInterruptedPollToTheNextTick: a poll that a
// signal interrupts is no answer and no error, and is not asked again at
// once, so signals that keep coming cannot make it spin.
func TestCallerGonePollLeavesAnInterruptedPollToTheNextTick(t *testing.T) {
	oldPoll := unixPoll
	defer func() { unixPoll = oldPoll }()
	calls := 0
	unixPoll = func([]unix.PollFd, int) (int, error) {
		calls++
		return 0, unix.EINTR
	}
	polls := []unix.PollFd{{Fd: 1, Events: unix.POLLERR | unix.POLLHUP}, {Fd: 2, Events: unix.POLLERR | unix.POLLHUP}}
	gone, err := pollCallerGone(polls)
	if err != nil || gone {
		t.Fatalf("pollCallerGone on EINTR = %v, %v; want false, nil", gone, err)
	}
	if calls != 1 {
		t.Fatalf("pollCallerGone on EINTR polled %d times, want 1", calls)
	}
}

// TestCallerWatcherSurvivesAnInterruptedPoll: after polls that signals
// interrupt, the watcher still cancels when the caller is gone.
func TestCallerWatcherSurvivesAnInterruptedPoll(t *testing.T) {
	var pipes []*os.File
	for range 2 {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		defer w.Close()
		pipes = append(pipes, w)
	}
	oldStdout, oldStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = pipes[0], pipes[1]
	defer func() { os.Stdout, os.Stderr = oldStdout, oldStderr }()

	oldPoll := unixPoll
	defer func() { unixPoll = oldPoll }()
	var mu sync.Mutex
	calls := 0
	unixPoll = func(fds []unix.PollFd, timeout int) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls <= 3 {
			return 0, unix.EINTR
		}
		fds[0].Revents = unix.POLLHUP
		return 1, nil
	}
	ctx, cancel := WatchCaller(context.Background(), time.Millisecond)
	defer cancel()
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the watcher did not cancel within 10s after interrupted polls")
	}
	if cause := context.Cause(ctx); !errors.Is(cause, errorsCallerGone) {
		t.Fatalf("cause = %v, want %v", cause, errorsCallerGone)
	}
}
