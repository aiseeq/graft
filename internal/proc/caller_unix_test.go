//go:build !windows

package proc

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
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

func TestCallerGonePollRetriesAnInterruptedPoll(t *testing.T) {
	oldPoll := unixPoll
	defer func() { unixPoll = oldPoll }()
	calls := 0
	unixPoll = func(fds []unix.PollFd, timeout int) (int, error) {
		calls++
		if calls == 1 {
			return 0, unix.EINTR
		}
		fds[0].Revents = unix.POLLHUP
		return 1, nil
	}
	polls := []unix.PollFd{{Fd: 1, Events: unix.POLLERR | unix.POLLHUP}, {Fd: 2, Events: unix.POLLERR | unix.POLLHUP}}
	gone, err := pollCallerGone(polls)
	if err != nil {
		t.Fatalf("pollCallerGone after EINTR: %v", err)
	}
	if !gone {
		t.Fatal("pollCallerGone = false, want true: both fds hung up after the interrupted poll")
	}
}
