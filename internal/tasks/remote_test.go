package tasks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	akitproc "github.com/aiseeq/akit/proc"
	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/stats"
	"github.com/aiseeq/graft/v2/internal/userconfig"
)

func TestLoadCheckWaitAccounting(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv(AdmittedEnv, "")
	old := checkMachineBusy
	t.Cleanup(func() { checkMachineBusy = old })
	for _, nested := range []bool{false, true} {
		for _, slow := range []bool{false, true} {
			t.Run(fmt.Sprintf("nested_%t_slow_%t", nested, slow), func(t *testing.T) {
				checkMachineBusy = func(_ context.Context, _, _ string, _ userconfig.MachineLoad, _ string, _ io.Writer) (bool, error) {
					if slow {
						time.Sleep(checkWaitThreshold + 20*time.Millisecond)
					}
					return true, nil
				}
				r := NewRunner(t.TempDir(), &config.Config{}, io.Discard, io.Discard)
				r.Stats = stats.NewRecorder(io.Discard)
				r.Load = func() (userconfig.MachineLoad, error) { return userconfig.MachineLoad{Threads: 1}, nil }
				f := r.begin(stats.GateTask)
				busy, err := r.gateBusyNow(context.Background(), stats.GateTask, nested)
				if err != nil || !busy {
					t.Fatalf("busy=%t err=%v", busy, err)
				}
				use := r.end(f, nil)
				if slow && (use.WaitWhy["load-check"] < checkWaitThreshold || use.Wait < checkWaitThreshold) {
					t.Fatalf("missing wait: %+v", use)
				}
				if !slow && (use.Wait != 0 || len(use.WaitWhy) != 0) {
					t.Fatalf("fast check: %+v", use)
				}
				path, err := stats.Path()
				if err != nil {
					t.Fatal(err)
				}
				records, _, err := stats.Load(path, time.Time{})
				if err != nil || len(records) == 0 {
					t.Fatalf("record: %v %v", records, err)
				}
				rec := records[len(records)-1]
				if slow && (rec.WaitWhy["load-check"] < 1 || rec.WaitS < 1) {
					t.Fatalf("record missing wait: %+v", rec)
				}
				if !slow && (rec.WaitS != 0 || len(rec.WaitWhy) != 0) {
					t.Fatalf("fast record has wait: %+v", rec)
				}
			})
		}
	}
}

func TestMergeAfterHeavyTaskStaysLocal(t *testing.T) {
	t.Setenv(HeavyEnv, "")
	t.Setenv(HeavyOriginEnv, "")
	t.Setenv(HeavyByEnv, "")
	t.Setenv(AdmittedEnv, "")
	root := t.TempDir()
	remoteMarker := filepath.Join(root, "remote-used")
	r := NewRunner(root, &config.Config{RemoteHeavy: &config.RemoteHeavy{
		Host: "hostA", Up: []string{"true"}, Run: []string{"sh", "-c", "touch " + remoteMarker + "; exec \"$@\"", "sh"},
	}}, io.Discard, io.Discard)
	r.LocalOnly = true
	task := &config.Task{Heavy: true, Run: []config.Step{{Argv: []string{"sh", "-c", "true"}}}}
	remote, why, err := r.placeHeavy(context.Background(), "projectA", task, root, nil)
	if err != nil || remote || why == "" {
		t.Fatalf("placement remote=%t why=%q err=%v; want local", remote, why, err)
	}
	if _, err := os.Stat(remoteMarker); !os.IsNotExist(err) {
		t.Fatalf("remote command ran: %v", err)
	}
}

