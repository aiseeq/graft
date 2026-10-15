package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// goTestStream builds a go test -json stream: one event per line.
func goTestStream(events ...string) string {
	return strings.Join(events, "\n") + "\n"
}

// A failing run: TestFlaky fails, a subtest fails, a package does not build,
// a cached package replays, and a line that is no event passes through.
var failingRun = goTestStream(
	`{"ImportPath":"example.com/q [example.com/q.test]","Action":"build-output","Output":"# example.com/q [example.com/q.test]\n"}`,
	`{"ImportPath":"example.com/q [example.com/q.test]","Action":"build-output","Output":"q/q_test.go:3:28: undefined: missing\n"}`,
	`{"ImportPath":"example.com/q [example.com/q.test]","Action":"build-fail"}`,
	`not an event: a step may print other lines`,
	`{"Action":"start","Package":"example.com/p"}`,
	`{"Action":"start","Package":"example.com/q"}`,
	`{"Action":"output","Package":"example.com/q","Output":"FAIL\texample.com/q [build failed]\n"}`,
	`{"Action":"fail","Package":"example.com/q","Elapsed":0,"FailedBuild":"example.com/q [example.com/q.test]"}`,
	`{"Action":"run","Package":"example.com/p","Test":"TestSlow"}`,
	`{"Action":"output","Package":"example.com/p","Test":"TestSlow","Output":"=== RUN   TestSlow\n"}`,
	`{"Action":"output","Package":"example.com/p","Test":"TestSlow","Output":"    p_test.go:3: quiet when passing\n"}`,
	`{"Action":"output","Package":"example.com/p","Test":"TestSlow","Output":"--- PASS: TestSlow (2.50s)\n"}`,
	`{"Action":"pass","Package":"example.com/p","Test":"TestSlow","Elapsed":2.5}`,
	`{"Action":"run","Package":"example.com/p","Test":"TestFlaky"}`,
	`{"Action":"output","Package":"example.com/p","Test":"TestFlaky","Output":"=== RUN   TestFlaky\n"}`,
	`{"Action":"output","Package":"example.com/p","Test":"TestFlaky","Output":"    p_test.go:9: boom\n"}`,
	`{"Action":"output","Package":"example.com/p","Test":"TestFlaky","Output":"--- FAIL: TestFlaky (0.10s)\n"}`,
	`{"Action":"fail","Package":"example.com/p","Test":"TestFlaky","Elapsed":0.1}`,
	`{"Action":"run","Package":"example.com/p","Test":"TestTable"}`,
	`{"Action":"output","Package":"example.com/p","Test":"TestTable","Output":"=== RUN   TestTable\n"}`,
	`{"Action":"run","Package":"example.com/p","Test":"TestTable/ok"}`,
	`{"Action":"output","Package":"example.com/p","Test":"TestTable/ok","Output":"=== RUN   TestTable/ok\n"}`,
	`{"Action":"output","Package":"example.com/p","Test":"TestTable/ok","Output":"--- PASS: TestTable/ok (0.00s)\n"}`,
	`{"Action":"pass","Package":"example.com/p","Test":"TestTable/ok","Elapsed":0}`,
	`{"Action":"run","Package":"example.com/p","Test":"TestTable/bad"}`,
	`{"Action":"output","Package":"example.com/p","Test":"TestTable/bad","Output":"=== RUN   TestTable/bad\n"}`,
	`{"Action":"output","Package":"example.com/p","Test":"TestTable/bad","Output":"    p_test.go:14: sub boom\n"}`,
	`{"Action":"output","Package":"example.com/p","Test":"TestTable/bad","Output":"--- FAIL: TestTable/bad (0.00s)\n"}`,
	`{"Action":"fail","Package":"example.com/p","Test":"TestTable/bad","Elapsed":0}`,
	`{"Action":"output","Package":"example.com/p","Test":"TestTable","Output":"--- FAIL: TestTable (0.00s)\n"}`,
	`{"Action":"fail","Package":"example.com/p","Test":"TestTable","Elapsed":0}`,
	`{"Action":"run","Package":"example.com/p","Test":"TestLater"}`,
	`{"Action":"output","Package":"example.com/p","Test":"TestLater","Output":"--- SKIP: TestLater (0.00s)\n"}`,
	`{"Action":"skip","Package":"example.com/p","Test":"TestLater","Elapsed":0}`,
	`{"Action":"output","Package":"example.com/p","Output":"FAIL\n"}`,
	`{"Action":"output","Package":"example.com/p","Output":"FAIL\texample.com/p\t2.700s\n"}`,
	`{"Action":"fail","Package":"example.com/p","Elapsed":2.7}`,
	`{"Action":"start","Package":"example.com/r"}`,
	`{"Action":"run","Package":"example.com/r","Test":"TestCached"}`,
	`{"Action":"output","Package":"example.com/r","Test":"TestCached","Output":"--- PASS: TestCached (9.00s)\n"}`,
	`{"Action":"pass","Package":"example.com/r","Test":"TestCached","Elapsed":9}`,
	`{"Action":"output","Package":"example.com/r","Output":"ok  \texample.com/r\t(cached)\n"}`,
	`{"Action":"pass","Package":"example.com/r","Elapsed":0}`,
)

