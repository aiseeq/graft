package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// calmAfter makes the loaded machine of state calm after d. The returned
// stop drops a calm still to come once a graft that need not wait is over.
func calmAfter(t *testing.T, state string, d time.Duration) (stop func()) {
	t.Helper()
	done := make(chan struct{})
	timer := time.AfterFunc(d, func() {
		defer close(done)
		setRunnable(t, filepath.Join(state, "proc"), 1)
	})
	stop = sync.OnceFunc(func() {
		if !timer.Stop() {
			<-done
		}
	})
	t.Cleanup(stop)
	return stop
}

const splitGateConfig = "schema: 1\nversion: {mode: none}\n" +
	"gate:\n  - task: unit\n  - task: script\n" +
	"tasks:\n" +
	"  unit:\n    heavy: true\n    remote: true\n    run: [[go, test, -count=1, ./...]]\n" +
	"  script:\n    run: [[sh, -c, 'echo here >> out']]\n"

// TestBusyGateSendsItsGoStepsToTheHost: a gate that would wait for this
// machine sends the go test and go vet steps that write no file of its
// remote: true tasks to the user's remote_heavy host, nested tasks' too, and
// runs every other step here, the gate's own go vet included, under one
// admission of this machine's queue; on a calm machine the whole gate runs
// here.
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
	out := runUntilMachineWait(t, f, state, "gate")
	calls := callsOf(t, state)
	if !strings.Contains(calls, "go test -count=1 ./...") {
		t.Errorf("run got %q, want the remote: true task's go test:\n%s", calls, out)
	}
	for _, here := range []string{"echo here", "fix", "gofmt"} {
		if strings.Contains(calls, here) {
			t.Errorf("%q went to pc: %q", here, calls)
		}
	}
	if got := readFile(t, f.path("out")); got != "here\n" {
		t.Errorf("the script step did not run here: out = %q\n%s", got, out)
	}
	if !strings.Contains(out, "graft: gate: busy here: built-in go vet and go test and go vet steps of remote: true tasks on pc, the rest waits for this machine once") {
		t.Errorf("the gate does not say it split:\n%s", out)
	}
	// The steps kept here waited for this machine.
	if !strings.Contains(out, "starting after") {
		t.Errorf("no step waited for this machine:\n%s", out)
	}
}

const builtinSplitGateConfig = "schema: 1\nversion: {mode: none}\ngo_fix: {enabled: true}\n" +
	"gate:\n  - task: script\ntasks:\n" +
	"  script:\n    run: [[sh, -c, 'echo here >> out']]\n"

// TestBusyGateSendsBuiltinVetToTheHost sends the read-only built-in Go
// checks to the user's host while gofmt and configured scripts stay here.
func TestBusyGateSendsBuiltinVetToTheHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, "")
	f.write(".graft.yaml", builtinSplitGateConfig)
	loaded(t, state)
	out := runUntilMachineWait(t, f, state, "gate")
	calls := callsOf(t, state)
	for _, want := range []string{"go vet ./...", "go fix -diff ./..."} {
		if !strings.Contains(calls, want) {
			t.Errorf("run got %q, want built-in %q:\n%s", calls, want, out)
		}
	}
	if strings.Contains(calls, "gofmt") || strings.Contains(calls, "echo here") {
		t.Errorf("a local gate step went to pc: %q", calls)
	}
	if got := readFile(t, f.path("out")); got != "here\n" {
		t.Errorf("the script step did not run here: out = %q\n%s", got, out)
	}
	if n := strings.Count(out, "starting after"); n != 1 {
		t.Errorf("the steps here waited for this machine %d times, want once:\n%s", n, out)
	}
	if !strings.Contains(out, "built-in go vet and built-in go fix -diff") {
		t.Errorf("the gate does not name its built-in remote steps:\n%s", out)
	}
}

