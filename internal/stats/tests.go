package stats

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
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
	// Reason is the package's build or test process output on failure.
	Reason string `json:"reason,omitempty"`
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
// result] to keep large suites small, or with a fourth field when it has a
// reason.
type TestResult struct {
	Name   string
	WallS  float64
	Result string
	// Reason is the output of a failing test or the reason a test was skipped.
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
	var fields [4][]byte
	count, err := testFields(data, &fields)
	if err != nil {
		return err
	}
	if count != 3 && count != 4 {
		return fmt.Errorf("a test is [name, wall_s, result] or [name, wall_s, result, reason], got %s", data)
	}
	if err := json.Unmarshal(fields[0], &t.Name); err != nil {
		return err
	}
	if err := json.Unmarshal(fields[1], &t.WallS); err != nil {
		return err
	}
	if err := json.Unmarshal(fields[2], &t.Result); err != nil {
		return err
	}
	t.Reason = ""
	if count == 4 {
		return json.Unmarshal(fields[3], &t.Reason)
	}
	return nil
}

// testFields splits the flat array without allocating RawMessages for each
// test result. Each field still points into the scanner's current line.
func testFields(data []byte, fields *[4][]byte) (int, error) {
	i := skipJSONSpace(data, 0)
	if i == len(data) || data[i] != '[' {
		return 0, fmt.Errorf("a test result must be an array")
	}
	i++
	count := 0
	for {
		i = skipJSONSpace(data, i)
		if i == len(data) {
			return 0, fmt.Errorf("unterminated test result array")
		}
		if data[i] == ']' {
			if count != 0 {
				return 0, fmt.Errorf("trailing comma in test result array")
			}
			i = skipJSONSpace(data, i+1)
			if i != len(data) {
				return 0, fmt.Errorf("trailing data after test result array")
			}
			return count, nil
		}
		if count == len(fields) {
			return 0, fmt.Errorf("too many test result fields")
		}
		end, sep, err := testFieldEnd(data, i)
		if err != nil {
			return 0, err
		}
		fields[count] = trimJSONSpace(data[i:end])
		if len(fields[count]) == 0 {
			return 0, fmt.Errorf("empty test result field")
		}
		count++
		i = end + 1
		if sep == ']' {
			i = skipJSONSpace(data, i)
			if i != len(data) {
				return 0, fmt.Errorf("trailing data after test result array")
			}
			return count, nil
		}
	}
}

