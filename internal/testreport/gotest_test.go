package testreport

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/stats"
)

// feed writes the stream in small pieces, as a pipe delivers it.
func feed(t *testing.T, g *GoTest, stream string) []stats.PackageRun {
	t.Helper()
	for len(stream) > 0 {
		n := min(len(stream), 7)
		if _, err := g.Write([]byte(stream[:n])); err != nil {
			t.Fatal(err)
		}
		stream = stream[n:]
	}
	runs, err := g.Close()
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func summary(runs []stats.PackageRun) string {
	var b strings.Builder
	for _, r := range runs {
		tests, _ := json.Marshal(r.Tests)
		fmt.Fprintf(&b, "%s %s %s\n", r.Package, r.Result, tests)
	}
	return b.String()
}

// TestGoTestPrintsWhatPlainGoTestPrints: a stream recorded from go test
// -json (a passing, a failing, a skipped test, a failing subtest, a panic and
// a package that does not build) prints what go test without -json printed
// for the same packages.
func TestGoTestPrintsWhatPlainGoTestPrints(t *testing.T) {
	stream, err := os.ReadFile("testdata/go-test.json")
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	runs := feed(t, NewGoTest(&out, false), string(stream))
	for _, want := range []string{
		"# toy/c [toy/c.test]\nc/c_test.go:3:28: undefined: undefined\nFAIL\ttoy/c [build failed]\n",
		"    a_test.go:4: before\n    a_test.go:4: boom\n--- FAIL: TestFail (0.00s)\n",
		"    a_test.go:8: sub boom\n--- FAIL: TestSub/bad (0.00s)\n--- FAIL: TestSub (0.00s)\nFAIL\nFAIL\ttoy/a\t0.004s\n",
		"--- FAIL: TestPanic (0.00s)\npanic: oops [recovered, repanicked]\n",
		"created by testing.(*T).Run in goroutine 1\n",
		"FAIL\ttoy/b\t0.005s\n",
		"FAIL\ttoy/a\t0.004s\nskipped 1 test in toy/a:\n    TestSkip: a_test.go:5: later\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	for _, absent := range []string{"=== RUN", "hello", "--- PASS", "--- SKIP"} {
		if strings.Contains(out.String(), absent) {
			t.Errorf("output shows %q:\n%s", absent, out.String())
		}
	}
	if !strings.HasSuffix(out.String(), "\nskipped 1 test in 1 package\n") {
		t.Errorf("output does not end in the count of skipped tests:\n%s", out.String())
	}
	want := `toy/c build-fail null
toy/a fail [["TestPass",0,"pass"],["TestFail",0,"fail","a_test.go:4: before\na_test.go:4: boom"],["TestSkip",0,"skip","a_test.go:5: later"],["TestSub/bad",0,"fail","a_test.go:8: sub boom"],["TestSub",0,"fail"]]
toy/b fail [["TestOK",0,"pass"],["TestPanic",0,"fail","panic: oops [recovered, repanicked]\ngoroutine 8 [running]:\ntesting.tRunner.func1.2({0x550fc0, 0x594560})\n/usr/local/go/src/testing/testing.go:1974 +0x232\ntesting.tRunner.func1()\n/usr/local/go/src/testing/testing.go:1977 +0x349\npanic({0x550fc0?, 0x594560?})\n/usr/local/go/src/runtime/panic.go:860 +0x13a\ntoy/b.TestPanic(0xbec77926488?)\n/src/toy/b/b_test.go:4 +0x25"]]
`
	if got := summary(runs); got != want {
		t.Errorf("runs:\n%s\nwant\n%s", got, want)
	}
}

// TestGoTestVerbose: with -v every line of output shows, the passing tests'
// too.
func TestGoTestFailureReasonsAndPackageFailure(t *testing.T) {
	stream := strings.Join([]string{
		`{"Action":"start","Package":"m/testmain"}`,
		`{"Action":"output","Package":"m/testmain","Output":"panic: setup failed\nstack line\n"}`,
		`{"Action":"fail","Package":"m/testmain","Elapsed":0.01}`,
		`{"Action":"start","Package":"m/tests"}`,
		`{"Action":"run","Package":"m/tests","Test":"TestBad"}`,
		`{"Action":"output","Package":"m/tests","Test":"TestBad","Output":"=== RUN   TestBad\n"}`,
		`{"Action":"output","Package":"m/tests","Test":"TestBad","Output":"bad assertion\n"}`,
		`{"Action":"output","Package":"m/tests","Test":"TestBad","Output":"--- FAIL: TestBad (0.00s)\n"}`,
		`{"Action":"fail","Package":"m/tests","Test":"TestBad"}`,
		`{"Action":"fail","Package":"m/tests","Elapsed":0.01}`,
	}, "\n") + "\n"
	runs := feed(t, NewGoTest(&strings.Builder{}, false), stream)
	if len(runs) != 2 || runs[0].Reason != "panic: setup failed\nstack line" {
		t.Fatalf("package reason not kept: %+v", runs)
	}
	if len(runs[1].Tests) != 1 || runs[1].Tests[0].Reason != "bad assertion" || runs[1].Reason != "" {
		t.Fatalf("test output not kept separately: %+v", runs[1])
	}
}

func TestGoTestVerbose(t *testing.T) {
	stream, err := os.ReadFile("testdata/go-test.json")
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	feed(t, NewGoTest(&out, true), string(stream))
	for _, want := range []string{"=== RUN   TestPass\n    a_test.go:3: hello\n--- PASS: TestPass (0.00s)\n", "--- SKIP: TestSkip", "=== RUN   TestSub/ok\n--- PASS: TestSub/ok",
		"skipped 1 test in toy/a:\n    TestSkip: a_test.go:5: later\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("verbose output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestGoTestBenchmarksAndCutStreams: a benchmark's results show though it
// has no result event, and a stream cut mid-test (the process killed) fails
// the package and the test that was running.
func TestGoTestBenchmarksAndCutStreams(t *testing.T) {
	stream := strings.Join([]string{
		`{"Action":"start","Package":"m/a"}`,
		`{"Action":"output","Package":"m/a","Output":"goos: linux\n"}`,
		`{"Action":"run","Package":"m/a","Test":"BenchmarkX"}`,
		`{"Action":"output","Package":"m/a","Test":"BenchmarkX","Output":"=== RUN   BenchmarkX\n"}`,
		`{"Action":"output","Package":"m/a","Test":"BenchmarkX","Output":"BenchmarkX\n"}`,
		`{"Action":"output","Package":"m/a","Test":"BenchmarkX","Output":"BenchmarkX-8 \t 100\t 4.7 ns/op\n"}`,
		`{"Action":"output","Package":"m/a","Output":"PASS\n"}`,
		`{"Action":"output","Package":"m/a","Output":"ok  \tm/a\t0.004s\n"}`,
		`{"Action":"pass","Package":"m/a","Elapsed":0.004}`,
		`{"Action":"start","Package":"m/b"}`,
		`{"Action":"run","Package":"m/b","Test":"TestHangs"}`,
		`{"Action":"output","Package":"m/b","Test":"TestHangs","Output":"    b_test.go:5: waiting\n"}`,
	}, "\n") // no newline after the last line either
	var out strings.Builder
	runs := feed(t, NewGoTest(&out, false), stream)
	if want := "goos: linux\nBenchmarkX-8 \t 100\t 4.7 ns/op\nok  \tm/a\t0.004s\n    b_test.go:5: waiting\n"; out.String() != want {
		t.Errorf("output:\n%q\nwant\n%q", out.String(), want)
	}
	if got := summary(runs); got != "m/b fail [[\"TestHangs\",0,\"fail\",\"b_test.go:5: waiting\"]]\n" {
		t.Errorf("runs:\n%s", got)
	}
}

// TestGoTestNamesSkippedTests: the skipped tests of a package, a subtest of
// a table and one that gave no reason among them, are listed under the
// package line, a cached package's too, and counted at the end; the
// subtest is recorded with the top-level tests.
func TestGoTestNamesSkippedTests(t *testing.T) {
	stream := strings.Join([]string{
		`{"Action":"start","Package":"m/a"}`,
		`{"Action":"run","Package":"m/a","Test":"TestCorpus"}`,
		`{"Action":"output","Package":"m/a","Test":"TestCorpus","Output":"=== RUN   TestCorpus\n"}`,
		`{"Action":"output","Package":"m/a","Test":"TestCorpus","Output":"    a_test.go:7: reading testdata/corpus\n"}`,
		`{"Action":"output","Package":"m/a","Test":"TestCorpus","Output":"    a_test.go:9: no corpus: run make corpus\n"}`,
		`{"Action":"output","Package":"m/a","Test":"TestCorpus","Output":"--- SKIP: TestCorpus (0.00s)\n"}`,
		`{"Action":"skip","Package":"m/a","Test":"TestCorpus","Elapsed":0}`,
		`{"Action":"run","Package":"m/a","Test":"TestTable"}`,
		`{"Action":"run","Package":"m/a","Test":"TestTable/ok"}`,
		`{"Action":"pass","Package":"m/a","Test":"TestTable/ok","Elapsed":0}`,
		`{"Action":"run","Package":"m/a","Test":"TestTable/traces"}`,
		`{"Action":"output","Package":"m/a","Test":"TestTable/traces","Output":"    --- SKIP: TestTable/traces (0.00s)\n"}`,
		`{"Action":"skip","Package":"m/a","Test":"TestTable/traces","Elapsed":0}`,
		`{"Action":"pass","Package":"m/a","Test":"TestTable","Elapsed":0.1}`,
		`{"Action":"output","Package":"m/a","Output":"ok  \tm/a\t0.104s\n"}`,
		`{"Action":"pass","Package":"m/a","Elapsed":0.104}`,
		`{"Action":"start","Package":"m/b"}`,
		`{"Action":"run","Package":"m/b","Test":"TestLive"}`,
		`{"Action":"output","Package":"m/b","Test":"TestLive","Output":"    b_test.go:3: set B_LIVE to run\n"}`,
		`{"Action":"output","Package":"m/b","Test":"TestLive","Output":"--- SKIP: TestLive (0.00s)\n"}`,
		`{"Action":"skip","Package":"m/b","Test":"TestLive","Elapsed":0}`,
		`{"Action":"output","Package":"m/b","Output":"ok  \tm/b\t(cached)\n"}`,
		`{"Action":"pass","Package":"m/b","Elapsed":0}`,
	}, "\n") + "\n"
	var out strings.Builder
	runs := feed(t, NewGoTest(&out, false), stream)
	want := "ok  \tm/a\t0.104s\n" +
		"skipped 2 tests in m/a:\n" +
		"    TestCorpus: a_test.go:9: no corpus: run make corpus\n" +
		"    TestTable/traces: no reason given\n" +
		"ok  \tm/b\t(cached)\n" +
		"skipped 1 test in m/b:\n" +
		"    TestLive: b_test.go:3: set B_LIVE to run\n" +
		"skipped 3 tests in 2 packages\n"
	if out.String() != want {
		t.Errorf("output:\n%s\nwant\n%s", out.String(), want)
	}
	if got := summary(runs); got != `m/a pass [["TestCorpus",0,"skip","a_test.go:9: no corpus: run make corpus"],["TestTable/traces",0,"skip"],["TestTable",0.1,"pass"]]`+"\n" {
		t.Errorf("runs:\n%s", got)
	}
}

// TestGoTestSkipReasonAmongParallelOutput: a stream recorded from go test
// -json, where a parallel test writes to stderr before and after the
// skipped one's --- SKIP line and go test gives those lines to the skipped
// test. Its reason is its t.Skipf message, the continuation line included,
// not its earlier t.Log nor the neighbour's stderr.
func TestGoTestSkipReasonAmongParallelOutput(t *testing.T) {
	stream, err := os.ReadFile("testdata/go-test-skip-parallel.json")
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	runs := feed(t, NewGoTest(&out, false), string(stream))
	if want := "    TestDebt: a_test.go:22: debt (candidate groundPick):; second line\n"; !strings.Contains(out.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, out.String())
	}
	if got := summary(runs); !strings.Contains(got, `["TestDebt",0.01,"skip","a_test.go:22: debt (candidate groundPick):; second line"]`) {
		t.Errorf("runs:\n%s", got)
	}
}

// A stream through a terminal (ssh -tt) ends its lines with \r\n and carries
// the remote graft's own stderr lines between the events: the runs are the
// same as from the plain stream.
func TestGoTestThroughATerminal(t *testing.T) {
	stream, err := os.ReadFile("testdata/go-test.json")
	if err != nil {
		t.Fatal(err)
	}
	plain := feed(t, NewGoTest(&strings.Builder{}, false), string(stream))
	tty := "graft: waiting for the write lock\r\n" + strings.ReplaceAll(string(stream), "\n", "\r\n") + "graft: done\r\n"
	var out strings.Builder
	got := feed(t, NewGoTest(&out, false), tty)
	for i := range got {
		got[i].Time = time.Time{}
	}
	for i := range plain {
		plain[i].Time = time.Time{}
	}
	if !reflect.DeepEqual(got, plain) {
		t.Fatalf("runs through a terminal differ:\n got %+v\nwant %+v", got, plain)
	}
	if !strings.Contains(out.String(), "graft: waiting for the write lock") {
		t.Fatalf("graft's own line not printed:\n%s", out.String())
	}
}
