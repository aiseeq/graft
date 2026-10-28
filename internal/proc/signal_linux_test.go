package proc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestStopOnCancelKillsTheTreeAfterGrace: a command that ignores SIGTERM is
// killed grace after its context ends, and so is the child it started.
func TestStopOnCancelKillsTheTreeAfterGrace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	childFile := filepath.Join(t.TempDir(), "child")
	cmd := exec.CommandContext(ctx, "sh", "-c", `trap '' TERM; sleep 60 & echo $! > "$1"; wait`, "sh", childFile)
	warned := make(chan error, 4)
	StopOnCancel(cmd, 200*time.Millisecond, func(err error) { warned <- err })
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var child int
	for deadline := time.Now().Add(10 * time.Second); child == 0; time.Sleep(10 * time.Millisecond) {
		if data, err := os.ReadFile(childFile); err == nil && strings.HasSuffix(string(data), "\n") {
			child, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		if time.Now().After(deadline) {
			t.Fatal("the command did not start its child")
		}
	}
	cancel()
	start := time.Now()
	_ = cmd.Wait()
	if took := time.Since(start); took < 150*time.Millisecond || took > 5*time.Second {
		t.Errorf("Wait returned %s after the cancel, want about the 200ms grace", took)
	}
	for deadline := time.Now().Add(5 * time.Second); running(child); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("child %d runs on after its parent was killed", child)
		}
	}
	select {
	case err := <-warned:
		t.Errorf("warned: %v", err)
	default:
	}
}

// running tells whether pid is a process that is not a zombie.
func running(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	_, after, _ := strings.Cut(string(data), ") ")
	return !strings.HasPrefix(after, "Z")
}
