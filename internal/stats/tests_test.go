package stats

import (
	"encoding/json"
	"fmt"
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

func TestFailedTestsIncludeFastFailuresAndRespectLimit(t *testing.T) {
	at := time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC)
	first := PackageRun{Time: at, Project: "projectA", Package: "pkg", Result: ResultPass}
	second := PackageRun{Time: at.Add(time.Minute), Project: "projectA", Package: "pkg", Result: ResultPass}
	for i := range 24 {
		name := fmt.Sprintf("TestSlow%02d", i)
		first.Tests = append(first.Tests, TestResult{Name: name, WallS: float64(10 + i), Result: ResultPass})
		second.Tests = append(second.Tests, TestResult{Name: name, WallS: float64(10 + i), Result: ResultPass})
	}
	first.Tests = append(first.Tests, TestResult{Name: "TestFastFail", WallS: 0.01, Result: ResultFail})
	second.Tests = append(second.Tests, TestResult{Name: "TestFastFail", WallS: 0.01, Result: ResultPass})

	report := summarizeTests([]PackageRun{first, second}, TestFilter{}, 20)
	if len(report.Tests) != 20 {
		t.Fatalf("slow tests = %d, want 20", len(report.Tests))
	}
	for _, row := range report.Tests {
		if row.Test == "TestFastFail" {
			t.Fatal("fast failure appeared among slow tests")
		}
	}
	if len(report.FailedTests) != 1 {
		t.Fatalf("failed tests: %+v", report.FailedTests)
	}
	row := report.FailedTests[0]
	if row.Test != "TestFastFail" || row.Failed != 1 || row.Runs != 2 || row.LastFailure != at {
		t.Errorf("failed test row: %+v", row)
	}
	var output strings.Builder
	if err := RenderTests(&output, report, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "failed tests: failed in some runs") || !strings.Contains(output.String(), "TestFastFail") || !strings.Contains(output.String(), "50%") {
		t.Errorf("failed section missing expected test and share:\n%s", output.String())
	}
	data, err := json.Marshal(report)
	if err != nil || !strings.Contains(string(data), "FailedTests") {
		t.Errorf("JSON omits failed tests: %s (%v)", data, err)
	}

	runs := []PackageRun{
		{Time: at, Package: "pkg", Result: ResultPass, Tests: []TestResult{{Name: "TestThree", Result: ResultFail}, {Name: "TestTwo", Result: ResultFail}, {Name: "TestOne", Result: ResultFail}}},
		{Time: at.Add(time.Minute), Package: "pkg", Result: ResultPass, Tests: []TestResult{{Name: "TestThree", Result: ResultFail}, {Name: "TestTwo", Result: ResultFail}, {Name: "TestOne", Result: ResultPass}}},
		{Time: at.Add(2 * time.Minute), Package: "pkg", Result: ResultPass, Tests: []TestResult{{Name: "TestThree", Result: ResultFail}, {Name: "TestTwo", Result: ResultPass}, {Name: "TestOne", Result: ResultPass}}},
	}
	limited := summarizeTests(runs, TestFilter{}, 2)
	if limited.FailedAll != 3 || len(limited.FailedTests) != 2 || limited.FailedTests[0].Test != "TestThree" || limited.FailedTests[1].Test != "TestTwo" {
		t.Errorf("failed-test order/limit: all=%d rows=%+v", limited.FailedAll, limited.FailedTests)
	}
	if got, want := limited.FailedTests[0].LastFailure, at.Add(2*time.Minute); !got.Equal(want) {
		t.Errorf("TestThree last failure = %v, want the latest of its failures %v", got, want)
	}
	output.Reset()
	if err := RenderTests(&output, limited, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "and 1 more") {
		t.Errorf("failed-test overflow is not reported:\n%s", output.String())
	}
	ranked := summarizeTests([]PackageRun{
		{Time: at, Package: "pkg", Result: ResultPass, Tests: []TestResult{{Name: "TestAHigh", Result: ResultFail}, {Name: "TestZLow", Result: ResultFail}}},
		{Time: at.Add(time.Minute), Package: "pkg", Result: ResultPass, Tests: []TestResult{{Name: "TestZLow", Result: ResultPass}}},
		{Time: at.Add(2 * time.Minute), Package: "pkg", Result: ResultPass, Tests: []TestResult{{Name: "TestZLow", Result: ResultPass}}},
	}, TestFilter{}, 10)
	if len(ranked.FailedTests) != 2 || ranked.FailedTests[0].Test != "TestAHigh" || ranked.FailedTests[1].Test != "TestZLow" {
		t.Errorf("equal failure counts should rank by failure share: %+v", ranked.FailedTests)
	}
	output.Reset()
	if err := RenderTests(&output, TestReport{}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "failed tests: failed in some runs\nnone") {
		t.Errorf("empty failed-test section is not none:\n%s", output.String())
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
