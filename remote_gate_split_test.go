package main

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestBusyGateKeepsHereAStepItsTaskKeepsHere: a go test step of a split gate
// whose task could not go to the host (remote: false here) runs on this
// machine, as the task would outside the gate.
func TestBusyGateKeepsHereAStepItsTaskKeepsHere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, "")
	f.write(".graft.yaml", strings.Replace(splitGateConfig, "    heavy: true\n", "    heavy: true\n    remote: false\n", 1))
	loaded(t, state)
	calmAfter(t, state, 1500*time.Millisecond)
	out := f.mustGraft("gate")
	calls := callsOf(t, state)
	if strings.Contains(calls, "go test") {
		t.Errorf("the go test step of a remote: false task went to pc: %q\n%s", calls, out)
	}
	if !strings.Contains(calls, "go vet ./...") {
		t.Errorf("the gate's own go vet did not go to pc: %q\n%s", calls, out)
	}
}