// The same tests passing.
var passingRun = goTestStream(
	`{"Action":"start","Package":"example.com/p"}`,
	`{"Action":"run","Package":"example.com/p","Test":"TestSlow"}`,
	`{"Action":"pass","Package":"example.com/p","Test":"TestSlow","Elapsed":3.5}`,
	`{"Action":"run","Package":"example.com/p","Test":"TestFlaky"}`,
	`{"Action":"pass","Package":"example.com/p","Test":"TestFlaky","Elapsed":0.2}`,
	`{"Action":"run","Package":"example.com/p","Test":"TestTable"}`,
	`{"Action":"pass","Package":"example.com/p","Test":"TestTable","Elapsed":0}`,
	`{"Action":"output","Package":"example.com/p","Output":"PASS\n"}`,
	`{"Action":"output","Package":"example.com/p","Output":"ok  \texample.com/p\t3.800s\n"}`,
	`{"Action":"pass","Package":"example.com/p","Elapsed":3.8}`,
)

// testsLine is one line of tests.jsonl as the tests read it.
type testsLine struct {
	Project string   `json:"project"`
	Task    string   `json:"task"`
	Top     string   `json:"top"`
	Tree    string   `json:"tree"`
	Runner  string   `json:"runner"`
	Package string   `json:"package"`
	Result  string   `json:"result"`
	WallS   float64  `json:"wall_s"`
	Tests   [][]any  `json:"tests"`
	LoadMax *float64 `json:"load_max"`
}

func readTests(t *testing.T, path string) []testsLine {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var lines []testsLine
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		var l testsLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		lines = append(lines, l)
	}
	return lines
}