func TestTaskConfirmationChain(t *testing.T) {
	for _, refusal := range []bool{false, true} {
		t.Run(fmt.Sprint(refusal), func(t *testing.T) {
			root := t.TempDir()
			cfg := &config.Config{Tasks: map[string]*config.Task{
				"first":     {Run: []config.Step{{Shell: "echo dep >> order"}}},
				"protected": {Deps: []string{"first"}, Confirm: "sudo", Run: []config.Step{{Shell: "echo run >> order"}}},
				"second":    {Confirm: "sudo", Run: []config.Step{{Shell: "echo second >> order"}}},
			}}
			r := NewRunner(root, cfg, io.Discard, io.Discard)
			calls := 0
			r.RefuseSudo = func() error { return nil }
			r.ConfirmTask = func(_ context.Context, name string, _ io.Writer) error {
				calls++
				got, err := os.ReadFile(filepath.Join(root, "order"))
				if err != nil || string(got) != "dep\n" || name != "protected" {
					t.Fatalf("order %q, name %q, err %v", got, name, err)
				}
				if refusal {
					return errors.New("not confirmed")
				}
				return nil
			}
			r.ConfirmDeploy = func(context.Context, string, io.Writer) error { t.Fatal("deploy confirmed twice"); return nil }
			err := r.RunDeps(context.Background(), []string{"protected", "second"})
			if refusal {
				if err == nil || !strings.Contains(err.Error(), "not confirmed") {
					t.Fatalf("error: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err := r.Confirm(context.Background(), "prod", true); err != nil {
					t.Fatal(err)
				}
			}
			got, err := os.ReadFile(filepath.Join(root, "order"))
			if err != nil {
				t.Fatal(err)
			}
			want := "dep\nrun\nsecond\n"
			if refusal {
				want = "dep\n"
			}
			if string(got) != want || calls != 1 {
				t.Fatalf("order %q calls %d", got, calls)
			}
		})
	}
}

func TestTaskConfirmationRefusedBeforeSudo(t *testing.T) {
	r := NewRunner(t.TempDir(), &config.Config{}, io.Discard, io.Discard)
	r.RefuseSudo = func() error { return errors.New("no_new_privs; run graft outside the sandbox") }
	r.ConfirmTask = func(context.Context, string, io.Writer) error { t.Fatal("sudo called"); return nil }
	err := r.Confirm(context.Background(), "projectA", false)
	if err == nil || !strings.Contains(err.Error(), "no_new_privs") {
		t.Fatalf("error: %v", err)
	}
}

func TestHeavyConfirmBeforeRemote(t *testing.T) {
	t.Setenv(HeavyEnv, "")
	root := t.TempDir()
	cfg := &config.Config{
		RemoteHeavy: &config.RemoteHeavy{Host: "hostA", Up: []string{"sh", "-c", "test -f confirmed"}, Run: []string{"sh", "-c", "echo remote >> order; exec \"$@\"", "sh"}},
		Tasks:       map[string]*config.Task{"projectA": {Heavy: true, Confirm: "sudo", Run: []config.Step{{Shell: "echo run >> order"}}}},
	}
	r := NewRunner(root, cfg, io.Discard, &bytes.Buffer{})
	r.RefuseSudo = func() error { return nil }
	r.ConfirmTask = func(context.Context, string, io.Writer) error {
		return os.WriteFile(filepath.Join(root, "confirmed"), []byte("yes"), 0600)
	}
	if err := r.Run(context.Background(), "projectA", nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "order"))
	if err != nil || string(got) != "remote\nrun\n" {
		t.Fatalf("order %q: %v", got, err)
	}
}

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

