package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// calmAfter makes the loaded machine of state calm after d, while a graft
// waits for it.
func calmAfter(t *testing.T, state string, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(d)
		setRunnable(t, filepath.Join(state, "proc"), 1)
	}()
	t.Cleanup(func() { <-done })
}

const splitGateConfig = "schema: 1\nversion: {mode: none}\n" +
	"gate:\n  - task: unit\n  - task: script\n" +
	"tasks:\n" +
	"  unit:\n    heavy: true\n    run: [[go, test, -count=1, ./...]]\n" +
	"  script:\n    run: [[sh, -c, 'echo here >> out']]\n"

// TestBusyGateSendsItsGoStepsToTheHost: a gate that would wait for this
// machine sends its go test and go vet steps that write no file to the
// user's remote_heavy host, nested tasks' too, and runs every other step
// here, each admitted by this machine's queue on its own; on a calm machine
// the whole gate runs here.
func TestBusyGateSendsItsGoStepsToTheHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, "")
	f.write(".graft.yaml", splitGateConfig)

	if out := f.mustGraft("gate"); callsOf(t, state) != "" {
		t.Fatalf("a calm machine sent gate steps to pc:\n%s\ncalls: %q", out, callsOf(t, state))
	}
	if err := os.Remove(f.path("out")); err != nil {
		t.Fatal(err)
	}

	loaded(t, state)
	calmAfter(t, state, 1500*time.Millisecond)
	out := f.mustGraft("gate")
	calls := callsOf(t, state)
	for _, want := range []string{"go vet ./...", "go test -count=1 ./..."} {
		if !strings.Contains(calls, want) {
			t.Errorf("run got %q, want %q:\n%s", calls, want, out)
		}
	}
	for _, here := range []string{"echo here", "fix", "gofmt"} {
		if strings.Contains(calls, here) {
			t.Errorf("%q went to pc: %q", here, calls)
		}
	}
	if got := readFile(t, f.path("out")); got != "here\n" {
		t.Errorf("the script step did not run here: out = %q\n%s", got, out)
	}
	if !strings.Contains(out, "graft: gate: busy here: go test and go vet steps on pc, the rest waits for this machine step by step") {
		t.Errorf("the gate does not say it split:\n%s", out)
	}
	// The steps kept here waited for this machine.
	if !strings.Contains(out, "starting after") {
		t.Errorf("no step waited for this machine:\n%s", out)
	}
}

// TestBusyGateBrokenPathRunsTheStepHere: a go step whose path to the host
// breaks runs here, admitted by this machine's queue, and the gate passes.
func TestBusyGateBrokenPathRunsTheStepHere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, "")
	f.write(".graft.yaml", splitGateConfig)
	writeFile(t, filepath.Join(state, "broken"), "")
	loaded(t, state)
	calmAfter(t, state, time.Second)
	out := f.mustGraft("gate")
	if !strings.Contains(out, "pc path broken") || !strings.Contains(out, "running here") {
		t.Errorf("the broken path is not said:\n%s", out)
	}
}

// TestExecOfGraftLetsTheInnerGraftPlaceIt: graft exec --heavy of graft
// itself takes no place in the queue: the inner graft places its task, heavy
// as the exec asked, sends it to the host when it may, and else waits for
// this machine once.
func TestExecOfGraftLetsTheInnerGraftPlaceIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, "  vet:\n    run: [[go, vet, ./...]]\n  script:\n    run: [[sh, -c, 'echo here >> out']]\n")
	loaded(t, state)
	// Calm later, so that an exec that queued itself still ends.
	calmAfter(t, state, 3*time.Second)
	out := f.mustGraft("exec", "--heavy", "--", "graft", "vet")
	if !strings.Contains(callsOf(t, state), "go vet ./...") || !strings.Contains(out, "graft: vet: heavy, on pc") {
		t.Errorf("the inner vet, heavy by the exec, did not go to pc:\n%s\ncalls: %q", out, callsOf(t, state))
	}
	if strings.Contains(out, "graft: exec: heavy, on this machine") || strings.Contains(out, "starting after") {
		t.Errorf("the outer exec placed itself or waited:\n%s", out)
	}

	loaded(t, state)
	calmAfter(t, state, time.Second)
	out = f.mustGraft("exec", "--heavy", "--", "graft", "script")
	if n := strings.Count(out, "starting after"); n != 1 || !strings.Contains(out, "graft: script: runnable") {
		t.Errorf("want the inner script alone to wait for this machine, %d waits:\n%s", n, out)
	}
	if got := readFile(t, f.path("out")); got != "here\n" {
		t.Errorf("out = %q", got)
	}
}
