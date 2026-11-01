//go:build !windows

package proc

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCallerWatcherPollPanicIsLogged(t *testing.T) {
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stderrR.Close()
	defer stderrW.Close()
	oldStderr := os.Stderr
	os.Stderr = stderrW
	defer func() { os.Stderr = oldStderr }()

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