func TestHeavyLocalOriginPlacement(t *testing.T) {
	t.Setenv(HeavyEnv, "local")
	t.Setenv(HeavyByEnv, "parent-run")
	t.Setenv(stats.EnvRun, "nested-run")
	t.Setenv(HeavyOriginEnv, "exec")
	r := NewRunner(t.TempDir(), &config.Config{RemoteHeavy: &config.RemoteHeavy{Host: "hostA"}}, io.Discard, io.Discard)
	if remote, why, err := r.placeHeavy(context.Background(), ExecTask, &config.Task{Run: []config.Step{{Argv: []string{"true"}}}}, r.Root, nil); err != nil || remote || why == "" {
		t.Fatalf("nested exec inherits local: remote=%t why=%q err=%v", remote, why, err)
	}
	if why, err := r.stayHere(&config.Task{}, r.Root, true); err != nil || why != "" {
		t.Fatalf("exec-origin inherited local: why=%q err=%v", why, err)
	}
	noRemote := false
	if why, err := r.stayHere(&config.Task{Remote: &noRemote}, r.Root, true); err != nil || why != "the task has remote: false" {
		t.Fatalf("remote false: why=%q err=%v", why, err)
	}
	for _, origin := range []string{"host", ""} {
		t.Setenv(HeavyOriginEnv, origin)
		if why, err := r.stayHere(&config.Task{}, r.Root, true); err != nil || why == "" {
			t.Errorf("origin %q: expected local placement, why=%q err=%v", origin, why, err)
		}
	}
	t.Setenv(HeavyOriginEnv, "exec")
	t.Setenv(HeavyByEnv, "")
	if why, err := r.stayHere(&config.Task{}, r.Root, true); err != nil || why == "" {
		t.Fatalf("manual local with empty by: why=%q err=%v", why, err)
	}
	t.Setenv(HeavyByEnv, "nested-run") // this graft's own --local
	if why, err := r.stayHere(&config.Task{}, r.Root, true); err != nil || why == "" {
		t.Fatalf("own --local: why=%q err=%v", why, err)
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

func TestTailCaptureBoundsBytes(t *testing.T) {
	var capture tailCapture
	_, _ = capture.Write([]byte("discarded-prefix\n" + strings.Repeat("x", 70<<10) + "\nlast line\n"))
	if len(capture.data) > 64<<10 || strings.Contains(string(capture.data), "discarded-prefix") || !strings.Contains(string(capture.data), "last line") {
		t.Fatalf("tail retains %d bytes or the wrong end of the output", len(capture.data))
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

func TestRemoteCheckWaitRecordedOnGate(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	root := t.TempDir()
	var log bytes.Buffer
	r := NewRunner(root, &config.Config{}, io.Discard, &log)
	r.remoteHeavy = &config.RemoteHeavy{Host: "hostA", Up: []string{"sh", "-c", "sleep 1.1"}}
	r.Stats = stats.NewRecorder(&log)
	r.Limits = func() (userconfig.LimitSelection, error) {
		return userconfig.LimitSelection{Values: userconfig.Limits{Gate: 100 * time.Millisecond}}, nil
	}
	f := r.begin(stats.GateTask)
	if reason := r.remoteUp(context.Background()); reason != "" {
		t.Fatal(reason)
	}
	use := r.end(f, nil)
	if use.WaitWhy["host-check"] < time.Second || use.Wall-use.Wait > 100*time.Millisecond {
		t.Fatalf("usage: %+v", use)
	}
	if strings.Contains(log.String(), "over the limit") {
		t.Fatalf("wait counted as work: %s", log.String())
	}
	path, err := stats.Path()
	if err != nil {
		t.Fatal(err)
	}
	records, _, err := stats.Load(path, time.Time{})
	if err != nil || len(records) != 1 {
		t.Fatalf("records %v: %v", records, err)
	}
	if records[0].WaitWhy["host-check"] < 1 || records[0].WaitS < 1 {
		t.Fatalf("record: %+v", records[0])
	}
}

func TestFastRemoteCheckDoesNotAddWait(t *testing.T) {
	r := NewRunner(t.TempDir(), &config.Config{}, io.Discard, io.Discard)
	r.remoteHeavy = &config.RemoteHeavy{Host: "hostA", Up: []string{"sh", "-c", "exit 0"}}
	f := r.begin(stats.GateTask)
	if reason := r.remoteUp(context.Background()); reason != "" {
		t.Fatal(reason)
	}
	use := r.end(f, nil)
	if use.Wait != 0 || len(use.WaitWhy) != 0 {
		t.Fatalf("fast check wait = %+v", use)
	}
}