// TestBusyGateBuiltinVetBackHereWhenThePathBreaks reruns a built-in vet here
// after the host path breaks and lets the gate pass.
func TestBusyGateBuiltinVetBackHereWhenThePathBreaks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, "")
	f.write(".graft.yaml", builtinSplitGateConfig)
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(f.base, "go-bin")
	if err := os.MkdirAll(shim, 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(f.base, "go-calls")
	writeFile(t, filepath.Join(shim, "go"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+yamlQuote(log)+"\nexec "+yamlQuote(goPath)+" \"$@\"\n")
	if err := os.Chmod(filepath.Join(shim, "go"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.env = append(f.env, "PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	writeFile(t, filepath.Join(state, "broken"), "")
	loaded(t, state)
	out := runUntilMachineWait(t, f, state, "gate")
	if !strings.Contains(out, "pc path broken") || !strings.Contains(out, "running here") {
		t.Errorf("the broken built-in vet path is not said:\n%s", out)
	}
	if got := readFile(t, log); !strings.Contains(got, "vet ./...") {
		t.Errorf("go vet was not repeated here after the path broke: go calls = %q\n%s", got, out)
	}
}

func TestBusyGateDescriptionOmitsBuiltinKeptInSubdirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, "")
	f.write(".graft.yaml", "schema: 1\nversion: {mode: none}\n"+
		"gate:\n  - task: lint\n  - task: script\ntasks:\n"+
		"  lint:\n    heavy: true\n    remote: true\n    run: [[glint, check, ./...]]\n"+
		"  script:\n    run: [[sh, -c, 'echo here >> out']]\n")
	if err := os.Remove(f.path("go.mod")); err != nil {
		t.Fatal(err)
	}
	f.write("sub/go.mod", "module example.com/sub\n\ngo 1.22\n")
	f.write("sub/a.go", "package sub\n")
	path, sum := fakeGlint(t, f)
	writeHostGlint(t, state, "pc", path, sum)
	loaded(t, state)
	out := runUntilMachineWait(t, f, state, "gate")
	if !strings.Contains(out, "glint check steps of remote: true tasks") || strings.Contains(out, "built-in go vet") {
		t.Errorf("the description named a built-in check kept in the subdirectory:\n%s", out)
	}
	if calls := callsOf(t, state); !strings.Contains(calls, "glint check ./...") || strings.Contains(calls, "go vet") {
		t.Errorf("unexpected host steps with the subdirectory check kept here: %q\n%s", calls, out)
	}
}

func TestBusyGateDescriptionNamesRemoteGlint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, "  lint:\n    heavy: true\n    remote: true\n    run: [[glint, check, ./...]]\n")
	cfg := strings.Replace(builtinSplitGateConfig, "gate:\n  - task: script\n", "gate:\n  - task: lint\n  - task: script\n", 1) +
		"  lint:\n    heavy: true\n    remote: true\n    run: [[glint, check, ./...]]\n"
	f.write(".graft.yaml", cfg)
	path, sum := fakeGlint(t, f)
	writeHostGlint(t, state, "pc", path, sum)
	loaded(t, state)
	out := runUntilMachineWait(t, f, state, "gate")
	if !strings.Contains(out, "glint check steps of remote: true tasks") {
		t.Errorf("the description omitted the eligible glint step:\n%s", out)
	}
	if !strings.Contains(callsOf(t, state), "glint check ./...") {
		t.Errorf("the eligible glint step did not go to pc: %q\n%s", callsOf(t, state), out)
	}
}

// TestBusyGateBuiltinVetStaysWithProjectRemoteHeavy keeps the built-in vet
// local when the project has its own remote_heavy setting.
func TestBusyGateBuiltinVetStaysWithProjectRemoteHeavy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, "")
	f.write(".graft.yaml", "schema: 1\nversion: {mode: none}\n"+
		"go_fix: {enabled: true}\n"+
		"remote_heavy: {host: pchost, up: [sh, -c, 'true'], run: [sh, -c, 'true'], threads: 8}\n"+
		"gate: [{task: script}]\ntasks:\n  script:\n    run: [[sh, -c, 'echo here >> out']]\n")
	loaded(t, state)
	out := runUntilMachineWait(t, f, state, "gate")
	if calls := callsOf(t, state); calls != "" {
		t.Errorf("built-in checks went to the user's host: %q\n%s", calls, out)
	}
	if strings.Contains(out, "gate: busy here") {
		t.Errorf("project remote_heavy gate was split:\n%s", out)
	}
}

