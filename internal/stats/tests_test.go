package stats

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestSkippedTests: the skipped tests rank by how many runs skipped them,
// with the runs of their package and the reason of the last skip; a long
// reason is cut, a missing one shown as -, and rows past the limit counted.
func TestSkippedTests(t *testing.T) {
	at := time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC)
	run := func(min int, tests ...TestResult) PackageRun {
		return PackageRun{Time: at.Add(time.Duration(min) * time.Minute), Package: "m/a", Result: ResultPass, Tests: tests}
	}
	skip := func(name, reason string) TestResult {
		return TestResult{Name: name, Result: ResultSkip, Reason: reason}
	}
	long := strings.Repeat("x", 150)
	runs := []PackageRun{
		run(0, skip("TestCorpus", "old reason"), skip("TestLive", ""), skip("TestTable/traces", long)),
		run(1, skip("TestCorpus", "no corpus"), TestResult{Name: "TestLive", Result: ResultPass}),
		run(2, TestResult{Name: "TestCorpus", Result: ResultPass}),
	}
	rep := summarizeTests(runs, TestFilter{}, 2)
	if rep.SkippedAll != 3 || len(rep.Skipped) != 2 {
		t.Fatalf("skipped %d, rows %+v", rep.SkippedAll, rep.Skipped)
	}
	if r := rep.Skipped[0]; r.Test != "TestCorpus" || r.Skipped != 2 || r.Runs != 3 || r.Reason != "no corpus" {
		t.Errorf("first row %+v", r)
	}
	var out strings.Builder
	if err := RenderTests(&out, summarizeTests(runs, TestFilter{}, 3), false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"PACKAGE  TEST              SKIPPED  RUNS  REASON\n",
		"m/a      TestCorpus        2        3     no corpus\n",
		"m/a      TestLive          1        3     -\n",
		"m/a      TestTable/traces  1        3     " + strings.Repeat("x", maxReason-1) + "…\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if err := RenderTests(&out, rep, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "\nand 1 more\n") {
		t.Errorf("report does not count the rows left out:\n%s", out.String())
	}
}

// TestTestResultJSON: a skip's reason is a fourth element, which the tests
// file reads back; other tests stay [name, wall_s, result].
func TestTestResultJSON(t *testing.T) {
	for _, tc := range []struct {
		res  TestResult
		json string
	}{
		{TestResult{Name: "TestA", WallS: 1.5, Result: ResultPass}, `["TestA",1.5,"pass"]`},
		{TestResult{Name: "TestB", Result: ResultSkip, Reason: "no corpus"}, `["TestB",0,"skip","no corpus"]`},
	} {
		data, err := json.Marshal(tc.res)
		if err != nil || string(data) != tc.json {
			t.Errorf("%+v: %s %v, want %s", tc.res, data, err, tc.json)
		}
		var back TestResult
		if err := json.Unmarshal(data, &back); err != nil || back != tc.res {
			t.Errorf("%s reads back as %+v %v", data, back, err)
		}
	}
	var r TestResult
	if err := json.Unmarshal([]byte(`["TestC",0]`), &r); err == nil {
		t.Error("a test of two elements reads")
	}
}

func TestSlashNamesAndFlakyTestsRemainVisible(t *testing.T) {
	at := time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC)
	runs := []PackageRun{
		{Time: at, Project: "projectA", Package: "web", Runner: "jest", Result: ResultPass, Tree: "tree", Tests: []TestResult{{Name: "api GET /login returns 200", Result: ResultFlaky, WallS: 2}}},
		{Time: at.Add(time.Minute), Project: "projectA", Package: "web", Runner: "playwright", Result: ResultPass, Tree: "tree", Tests: []TestResult{{Name: "[chromium] opens /login", Result: ResultFlaky, WallS: 3}}},
		{Time: at, Project: "projectA", Package: "go/pkg", Runner: "go", Result: ResultPass, Tests: []TestResult{{Name: "TestTable/case", Result: ResultFail}}},
	}
	report := summarizeTests(runs, TestFilter{}, 10)
	if len(report.Tests) != 2 || len(report.Flaky) != 2 {
		t.Fatalf("slash tests missing: %+v", report)
	}
	for _, row := range report.Tests {
		if row.Failed != 0 {
			t.Errorf("flaky test counted as failed in slow tests: %+v", row)
		}
	}
	if len(report.Flaky) != 2 || report.Flaky[0].Test == "TestTable/case" {
		t.Errorf("Go subtest was counted or external flaky tests missing: %+v", report.Flaky)
	}
}