func testFieldEnd(data []byte, start int) (int, byte, error) {
	quoted, escaped := false, false
	for i := start; i < len(data); i++ {
		c := data[i]
		if quoted {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		if c == '"' {
			quoted = true
			continue
		}
		if c == ',' || c == ']' {
			return i, c, nil
		}
	}
	return 0, 0, fmt.Errorf("unterminated test result array")
}

func skipJSONSpace(data []byte, i int) int {
	for i < len(data) && (data[i] == ' ' || data[i] == '\n' || data[i] == '\r' || data[i] == '\t') {
		i++
	}
	return i
}

func trimJSONSpace(data []byte) []byte {
	start := skipJSONSpace(data, 0)
	end := len(data)
	for end > start && (data[end-1] == ' ' || data[end-1] == '\n' || data[end-1] == '\r' || data[end-1] == '\t') {
		end--
	}
	return data[start:end]
}

// ReadTests visits valid package runs in the old copies and the file,
// oldest first, as Load reads the stats.
func ReadTests(path string, since time.Time, visit func(PackageRun) error) (malformed int, err error) {
	files, err := Files(path, since)
	if err != nil {
		return malformed, err
	}
	var readErr error
	for _, p := range files {
		f, err := os.Open(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			readErr = errors.Join(readErr, fmt.Errorf("reading the tests: %w", err))
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 64<<20)
		for sc.Scan() {
			var r PackageRun
			if json.Unmarshal(sc.Bytes(), &r) != nil || r.Package == "" || r.Result == "" {
				malformed++
				continue
			}
			if err := visit(r); err != nil {
				f.Close()
				return malformed, err
			}
		}
		err = sc.Err()
		f.Close()
		if err != nil {
			readErr = errors.Join(readErr, fmt.Errorf("reading %s: %w", p, err))
		}
	}
	return malformed, readErr
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

type compactKey struct{ project, pkg, test uint32 }

type timing struct {
	walls  []uint32
	failed int
	id     uint32
}

type outcome struct{ failed, passed uint32 }

type outcomeSlot struct {
	test, tree     uint32
	failed, passed uint16
}

type outcomes struct {
	slots    []outcomeSlot
	count    int
	overflow map[uint64]outcome
}

const outcomeHashFinalShift = 27

func (o *outcomes) add(pair uint64, failed, passed bool) {
	test, tree := uint32(pair>>32), uint32(pair)
	if len(o.slots) != 0 {
		i := o.find(pair, test, tree)
		if o.slots[i].test != 0 {
			o.update(pair, &o.slots[i], failed, passed)
			return
		}
	}
	if len(o.slots) == 0 || (o.count+1)*10 >= len(o.slots)*8 {
		o.grow()
	}
	i := o.find(pair, test, tree)
	s := &o.slots[i]
	s.test, s.tree = test, tree
	o.count++
	o.update(pair, s, failed, passed)
}

func (o *outcomes) find(pair uint64, test, tree uint32) uint64 {
	i := outcomeHash(pair) & uint64(len(o.slots)-1)
	for o.slots[i].test != 0 && (o.slots[i].test != test || o.slots[i].tree != tree) {
		i = (i + 1) & uint64(len(o.slots)-1)
	}
	return i
}

func (o *outcomes) update(pair uint64, slot *outcomeSlot, failed, passed bool) {
	if failed {
		o.increment(pair, slot, true)
	}
	if passed {
		o.increment(pair, slot, false)
	}
}

func (o *outcomes) increment(pair uint64, slot *outcomeSlot, failed bool) {
	count := &slot.passed
	if failed {
		count = &slot.failed
	}
	if counts, ok := o.overflow[pair]; ok {
		if failed {
			counts.failed++
		} else {
			counts.passed++
		}
		o.overflow[pair] = counts
		return
	}
	if *count < ^uint16(0) {
		(*count)++
		return
	}
	if o.overflow == nil {
		o.overflow = map[uint64]outcome{}
	}
	counts := outcome{failed: uint32(slot.failed), passed: uint32(slot.passed)}
	if failed {
		counts.failed++
	} else {
		counts.passed++
	}
	o.overflow[pair] = counts
}

func (o *outcomes) counts(slot outcomeSlot) outcome {
	pair := uint64(slot.test)<<32 | uint64(slot.tree)
	if counts, ok := o.overflow[pair]; ok {
		return counts
	}
	return outcome{failed: uint32(slot.failed), passed: uint32(slot.passed)}
}

func (o *outcomes) grow() {
	slots := make([]outcomeSlot, max(1024, len(o.slots)*2))
	for _, old := range o.slots {
		if old.test == 0 {
			continue
		}
		pair := uint64(old.test)<<32 | uint64(old.tree)
		i := outcomeHash(pair) & uint64(len(slots)-1)
		for slots[i].test != 0 {
			i = (i + 1) & uint64(len(slots)-1)
		}
		slots[i] = old
	}
	o.slots = slots
}

func outcomeHash(v uint64) uint64 {
	v ^= v >> 30
	v *= 0xbf58476d1ce4e5b9
	v ^= v >> outcomeHashFinalShift
	v *= 0x94d049bb133111eb
	return v ^ (v >> 31)
}

// testTally gathers the runs a report covers.
type testTally struct {
	tests, pkgs map[compactKey]*timing
	// byTree holds each test's results per tree.
	byTree   outcomes
	testKeys []compactKey
	nextID   uint32
	treeIDs  map[string]uint32
	names    nameInterner
	wallIDs  map[float64]uint32
	walls    []float64
	skips    map[testKey]*SkipRow
}

type nameSlot struct {
	hash uint64
	id   uint32
}

type nameInterner struct {
	slots []nameSlot
	count int
	names []string
	seed  maphash.Seed
}

func newNameInterner() nameInterner {
	return nameInterner{names: []string{""}, seed: maphash.MakeSeed()}
}

func (n *nameInterner) intern(s string) uint32 {
	if s == "" {
		return 0
	}
	h := maphash.String(n.seed, s)
	if h == 0 {
		h = 1
	}
	if len(n.slots) != 0 {
		i := n.find(h, s)
		if n.slots[i].hash != 0 {
			return n.slots[i].id
		}
	}
	if len(n.slots) == 0 || (n.count+1)*10 >= len(n.slots)*8 {
		n.grow()
	}
	i := n.find(h, s)
	id := uint32(len(n.names))
	n.names = append(n.names, s)
	n.slots[i] = nameSlot{hash: h, id: id}
	n.count++
	return id
}

func (n *nameInterner) find(hash uint64, name string) uint64 {
	i := hash & uint64(len(n.slots)-1)
	for n.slots[i].hash != 0 {
		slot := n.slots[i]
		if slot.hash == hash && n.names[slot.id] == name {
			return i
		}
		i = (i + 1) & uint64(len(n.slots)-1)
	}
	return i
}

func (n *nameInterner) grow() {
	slots := make([]nameSlot, max(1024, len(n.slots)*2))
	for _, old := range n.slots {
		if old.hash == 0 {
			continue
		}
		i := old.hash & uint64(len(slots)-1)
		for slots[i].hash != 0 {
			i = (i + 1) & uint64(len(slots)-1)
		}
		slots[i] = old
	}
	n.slots = slots
}

func (t *testTally) intern(s string) uint32 {
	return t.names.intern(s)
}

func (t *testTally) compact(k testKey) compactKey {
	return compactKey{project: t.intern(k.project), pkg: t.intern(k.pkg), test: t.intern(k.test)}
}

func (t *testTally) time(m map[compactKey]*timing, k compactKey, wall float64, failed bool) *timing {
	tm := m[k]
	if tm == nil {
		tm = &timing{}
		m[k] = tm
	}
	id, ok := t.wallIDs[wall]
	if !ok {
		id = uint32(len(t.walls))
		t.wallIDs[wall] = id
		t.walls = append(t.walls, wall)
	}
	tm.walls = append(tm.walls, id)
	if failed {
		tm.failed++
	}
	return tm
}

func (t *testTally) add(r *PackageRun, project string) {
	t.time(t.pkgs, t.compact(testKey{project, r.Package, ""}), r.WallS, r.Result != ResultPass)
	for _, tr := range r.Tests {
		if tr.Result == ResultSkip {
			t.skip(r, project, tr)
			continue
		}
		if r.Runner == "go" && strings.Contains(tr.Name, "/") {
			continue
		}
		k := t.compact(testKey{project, r.Package, tr.Name})
		failed, flaky := tr.Result == ResultFail, tr.Result == ResultFlaky
		tm := t.time(t.tests, k, tr.WallS, failed)
		if r.Tree == "" {
			continue
		}
		if tm.id == 0 {
			t.nextID++
			tm.id = t.nextID
			t.testKeys = append(t.testKeys, k)
		}
		testID := tm.id
		treeID := t.treeIDs[r.Tree]
		if treeID == 0 {
			treeID = uint32(len(t.treeIDs) + 1)
			t.treeIDs[r.Tree] = treeID
		}
		pair := uint64(testID)<<32 | uint64(treeID)
		t.byTree.add(pair, failed || flaky, !failed || flaky)
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
		s.Runs = len(t.pkgs[compactKey{t.intern(k.project), t.intern(k.pkg), 0}].walls)
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
func summarizeTests(runs []PackageRun, f TestFilter, limit int) TestReport {
	t := newTestTally()
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
	return t.report(limit)
}

// SummarizeTestFile reads package runs one at a time and retains only the
// values needed to build the report. earliest is the first valid run in the
// files, including runs excluded by the filter.
func SummarizeTestFile(path string, f TestFilter, limit int) (rep TestReport, earliest time.Time, malformed int, err error) {
	t := newTestTally()
	malformed, err = ReadTests(path, f.Since, func(r PackageRun) error {
		if earliest.IsZero() || r.Time.Before(earliest) {
			earliest = r.Time
		}
		if !f.takes(&r) {
			return nil
		}
		project := ""
		if f.Project == "" {
			project = r.Project
		}
		t.add(&r, project)
		return nil
	})
	if err != nil {
		return TestReport{}, time.Time{}, malformed, err
	}
	return t.report(limit), earliest, malformed, nil
}

func newTestTally() *testTally {
	return &testTally{tests: map[compactKey]*timing{}, pkgs: map[compactKey]*timing{}, treeIDs: map[string]uint32{}, names: newNameInterner(), wallIDs: map[float64]uint32{}, skips: map[testKey]*SkipRow{}}
}

func (t *testTally) report(limit int) TestReport {
	rep := TestReport{Tests: t.slowest(t.tests, limit), Packages: t.slowest(t.pkgs, limit), Flaky: t.flaky()}
	rep.Skipped, rep.SkippedAll = t.mostSkipped(limit)
	return rep
}

func (t *testTally) slowest(m map[compactKey]*timing, limit int) []TimingRow {
	var out []TimingRow
	for k, timing := range m {
		out = append(out, TimingRow{Project: t.names.names[k.project], Package: t.names.names[k.pkg], Test: t.names.names[k.test], Runs: len(timing.walls),
			WallMed: medianIDs(timing.walls, t.walls), WallMax: maxID(timing.walls, t.walls), Failed: timing.failed})
	}
	slices.SortFunc(out, func(a, b TimingRow) int {
		if c := cmpFloat(b.WallMed, a.WallMed); c != 0 {
			return c
		}
		return strings.Compare(testKey{a.Project, a.Package, a.Test}.String(), testKey{b.Project, b.Package, b.Test}.String())
	})
	return out[:min(len(out), limit)]
}

func medianIDs(ids []uint32, walls []float64) float64 {
	slices.SortFunc(ids, func(a, b uint32) int { return cmpFloat(walls[a], walls[b]) })
	n := len(ids)
	if n%2 == 1 {
		return walls[ids[n/2]]
	}
	return (walls[ids[n/2-1]] + walls[ids[n/2]]) / 2
}

func maxID(ids []uint32, walls []float64) float64 {
	max := walls[ids[0]]
	for _, id := range ids[1:] {
		if walls[id] > max {
			max = walls[id]
		}
	}
	return max
}

// flaky are the tests that both failed and passed on some tree, the most
// trees first.
func (t *testTally) flaky() []FlakyRow {
	rowsByID := map[uint32]*FlakyRow{}
	for _, slot := range t.byTree.slots {
		if slot.test == 0 {
			continue
		}
		o := t.byTree.counts(slot)
		if o.failed == 0 || o.passed == 0 {
			continue
		}
		id := slot.test
		row := rowsByID[id]
		if row == nil {
			k := t.testKeys[id-1]
			row = &FlakyRow{Project: t.names.names[k.project], Package: t.names.names[k.pkg], Test: t.names.names[k.test]}
			rowsByID[id] = row
		}
		row.Trees++
		row.Failed += int(o.failed)
		row.Passed += int(o.passed)
	}
	out := make([]FlakyRow, 0, len(rowsByID))
	for _, row := range rowsByID {
		out = append(out, *row)
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