// TestBusyGateWaitsForThisMachineOnce: the steps a split gate keeps here
// share one admission, taken by the first of them and held to the gate's
// end: a busy machine does not hold each cheap step of the gate in the queue
// again.
func TestBusyGateWaitsForThisMachineOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, "")
	proc := filepath.Join(state, "proc")
	// The first step here loads the machine again, as the other tasks of a
	// busy machine do.
	writeFile(t, filepath.Join(state, "first.sh"), "echo first >> out\n"+
		"sed -i 's/^procs_running .*/procs_running 31/' "+yamlQuote(filepath.Join(proc, "stat"))+"\n")
	f.write(".graft.yaml", "schema: 1\nversion: {mode: none}\n"+
		"gate:\n  - task: unit\n  - task: first\n  - task: second\n"+
		"tasks:\n"+
		"  unit:\n    heavy: true\n    remote: true\n    run: [[go, test, -count=1, ./...]]\n"+
		"  first:\n    run: [[sh, "+yamlQuote(filepath.Join(state, "first.sh"))+"]]\n"+
		"  second:\n    run: [[sh, -c, 'echo second >> out']]\n")
	loaded(t, state)
	cmd, outBuf := startWaitingForMachine(t, f, "gate")
	setRunnable(t, proc, 1)
	// A gate that waits again for the second step finds the machine calm a
	// second after the first step loaded it, so that it still ends.
	done := make(chan struct{})
	go func() {
		defer close(done)
		var seen string
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			data, _ := os.ReadFile(f.path("out"))
			if seen = string(data); strings.Contains(seen, "first") {
				time.Sleep(time.Second)
				setRunnable(t, proc, 1)
				return
			}
		}
		t.Errorf("the first step did not run in 20s: out = %q", seen)
		setRunnable(t, proc, 1)
	}()
	t.Cleanup(func() { <-done })
	if err := cmd.Wait(); err != nil {
		t.Fatalf("graft gate: %v\n%s", err, outBuf.String())
	}
	out := outBuf.String()
	if got := readFile(t, f.path("out")); got != "first\nsecond\n" {
		t.Errorf("the steps did not run here in order: out = %q\n%s", got, out)
	}
	if !strings.Contains(callsOf(t, state), "go test -count=1 ./...") {
		t.Errorf("the go test did not go to pc: %q\n%s", callsOf(t, state), out)
	}
	if n := strings.Count(out, "starting after"); n != 1 {
		t.Errorf("the steps here waited for this machine %d times, want once:\n%s", n, out)
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
	out := runUntilMachineWait(t, f, state, "gate")
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
	stop := calmAfter(t, state, 3*time.Second)
	out := f.mustGraft("exec", "--heavy", "--", "graft", "vet")
	stop()
	if !strings.Contains(callsOf(t, state), "go vet ./...") || !strings.Contains(out, "graft: vet: heavy, on pc") {
		t.Errorf("the inner vet, heavy by the exec, did not go to pc:\n%s\ncalls: %q", out, callsOf(t, state))
	}
	if strings.Contains(out, "graft: exec: heavy, on this machine") || strings.Contains(out, "starting after") {
		t.Errorf("the outer exec placed itself or waited:\n%s", out)
	}

	loaded(t, state)
	out = runUntilMachineWait(t, f, state, "exec", "--heavy", "--", "graft", "script")
	if n := strings.Count(out, "starting after"); n != 1 || !strings.Contains(out, "graft: script: runnable") {
		t.Errorf("want the inner script alone to wait for this machine, %d waits:\n%s", n, out)
	}
	if got := readFile(t, f.path("out")); got != "here\n" {
		t.Errorf("out = %q", got)
	}
}
