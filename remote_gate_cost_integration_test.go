package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/stats"
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
