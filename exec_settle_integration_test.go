package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestExecSettleHoldsTheQueueForThisCommand: graft exec --heavy --settle
// keeps the queue for its duration after the command starts, in place of
// load.settle, or until the command ends if that comes first; it needs
// --heavy and a duration above 0.
func TestExecSettleHoldsTheQueueForThisCommand(t *testing.T) {
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none}\n"})
	// first runs a heavy exec of log for ms in the background, with settle,
	// and returns once its command has started.
	first := func(settle string, ms int) *exec.Cmd {
		t.Helper()
		logFile := filepath.Join(t.TempDir(), "first.log")
		cmd := exec.Command(graftBin, "exec", "--heavy", "--settle", settle, "--", helperBin, "log", logFile, strconv.Itoa(ms))
		cmd.Dir, cmd.Env = f.work, append(slices.Clone(testEnv), f.env...)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if data, _ := os.ReadFile(logFile); strings.HasPrefix(string(data), "start") {
				return cmd
			}
			if time.Now().After(deadline) {
				t.Fatal("the first exec did not start its command")
			}
		}
	}
	second := func() time.Duration {
		t.Helper()
		start := time.Now()
		if out, code := f.graft("", "exec", "--heavy", "--", helperBin, "ok"); code != 0 {
			t.Fatalf("second exec: exit %d\n%s", code, out)
		}
		return time.Since(start)
	}

	// The command runs past the settle: the next waits out the settle only.
	first("2s", 8000)
	if took := second(); took < 1500*time.Millisecond || took > 6*time.Second {
		t.Errorf("behind a 2s settle of a command running 8s, the next exec took %s", took)
	}

	// The command ends before the settle: the next starts when it ends.
	cmd := first("1h", 500)
	if took := second(); took > 6*time.Second {
		t.Errorf("behind a 1h settle of a command running 0.5s, the next exec took %s", took)
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("first exec: %v", err)
	}

	for _, args := range [][]string{
		{"exec", "--settle", "1s", "--", helperBin, "ok"},
		{"exec", "--heavy", "--settle", "0s", "--", helperBin, "ok"},
		{"exec", "--heavy", "--settle", "soon", "--", helperBin, "ok"},
		{"exec", "--heavy", "--settle"},
	} {
		if out, code := f.graft("", args...); code != 125 || !strings.Contains(out, "graft exec") && !strings.Contains(out, "--settle") {
			t.Errorf("graft %q: exit %d, want the usage with 125\n%s", args, code, out)
		}
	}
}