// TestGoTestsAreRecordedPerTest: a task with tests: go reads its steps'
// go test -json stream, prints it the way plain go test does and records
// every package run with its top-level tests (subtests when they fail);
// graft stats --tests ranks the slow ones and names a test that failed and
// passed on the same tree.
func TestGoTestsAreRecordedPerTest(t *testing.T) {
	stream := filepath.Join(t.TempDir(), "stream.json")
	writeFile(t, stream, failingRun)
	f, statsFile := statsRepo(t, "tasks:\n  test:\n    tests: go\n    run: ["+helperStep("cat", stream, "1")+"]\n  test-ok:\n    tests: go\n    run: ["+helperStep("cat", stream)+"]\n")
	testsFile := filepath.Join(filepath.Dir(statsFile), "tests.jsonl")

	out, code := f.graft("", "test")
	if code == 0 {
		t.Fatalf("the failing run passed:\n%s", out)
	}
	for _, want := range []string{
		"# example.com/q [example.com/q.test]\nq/q_test.go:3:28: undefined: missing\n",
		"not an event: a step may print other lines\n",
		"FAIL\texample.com/q [build failed]\n",
		"    p_test.go:9: boom\n--- FAIL: TestFlaky (0.10s)\n",
		"    p_test.go:14: sub boom\n--- FAIL: TestTable/bad (0.00s)\n",
		"--- FAIL: TestTable (0.00s)\n",
		"FAIL\texample.com/p\t2.700s\n",
		"ok  \texample.com/r\t(cached)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, absent := range []string{"=== RUN", "quiet when passing", "--- PASS", "--- SKIP", `"Action"`} {
		if strings.Contains(out, absent) {
			t.Errorf("output shows %q:\n%s", absent, out)
		}
	}

	lines := readTests(t, testsFile)
	if len(lines) != 2 {
		t.Fatalf("want the runs of p and q, the cached r left out: %+v", lines)
	}
	q, p := lines[0], lines[1]
	if q.Package != "example.com/q" || q.Result != "build-fail" || len(q.Tests) != 0 {
		t.Errorf("q: %+v", q)
	}
	if runtime.GOOS == "linux" && p.LoadMax == nil {
		t.Errorf("p carries no load: %+v", p)
	}
	if p.Package != "example.com/p" || p.Result != "fail" || p.WallS != 2.7 || p.Runner != "go" ||
		p.Task != "test" || p.Top != "test" || p.Project != realRoot(t, f) || len(p.Tree) < 40 {
		t.Errorf("p: %+v", p)
	}
	var got []string
	for _, tc := range p.Tests {
		data, _ := json.Marshal(tc)
		got = append(got, string(data))
	}
	want := `["TestSlow",2.5,"pass"] ["TestFlaky",0.1,"fail"] ["TestTable/bad",0,"fail"] ["TestTable",0,"fail"] ["TestLater",0,"skip"]`
	if strings.Join(got, " ") != want {
		t.Errorf("tests of p:\n%s\nwant\n%s", strings.Join(got, " "), want)
	}

	// The same tree passes: TestFlaky failed and passed on it.
	writeFile(t, stream, passingRun)
	if out, code := f.graft("", "test-ok"); code != 0 || !strings.Contains(out, "ok  \texample.com/p\t3.800s") || strings.Contains(out, "PASS\n") {
		t.Fatalf("the passing run: exit %d\n%s", code, out)
	}
	if lines := readTests(t, testsFile); len(lines) != 3 || lines[2].Tree != p.Tree || lines[2].Task != "test-ok" {
		t.Fatalf("the passing run on the same tree: %+v", lines)
	}
	// A changed tree fails TestFlaky again: a change may break a test, so
	// that alone is no flake.
	f.write("new.txt", "x\n")
	writeFile(t, stream, failingRun)
	f.graft("", "test")
	if lines := readTests(t, testsFile); len(lines) != 5 || lines[4].Tree == p.Tree {
		t.Fatalf("the run on a changed tree: %+v", lines)
	}

	rows := func(out string) []string {
		var rows []string
		for l := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
			if l = strings.Join(strings.Fields(l), " "); l != "" && !strings.HasPrefix(l, "graft:") {
				rows = append(rows, l)
			}
		}
		return rows
	}
	report := f.mustGraft("stats", "--tests")
	wantRows := []string{
		"slow tests",
		"PACKAGE TEST RUNS WALL MED WALL MAX FAILED",
		"example.com/p TestSlow 3 2.5s 3.5s 0%",
		"example.com/p TestFlaky 3 100ms 200ms 67%",
		"example.com/p TestTable 3 0s 0s 67%",
		"slow packages",
		"PACKAGE RUNS WALL MED WALL MAX FAILED",
		"example.com/p 3 2.7s 3.8s 67%",
		"example.com/q 2 0s 0s 100%",
		"flaky: failed and passed on the same tree",
		"PACKAGE TEST TREES FAILED PASSED",
		"example.com/p TestFlaky 1 1 1",
		"example.com/p TestTable 1 1 1",
	}
	if strings.Join(rows(report), "\n") != strings.Join(wantRows, "\n") {
		t.Errorf("graft stats --tests:\n%s\nwant\n%s", strings.Join(rows(report), "\n"), strings.Join(wantRows, "\n"))
	}
	if got := rows(f.mustGraft("stats", "--tests", "--task", "test-ok")); len(got) < 3 || got[2] != "example.com/p TestSlow 1 3.5s 3.5s 0%" {
		t.Errorf("--task test-ok: %q", got)
	}
	if got := rows(f.mustGraft("stats", "--tests", "--all")); len(got) < 3 || got[2] != realRoot(t, f)+" example.com/p TestSlow 3 2.5s 3.5s 0%" {
		t.Errorf("--all: %q", got)
	}
}
