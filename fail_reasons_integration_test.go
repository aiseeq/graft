package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFailReasonsInTestsJournal: a failed test keeps the first lines of
// what it printed, without the RUN and FAIL framing; a package that did not
// build keeps its build errors.
func TestFailReasonsInTestsJournal(t *testing.T) {
	stream := filepath.Join(t.TempDir(), "stream.json")
	writeFile(t, stream, failingRun)
	f, statsFile := statsRepo(t, "tasks:\n  test:\n    tests: go\n    run: ["+helperStep("cat", stream, "1")+"]\n")
	if out, code := f.graft("", "test"); code == 0 {
		t.Fatalf("the failing run passed:\n%s", out)
	}
	reasons := map[string]string{}
	var buildReason string
	for _, l := range readTestsRaw(t, filepath.Join(filepath.Dir(statsFile), "tests.jsonl")) {
		if l.Package == "example.com/q" {
			buildReason = l.Reason
		}
		for _, tr := range l.Tests {
			if len(tr) == 4 && tr[2] == "fail" {
				reasons[l.Package+" "+tr[0].(string)] = tr[3].(string)
			}
		}
	}
	for key, want := range map[string]string{
		"example.com/p TestFlaky":     "p_test.go:9: boom",
		"example.com/p TestTable/bad": "p_test.go:14: sub boom",
	} {
		got := reasons[key]
		if !strings.Contains(got, want) {
			t.Errorf("%s: reason %q, want it to hold %q", key, got, want)
		}
		if strings.Contains(got, "=== RUN") || strings.Contains(got, "--- FAIL") {
			t.Errorf("%s: reason keeps the framing: %q", key, got)
		}
	}
	if !strings.Contains(buildReason, "q_test.go:3:28: undefined: missing") {
		t.Errorf("build-fail package reason %q, want its build error", buildReason)
	}
}

// TestFailReasonsInStats: a step and its task that exit non-zero keep
// the last lines the step wrote to stderr; a passing one has no reason.
func TestFailReasonsInStats(t *testing.T) {
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliett",
		"kilo", "lima", "mike", "november", "oscar", "papa", "quebec", "romeo", "sierra", "tango"}
	var script strings.Builder
	for _, w := range words {
		script.WriteString("echo " + w + " >&2; ")
	}
	script.WriteString("exit 3")
	f, statsFile := statsRepo(t, "tasks:\n  red:\n    run:\n      - {sh: '"+script.String()+"'}\n  green:\n    run: ["+helperStep("ok")+"]\n")
	if out, code := f.graft("", "red"); code == 0 {
		t.Fatalf("red passed:\n%s", out)
	}
	f.mustGraft("green")
	var stepReason, taskReason string
	for _, l := range readStatsRaw(t, statsFile) {
		switch {
		case l.Task == "red" && l.Kind == "step":
			stepReason = l.Reason
		case l.Task == "red" && l.Kind == "task":
			taskReason = l.Reason
		case l.Task == "green" && l.Reason != "":
			t.Errorf("a passing %s line has a reason: %q", l.Kind, l.Reason)
		}
	}
	for name, got := range map[string]string{"step": stepReason, "task": taskReason} {
		if !strings.Contains(got, "tango") || !strings.Contains(got, "sierra") {
			t.Errorf("%s reason %q, want the last stderr lines", name, got)
		}
		if strings.Contains(got, "alpha") {
			t.Errorf("%s reason %q keeps the head of stderr; want its tail", name, got)
		}
	}
}

type reasonTestsLine struct {
	Package string  `json:"package"`
	Reason  string  `json:"reason"`
	Tests   [][]any `json:"tests"`
}

func readTestsRaw(t *testing.T, path string) []reasonTestsLine {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []reasonTestsLine
	for s := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var l reasonTestsLine
		if err := json.Unmarshal([]byte(s), &l); err != nil {
			t.Fatalf("line %q: %v", s, err)
		}
		lines = append(lines, l)
	}
	return lines
}

type reasonStatsLine struct {
	Kind   string `json:"kind"`
	Task   string `json:"task"`
	Reason string `json:"reason"`
}

func readStatsRaw(t *testing.T, path string) []reasonStatsLine {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []reasonStatsLine
	for s := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var l reasonStatsLine
		if err := json.Unmarshal([]byte(s), &l); err != nil {
			t.Fatalf("line %q: %v", s, err)
		}
		lines = append(lines, l)
	}
	return lines
}

// TestFailReasonsDoNotWaitForABackgroundChild: a step that leaves a
// process holding its stderr does not keep graft from the next step once it
// exits.
func TestFailReasonsDoNotWaitForABackgroundChild(t *testing.T) {
	f, statsFile := statsRepo(t, "tasks:\n  red:\n    run:\n      - {sh: 'sleep 30 >&2 & echo $! > bg.pid'}\n      - {sh: 'echo boom >&2; exit 3'}\n")
	cmd, out := f.start(f.work, "red")
	defer func() {
		if pid, err := os.ReadFile(filepath.Join(f.work, "bg.pid")); err == nil {
			_ = exec.Command("kill", strings.TrimSpace(string(pid))).Run()
		}
		_ = cmd.Wait()
	}()
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		data, _ := os.ReadFile(statsFile)
		if strings.Contains(string(data), "boom") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the second step's reason is not in the journal after 15s: graft waits for the background child\n%s", out.String())
		}
	}
}

// TestFailReasonsOfAGoTestStepAreParsed: a failed step of a tests: go task has
// the reason graft read from the go test -json stream, not the stream.
func TestFailReasonsOfAGoTestStepAreParsed(t *testing.T) {
	stream := filepath.Join(t.TempDir(), "stream.json")
	writeFile(t, stream, failingRun)
	f, statsFile := statsRepo(t, "tasks:\n  test:\n    tests: go\n    run: ["+helperStep("cat", stream, "1")+"]\n")
	if out, code := f.graft("", "test"); code == 0 {
		t.Fatalf("the failing run passed:\n%s", out)
	}
	got := map[string]string{}
	for _, l := range readStatsRaw(t, statsFile) {
		if l.Task == "test" {
			got[l.Kind] = l.Reason
		}
	}
	for _, kind := range []string{"step", "task"} {
		reason := got[kind]
		for _, want := range []string{"q_test.go:3:28: undefined: missing", "TestFlaky", "p_test.go:9: boom", "TestTable/bad", "p_test.go:14: sub boom"} {
			if !strings.Contains(reason, want) {
				t.Errorf("%s reason %q, want it to hold %q", kind, reason, want)
			}
		}
		if strings.Contains(reason, `"Action"`) {
			t.Errorf("%s reason is the raw go test -json stream: %q", kind, reason)
		}
	}
}
