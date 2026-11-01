package main

import (
	"runtime"
	"strings"
	"testing"
	"time"
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
	calmAfter(t, state, 1500*time.Millisecond)
	out := f.mustGraft("gate")
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
	calmAfter(t, state, 1500*time.Millisecond)
	out := f.mustGraft("gate")
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
