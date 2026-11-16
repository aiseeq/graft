package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBusyGateKeepsHereAStepItsTaskKeepsHere: a split gate sends eligible
// steps to the host and keeps tasks without remote: true on this machine.
func TestBusyGateKeepsHereAStepItsTaskKeepsHere(t *testing.T) {
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
	const genericHere = "the gate only sends steps of remote: true tasks to user remote_heavy"
	const remoteFalseHere = "the task has remote: false"
	foundHere := map[string]string{}
	for _, rec := range readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl")) {
		if rec.Kind == "step" && strings.HasPrefix(rec.Task, "local-") {
			foundHere[rec.Task] = rec.Here
		}
	}
	for task, want := range map[string]string{
		"local-heavy": genericHere,
		"local-plain": genericHere,
		"local-kept":  remoteFalseHere,
	} {
		if foundHere[task] != want {
			t.Errorf("%s here = %q, want %q", task, foundHere[task], want)
		}
		if strings.Contains(out, want) {
			t.Errorf("non-vouched placement reason for %s leaked to output:\n%s", task, out)
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
	foundFmtHere := false
	for _, rec := range readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl")) {
		if rec.Kind == "step" && rec.Step == "gofmt -l" {
			foundFmtHere = rec.Here == "gofmt is a local gate check"
		}
	}
	if !foundFmtHere {
		t.Errorf("gofmt step stats lack its local reason:\n%s", out)
	}
}

// TestBusyGateWithoutEligibleStepsWaitsWhole keeps a gate together when it
// has no built-in Go checks and no remote: true task checks.
func TestBusyGateWithoutEligibleStepsWaitsWhole(t *testing.T) {
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

func TestBusySplitGateSendsRemoteVouchedShellStep(t *testing.T) {
	f, state := userRemoteRepo(t, "")
	hostDir := filepath.Join(state, "pc-work")
	writeFile(t, filepath.Join(hostDir, "placeholder"), "")
	writeFile(t, filepath.Join(state, "run.sh"), `echo "$*" >> "`+filepath.Join(state, "calls")+`"
cd "`+hostDir+`" || exit 1
exec "$@"
`)
	f.write("cmd/tool/main.go", "package main\nfunc main() {}\n")
	installDir := filepath.Join(state, "installed")
	f.write(".graft.yaml", "schema: 1\nversion: {mode: none}\n"+
		"go_vet: {enabled: false, reason: disabled in this test}\ngo_fix: {enabled: false}\ngo_fmt: {enabled: false, reason: disabled in this test}\n"+
		"gate: [{sh: 'echo direct > direct-output'}, {task: single}, {task: unit}, {task: light}, {task: plain}, {task: installer}]\ntasks:\n"+
		"  build-tool:\n    run: [[go, build, -o, bin/tool, ./cmd/tool]]\n"+
		"  single:\n    heavy: true\n    remote: true\n    run: [{sh: 'echo host > single-output'}]\n"+
		"  unit:\n    heavy: true\n    remote: true\n    run:\n"+"      - {sh: 'printf local > generated'}\n"+"      - {sh: 'test \"$(cat generated)\" = local && echo consumed > consumed'}\n"+
		"  light:\n    remote: true\n    run:\n"+"      - {sh: 'echo light > light-file'}\n"+"      - {sh: 'test \"$(cat light-file)\" = light'}\n"+
		"  plain:\n    heavy: true\n    run: [{sh: 'test -e generated && echo local > local-output'}]\n"+
		"  installer:\n    heavy: true\n    remote: true\n    run: [{install: bin/tool, to: "+yamlQuote(installDir)+"}]\n")
	f.mustPlainCommit("split gate placement fixture")
	f.mustGraft("build-tool")
	loaded(t, state)
	out := runUntilMachineWait(t, f, state, "gate")
	calls := callsOf(t, state)
	if !strings.Contains(calls, "echo host > single-output") {
		t.Errorf("one-step remote shell did not run on pc: calls=%q\n%s", calls, out)
	}
	if got := readFile(t, filepath.Join(hostDir, "single-output")); got != "host\n" {
		t.Errorf("the host did not create the single-step output: %q\n%s", got, out)
	}
	if got := f.read("consumed"); got != "consumed\n" {
		t.Errorf("two-step shell task did not keep its file local: %q\n%s", got, out)
	}
	if got := f.read("local-output"); got != "local\n" {
		t.Errorf("task without remote: true did not run locally: %q\n%s", got, out)
	}
	if _, err := os.Stat(filepath.Join(installDir, "tool")); err != nil {
		t.Errorf("remote: true install step was not performed locally: %v\n%s", err, out)
	}
	const localReason = "the gate only sends steps of remote: true tasks to user remote_heavy"
	const directReason = "only built-in go vet and go fix -diff steps can use user remote_heavy"
	const installReason = "install steps run on this machine"
	const mixedReason = "a split gate sends a step of a remote: true task only when it is the whole task"
	if strings.Contains(out, localReason) || strings.Contains(out, directReason) {
		t.Errorf("non-vouched step reasons should be stats-only:\n%s", out)
	}
	for _, line := range []string{"graft: unit: " + mixedReason, "graft: light: " + mixedReason, "graft: installer: " + installReason} {
		if strings.Count(out, line) != 1 {
			t.Errorf("expected one reason line %q:\n%s", line, out)
		}
	}
	unitSteps, singleHost, localStep, directStep, installTask, lightHere := 0, false, false, false, false, false
	for _, rec := range readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl")) {
		switch {
		case rec.Kind == "step" && rec.Task == "unit":
			unitSteps++
			if rec.Host != "" || rec.Here != mixedReason {
				t.Errorf("multi-step task step placement = host %q here %q, want local with reason: %+v", rec.Host, rec.Here, rec)
			}
		case rec.Kind == "step" && rec.Task == "single":
			singleHost = rec.Host == "pc"
		case rec.Kind == "step" && rec.Task == "light":
			lightHere = rec.Host == "" && rec.Here == mixedReason
		case rec.Kind == "step" && rec.Task == "plain" && strings.Contains(rec.Step, "local-output"):
			localStep = rec.Host == "" && rec.Here == localReason
		case rec.Kind == "step" && rec.Task == "gate" && strings.Contains(rec.Step, "direct-output"):
			directStep = rec.Host == "" && rec.Here == directReason
		case rec.Kind == "task" && rec.Task == "installer":
			installTask = rec.Host == "" && rec.Here == installReason
		}
	}
	if unitSteps != 2 {
		t.Errorf("multi-step task has %d steps, want 2\n%s", unitSteps, out)
	}
	if !singleHost {
		t.Errorf("single-step remote task has no host record:\n%s", out)
	}
	if !localStep {
		t.Errorf("task without remote: true has no exact local here reason:\n%s", out)
	}
	if !directStep {
		t.Errorf("direct gate step has no exact local here reason:\n%s", out)
	}
	if !installTask {
		t.Errorf("install task was not recorded here with its reason:\n%s", out)
	}
	if !lightHere {
		t.Errorf("non-heavy remote task step has no exact here reason:\n%s", out)
	}
}

