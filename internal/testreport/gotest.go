// Package testreport reads the machine-readable reports test runners print,
// prints what a person would have seen without them and returns the results
// of every test for the stats.
package testreport

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/stats"
)

// GoTest reads a go test -json stream written to it. It prints what plain
// go test prints: package lines, build errors, benchmark results and the
// output of failing tests, or, verbose, every line of output. Lines that are
// not events pass through as they are.
type GoTest struct {
	out     io.Writer
	verbose bool
	partial []byte
	pkgs    map[string]*goPackage
	order   []string
	runs    []stats.PackageRun
	events  int
	err     error
}

type goPackage struct {
	tests map[string]*goTest
	// seen are the tests in the order they first appeared.
	seen    []string
	results []stats.TestResult
	cached  bool
}

type goTest struct {
	lines []string
	done  bool
	// failed is set once the test failed: output that comes after its
	// result (a panic's stack) is still shown.
	failed bool
}

// event is a line of go test -json (cmd/test2json).
type event struct {
	Action      string
	Package     string
	Test        string
	Output      string
	Elapsed     float64
	FailedBuild string
}

// NewGoTest prints to out; verbose prints every line of output, as go test
// -v does.
func NewGoTest(out io.Writer, verbose bool) *GoTest {
	return &GoTest{out: out, verbose: verbose, pkgs: map[string]*goPackage{}}
}

// Write takes the stream in pieces of any size.
func (g *GoTest) Write(p []byte) (int, error) {
	g.partial = append(g.partial, p...)
	for {
		i := bytes.IndexByte(g.partial, '\n')
		if i < 0 {
			break
		}
		g.line(g.partial[:i+1])
		g.partial = g.partial[i+1:]
	}
	if g.err != nil {
		return 0, g.err
	}
	return len(p), nil
}

// Events is how many events the stream held.
func (g *GoTest) Events() int { return g.events }

// Close reads what is left of the stream and returns the package runs, the
// cached ones left out: they did not run. A package the stream left
// unfinished, its process killed, counts as failed.
func (g *GoTest) Close() ([]stats.PackageRun, error) {
	if len(g.partial) > 0 {
		g.line(append(g.partial, '\n'))
		g.partial = nil
	}
	for _, name := range g.order {
		if g.pkgs[name] != nil {
			g.finish(name, event{Action: stats.ResultFail})
		}
	}
	return g.runs, g.err
}

func (g *GoTest) print(s string) {
	if g.err == nil && s != "" {
		_, g.err = io.WriteString(g.out, s)
	}
}

func (g *GoTest) line(raw []byte) {
	var e event
	if raw[0] != '{' || json.Unmarshal(raw, &e) != nil || e.Action == "" {
		g.print(string(raw))
		return
	}
	g.events++
	if e.Action == "build-output" {
		g.print(e.Output)
		return
	}
	if e.Package == "" {
		return // build-fail: the package's own fail event follows
	}
	pkg := g.pkg(e.Package)
	if e.Test == "" {
		switch e.Action {
		case "output":
			g.packageOutput(pkg, e.Output)
		case stats.ResultPass, stats.ResultFail, stats.ResultSkip:
			g.finish(e.Package, e)
		}
		return
	}
	t := pkg.test(e.Test)
	switch e.Action {
	case "output":
		g.testOutput(e.Test, t, e.Output)
	case stats.ResultPass, stats.ResultFail, stats.ResultSkip:
		g.result(pkg, e.Test, e.Elapsed, e.Action)
	}
}

func (g *GoTest) pkg(name string) *goPackage {
	pkg := g.pkgs[name]
	if pkg == nil {
		pkg = &goPackage{tests: map[string]*goTest{}}
		g.pkgs[name] = pkg
		g.order = append(g.order, name)
	}
	return pkg
}

func (pkg *goPackage) test(name string) *goTest {
	t := pkg.tests[name]
	if t == nil {
		t = &goTest{}
		pkg.tests[name] = t
		pkg.seen = append(pkg.seen, name)
	}
	return t
}

func (g *GoTest) packageOutput(pkg *goPackage, out string) {
	if strings.HasPrefix(out, "ok") && strings.Contains(out, "(cached)") {
		pkg.cached = true
	}
	if !g.verbose && (out == "PASS\n" || out == "testing: warning: no tests to run\n") {
		return // plain go test does not print them
	}
	g.print(out)
}

func (g *GoTest) testOutput(name string, t *goTest, out string) {
	bench := strings.HasPrefix(name, "Benchmark")
	switch {
	case g.verbose:
		g.print(out)
	case t.done:
		if t.failed {
			g.print(out)
		}
	case strings.HasPrefix(out, "=== "), bench && out == name+"\n":
		// The framing of -v, which plain go test does not print.
	case bench:
		// A benchmark has no result event: its lines are its results.
		g.print(out)
	default:
		t.lines = append(t.lines, out)
	}
}

// result ends a test: a failed one shows its output. Every top-level test is
// kept, and the subtests that failed, whose names tell which case of a table
// broke.
func (g *GoTest) result(pkg *goPackage, name string, elapsed float64, result string) {
	t := pkg.test(name)
	t.done, t.failed = true, result == stats.ResultFail
	if t.failed && !g.verbose {
		g.print(strings.Join(t.lines, ""))
	}
	t.lines = nil
	if !strings.Contains(name, "/") || t.failed {
		pkg.results = append(pkg.results, stats.TestResult{Name: name, WallS: round(elapsed), Result: result})
	}
}

// finish closes a package on its result event.
func (g *GoTest) finish(name string, e event) {
	pkg := g.pkgs[name]
	delete(g.pkgs, name)
	run := newRun(name, e.Action, e.Elapsed)
	if e.FailedBuild != "" {
		run.Result = stats.ResultBuildFail
	}
	if run.Result != stats.ResultPass {
		// Tests cut off by a panic, a timeout or a killed process failed.
		for _, test := range pkg.seen {
			if t := pkg.tests[test]; !t.done && !strings.HasPrefix(test, "Benchmark") {
				g.result(pkg, test, 0, stats.ResultFail)
			}
		}
	}
	run.Tests = pkg.results
	if pkg.cached || run.Result == stats.ResultSkip || (run.Result == stats.ResultPass && len(run.Tests) == 0) {
		return
	}
	g.runs = append(g.runs, run)
}

func round(s float64) float64 { return math.Round(s*1000) / 1000 }

// newRun is a package run finished now; the runner fills where it ran.
func newRun(pkg, result string, wallS float64) stats.PackageRun {
	return stats.PackageRun{Time: time.Now(), Package: pkg, Result: result, WallS: round(wallS)}
}
