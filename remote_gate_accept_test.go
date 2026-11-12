package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/lock"
	"github.com/aiseeq/graft/v2/internal/stats"
	"github.com/aiseeq/graft/v2/internal/tasks"
)

const splitGateConfig = "schema: 1\nversion: {mode: none}\n" +
	"gate:\n  - task: unit\n  - task: script\n" +
	"tasks:\n" +
	"  unit:\n    heavy: true\n    remote: true\n    run: [[go, test, -count=1, ./...]]\n" +
	"  script:\n    run: [[sh, -c, 'echo here >> out']]\n"

func startNestedGate(t *testing.T, f *fixture, state, nested string) (*exec.Cmd, *syncBuffer) {
	t.Helper()
	ready := filepath.Join(state, "parent-ready")
	trigger := filepath.Join(state, "start-nested")
	script := "echo ready > " + yamlQuote(ready) + "; while [ ! -e " + yamlQuote(trigger) + " ]; do sleep 0.01; done; " + nested
	cmd, out := f.start(f.work, "exec", "--heavy", "--", "sh", "-c", script)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			stopTestChild(cmd)
			_ = cmd.Wait()
			t.Fatalf("outer heavy command did not start its child:\n%s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	loaded(t, state)
	writeFile(t, trigger, "")
	return cmd, out
}

func nestedGateFixture(t *testing.T) (*fixture, string, string) {
	t.Helper()
	f, state := userRemoteRepo(t, "")
	cfg := "schema: 1\nversion: {mode: none}\ngo_fix: {enabled: false}\ngo_fmt: {enabled: false, reason: disabled in this test}\ngo_vet: {enabled: false, reason: disabled in this test}\n" +
		"gate:\n  - task: unit\n  - task: script\ntasks:\n" +
		"  unit:\n    heavy: true\n    remote: true\n    run: [[go, test, -count=1, ./...]]\n" +
		"  script:\n    run: [[sh, -c, 'echo here >> out']]\n"
	f.write(".graft.yaml", cfg)
	return f, state, filepath.Join(state, "cache", "graft", "stats.jsonl")
}

func waitNestedCommand(cmd *exec.Cmd) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		stopTestChild(cmd)
		<-done
		return os.ErrDeadlineExceeded
	}
}

func TestNestedBusyGateSplits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state, statsFile := nestedGateFixture(t)
	cmd, out := startNestedGate(t, f, state, graftBin+" gate")
	err := waitNestedCommand(cmd)
	text := out.String()
	calls := callsOf(t, state)
	if err != nil || !strings.Contains(calls, "go test -count=1 ./...") || readFile(t, f.path("out")) != "here\n" {
		t.Fatalf("nested gate did not split go and shell steps (exit %v):\n%s\ncalls: %q", err, text, calls)
	}
	if !strings.Contains(text, "the rest runs under the admitted task") || strings.Contains(text, "starting after") || strings.Contains(text, "waiting in the load.lock queue") {
		t.Errorf("nested split message or admission is wrong:\n%s", text)
	}
	found := false
	for _, rec := range readStats(t, statsFile) {
		if rec.Kind == "task" && rec.Task == "gate" && rec.Here == tasks.WhyGateSplit {
			found = true
		}
	}
	if !found {
		t.Errorf("nested gate stats lack WhyGateSplit: %+v", readStats(t, statsFile))
	}
}

func TestNestedCalmGateStaysHere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state, statsFile := nestedGateFixture(t)
	ready := filepath.Join(state, "parent-ready")
	script := "echo ready > " + yamlQuote(ready) + "; while [ ! -e " + yamlQuote(filepath.Join(state, "start-nested")) + "; do sleep 0.01; done; " + graftBin + " gate"
	cmd, out := f.start(f.work, "exec", "--heavy", "--", "sh", "-c", script)
	waitFor(t, func() bool { _, err := os.Stat(ready); return err == nil })
	reserveData, err := json.Marshal(struct {
		Cores    int    `json:"cores"`
		Admitted string `json:"admitted"`
		Task     string `json:"task"`
	}{22, time.Now().Format(time.RFC3339Nano), "projectA"})
	if err != nil {
		t.Fatal(err)
	}
	releaseReserve, err := lock.Reserve(filepath.Join(state, "cache", "graft"), "load.lock.*.reserve", reserveData)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := releaseReserve(); err != nil {
			t.Error(err)
		}
	}()
	writeFile(t, filepath.Join(state, "start-nested"), "")
	if err := waitNestedCommand(cmd); err != nil {
		t.Fatalf("nested calm gate: %v\n%s", err, out.String())
	}
	if callsOf(t, state) != "" {
		t.Errorf("calm nested gate called remote run: %q", callsOf(t, state))
	}
	parentReserved := false
	for _, rec := range readStats(t, statsFile) {
		if rec.Kind == "task" && rec.Task == "exec" && rec.ReserveCores > 0 {
			parentReserved = true
		}
	}
	if !parentReserved {
		t.Errorf("outer admitted task did not retain a live reservation: %+v", readStats(t, statsFile))
	}
	found := false
	for _, rec := range readStats(t, statsFile) {
		if rec.Kind == "task" && rec.Task == "gate" && rec.Here == tasks.WhyInsideHeavy {
			found = true
		}
	}
	if !found {
		t.Errorf("calm nested gate lacks WhyInsideHeavy: %+v", readStats(t, statsFile))
	}
}