func TestBusyGateRemoteGoCheckWithInstallStillGoesToHost(t *testing.T) {
	f, state := userRemoteRepo(t, "")
	f.write("cmd/tool/main.go", "package main\nfunc main() {}\n")
	installDir := filepath.Join(state, "installed")
	f.write(".graft.yaml", "schema: 1\nversion: {mode: none}\n"+
		"go_vet: {enabled: false, reason: disabled in this test}\ngo_fix: {enabled: false}\ngo_fmt: {enabled: false, reason: disabled in this test}\n"+
		"gate: [{task: unit}]\ntasks:\n"+
		"  build-tool:\n    run: [[go, build, -o, bin/tool, ./cmd/tool]]\n"+
		"  unit:\n    heavy: true\n    remote: true\n    run:\n"+"      - [go, test, -count=1, ./...]\n"+"      - install: bin/tool\n        to: "+yamlQuote(installDir)+"\n")
	f.mustPlainCommit("remote go check with install fixture")
	f.mustGraft("build-tool")
	loaded(t, state)
	out := runUntilMachineWait(t, f, state, "gate")
	if !strings.Contains(callsOf(t, state), "go test -count=1 ./...") {
		t.Fatalf("go test did not go to pc with a later install step:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(installDir, "tool")); err != nil {
		t.Fatalf("install step did not remain local: %v\n%s", err, out)
	}
	goOnHost, installHere := false, false
	for _, rec := range readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl")) {
		if rec.Kind == "step" && rec.Task == "unit" && strings.HasPrefix(rec.Step, "go test") {
			goOnHost = rec.Host == "pc"
		}
		if rec.Kind == "task" && rec.Task == "unit" {
			installHere = rec.Host == "" && rec.Here == "install steps run on this machine"
		}
	}
	if !goOnHost || !installHere {
		t.Errorf("go/install placement: go on host=%v, install here=%v\n%s", goOnHost, installHere, out)
	}
}
