package testreport

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

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
	want := `toy/c build-fail null
toy/a fail [["TestPass",0,"pass"],["TestFail",0,"fail"],["TestSkip",0,"skip"],["TestSub/bad",0,"fail"],["TestSub",0,"fail"]]
toy/b fail [["TestOK",0,"pass"],["TestPanic",0,"fail"]]
`
	if got := summary(runs); got != want {
		t.Errorf("runs:\n%s\nwant\n%s", got, want)
	}
}

// TestGoTestVerbose: with -v every line of output shows, the passing tests'
// too.
func TestGoTestVerbose(t *testing.T) {
	stream, err := os.ReadFile("testdata/go-test.json")
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	feed(t, NewGoTest(&out, true), string(stream))
	for _, want := range []string{"=== RUN   TestPass\n    a_test.go:3: hello\n--- PASS: TestPass (0.00s)\n", "--- SKIP: TestSkip", "=== RUN   TestSub/ok\n--- PASS: TestSub/ok"} {
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
	if got := summary(runs); got != "m/b fail [[\"TestHangs\",0,\"fail\"]]\n" {
		t.Errorf("runs:\n%s", got)
	}
}
