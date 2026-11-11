package main

import (
	"runtime"
	"strings"
	"testing"
)

// TestBusyGateKeepsHereAStepItsTaskKeepsHere: a split gate sends built-in
// checks and go steps of tasks with remote: true to the host; go steps of
// other tasks run on this machine, whose files their tests may read.
func TestBusyGateKeepsHereAStepItsTaskKeepsHere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	// One gate holds the three kinds, each its own task and test name: the
	// remote: true task makes it split, and every step waits for one calm.
	kinds := map[string]string{
		"Heavy": "    heavy: true\n",
		"Plain": "",
		"Kept":  "    heavy: true\n    remote: false\n",
	}
	gate, tasks := "", ""
	for name, task := range kinds {
		gate += "  - task: local-" + strings.ToLower(name) + "\n"
		tasks += "  local-" + strings.ToLower(name) + ":\n" + task + "    run: [[go, test, -run, Local" + name + ", ./...]]\n"
	}
	f, state := userRemoteRepo(t, "")
	f.write(".graft.yaml", strings.Replace(splitGateConfig, "  - task: script\n", gate+"  - task: script\n", 1)+tasks)
	loaded(t, state)
	out := runUntilMachineWait(t, f, state, "gate")
	calls := callsOf(t, state)
	for name := range kinds {
		if strings.Contains(calls, "Local"+name) {
			t.Errorf("the go test step of local-%s, a task without remote: true, went to pc: %q\n%s", strings.ToLower(name), calls, out)
		}
		if !strings.Contains(out, "go test -run Local"+name+" ./...") {
			t.Errorf("the go test step of local-%s did not run:\n%s", strings.ToLower(name), out)
		}
	}
	if !strings.Contains(calls, "go test -count=1 ./...") {
		t.Errorf("the remote: true task's go test did not go to pc: %q\n%s", calls, out)
	}
}

// TestBusyGateWithoutRemoteTasksSendsBuiltinVet: a gate none of whose tasks
// has remote: true still splits for its built-in vet, while its task waits
// for this machine.
func TestBusyGateWithoutRemoteTasksSendsBuiltinVet(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, "")
	f.write(".graft.yaml", strings.Replace(splitGateConfig, "    remote: true\n", "", 1))
	loaded(t, state)
	out := runUntilMachineWait(t, f, state, "gate")
	calls := callsOf(t, state)
	if !strings.Contains(calls, "go vet ./...") {
		t.Errorf("the built-in go vet did not go to pc: %q\n%s", calls, out)
	}
	if strings.Contains(calls, "go test -count=1 ./...") || !strings.Contains(out, "busy here") {
		t.Errorf("the gate task did not stay here in a split gate: %q\n%s", calls, out)
	}
	if !strings.Contains(out, "starting after") {
		t.Errorf("the gate did not wait for this machine:\n%s", out)
	}
}

// TestBusyGateWithoutEligibleStepsWaitsWhole keeps a gate together when it
// has no built-in Go checks and no remote: true task checks.
func TestBusyGateWithoutEligibleStepsWaitsWhole(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, "")
	f.write(".graft.yaml", "schema: 1\nversion: {mode: none}\ngo_vet: {enabled: false, reason: checks disabled in this test}\n"+
		"gate: [{task: script}]\ntasks:\n  script:\n    run: [[sh, -c, 'echo here >> out']]\n")
	loaded(t, state)
	out := runUntilMachineWait(t, f, state, "gate")
	if calls := callsOf(t, state); calls != "" {
		t.Errorf("a gate without eligible steps went to pc: %q\n%s", calls, out)
	}
	if strings.Contains(out, "gate: busy here") || !strings.Contains(out, "starting after") {
		t.Errorf("the gate did not wait here as a whole:\n%s", out)
	}
}

// TestBusyGateStepsGetTheirThreads: a gate that splits places each step
// first, so a step here gets this machine's threads over the environment's
// GOMAXPROCS and -p, and one on the host the host's.
func TestBusyGateStepsGetTheirThreads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, "")
	f.write(".graft.yaml", strings.Replace(splitGateConfig, "gate:\n",
		"go_fmt: {enabled: false, reason: the test has no go files to format}\ngate:\n  - [sh, -c, 'echo first procs=$GOMAXPROCS flags=$GOFLAGS threads=$GRAFT_THREADS >> out']\n", 1))
	f.env = append(f.env, "GOMAXPROCS=1", "GOFLAGS=-p=1")
	loaded(t, state)
	out := runUntilMachineWait(t, f, state, "gate")
	calls := callsOf(t, state)
	if !strings.Contains(calls, "env GRAFT_HEAVY=local GRAFT_HEAVY_BY=") {
		t.Errorf("remote command missing heavy source marker: %q", calls)
	}
	for _, want := range []string{
		"GOFLAGS=-p=1 -p=8 GRAFT_THREADS=8 GOMAXPROCS=8 go vet ./...",
		"GOFLAGS=-p=1 -p=8 GRAFT_THREADS=8 GOMAXPROCS=8 go test -count=1 ./...",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("run got %q, want %q\n%s", calls, want, out)
		}
	}
	if got := f.read("out"); !strings.Contains(got, "first procs=6 flags=-p=1 -p=6 threads=6\n") {
		t.Errorf("the step here saw %q\n%s", got, out)
	}
}
