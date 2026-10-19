package stats

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

// Test results.
const (
	ResultPass      = "pass"
	ResultFail      = "fail"
	ResultSkip      = "skip"
	ResultBuildFail = "build-fail" // a package that did not build
	// ResultFlaky is a test that failed and then passed on a retry within
	// the run: a flake on its tree by itself.
	ResultFlaky = "flaky"
)

// PackageRun is one line of the tests file: a package's run in a step.
type PackageRun struct {
	// Time is when the package finished.
	Time    time.Time `json:"time"`
	Project string    `json:"project"`
	Task    string    `json:"task"`
	Top     string    `json:"top"`
	// Tree identifies the work tree the tests ran on, HEAD and every
	// change on top of it: the same tree failing and passing a test is a
	// flake. Empty when it could not be read.
	Tree string `json:"tree,omitempty"`
	// Runner is the report the run was read from (go).
	Runner  string `json:"runner"`
	Package string `json:"package"`
	Result  string `json:"result"`
	// WallS is the run of the test binary, the build not included.
	WallS float64 `json:"wall_s"`
	// Tests are the top-level tests, and the subtests that failed.
	Tests []TestResult `json:"tests,omitempty"`
	// LoadStart and LoadMax are the load of the step that ran the package,
	// as on its stats line.
	LoadStart *float64 `json:"load_start,omitempty"`
	LoadMax   *float64 `json:"load_max,omitempty"`
}

// TestResult is one test of a package run, written as [name, wall_s,
// result] to keep large suites small, or [name, wall_s, "skip", reason] for
// a test skipped with a reason.
type TestResult struct {
	Name   string
	WallS  float64
	Result string
	// Reason is what a skipped test said, when its runner tells.
	Reason string
}

// MarshalJSON writes the test as [name, wall_s, result] or with its reason.
func (t TestResult) MarshalJSON() ([]byte, error) {
	if t.Reason != "" {
		return json.Marshal([]any{t.Name, t.WallS, t.Result, t.Reason})
	}
	return json.Marshal([]any{t.Name, t.WallS, t.Result})
}

// UnmarshalJSON reads [name, wall_s, result] or [name, wall_s, result,
// reason].
func (t *TestResult) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw) != 3 && len(raw) != 4 {
		return fmt.Errorf("a test is [name, wall_s, result] or [name, wall_s, result, reason], got %s", data)
	}
	err := errors.Join(json.Unmarshal(raw[0], &t.Name), json.Unmarshal(raw[1], &t.WallS), json.Unmarshal(raw[2], &t.Result))
	if len(raw) == 4 {
		err = errors.Join(err, json.Unmarshal(raw[3], &t.Reason))
	}
	return err
}

