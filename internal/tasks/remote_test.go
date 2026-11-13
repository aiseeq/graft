package tasks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	akitproc "github.com/aiseeq/akit/proc"
	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/stats"
)

func TestHeavyLocalReason(t *testing.T) {
	t.Setenv(HeavyEnv, "local")
	t.Setenv(stats.EnvRun, "run-child")
	for _, tc := range []struct {
		name, by, want string
	}{
		{"flag", "run-child", HeavyEnv + "=local (--local)"},
		{"parent", "run-parent", HeavyEnv + "=local (from graft run-parent)"},
		{"caller", "", HeavyEnv + "=local (from the caller)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(HeavyByEnv, tc.by)
			if got := heavyLocalReason(); got != tc.want {
				t.Errorf("heavyLocalReason() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTaskLoadKeepsOneMachineOnly(t *testing.T) {
	local := stats.Usage{LoadStart: 30, LoadMax: 30, LoadKnown: true}
	frame := &frame{
		loadMachine:    "hostA",
		loadMachineSet: true,
		stepLoads:      stats.Usage{LoadStart: 3.5, LoadMax: 7.25, LoadKnown: true},
	}
	got := taskLoad(local, frame, "hostA")
	if !got.LoadKnown || got.LoadStart != 3.5 || got.LoadMax != 7.25 {
		t.Fatalf("same-host summary = %+v, want host load 3.5..7.25", got)
	}
	frame.mixedLoadMachine = true
	if got := taskLoad(local, frame, "hostA"); got.LoadKnown {
		t.Errorf("mixed-machine task retained load: %+v", got)
	}
	frame.mixedLoadMachine = false
	frame.loadStepUnknown = true
	if got := taskLoad(local, frame, "hostA"); got.LoadKnown {
		t.Errorf("task with an unknown step retained load: %+v", got)
	}
	frame.loadStepUnknown = false
	if got := taskLoad(local, frame, ""); got.LoadKnown {
		t.Errorf("task without a host retained remote load: %+v", got)
	}
}

func TestRecordStepTracksMixedHostMachines(t *testing.T) {
	f := &frame{remote: true}
	r := &Runner{
		noRepoName:  true,
		remoteHeavy: &config.RemoteHeavy{Host: "hostA"},
		frames:      []*frame{f},
	}
	r.recordStep("first", stats.Usage{LoadStart: 3, LoadMax: 3, LoadKnown: true}, 0, "")
	r.remoteHeavy.Host = "hostB"
	r.recordStep("second", stats.Usage{LoadStart: 7, LoadMax: 7, LoadKnown: true}, 0, "")
	if !f.mixedLoadMachine {
		t.Fatal("steps on different hosts were not marked mixed")
	}
	if got := taskLoad(stats.Usage{LoadKnown: true}, f, "hostA"); got.LoadKnown {
		t.Errorf("mixed task retained host load: %+v", got)
	}
}

func TestReadRemoteLoadWaitDelay(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell command")
	}
	if testing.Short() {
		t.Skip("waits for the remote load timeout")
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	command := "sleep 12 & echo $! > " + pidFile + "; wait"
	started := time.Now()
	_, err := readRemoteLoad(context.Background(), []string{"sh", "-c", command})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("load command unexpectedly succeeded without load output")
	}
	if elapsed < 4*time.Second || elapsed > 7*time.Second {
		t.Errorf("load command with inherited stdout took %v, want about 5s", elapsed)
	}
	if data, readErr := os.ReadFile(pidFile); readErr == nil {
		if pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data))); parseErr == nil {
			_ = akitproc.SignalDescendant(pid, syscall.SIGKILL)
		}
	}
}

func TestTailCaptureReason(t *testing.T) {
	var capture tailCapture
	for i := range 15 {
		fmt.Fprintf(&capture, "line %d\n", i)
	}
	if got := capture.reason(); got != "line 5\nline 6\nline 7\nline 8\nline 9\nline 10\nline 11\nline 12\nline 13\nline 14" {
		t.Fatalf("tail reason: %q", got)
	}
}

