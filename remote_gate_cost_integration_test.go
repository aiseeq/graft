package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
	var sampleN int
	write := func(rec stats.Record) {
		if rec.Time.IsZero() {
			rec.Time = time.Now().Add(time.Duration(sampleN) * time.Millisecond)
			sampleN++
		}
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

func seedGatePlacementCosts(t *testing.T, f *fixture, state string, profileSteps map[string][]float64) {
	t.Helper()
	project, err := filepath.EvalSymlinks(f.work)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, "cache", "graft", "stats.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	appendRecord := func(rec stats.Record) {
		t.Helper()
		data, marshalErr := json.Marshal(rec)
		if marshalErr != nil {
			file.Close()
			t.Fatal(marshalErr)
		}
		if _, writeErr := file.Write(append(data, '\n')); writeErr != nil {
			file.Close()
			t.Fatal(writeErr)
		}
	}
	for i, fee := range []float64{40, 41, 42} {
		appendRecord(stats.Record{Time: time.Now().Add(time.Duration(i) * time.Millisecond), Kind: stats.KindStep, Task: "gate", Project: project, Profile: "work:gate", Step: "go vet ./...", Host: "pc", Exit: 0, WallS: fee, Run: fmt.Sprintf("host-%d", i)})
	}
	idx := 0
	for key, values := range profileSteps {
		profile, step, ok := strings.Cut(key, "\x00")
		if !ok {
			file.Close()
			t.Fatalf("bad test profile/step key %q", key)
		}
		for _, wall := range values {
			appendRecord(stats.Record{Time: time.Now().Add(time.Duration(idx) * time.Millisecond), Kind: stats.KindStep, Task: strings.SplitN(profile, ":", 2)[1], Project: project, Profile: profile, Step: step, Exit: 0, WallS: wall, Run: fmt.Sprintf("local-%d", idx)})
			idx++
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
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
	if calls != "" || strings.Contains(out, "cheaper here:") {
		t.Fatalf("built-in vet reason should be stats-only; calls=%q\n%s", calls, out)
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

func TestBusyGateRecoversPriceBehindForeignJournalTail(t *testing.T) {
	g := newGateCostFixture(t, gateCostVetConfig)
	locals := gateCostRows([]float64{0.2, 0.3, 0.4}, "", 0)
	for i := range locals {
		locals[i].Time = time.Now().Add(-48 * time.Hour)
	}
	g.writeHistory(t, locals, nil)
	noise := `{"time":"2026-11-24T12:00:00Z","kind":"step","task":"noise","project":"/projectB","profile":"projectB:gate","step":"foreign","run":"noise","wall_s":1,"exit":0,"padding":"` + strings.Repeat("x", 200) + `"}` + "\n"
	file, err := os.OpenFile(g.statsFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(strings.Repeat(noise, (8<<20)/len(noise)+1)); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(g.statsFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range gateCostRows([]float64{40, 41}, "pc", 0) {
		rec.Time = time.Now()
		rec.Kind = stats.KindStep
		rec.Task = stats.GateTask
		rec.Project = g.project
		rec.Profile = g.profile
		rec.Step = g.step
		b, marshalErr := json.Marshal(rec)
		if marshalErr != nil {
			f.Close()
			t.Fatal(marshalErr)
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			f.Close()
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	window, _, err := stats.LoadWindow(g.statsFile, time.Now().Add(-7*24*time.Hour), strings.SplitN(g.profile, ":", 2)[0]+":", 256<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(window) != 5 {
		t.Fatalf("deep window returned %d matching rows, want three local and two host records", len(window))
	}
	out, calls := runBusyCostGate(t, g, false)
	foundLocalPrice := false
	for _, rec := range readStats(t, g.statsFile) {
		if rec.Kind == stats.KindStep && rec.Step == g.step && rec.Host == "" && strings.HasPrefix(rec.Here, "cheaper here:") {
			foundLocalPrice = true
		}
	}
	if calls != "" || !foundLocalPrice {
		t.Fatalf("old local price behind foreign tail was not found; calls=%q\n%s", calls, out)
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
	if calls != "" || strings.Contains(out, "cheaper here:") {
		t.Fatalf("built-in vet reason should be stats-only; calls=%q\n%s", calls, out)
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
	var sampleN int
	for _, rec := range records {
		if rec.Kind == "step" && strings.Contains(rec.Step, "go test -run Probe") {
			for range 2 {
				local = append(local, stats.Record{Time: time.Now().Add(time.Duration(sampleN) * time.Millisecond), Kind: "step", Project: g.project, Profile: rec.Profile, Task: rec.Task, Step: rec.Step, WallS: 0.2, Exit: 0})
				sampleN++
			}
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
	if strings.Count(out, "cheaper here:") != 1 {
		t.Errorf("want one local-cost reason line for the remote task:\n%s", out)
	}
	localReasons := 0
	for _, rec := range readStats(t, g.statsFile) {
		if rec.Kind == "step" && rec.Task == "unit" && rec.Host == "" && strings.HasPrefix(rec.Here, "cheaper here:") {
			localReasons++
		}
	}
	if localReasons != 3 {
		t.Errorf("got %d local-cost step records with reasons, want 3", localReasons)
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

func TestBusyGateUnknownLocalStepIsProbedHere(t *testing.T) {
	g := newGateCostFixture(t, gateCostVetConfig)
	g.writeHistory(t, nil, gateCostRows([]float64{1}, "pc", 0))
	out, calls := runBusyCostGate(t, g, false)
	if calls != "" || strings.Contains(out, "cheaper here") {
		t.Fatalf("step with unknown local price was not probed here; calls=%q\n%s", calls, out)
	}
	found := false
	for _, rec := range readStats(t, g.statsFile) {
		if rec.Kind == stats.KindStep && rec.Step == "go vet ./..." && rec.Host == "" && strings.HasPrefix(rec.Here, "price probe here:") {
			found = true
		}
	}
	if !found {
		t.Errorf("local step has no probe reason: %+v", readStats(t, g.statsFile))
	}
}

// TestBusyGateKeepsTheTaskHereReason: the cost rule leaves the here of a
// split gate's heavy task as it was, and a step the host ran has none.
func TestBusyGateNewStepGetsCheaperHereAfterProbe(t *testing.T) {
	const cfg = "schema: 1\nversion: {mode: none}\ngo_vet: {enabled: false, reason: disabled in this test}\n" +
		"gate:\n  - task: unit\n  - task: script\ntasks:\n" +
		"  unit:\n    heavy: true\n    remote: true\n    run: [[go, test, -run, NewText, ./...]]\n" +
		"  script:\n    run: [[sh, -c, 'echo local >> out']]\n"
	g := newGateCostFixture(t, cfg)
	g.writeHistory(t, nil, gateCostRows([]float64{40}, "pc", 0))
	_, calls := runBusyCostGate(t, g, false)
	if calls != "" {
		t.Fatalf("new step was not probed here: %q", calls)
	}
	probed := false
	for _, rec := range readStats(t, g.statsFile) {
		if rec.Kind == stats.KindStep && strings.Contains(rec.Step, "NewText") && rec.Host == "" && strings.HasPrefix(rec.Here, "price probe here:") {
			probed = true
		}
	}
	if !probed {
		t.Fatalf("first gate did not record a local price probe: %+v", readStats(t, g.statsFile))
	}
	for range 2 {
		previousRun := latestGateRun(t, g.statsFile)
		_, calls = runBusyCostGate(t, g, false)
		if calls != "" {
			t.Fatalf("step with a fresh local price went to host: %q", calls)
		}
		found := false
		currentRun := latestGateRun(t, g.statsFile)
		for _, rec := range readStats(t, g.statsFile) {
			if currentRun != previousRun && rec.Run == currentRun && rec.Kind == stats.KindStep && strings.Contains(rec.Step, "NewText") && rec.Host == "" && strings.HasPrefix(rec.Here, "cheaper here:") {
				found = true
			}
		}
		if !found {
			t.Fatalf("later gate has no cheaper-here step record: %+v", readStats(t, g.statsFile))
		}
	}
}

func latestGateRun(t *testing.T, path string) string {
	t.Helper()
	var latest string
	for _, rec := range readStats(t, path) {
		if rec.Kind == stats.KindTask && rec.Task == stats.GateTask {
			latest = rec.Run
		}
	}
	return latest
}

func TestBusyGateKeepsTheTaskHereReason(t *testing.T) {
	g := newGateCostFixture(t, splitGateConfig)
	if err := os.Remove(g.f.path("out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(g.statsFile); err != nil {
		t.Fatal(err)
	}
	seedGatePlacementCosts(t, g.f, g.state, map[string][]float64{
		"work:gate\x00go vet ./...":           {100},
		"work:unit\x00go test -count=1 ./...": {100},
	})
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

// A cheap local step stays here without querying an unavailable host.
func TestBusyGateCheapLocalStepDoesNotQueryHost(t *testing.T) {
	g := newGateCostFixture(t, gateCostHostAskedConfig)
	if g.step != "go vet ./..." {
		t.Fatalf("the sample step is %q, want the built-in vet", g.step)
	}
	locals := gateCostRows([]float64{0.2, 0.2}, "", 0)
	g.writeHistory(t, locals, gateCostRows([]float64{40}, "pc", 0))
	if err := os.Remove(filepath.Join(g.state, "up")); err != nil {
		t.Fatal(err)
	}
	previousRun := latestGateRun(t, g.statsFile)
	out, calls := runBusyCostGate(t, g, false)
	if strings.Contains(out, "did not answer") || calls != "" {
		t.Fatalf("cheap local step should not query unavailable host; calls=%q\n%s", calls, out)
	}
	_ = out // Step price reasons are recorded, not printed individually.
	records, _, err := stats.LoadTail(g.statsFile, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	saidDown := false
	for _, rec := range records {
		if rec.Run == previousRun || rec.Run == "" {
			continue
		}
		if rec.Kind == stats.KindStep && rec.Step == g.step && !strings.HasPrefix(rec.Here, "cheaper here:") {
			t.Errorf("cheap local step lost its price reason: %+v", rec)
		}
		if rec.Kind == "task" && rec.Task == stats.GateTask && strings.HasPrefix(rec.Here, "pc did not answer (") {
			saidDown = true
		}
	}
	if saidDown {
		t.Errorf("the gate queried unavailable host despite its cheap local step: %+v", records)
	}
}

// TestBusyGateHostDownKeepsNoStepHereForCost checks the original host-down
// fallback: inexpensive test commands have historical prices above the fee.
func TestBusyGateHostDownKeepsNoStepHereForCost(t *testing.T) {
	g := newGateCostFixture(t, gateCostHostAskedConfig)
	locals := gateCostRows([]float64{60, 60}, "", 0)
	locals[1].Profile = "work:unit"
	locals[1].Step = "go test -run Nothing ./..."
	g.writeHistory(t, locals, gateCostRows([]float64{40}, "pc", 0))
	if err := os.Remove(filepath.Join(g.state, "up")); err != nil {
		t.Fatal(err)
	}
	previousRun := latestGateRun(t, g.statsFile)
	out, calls := runBusyCostGate(t, g, false)
	if !strings.Contains(out, "did not answer") || calls != "" {
		t.Fatalf("expected unavailable host after expensive local price; calls=%q\n%s", calls, out)
	}
	records, _, err := stats.LoadTail(g.statsFile, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	saidDown := false
	for _, rec := range records {
		if rec.Run == previousRun || rec.Run == "" {
			continue
		}
		if strings.HasPrefix(rec.Here, "cheaper here:") {
			t.Errorf("expensive local step says cheaper here: %+v", rec)
		}
		if rec.Kind == "task" && rec.Task == stats.GateTask && strings.HasPrefix(rec.Here, "pc did not answer (") {
			saidDown = true
		}
	}
	if !saidDown {
		t.Errorf("no task line says the host did not answer: %+v", records)
	}
}

func TestBusyGateHostDownClearsMixedStepCosts(t *testing.T) {
	const cfg = "schema: 1\nversion: {mode: none}\ngo_vet: {enabled: false, reason: disabled in this test}\n" +
		"gate:\n  - task: unit\ntasks:\n  unit:\n    heavy: true\n    remote: true\n    run:\n" +
		"      - [go, test, -run, HostDownCheap, ./...]\n" +
		"      - [go, test, -run, HostDownExpensive, ./...]\n"
	g := newGateCostFixture(t, cfg)
	records, _, err := stats.LoadTail(g.statsFile, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	steps := map[string]bool{}
	for _, rec := range records {
		if rec.Kind == stats.KindStep && rec.Task == "unit" && strings.Contains(rec.Step, "HostDown") {
			steps[rec.Step] = true
		}
	}
	if len(steps) != 2 {
		t.Fatalf("calm run recorded %d test steps, want both configured steps: %v", len(steps), steps)
	}
	var locals []stats.Record
	for step := range steps {
		wall := 60.0
		if strings.Contains(step, "HostDownCheap") {
			wall = 0.2
		}
		for range 2 {
			locals = append(locals, stats.Record{Project: g.project, Profile: "work:unit", Task: "unit", Step: step, WallS: wall, Exit: 0})
		}
	}
	g.writeHistory(t, locals, gateCostRows([]float64{40}, "pc", 0))
	if err := os.Remove(filepath.Join(g.state, "up")); err != nil {
		t.Fatal(err)
	}
	previousRun := latestGateRun(t, g.statsFile)
	out, calls := runBusyCostGate(t, g, false)
	if !strings.Contains(out, "did not answer") || calls != "" {
		t.Fatalf("expected the unavailable host fallback; calls=%q\n%s", calls, out)
	}
	got := make(map[string]statsLine)
	for _, rec := range readStats(t, g.statsFile) {
		if rec.Run == previousRun || rec.Run == "" || rec.Kind != stats.KindStep || rec.Task != "unit" {
			continue
		}
		if strings.Contains(rec.Step, "HostDown") {
			got[rec.Step] = rec
		}
	}
	if len(got) != 2 {
		t.Fatalf("fallback recorded %d of the two local steps: %+v", len(got), got)
	}
	for step, rec := range got {
		if strings.HasPrefix(rec.Here, "cheaper here:") {
			t.Errorf("host-down fallback retained a step price for %q: %+v", step, rec)
		}
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
	wantRoot := "cheaper here: 0.2s here, 40.0s on pc"
	rootHere, subHere := "", ""
	for _, rec := range readStats(t, g.statsFile) {
		if rec.Kind != stats.KindStep || !strings.HasPrefix(rec.Step, "go vet") {
			continue
		}
		if strings.Contains(rec.Step, "(sub)") {
			subHere = rec.Here
		} else {
			rootHere = rec.Here
		}
	}
	if rootHere != wantRoot || !strings.Contains(subHere, "runs in sub") {
		t.Errorf("builtin vet stats reasons: root=%q sub=%q", rootHere, subHere)
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
		if out := g.f.mustGraft("gate"); !strings.Contains(out, "graft: gate: heavy, on this machine whole, its heavy tasks with it: "+tasks.WhyNotBusy+"\n") {
			t.Errorf("a calm gate does not say it stays here whole:\n%s", out)
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