// LoadTests reads the old copy of the tests file and the file itself, oldest
// first, as Load reads the stats.
func LoadTests(path string) (runs []PackageRun, malformed int, err error) {
	for _, p := range []string{path + ".1", path} {
		f, err := os.Open(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, 0, fmt.Errorf("reading the tests: %w", err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 64<<20)
		for sc.Scan() {
			var r PackageRun
			if json.Unmarshal(sc.Bytes(), &r) != nil || r.Package == "" || r.Result == "" {
				malformed++
				continue
			}
			runs = append(runs, r)
		}
		err = sc.Err()
		f.Close()
		if err != nil {
			return nil, 0, fmt.Errorf("reading %s: %w", p, err)
		}
	}
	return runs, malformed, nil
}

// TestFilter selects the package runs a report covers.
type TestFilter struct {
	Since time.Time
	// Project is the project root; empty takes every project.
	Project string
	// Task, when set, takes only the runs of that task.
	Task string
	// MaxLoad is Filter.MaxLoad.
	MaxLoad float64
}

func (f TestFilter) takes(r *PackageRun) bool {
	return !r.Time.Before(f.Since) && (f.Project == "" || r.Project == f.Project) && (f.Task == "" || r.Task == f.Task) &&
		calm(r.LoadMax, f.MaxLoad)
}

// TimingRow is the runs of a test or of a package.
type TimingRow struct {
	Project, Package, Test string
	Runs                   int
	WallMed, WallMax       float64
	Failed                 int
}

// FlakyRow is a test that failed and passed on the same tree.
type FlakyRow struct {
	Project, Package, Test string
	// Trees is how many trees saw it both fail and pass; Failed and Passed
	// count its runs on those trees.
	Trees, Failed, Passed int
}

// SkipRow is a test skipped in some runs of its package.
type SkipRow struct {
	Project, Package, Test string
	// Skipped is how many runs skipped it, of Runs runs of its package.
	Skipped, Runs int
	// Reason is the reason of the last skip.
	Reason string
	last   time.Time
}

// TestReport is what graft stats --tests shows.
type TestReport struct {
	Tests, Packages []TimingRow
	// Skipped are the most skipped tests, SkippedAll how many were skipped
	// in all.
	Skipped    []SkipRow
	SkippedAll int
	Flaky      []FlakyRow
	// Reruns are the tasks a graft reran to a pass (Reruns).
	Reruns []RerunRow
}

// testKey names a test, or with Test empty a package, of a project.
type testKey struct{ project, pkg, test string }

func (k testKey) String() string { return k.project + "\x00" + k.pkg + "\x00" + k.test }

type timing struct {
	walls  []float64
	failed int
}

type outcome struct{ failed, passed int }

// testTally gathers the runs a report covers.
type testTally struct {
	tests, pkgs map[testKey]*timing
	// byTree holds each test's results per tree.
	byTree map[testKey]map[string]*outcome
	skips  map[testKey]*SkipRow
}

func (t *testTally) time(m map[testKey]*timing, k testKey, wall float64, failed bool) {
	tm := m[k]
	if tm == nil {
		tm = &timing{}
		m[k] = tm
	}
	tm.walls = append(tm.walls, wall)
	if failed {
		tm.failed++
	}
}

func (t *testTally) add(r *PackageRun, project string) {
	t.time(t.pkgs, testKey{project, r.Package, ""}, r.WallS, r.Result != ResultPass)
	for _, tr := range r.Tests {
		if tr.Result == ResultSkip {
			t.skip(r, project, tr)
			continue
		}
		if strings.Contains(tr.Name, "/") {
			continue
		}
		k := testKey{project, r.Package, tr.Name}
		failed, flaky := tr.Result == ResultFail, tr.Result == ResultFlaky
		t.time(t.tests, k, tr.WallS, failed)
		if r.Tree == "" {
			continue
		}
		if t.byTree[k] == nil {
			t.byTree[k] = map[string]*outcome{}
		}
		o := t.byTree[k][r.Tree]
		if o == nil {
			o = &outcome{}
			t.byTree[k][r.Tree] = o
		}
		switch {
		case flaky:
			o.failed++
			o.passed++
		case failed:
			o.failed++
		default:
			o.passed++
		}
	}
}

func (t *testTally) skip(r *PackageRun, project string, tr TestResult) {
	k := testKey{project, r.Package, tr.Name}
	s := t.skips[k]
	if s == nil {
		s = &SkipRow{Project: project, Package: r.Package, Test: tr.Name}
		t.skips[k] = s
	}
	s.Skipped++
	if !r.Time.Before(s.last) {
		s.last, s.Reason = r.Time, tr.Reason
	}
}

// mostSkipped are the skipped tests, the most skipped first, at most limit,
// and how many there are.
func (t *testTally) mostSkipped(limit int) ([]SkipRow, int) {
	out := make([]SkipRow, 0, len(t.skips))
	for k, s := range t.skips {
		s.Runs = len(t.pkgs[testKey{k.project, k.pkg, ""}].walls)
		out = append(out, *s)
	}
	slices.SortFunc(out, func(a, b SkipRow) int {
		if a.Skipped != b.Skipped {
			return b.Skipped - a.Skipped
		}
		return strings.Compare(testKey{a.Project, a.Package, a.Test}.String(), testKey{b.Project, b.Package, b.Test}.String())
	})
	return out[:min(len(out), limit)], len(out)
}

// SummarizeTests ranks the tests and packages f selects by median wall time,
// the slowest first, at most limit rows each, and lists the most skipped
// tests and the flaky ones. Subtests are recorded only when they fail or are
// skipped, so the timing and the flakes are of top-level tests.
func SummarizeTests(runs []PackageRun, f TestFilter, limit int) TestReport {
	t := &testTally{tests: map[testKey]*timing{}, pkgs: map[testKey]*timing{}, byTree: map[testKey]map[string]*outcome{}, skips: map[testKey]*SkipRow{}}
	for i := range runs {
		r := &runs[i]
		if !f.takes(r) {
			continue
		}
		project := ""
		if f.Project == "" {
			project = r.Project
		}
		t.add(r, project)
	}
	rep := TestReport{Tests: slowest(t.tests, limit), Packages: slowest(t.pkgs, limit), Flaky: flaky(t.byTree)}
	rep.Skipped, rep.SkippedAll = t.mostSkipped(limit)
	return rep
}

func slowest(m map[testKey]*timing, limit int) []TimingRow {
	var out []TimingRow
	for k, t := range m {
		out = append(out, TimingRow{Project: k.project, Package: k.pkg, Test: k.test, Runs: len(t.walls),
			WallMed: median(t.walls), WallMax: slices.Max(t.walls), Failed: t.failed})
	}
	slices.SortFunc(out, func(a, b TimingRow) int {
		if c := cmpFloat(b.WallMed, a.WallMed); c != 0 {
			return c
		}
		return strings.Compare(testKey{a.Project, a.Package, a.Test}.String(), testKey{b.Project, b.Package, b.Test}.String())
	})
	return out[:min(len(out), limit)]
}

// flaky are the tests that both failed and passed on some tree, the most
// trees first.
func flaky(byTree map[testKey]map[string]*outcome) []FlakyRow {
	var out []FlakyRow
	for k, trees := range byTree {
		row := FlakyRow{Project: k.project, Package: k.pkg, Test: k.test}
		for _, o := range trees {
			if o.failed > 0 && o.passed > 0 {
				row.Trees++
				row.Failed += o.failed
				row.Passed += o.passed
			}
		}
		if row.Trees > 0 {
			out = append(out, row)
		}
	}
	slices.SortFunc(out, func(a, b FlakyRow) int {
		if a.Trees != b.Trees {
			return b.Trees - a.Trees
		}
		if a.Failed != b.Failed {
			return b.Failed - a.Failed
		}
		return strings.Compare(testKey{a.Project, a.Package, a.Test}.String(), testKey{b.Project, b.Package, b.Test}.String())
	})
	return out
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// maxReason is as much of a skip's reason as a row shows.
const maxReason = 100

// RenderTests prints the report as five tables; withProject adds the
// project column.
func RenderTests(w io.Writer, rep TestReport, withProject bool) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	project := func(p string) {
		if withProject {
			fmt.Fprintf(tw, "%s\t", p)
		}
	}
	header := func(cols string) {
		if withProject {
			cols = "PROJECT\t" + cols
		}
		fmt.Fprintln(tw, cols)
	}
	percent := func(n, of int) int { return (n*100 + of/2) / of }

	fmt.Fprintln(tw, "slow tests")
	header("PACKAGE\tTEST\tRUNS\tWALL MED\tWALL MAX\tFAILED")
	for _, r := range rep.Tests {
		project(r.Project)
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%d%%\n", r.Package, r.Test, r.Runs, formatSeconds(r.WallMed), formatSeconds(r.WallMax), percent(r.Failed, r.Runs))
	}
	fmt.Fprintln(tw, "\nslow packages")
	header("PACKAGE\tRUNS\tWALL MED\tWALL MAX\tFAILED")
	for _, r := range rep.Packages {
		project(r.Project)
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%d%%\n", r.Package, r.Runs, formatSeconds(r.WallMed), formatSeconds(r.WallMax), percent(r.Failed, r.Runs))
	}
	fmt.Fprintln(tw, "\nskipped tests: skipped in some runs of their package, the reason the last one gave")
	if len(rep.Skipped) == 0 {
		fmt.Fprintln(tw, "none")
	} else {
		header("PACKAGE\tTEST\tSKIPPED\tRUNS\tREASON")
	}
	for _, r := range rep.Skipped {
		project(r.Project)
		reason := strings.ReplaceAll(r.Reason, "\t", " ")
		if reason == "" {
			reason = "-"
		} else if rs := []rune(reason); len(rs) > maxReason {
			reason = string(rs[:maxReason-1]) + "…"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\n", r.Package, r.Test, r.Skipped, r.Runs, reason)
	}
	if more := rep.SkippedAll - len(rep.Skipped); more > 0 {
		fmt.Fprintf(tw, "and %d more\n", more)
	}
	fmt.Fprintln(tw, "\nflaky: failed and passed on the same tree")
	if len(rep.Flaky) == 0 {
		fmt.Fprintln(tw, "none")
	} else {
		header("PACKAGE\tTEST\tTREES\tFAILED\tPASSED")
	}
	for _, r := range rep.Flaky {
		project(r.Project)
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\n", r.Package, r.Test, r.Trees, r.Failed, r.Passed)
	}
	renderReruns(tw, rep.Reruns, withProject)
	return tw.Flush()
}
