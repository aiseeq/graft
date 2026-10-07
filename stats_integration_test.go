package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// statsLine is one line of stats.jsonl as the tests read it.
type statsLine struct {
	Time     time.Time `json:"time"`
	Kind     string    `json:"kind"`
	Project  string    `json:"project"`
	Task     string    `json:"task"`
	Top      string    `json:"top"`
	Step     string    `json:"step"`
	Steps    int       `json:"steps"`
	WallS    float64   `json:"wall_s"`
	UserS    float64   `json:"user_s"`
	SysS     float64   `json:"sys_s"`
	MaxRSSMB *float64  `json:"max_rss_mb"`
	Exit     int       `json:"exit"`
}

// statsRepo is a task repository whose graft keeps its stats in a cache of
// its own; it returns the stats file.
func statsRepo(t *testing.T, config string) (*fixture, string) {
	f := tasksRepo(t, config, nil)
	cache := filepath.Join(f.base, "cache")
	f.env = append(f.env, "XDG_CACHE_HOME="+cache, "LOCALAPPDATA="+cache)
	return f, filepath.Join(cache, "graft", "stats.jsonl")
}

func readStats(t *testing.T, path string) []statsLine {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var lines []statsLine
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		var l statsLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		lines = append(lines, l)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// realRoot is the project root as git reports it: symlinks resolved.
func realRoot(t *testing.T, f *fixture) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(f.work)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.002 }