func TestNestedGateHeavyLocal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state, _ := nestedGateFixture(t)
	cmd, out := startNestedGate(t, f, state, "GRAFT_HEAVY=local "+graftBin+" gate")
	if err := waitNestedCommand(cmd); err != nil {
		t.Fatalf("nested local gate: %v\n%s", err, out.String())
	}
	if calls := callsOf(t, state); calls != "" {
		t.Errorf("GRAFT_HEAVY=local called run: %q\n%s", calls, out.String())
	}
	if !strings.Contains(readFile(t, f.path("out")), "here") {
		t.Errorf("local nested gate did not run shell step:\n%s", out.String())
	}
}

func TestNestedGateHostFailureStaysAdmitted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	for _, mode := range []string{"up unavailable", "broken exit"} {
		t.Run(mode, func(t *testing.T) {
			f, state, _ := nestedGateFixture(t)
			nested := graftBin + " gate"
			if mode == "broken exit" {
				writeFile(t, filepath.Join(state, "broken"), "")
			}
			if mode == "up unavailable" {
				if err := os.Remove(filepath.Join(state, "up")); err != nil {
					t.Fatal(err)
				}
			}
			cmd, out := startNestedGate(t, f, state, nested)
			if err := waitNestedCommand(cmd); err != nil {
				t.Fatalf("nested gate %s: %v\n%s", mode, err, out.String())
			}
			if strings.Contains(out.String(), "starting after") || strings.Contains(out.String(), "waiting in the load.lock queue") {
				t.Errorf("nested gate queued again:\n%s", out.String())
			}
			if mode == "up unavailable" && (strings.Contains(out.String(), "waits for this machine") || !strings.Contains(out.String(), "runs here whole under the admitted task")) {
				t.Errorf("nested gate with the host down says it waits for this machine:\n%s", out.String())
			}
			if !strings.Contains(readFile(t, f.path("out")), "here") {
				t.Errorf("gate rest did not run under parent admission:\n%s", out.String())
			}
		})
	}
}

// TestNestedGateWhenAlways: with when: always a nested gate is not split and
// says it runs inside the admitted task, not that when: always placed it.
func TestNestedGateWhenAlways(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state, statsFile := nestedGateFixture(t)
	// userRemoteRepo's user config, with when: always.
	for _, kv := range f.env {
		if dir, ok := strings.CutPrefix(kv, "XDG_CONFIG_HOME="); ok {
			writeFile(t, filepath.Join(dir, "graft", "config.yaml"), "load: {threads: 6, poll: 20ms, settle: 10ms}\nremote_heavy: {host: pc, up: [sh, "+yamlQuote(filepath.Join(state, "up.sh"))+"], run: [sh, "+yamlQuote(filepath.Join(state, "run.sh"))+"], threads: 8, when: always}\n")
			writeTestPressure(t, dir)
		}
	}
	cmd, out := startNestedGate(t, f, state, graftBin+" gate")
	if err := waitNestedCommand(cmd); err != nil {
		t.Fatalf("nested gate with when: always: %v\n%s", err, out.String())
	}
	found := false
	for _, rec := range readStats(t, statsFile) {
		if rec.Kind == "task" && rec.Task == "gate" && rec.Here == tasks.WhyInsideHeavy {
			found = true
		}
	}
	if !found || strings.Contains(out.String(), "the rest runs under the admitted task") {
		t.Errorf("nested gate with when: always was split or lacks WhyInsideHeavy:\n%s\n%+v", out.String(), readStats(t, statsFile))
	}
}

func TestNestedNonGateTaskStaysInsideHeavy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, "  unit:\n    heavy: true\n    remote: true\n    run: [[go, vet, ./...]]\n")
	cmd, out := startNestedGate(t, f, state, graftBin+" unit")
	if err := waitNestedCommand(cmd); err != nil {
		t.Fatalf("nested task: %v\n%s", err, out.String())
	}
	found := false
	for _, rec := range readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl")) {
		if rec.Kind == "task" && rec.Task == "unit" && rec.Here == tasks.WhyInsideHeavy {
			found = true
		}
	}
	if callsOf(t, state) != "" || !found {
		t.Errorf("nested task behavior changed (inside reason recorded=%v):\n%s\ncalls: %q", found, out.String(), callsOf(t, state))
	}
}

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
	var gateHere []string
	for _, l := range readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl")) {
		if l.Kind == "task" && l.Task == stats.GateTask {
			gateHere = append(gateHere, l.Here)
		}
	}
	if want := []string{tasks.WhyNotBusy, tasks.WhyGateSplit}; !slices.Equal(gateHere, want) {
		t.Errorf("gate lines here = %q, want %q", gateHere, want)
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
	// go vet ran here after the path broke, go fix after it did not try pc.
	var here []string
	for _, l := range readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl")) {
		if l.Kind == "step" && l.Host == "" && (strings.HasPrefix(l.Step, "go vet") || strings.HasPrefix(l.Step, "go fix")) {
			here = append(here, l.Step+": "+l.Here)
		}
	}
	broken := "pc path broken: run exited 125: connection to pc lost"
	if want := []string{"go vet ./...: " + broken, "go fix -diff ./...: " + broken}; !slices.Equal(here, want) {
		t.Errorf("here of go steps run on this machine = %q, want %q", here, want)
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
	g := startWaitingForMachine(t, f, "gate")
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
	out := g.end(t)
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
	// The machine stays loaded until the exec ends: one that queued itself
	// is calmed after 10s so that it ends, and says it waited.
	g := startGraft(t, f, "exec", "--heavy", "--", "graft", "vet")
	select {
	case <-time.After(10 * time.Second):
		setRunnable(t, filepath.Join(state, "proc"), 1)
	case err := <-g.done:
		g.done <- err
	}
	out := g.end(t)
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
