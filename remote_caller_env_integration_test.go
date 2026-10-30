package main

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestCallerSetVariablesKeepTheTaskHere: a heavy exec whose caller set a
// variable stays on this machine and names it, since the host would not get
// it; so does graft exec --heavy of graft, for the inner graft's task, and a
// busy gate, whole. Without such a variable the exec goes to the host.
func TestCallerSetVariablesKeepTheTaskHere(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("graft reads its caller's environment from /proc")
	}
	f, state := userRemoteRepo(t, "  vet:\n    run: [[go, vet, ./...]]\n")
	loaded(t, state)
	calmAfter(t, state, 1500*time.Millisecond)
	out, code := f.exec(f.work, "", "sh", "-c", `PROBE_FLAG=1 "$@"; exit $?`, "sh", graftBin, "exec", "--heavy", "--", "go", "vet", "./...")
	if code != 0 || callsOf(t, state) != "" || !strings.Contains(out, "heavy, on this machine: the caller set PROBE_FLAG, which pc would not get") {
		t.Errorf("exit %d, calls %q:\n%s", code, callsOf(t, state), out)
	}

	loaded(t, state)
	calmAfter(t, state, 1500*time.Millisecond)
	out, code = f.exec(f.work, "", "sh", "-c", `PROBE_FLAG=1 "$@"; exit $?`, "sh", graftBin, "exec", "--heavy", "--", "graft", "vet")
	if code != 0 || callsOf(t, state) != "" || !strings.Contains(out, "graft: vet: heavy, on this machine: the caller set PROBE_FLAG") {
		t.Errorf("exec of graft: exit %d, calls %q:\n%s", code, callsOf(t, state), out)
	}

	f.write(".graft.yaml", splitGateConfig)
	loaded(t, state)
	calmAfter(t, state, 1500*time.Millisecond)
	out, code = f.exec(f.work, "", "sh", "-c", `PROBE_FLAG=1 "$@"; exit $?`, "sh", graftBin, "gate")
	if code != 0 || callsOf(t, state) != "" || !strings.Contains(out, "graft: gate: busy here, waits for this machine whole: the caller set PROBE_FLAG") {
		t.Errorf("gate: exit %d, calls %q:\n%s", code, callsOf(t, state), out)
	}

	loaded(t, state)
	out = f.mustGraft("exec", "--heavy", "--", "go", "vet", "./...")
	if !strings.Contains(callsOf(t, state), "go vet ./...") {
		t.Errorf("an exec without variables of its caller did not go to pc:\n%s", out)
	}
}