func TestTasksRecordResourceUse(t *testing.T) {
	f, statsFile := statsRepo(t, `gate:
  - task: pair
tasks:
  pair:
    run:
      - `+helperStep("alloc", "64")+`
      - `+helperStep("ok")+`
  bad:
    run: [`+helperStep("fail")+`]
`)
	before := time.Now().Add(-time.Second)
	f.mustGraft("pair")
	lines := readStats(t, statsFile)
	if len(lines) != 3 {
		t.Fatalf("want 2 step lines and the task line, got %d: %+v", len(lines), lines)
	}
	root := realRoot(t, f)
	steps, total := lines[:2], lines[2]
	for i, l := range lines {
		if l.Project != root || l.Task != "pair" || l.Top != "pair" || l.Exit != 0 {
			t.Errorf("line %d: %+v", i, l)
		}
		if l.Time.Before(before) || l.Time.After(time.Now()) {
			t.Errorf("line %d: time %s", i, l.Time)
		}
		if l.WallS <= 0 || l.UserS < 0 || l.SysS < 0 {
			t.Errorf("line %d: times %+v", i, l)
		}
		if runtime.GOOS == "linux" && (l.MaxRSSMB == nil || *l.MaxRSSMB <= 0) {
			t.Errorf("line %d: no peak memory", i)
		}
	}
	if steps[0].Kind != "step" || !strings.Contains(steps[0].Step, "alloc 64") || steps[1].Kind != "step" || !strings.HasSuffix(steps[1].Step, " ok") {
		t.Errorf("step lines: %+v", steps)
	}
	if runtime.GOOS == "linux" && *steps[0].MaxRSSMB < 64 {
		t.Errorf("alloc 64 peaked at %.1f MB", *steps[0].MaxRSSMB)
	}
	if total.Kind != "task" || total.Step != "" || total.Steps != 2 {
		t.Errorf("task line: %+v", total)
	}
	if total.WallS < steps[0].WallS+steps[1].WallS-0.002 ||
		!near(total.UserS, steps[0].UserS+steps[1].UserS) || !near(total.SysS, steps[0].SysS+steps[1].SysS) {
		t.Errorf("task line does not sum its steps: %+v\nsteps: %+v", total, steps)
	}
	if runtime.GOOS == "linux" && *total.MaxRSSMB != max(*steps[0].MaxRSSMB, *steps[1].MaxRSSMB) {
		t.Errorf("task peak %.1f, steps %.1f and %.1f", *total.MaxRSSMB, *steps[0].MaxRSSMB, *steps[1].MaxRSSMB)
	}

	// The gate names the command it ran under; a failing step records its
	// exit status, and so does its task.
	f.mustGraft("gate")
	if _, code := f.graft("", "bad"); code != 1 {
		t.Fatalf("bad: exit %d", code)
	}
	lines = readStats(t, statsFile)[3:]
	var got []string
	for _, l := range lines {
		got = append(got, fmt.Sprintf("%s %s/%s exit %d", l.Kind, l.Top, l.Task, l.Exit))
	}
	want := []string{
		"step gate/pair exit 0", "step gate/pair exit 0", "task gate/pair exit 0", "task gate/gate exit 0",
		"step bad/bad exit 1", "task bad/bad exit 1",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if lines[3].Steps != 2 {
		t.Errorf("the gate counts the steps of its tasks: %+v", lines[3])
	}
}

func TestStatsFileRotates(t *testing.T) {
	f, statsFile := statsRepo(t, `tasks:
  one:
    run: [`+helperStep("ok")+`]
`)
	if err := os.MkdirAll(filepath.Dir(statsFile), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, statsFile+".1", "the copy before\n")
	writeFile(t, statsFile, "")
	const full = 20<<20 + 1
	if err := os.Truncate(statsFile, full); err != nil {
		t.Fatal(err)
	}
	f.mustGraft("one")
	if st, err := os.Stat(statsFile + ".1"); err != nil || st.Size() != full {
		t.Fatalf("the full file is not the old copy: %v %v", st, err)
	}
	if lines := readStats(t, statsFile); len(lines) != 2 {
		t.Errorf("new file: %+v", lines)
	}
	// Below the limit nothing moves.
	f.mustGraft("one")
	if lines := readStats(t, statsFile); len(lines) != 4 {
		t.Errorf("after a second run: %d lines", len(lines))
	}
}

func TestStatsFailureDoesNotFailTheTask(t *testing.T) {
	f, statsFile := statsRepo(t, `tasks:
  one:
    run: [`+helperStep("ok")+`, `+helperStep("ok")+`]
`)
	// A directory where the cache directory should be: nothing can be written.
	writeFile(t, filepath.Dir(filepath.Dir(statsFile)), "not a directory")
	out := f.mustGraft("one")
	if n := strings.Count(out, "resource stats not recorded"); n != 1 {
		t.Errorf("want one warning, got %d:\n%s", n, out)
	}
}

func TestStatsCommand(t *testing.T) {
	f, statsFile := statsRepo(t, "tasks:\n  one:\n    run: ["+helperStep("ok")+"]\n")
	root := realRoot(t, f)
	other := filepath.Join(f.base, "other")
	now := time.Now()
	var b strings.Builder
	line := func(age time.Duration, project, kind, task, step string, wall, cpu, rss float64, exit int) {
		l := map[string]any{"time": now.Add(-age), "kind": kind, "project": project, "task": task, "top": task,
			"wall_s": wall, "user_s": cpu * 0.75, "sys_s": cpu * 0.25, "max_rss_mb": rss, "exit": exit}
		if step != "" {
			l["step"] = step
		}
		data, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	line(time.Hour, root, "step", "lint", "shellcheck *.sh", 9, 4, 9000, 0)
	line(time.Hour, root, "step", "lint", "go vet ./...", 1, 1, 300, 0)
	line(time.Hour, root, "task", "lint", "", 10, 5, 9000, 0)
	line(2*time.Hour, root, "task", "lint", "", 20, 7, 100, 1)
	line(3*time.Hour, root, "task", "lint", "", 30, 9, 200, 0)
	line(4*time.Hour, root, "task", "test", "", 60, 30, 500, 0)
	line(5*time.Hour, root, "task", "test", "", 120, 50, 700, 0)
	line(10*24*time.Hour, root, "task", "lint", "", 5, 1, 20000, 0)
	line(time.Hour, other, "task", "build", "", 3, 2, 1000, 0)
	b.WriteString("{torn line\n")
	if err := os.MkdirAll(filepath.Dir(statsFile), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, statsFile, b.String())

	rows := func(out string) []string {
		var rows []string
		for l := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
			if !strings.HasPrefix(l, "graft:") {
				rows = append(rows, strings.Join(strings.Fields(l), " "))
			}
		}
		return rows
	}
	out := f.mustGraft("stats")
	want := []string{
		"TASK RUNS WALL MED WALL MAX CPU MED MEM MAX FAILED",
		"lint 3 20s 30s 7s 8.8 GB 33%",
		"test 2 1m30s 2m0s 40s 700 MB 0%",
	}
	if got := rows(out); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("graft stats:\n%s", out)
	}
	if !strings.Contains(out, "1 malformed line") {
		t.Errorf("the torn line is not reported:\n%s", out)
	}
	if got := rows(f.mustGraft("stats", "--since", "30d")); len(got) != 3 || got[1] != "lint 4 15s 30s 6s 19.5 GB 25%" {
		t.Errorf("30 days: %q", got)
	}
	if got := rows(f.mustGraft("stats", "--since", "90m", "--task", "lint")); strings.Join(got, "\n") != strings.Join([]string{
		"STEP RUNS WALL MED WALL MAX CPU MED MEM MAX FAILED",
		"(task) 1 10s 10s 5s 8.8 GB 0%",
		"shellcheck *.sh 1 9s 9s 4s 8.8 GB 0%",
		"go vet ./... 1 1s 1s 1s 300 MB 0%",
	}, "\n") {
		t.Errorf("--task lint: %q", got)
	}
	if got := rows(f.mustGraft("stats", "--project", other)); len(got) != 2 || got[1] != "build 1 3s 3s 2s 1000 MB 0%" {
		t.Errorf("--project: %q", got)
	}
	got := rows(f.mustGraft("stats", "--all"))
	if len(got) != 4 || !strings.HasPrefix(got[0], "PROJECT TASK") || got[1] != root+" lint 3 20s 30s 7s 8.8 GB 33%" || got[3] != root+" test 2 1m30s 2m0s 40s 700 MB 0%" {
		t.Errorf("--all: %q", got)
	}
	if out, code := f.graft("", "stats", "--all", "--project", other); code != 2 {
		t.Errorf("--all with --project: exit %d\n%s", code, out)
	}
	if out, code := f.graft("", "stats", "--since", "soon"); code != 2 {
		t.Errorf("bad --since: exit %d\n%s", code, out)
	}
}
