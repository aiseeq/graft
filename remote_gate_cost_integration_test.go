package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/stats"
	"github.com/aiseeq/graft/v2/internal/tasks"
)

type gateCostFixture struct {
	f         *fixture
	state     string
	statsFile string
	profile   string
	step      string
	project   string
}

func newGateCostFixture(t *testing.T, cfg string) gateCostFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake remote command uses sh")
	}
	f, state := userRemoteRepo(t, "")
	f.write(".graft.yaml", cfg)
	statsFile := filepath.Join(state, "cache", "graft", "stats.jsonl")
	// A calm run supplies the exact profile, project and normalized step text.
	f.mustGraft("gate")
	var sample stats.Record
	records, _, err := stats.LoadTail(statsFile, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range records {
		if rec.Kind == stats.KindStep {
			sample = rec
			break
		}
	}
	if sample.Profile == "" || sample.Project == "" || sample.Step == "" {
		t.Fatalf("calm run did not record built-in vet sample: %+v", sample)
	}
	return gateCostFixture{f: f, state: state, statsFile: statsFile, profile: sample.Profile, step: sample.Step, project: sample.Project}
}

func (g gateCostFixture) writeHistory(t *testing.T, locals, hosts []stats.Record) {
	t.Helper()
	var file strings.Builder
	write := func(rec stats.Record) {
		rec.Kind = stats.KindStep
		if rec.Project == "" {
			rec.Project = g.project
		}
		if rec.Profile == "" {
			rec.Profile = g.profile
		}
		if rec.Step == "" {
			rec.Step = g.step
		}
		if rec.Task == "" {
			rec.Task = "gate"
		}
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		file.Write(data)
		file.WriteByte('\n')
	}
	if g.project == "" {
		g.project = gProject(t, g)
	}
	for _, rec := range locals {
		write(rec)
	}
	for _, rec := range hosts {
		write(rec)
	}
	if err := os.WriteFile(g.statsFile, []byte(file.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gProject(t *testing.T, g gateCostFixture) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(g.f.work)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func gateCostRows(walls []float64, host string, exit int) []stats.Record {
	rows := make([]stats.Record, len(walls))
	for i, wall := range walls {
		rows[i] = stats.Record{WallS: wall, Host: host, Exit: exit}
	}
	return rows
}

func runBusyCostGate(t *testing.T, g gateCostFixture, remoteDelay bool) (string, string) {
	t.Helper()
	if remoteDelay {
		writeFile(t, filepath.Join(g.state, "run.sh"), "echo \"$*\" >> \""+filepath.Join(g.state, "calls")+"\"\nsleep 1.2\nexit 0\n")
	}
	loaded(t, g.state)
	out := runUntilMachineWait(t, g.f, g.state, "gate")
	return out, callsOf(t, g.state)
}

const gateCostVetConfig = "schema: 1\nversion: {mode: none}\ngo_fix: {enabled: false}\n" +
	"gate: [{task: script}]\ntasks:\n  script:\n    run: [[sh, -c, 'echo local >> out']]\n"

// gateCostHostAskedConfig has a remote task whose step has no local price.
const gateCostHostAskedConfig = "schema: 1\nversion: {mode: none}\ngo_fix: {enabled: false}\n" +
	"gate:\n  - task: unit\n  - task: script\ntasks:\n" +
	"  unit:\n    heavy: true\n    remote: true\n    run: [[go, test, -run, Nothing, ./...]]\n" +
	"  script:\n    run: [[sh, -c, 'echo local >> out']]\n"

func TestBusyGateCheaperHereHistory(t *testing.T) {
	g := newGateCostFixture(t, gateCostVetConfig)
	g.writeHistory(t, gateCostRows([]float64{0.1, 0.2, 0.3}, "", 0), gateCostRows([]float64{40, 41}, "pc", 0))
	out, calls := runBusyCostGate(t, g, false)
	if calls != "" || !strings.Contains(out, "cheaper here: 0.2s here, 40.0s on pc") {
		t.Fatalf("expected vet kept here with its reason; calls=%q\n%s", calls, out)
	}
	if strings.Contains(out, "gate: busy here:") || !strings.Contains(out, "starting after") {
		t.Errorf("all-local gate should wait whole, without splitting:\n%s", out)
	}
	found := false
	for _, rec := range readStats(t, g.statsFile) {
		if rec.Kind == "step" && rec.Step == "go vet ./..." && rec.Here == "cheaper here: 0.2s here, 40.0s on pc" {
			found = true
		}
	}
	if !found {
		t.Errorf("step history has no here reason: %+v", readStats(t, g.statsFile))
	}
}

func TestBusyGateLocalHistoryIsProfileSpecificAcrossProjects(t *testing.T) {
	g := newGateCostFixture(t, gateCostVetConfig)
	locals := gateCostRows([]float64{0.2, 0.2}, "", 0)
	for i := range locals {
		locals[i].Project = filepath.Join(filepath.Dir(g.project), "projectB")
	}
	g.writeHistory(t, locals, gateCostRows([]float64{40}, "pc", 0))
	out, calls := runBusyCostGate(t, g, false)
	if calls != "" || !strings.Contains(out, "cheaper here: 0.2s here, 40.0s on pc") {
		t.Fatalf("same-profile local history from another tree should keep vet here; calls=%q\n%s", calls, out)
	}
}

func TestBusyGateHeavyStepHereOverridesGateHere(t *testing.T) {
	const cfg = "schema: 1\nversion: {mode: none}\ngo_fix: {enabled: false}\n" +
		"gate:\n  - task: unit\n  - task: script\ntasks:\n" +
		"  unit:\n    heavy: true\n    remote: true\n    run: [[go, test, -run, Nothing, ./...]]\n" +
		"  script:\n    run: [[sh, -c, 'echo local >> out']]\n"
	g := newGateCostFixture(t, cfg)
	var sample stats.Record
	for _, rec := range readStats(t, g.statsFile) {
		if rec.Kind == stats.KindStep && rec.Task == "unit" && strings.HasPrefix(rec.Step, "go test -run Nothing") {
			sample = stats.Record{Project: rec.Project, Profile: rec.Profile, Task: rec.Task, Step: rec.Step}
			break
		}
	}
	if sample.Profile == "" || sample.Step == "" {
		t.Fatal("calm gate did not record the remote heavy task step")
	}
	locals, hosts := gateCostRows([]float64{0.01, 0.01}, "", 0), gateCostRows([]float64{40}, "pc", 0)
	for i := range locals {
		locals[i].Project, locals[i].Profile, locals[i].Task, locals[i].Step = sample.Project, sample.Profile, sample.Task, sample.Step
	}
	for i := range hosts {
		hosts[i].Project, hosts[i].Profile, hosts[i].Task, hosts[i].Step = sample.Project, sample.Profile, sample.Task, sample.Step
	}
	g.writeHistory(t, locals, hosts)
	_, calls := runBusyCostGate(t, g, false)
	if strings.Contains(calls, "go test -run Nothing") {
		t.Fatalf("cheaper local step was sent to host: %q; sample=%+v; history=%+v", calls, sample, readStats(t, g.statsFile))
	}
	var taskStep *statsLine
	for _, rec := range readStats(t, g.statsFile) {
		if rec.Kind == stats.KindStep && rec.Task == "unit" && rec.Step == sample.Step && rec.Run != "" {
			copy := rec
			taskStep = &copy
		}
	}
	if taskStep == nil || !strings.HasPrefix(taskStep.Here, "cheaper here: 0.0s here,") || taskStep.Here == tasks.WhyGateSplit {
		t.Errorf("heavy gate step here = %+v, want its cheaper-here reason rather than the task's split reason", taskStep)
	}
}

func TestBusyGateHostCheaperHistory(t *testing.T) {
	g := newGateCostFixture(t, gateCostVetConfig)
	g.writeHistory(t, gateCostRows([]float64{60, 60}, "", 0), gateCostRows([]float64{10}, "pc", 0))
	out, calls := runBusyCostGate(t, g, false)
	if !strings.Contains(calls, "go vet ./...") || strings.Contains(out, "cheaper here") {
		t.Fatalf("expected vet sent to host; calls=%q\n%s", calls, out)
	}
}

func TestBusyGateColdProbeThenCheaperHere(t *testing.T) {
	const cfg = "schema: 1\nversion: {mode: none}\ngo_vet: {enabled: false, reason: disabled in this test}\n" +
		"gate:\n  - task: unit\n  - task: script\ntasks:\n" +
		"  unit:\n    heavy: true\n    remote: true\n    run:\n      - [go, test, -run, ProbeOne, ./...]\n      - [go, test, -run, ProbeTwo, ./...]\n      - [go, test, -run, ProbeThree, ./...]\n      - [go, test, -run, ProbeFour, ./...]\n" +
		"  script:\n    run: [[sh, -c, 'echo local >> out']]\n"
	g := newGateCostFixture(t, cfg)
	records, _, err := stats.LoadTail(g.statsFile, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	var local []stats.Record
	for _, rec := range records {
		if rec.Kind == "step" && strings.Contains(rec.Step, "go test -run Probe") {
			local = append(local, stats.Record{Kind: "step", Project: g.project, Profile: rec.Profile, Task: rec.Task, Step: rec.Step, WallS: 0.2, Exit: 0})
			local = append(local, stats.Record{Kind: "step", Project: g.project, Profile: rec.Profile, Task: rec.Task, Step: rec.Step, WallS: 0.2, Exit: 0})
		}
	}
	if len(local) != 8 {
		t.Fatalf("got %d local history rows, want 8", len(local))
	}
	var buf strings.Builder
	for _, rec := range local {
		data, _ := json.Marshal(rec)
		buf.Write(data)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(g.statsFile, []byte(buf.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(g.state, "run.sh"), "echo \"$*\" >> \""+filepath.Join(g.state, "calls")+"\"\nsleep 1.2\nexit 0\n")
	loaded(t, g.state)
	out := runUntilMachineWait(t, g.f, g.state, "gate")
	calls := callsOf(t, g.state)
	if got := strings.Count(calls, "go test -run Probe"); got != 1 {
		t.Fatalf("cold start sent %d steps, want exactly one; calls=%q\n%s", got, calls, out)
	}
	if strings.Count(out, "cheaper here:") != 3 {
		t.Errorf("want three local-cost reasons after the probe:\n%s", out)
	}
}

func TestBusyGateFailedHostHistoryIsProbe(t *testing.T) {
	const cfg = "schema: 1\nversion: {mode: none}\ngo_vet: {enabled: false, reason: disabled in this test}\n" +
		"gate:\n  - task: unit\ntasks:\n  unit:\n    heavy: true\n    remote: true\n    run:\n      - [go, test, -run, FailureOne, ./...]\n      - [go, test, -run, FailureTwo, ./...]\n"
	g := newGateCostFixture(t, cfg)
	baseline, _, err := stats.LoadTail(g.statsFile, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	var locals []stats.Record
	for _, rec := range baseline {
		if rec.Kind != stats.KindStep || !strings.Contains(rec.Step, "go test -run Failure") {
			continue
		}
		for range 2 {
			locals = append(locals, stats.Record{Kind: stats.KindStep, Project: g.project, Profile: rec.Profile, Task: rec.Task, Step: rec.Step, WallS: 0.2, Exit: 0})
		}
	}
	if len(locals) != 4 {
		t.Fatalf("got %d local history rows, want four", len(locals))
	}
	g.writeHistory(t, locals, gateCostRows([]float64{0.1, 0.1}, "pc", 1))
	out, calls := runBusyCostGate(t, g, true)
	if got := strings.Count(calls, "go test -run Failure"); got != 1 || !strings.Contains(out, "cheaper here:") {
		t.Fatalf("failed host rows should leave F unknown, then the successful probe should keep the next step here; calls=%q\n%s", calls, out)
	}
}

func TestBusyGateHostHistoryIsProjectSpecific(t *testing.T) {
	g := newGateCostFixture(t, gateCostVetConfig)
	rows := gateCostRows([]float64{0.1, 0.2}, "", 0)
	other := gateCostRows([]float64{40}, "pc", 0)[0]
	other.Project = filepath.Join(filepath.Dir(g.project), "other")
	g.writeHistory(t, rows, []stats.Record{other})
	out, calls := runBusyCostGate(t, g, false)
	if !strings.Contains(calls, "go vet ./...") || strings.Contains(out, "cheaper here") {
		t.Fatalf("another project's host cost was used; calls=%q\n%s", calls, out)
	}
}

func TestBusyGateAllStepsStayHereWaitsWhole(t *testing.T) {
	g := newGateCostFixture(t, gateCostVetConfig)
	g.writeHistory(t, gateCostRows([]float64{0.2, 0.2}, "", 0), gateCostRows([]float64{40}, "pc", 0))
	out, calls := runBusyCostGate(t, g, false)
	if calls != "" || strings.Contains(out, "gate: busy here:") || strings.Count(out, "starting after") != 1 {
		t.Fatalf("expected whole-gate wait with no host call; calls=%q\n%s", calls, out)
	}
}

func TestBusyGateUnknownLocalStepGoesToHost(t *testing.T) {
	g := newGateCostFixture(t, gateCostVetConfig)
	g.writeHistory(t, nil, gateCostRows([]float64{1}, "pc", 0))
	out, calls := runBusyCostGate(t, g, false)
	if !strings.Contains(calls, "go vet ./...") || strings.Contains(out, "cheaper here") {
		t.Fatalf("step with unknown local price did not use host; calls=%q\n%s", calls, out)
	}
}

// TestBusyGateKeepsTheTaskHereReason: the cost rule leaves the here of a
// split gate's heavy task as it was, and a step the host ran has none.
func TestBusyGateKeepsTheTaskHereReason(t *testing.T) {
	g := newGateCostFixture(t, splitGateConfig)
	if err := os.Remove(g.f.path("out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(g.statsFile); err != nil {
		t.Fatal(err)
	}
	loaded(t, g.state)
	out := runUntilMachineWait(t, g.f, g.state, "gate")
	records, _, err := stats.LoadTail(g.statsFile, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	var task, sent *stats.Record
	for i := range records {
		rec := &records[i]
		switch {
		case rec.Kind == stats.KindTask && rec.Task == "unit":
			task = rec
		case rec.Kind == stats.KindStep && rec.Host == "pc":
			sent = rec
		}
	}
	if task == nil || sent == nil {
		t.Fatalf("want the unit task line and a step sent to pc:\n%s", out)
	}
	if want := "in a gate split by step, each step placed for itself"; task.Here != want {
		t.Errorf("the unit task's here = %q, want %q", task.Here, want)
	}
	if sent.Here != "" {
		t.Errorf("the step pc ran has here %q, want none", sent.Here)
	}
}

// TestBusyGateHostDownKeepsNoStepHereForCost: a gate whose host did not
// answer waits here whole, and none of its steps says cheaper here: the
// unit step has no local price, so the gate asks the host.
func TestBusyGateHostDownKeepsNoStepHereForCost(t *testing.T) {
	g := newGateCostFixture(t, gateCostHostAskedConfig)
	if g.step != "go vet ./..." {
		t.Fatalf("the sample step is %q, want the built-in vet", g.step)
	}
	g.writeHistory(t, gateCostRows([]float64{0.2, 0.2}, "", 0), gateCostRows([]float64{40}, "pc", 0))
	if err := os.Remove(filepath.Join(g.state, "up")); err != nil {
		t.Fatal(err)
	}
	out, calls := runBusyCostGate(t, g, false)
	if !strings.Contains(out, "did not answer") || calls != "" {
		t.Fatalf("want a host that did not answer and no host call; calls=%q\n%s", calls, out)
	}
	if strings.Contains(out, "cheaper here") {
		t.Errorf("the console says cheaper here with the host down:\n%s", out)
	}
	records, _, err := stats.LoadTail(g.statsFile, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	saidDown := false
	for _, rec := range records {
		if strings.Contains(rec.Here, "cheaper here") {
			t.Errorf("a record says cheaper here with the host down: %+v", rec)
		}
		if rec.Kind == "task" && rec.Task == stats.GateTask && strings.HasPrefix(rec.Here, "pc did not answer (") {
			saidDown = true
		}
	}
	if !saidDown {
		t.Errorf("no task line says the host did not answer: %+v", records)
	}
}

// TestBusyGateAllHereWithASubmodule: a gate whose steps all stay here, one
// of them the vet of a module below the root, does not split, and the step
// lines name why each step stayed (the cost, or the subdirectory).
func TestBusyGateAllHereWithASubmodule(t *testing.T) {
	g := newGateCostFixture(t, gateCostVetConfig)
	g.f.write("sub/go.mod", "module example.com/sub\n\ngo 1.22\n")
	g.f.write("sub/b.go", "package b\n")
	g.f.mustGraft("gate")
	records, _, err := stats.LoadTail(g.statsFile, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	steps := map[string]bool{}
	for _, rec := range records {
		if rec.Kind == stats.KindStep && strings.HasPrefix(rec.Step, "go vet") {
			steps[rec.Step] = true
		}
	}
	if len(steps) != 2 {
		t.Fatalf("want the vet of the root and of sub, got %v", steps)
	}
	var locals []stats.Record
	for step := range steps {
		for range 2 {
			locals = append(locals, stats.Record{Step: step, WallS: 0.2})
		}
	}
	g.writeHistory(t, locals, gateCostRows([]float64{40}, "pc", 0))
	out, calls := runBusyCostGate(t, g, false)
	if calls != "" || strings.Contains(out, "gate: busy here:") || strings.Count(out, "starting after") != 1 {
		t.Fatalf("expected a whole-gate wait with no host call; calls=%q\n%s", calls, out)
	}
	if !strings.Contains(out, "go vet ./...: cheaper here: 0.2s here, 40.0s on pc") {
		t.Errorf("the root's vet does not say cheaper here:\n%s", out)
	}
}

// TestBusyGateLineSaysWhyItStayedWhole: the task line of a gate with the
// user's host that ran here whole says why: a calm machine, no step that may
// go to the host, or each such step cheaper here.
func TestBusyGateLineSaysWhyItStayedWhole(t *testing.T) {
	gateHere := func(t *testing.T, g gateCostFixture) []string {
		t.Helper()
		records, _, err := stats.LoadTail(g.statsFile, 8<<20)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, rec := range records {
			if rec.Kind == "task" && rec.Task == stats.GateTask {
				got = append(got, rec.Here)
			}
		}
		return got
	}
	t.Run("calm and cheaper here", func(t *testing.T) {
		g := newGateCostFixture(t, gateCostVetConfig)
		if got, want := gateHere(t, g), []string{tasks.WhyNotBusy}; !slices.Equal(got, want) {
			t.Errorf("calm gate lines here = %q, want %q", got, want)
		}
		// The history replaces the stats file.
		g.writeHistory(t, gateCostRows([]float64{0.1, 0.2, 0.3}, "", 0), gateCostRows([]float64{40, 41}, "pc", 0))
		runBusyCostGate(t, g, false)
		if got, want := gateHere(t, g), []string{tasks.WhyGateCheaperHere}; !slices.Equal(got, want) {
			t.Errorf("busy gate lines here = %q, want %q", got, want)
		}
	})
	t.Run("no step for the host", func(t *testing.T) {
		g := newGateCostFixture(t, gateCostVetConfig)
		g.f.write(".graft.yaml", strings.Replace(gateCostVetConfig, "go_fix: {enabled: false}\n",
			"go_fix: {enabled: false}\ngo_vet: {enabled: false, reason: none here}\n", 1))
		runBusyCostGate(t, g, false)
		got := gateHere(t, g)
		if len(got) == 0 || got[len(got)-1] != tasks.WhyGateNoStepForHost {
			t.Errorf("gate lines here = %q, want the last %q", got, tasks.WhyGateNoStepForHost)
		}
	})
}

// TestGateLineSaysWhyItWasNotSplit: a gate with a host that is not split by
// step says why on its task line: GRAFT_HEAVY=local, inside an admitted heavy
// task, when: always, or a project's own remote_heavy.
func TestGateLineSaysWhyItWasNotSplit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	cases := []struct {
		name string
		set  func(f *fixture)
		want string
	}{
		{"local", func(f *fixture) { f.env = append(f.env, tasks.HeavyEnv+"=local") }, tasks.HeavyEnv + "=local (from the caller)"},
		{"admitted", func(f *fixture) { f.env = append(f.env, tasks.AdmittedEnv+"=1") }, tasks.WhyInsideHeavy},
		{"always", func(f *fixture) {
			for _, kv := range f.env {
				if dir, ok := strings.CutPrefix(kv, "XDG_CONFIG_HOME="); ok {
					// The fixture's user config, its load kept: only when changes.
					file := filepath.Join(filepath.Join(dir, "graft"), "config.yaml")
					writeFile(t, file, strings.Replace(readFile(t, file), "threads: 8}", "threads: 8, when: always}", 1))
				}
			}
		}, tasks.WhyGateAlways},
		{"project host", func(f *fixture) {
			f.write(".graft.yaml", strings.Replace(gateCostVetConfig, "gate:", "remote_heavy: {host: box, up: [sh, -c, 'true'], run: [sh, -c, 'exec \"$@\"', run], threads: 4}\ngate:", 1))
		}, tasks.WhyGateProjectHost},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, state := userRemoteRepo(t, "")
			f.write(".graft.yaml", gateCostVetConfig)
			c.set(f)
			f.mustGraft("gate")
			var got []string
			for _, l := range readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl")) {
				if l.Kind == "task" && l.Task == stats.GateTask {
					got = append(got, l.Here)
				}
			}
			if !slices.Equal(got, []string{c.want}) {
				t.Errorf("gate lines here = %q, want %q", got, c.want)
			}
		})
	}
}