func TestGlintCheckNoOutput(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "glint")
	for _, c := range []struct {
		argv, args []string
		want       bool
	}{
		{[]string{"glint", "check", "./..."}, nil, true},
		{[]string{abs, "check", "./..."}, nil, true},
		{[]string{"glint", "check", "-o", "json", "./..."}, nil, true},
		{[]string{"glint", "check", "--cpuprofile=c.out", "./..."}, nil, false},
		{[]string{"glint", "check", ArgsPlaceholder}, []string{"--cpuprofile", "c.out"}, false},
		{[]string{"glint", "rules"}, nil, false},
		{[]string{"./glint", "check", "./..."}, nil, false},
		{[]string{"bin/glint", "check", "./..."}, nil, false},
	} {
		if got := glintCheckNoOutput(c.argv, c.args); got != c.want {
			t.Errorf("glintCheckNoOutput(%q, %q) = %v, want %v", c.argv, c.args, got, c.want)
		}
	}
}

func TestGoFixDiffOnlyGoesWithDiff(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want bool
	}{
		{[]string{"go", "fix", "-diff", "./..."}, true},
		{[]string{"go", "fix", "./..."}, false},
	} {
		if got := goFixDiffNoOutput(c.argv); got != c.want {
			t.Errorf("goFixDiffNoOutput(%q) = %v, want %v", c.argv, got, c.want)
		}
	}
}

func TestGoCheckNoOutput(t *testing.T) {
	for _, c := range []struct {
		argv, args []string
		want       bool
	}{
		{[]string{"go", "test", "./..."}, nil, true},
		{[]string{"go", "vet", "./..."}, nil, true},
		{[]string{"go", "build", "./..."}, nil, false},
		{[]string{"go", "test", "-coverprofile=c.out", "./..."}, nil, false},
		{[]string{"go", "test", "./...", "-coverprofile", "c.out"}, nil, false},
		{[]string{"go", "test", "-v", ArgsPlaceholder, "./..."}, nil, true},
		{[]string{"go", "test", "-v", ArgsPlaceholder, "./..."}, []string{"-run", "TestX"}, true},
		{[]string{"go", "test", "-v", ArgsPlaceholder, "./..."}, []string{"-coverprofile=c.out"}, false},
		{[]string{"go", "test", "-v", ArgsPlaceholder, "./..."}, []string{"-test.coverprofile=c.out"}, false},
		{[]string{"go", "test", ArgsPlaceholder}, []string{".", "-args", "-test.cpuprofile=c.out"}, false},
		{[]string{"go", "test", "-" + ArgsPlaceholder, "./..."}, []string{"coverprofile=c.out"}, false},
		{[]string{"go", "test", "-run=" + ArgsPlaceholder, "./..."}, []string{"-o"}, true},
		{[]string{"go", "test", "-fuzz=FuzzX", "./..."}, nil, false},
		{[]string{"go", "test", "-mod=mod", "./..."}, nil, false},
		{[]string{"go", "test", "-mod=readonly", "./..."}, nil, true},
	} {
		if got := goCheckNoOutput(c.argv, c.args); got != c.want {
			t.Errorf("goCheckNoOutput(%q, %q) = %v, want %v", c.argv, c.args, got, c.want)
		}
	}
}

// TestRemoteUpUsesShortStopGrace: remote up uses the short utility stop grace.
func TestRemoteUpUsesShortStopGrace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell command")
	}
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runner{
		Root:        dir,
		remoteHeavy: &config.RemoteHeavy{Up: []string{"sh", "-c", `trap '' TERM; : > "$1"; exec sleep 4`, "sh", ready}},
	}
	done := make(chan string, 1)
	var started time.Time
	go func() { done <- r.remoteUp(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("remote up command did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	started = time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("remote up command did not stop")
	}
	if elapsed := time.Since(started); elapsed >= 3*time.Second {
		t.Errorf("remote up took %s to stop; want short grace", elapsed)
	}
}

// TestRemoteLoadCancelIsNoFailure: a step cancelled (Ctrl-C) while its load
// command runs leaves the load unknown without a load failure to warn about.
func TestRemoteLoadCancelIsNoFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell command")
	}
	parent, cancel := context.WithCancel(context.Background())
	w := watchRemoteLoad(parent, []string{"sleep", "5"})
	cancel()
	select {
	case <-w.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the load sampling did not stop after its step was cancelled")
	}
	sample := w.stopAndSample()
	if sample.LoadErr != nil {
		t.Errorf("cancelled step reports a load failure: %v", sample.LoadErr)
	}
	if sample.LoadKnown {
		t.Errorf("cancelled step before any answer has a known load: %+v", sample)
	}
}
