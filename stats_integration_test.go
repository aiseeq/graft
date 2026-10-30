package main

import (
	"bufio"
	"encoding/json"
	"errors"
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
	Time     time.Time          `json:"time"`
	Kind     string             `json:"kind"`
	Project  string             `json:"project"`
	Task     string             `json:"task"`
	Top      string             `json:"top"`
	In       string             `json:"in"`
	Step     string             `json:"step"`
	Steps    int                `json:"steps"`
	WallS    float64            `json:"wall_s"`
	UserS    float64            `json:"user_s"`
	SysS     float64            `json:"sys_s"`
	MaxRSSMB *float64           `json:"max_rss_mb"`
	TreeMB   *float64           `json:"tree_mem_mb"`
	LoadMin  *float64           `json:"load_start"`
	LoadMax  *float64           `json:"load_max"`
	DBCPUS   *float64           `json:"db_cpu_s"`
	DBMemMB  *float64           `json:"db_mem_mb"`
	WaitS    float64            `json:"wait_s"`
	WaitWhy  map[string]float64 `json:"wait_why"`
	Host     string             `json:"host"`
	Here     string             `json:"here"`
	Exit     int                `json:"exit"`
	Run      string             `json:"run"`
	Parent   string             `json:"parent"`
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

// TestStepMemoryOfTheProcessTree: processes running side by side add up.
// max_rss_mb is the peak of one of them; tree_mem_mb, the peak of their sum,
// shows four 64 MB processes at about 256 MB.
func TestStepMemoryOfTheProcessTree(t *testing.T) {
	skipWaitInShort(t)
	if runtime.GOOS != "linux" {
		t.Skip("the process tree is read from /proc")
	}
	hold := helperBin + " hold 64 1500"
	f, statsFile := statsRepo(t, "tasks:\n  four:\n    run:\n      - sh: '"+hold+" & "+hold+" & "+hold+" & "+hold+" & wait'\n      - "+helperStep("ok")+"\n")
	f.mustGraft("four")
	lines := readStats(t, statsFile)
	if len(lines) != 3 {
		t.Fatalf("want 2 steps and the task: %+v", lines)
	}
	step, ok, task := lines[0], lines[1], lines[2]
	t.Logf("tree_mem_mb %v, max_rss_mb %v; small step %v", *step.TreeMB, *step.MaxRSSMB, *ok.TreeMB)
	if step.TreeMB == nil || *step.TreeMB < 240 || *step.TreeMB > 400 {
		t.Errorf("four processes of 64 MB: tree_mem_mb %v", step.TreeMB)
	}
	if step.MaxRSSMB == nil || *step.MaxRSSMB > 128 {
		t.Errorf("max_rss_mb is one process's peak: %v", step.MaxRSSMB)
	}
	if ok.TreeMB == nil || *ok.TreeMB > 64 {
		t.Errorf("a small step: tree_mem_mb %v", ok.TreeMB)
	}
	if task.TreeMB == nil || *task.TreeMB != *step.TreeMB {
		t.Errorf("the task's peak is its largest step's: %v, step %v", task.TreeMB, step.TreeMB)
	}
}

// TestStatsRecordTheMachineLoad: every line carries the 1-minute load
// average when its step or task started and the highest seen while it ran,
// so runs on a loaded machine can be told apart; graft stats --max-load
// leaves them out.
func TestStatsRecordTheMachineLoad(t *testing.T) {
	skipWaitInShort(t)
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	loadavg := filepath.Join(t.TempDir(), "loadavg")
	writeFile(t, loadavg, "3.50 2.00 1.00 1/400 1234\n")
	f, statsFile := statsRepo(t, "tasks:\n  noisy:\n    run:\n      - "+helperStep("ok")+
		"\n      - sh: 'sleep 0.5; echo 30.25 20.00 10.00 1/400 1234 > "+loadavg+"; sleep 0.5'\n")
	f.env = append(f.env, "GRAFT_LOADAVG="+loadavg)
	f.mustGraft("noisy")
	lines := readStats(t, statsFile)
	if len(lines) != 3 {
		t.Fatalf("want 2 steps and the task: %+v", lines)
	}
	got := func(v *float64) string {
		if v == nil {
			return "-"
		}
		return fmt.Sprint(*v)
	}
	var loads []string
	for _, l := range lines {
		loads = append(loads, l.Kind+" "+got(l.LoadMin)+" "+got(l.LoadMax))
	}
	if want := "step 3.5 3.5,step 3.5 30.25,task 3.5 30.25"; strings.Join(loads, ",") != want {
		t.Errorf("loads %s, want %s", strings.Join(loads, ","), want)
	}

	writeFile(t, loadavg, "4.00 2.00 1.00 1/400 1234\n")
	f.write(".graft.yaml", f.read(".graft.yaml")+"  quiet:\n    run: ["+helperStep("ok")+"]\n")
	f.mustGraft("quiet")
	out := f.mustGraft("stats", "--max-load", "16")
	if !strings.Contains(out, "quiet ") || strings.Contains(out, "noisy ") {
		t.Errorf("--max-load 16 keeps the noisy run:\n%s", out)
	}
	if out := f.mustGraft("stats"); !strings.Contains(out, "noisy ") {
		t.Errorf("without --max-load the noisy run is gone:\n%s", out)
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
	writeFile(t, statsFile+".2", "the copy of 10 days ago\n")
	writeFile(t, statsFile+".3", "the copy of 16 days ago\n")
	for n, age := range map[string]time.Duration{".2": 10 * 24 * time.Hour, ".3": 16 * 24 * time.Hour} {
		at := time.Now().Add(-age)
		if err := os.Chtimes(statsFile+n, at, at); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, statsFile, "")
	const full = 20<<20 + 1
	if err := os.Truncate(statsFile, full); err != nil {
		t.Fatal(err)
	}
	f.mustGraft("one")
	if st, err := os.Stat(statsFile + ".1"); err != nil || st.Size() != full {
		t.Fatalf("the full file is not the old copy: %v %v", st, err)
	}
	// The older copies move one number up; the one past the 15 days of
	// Retention goes.
	for n, want := range map[string]string{".2": "the copy before\n", ".3": "the copy of 10 days ago\n"} {
		if data, err := os.ReadFile(statsFile + n); err != nil || string(data) != want {
			t.Errorf("%s: %q %v, want %q", n, data, err, want)
		}
	}
	if _, err := os.Stat(statsFile + ".4"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the copy of 16 days ago is kept: %v", err)
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
	// Nor does the lint cache, which lives there too.
	if n := strings.Count(out, "no golangci-lint cache of this work tree"); n != 1 {
		t.Errorf("want one lint cache warning, got %d:\n%s", n, out)
	}
}

func TestStatsCommand(t *testing.T) {
	f, statsFile := statsRepo(t, "tasks:\n  one:\n    run: ["+helperStep("ok")+"]\n")
	root := realRoot(t, f)
	other := filepath.Join(f.base, "other")
	now := time.Now()
	var b strings.Builder
	line := func(age time.Duration, project, kind, task, step string, wall, cpu, rss float64, exit int, tree ...float64) {
		l := map[string]any{"time": now.Add(-age), "kind": kind, "project": project, "task": task, "top": task,
			"wall_s": wall, "user_s": cpu * 0.75, "sys_s": cpu * 0.25, "max_rss_mb": rss, "exit": exit}
		if step != "" {
			l["step"] = step
		}
		if len(tree) > 0 {
			l["tree_mem_mb"] = tree[0]
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
	line(4*time.Hour, root, "task", "test", "", 60, 30, 500, 0, 1800)
	line(5*time.Hour, root, "task", "test", "", 120, 50, 700, 0, 2600)
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
		"TASK RUNS WALL MED WALL MAX CPU MED TREE MEM MEM MAX FAILED SAVED",
		"test 2 1m30s 2m0s 40s 2.5 GB 700 MB 0% -",
		"lint 3 20s 30s 7s - 8.8 GB 33% -",
	}
	if got := rows(out); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("graft stats:\n%s", out)
	}
	if !strings.Contains(out, "1 malformed line") {
		t.Errorf("the torn line is not reported:\n%s", out)
	}
	month := f.mustGraft("stats", "--since", "30d")
	if got := rows(month); len(got) != 3 || got[2] != "lint 4 15s 30s 6s - 19.5 GB 25% -" {
		t.Errorf("30 days: %q", got)
	}
	// The log starts 10 days ago: the header says so, not 30 days.
	if first := now.Add(-10 * 24 * time.Hour).Format("2006-01-02 15:04"); !strings.Contains(month, "since "+first+" (the log starts there, after the ") {
		t.Errorf("the header does not name the first line, %s:\n%s", first, month)
	}
	if strings.Contains(out, "the log starts there") {
		t.Errorf("a 7-day report of a 10-day log names its first line:\n%s", out)
	}
	if got := rows(f.mustGraft("stats", "--since", "90m", "--task", "lint")); strings.Join(got, "\n") != strings.Join([]string{
		"STEP RUNS WALL MED WALL MAX CPU MED TREE MEM MEM MAX FAILED SAVED",
		"(task) 1 10s 10s 5s - 8.8 GB 0% -",
		"shellcheck *.sh 1 9s 9s 4s - 8.8 GB 0% -",
		"go vet ./... 1 1s 1s 1s - 300 MB 0% -",
	}, "\n") {
		t.Errorf("--task lint: %q", got)
	}
	if got := rows(f.mustGraft("stats", "--project", other)); len(got) != 2 || got[1] != "build 1 3s 3s 2s - 1000 MB 0% -" {
		t.Errorf("--project: %q", got)
	}
	got := rows(f.mustGraft("stats", "--all"))
	if len(got) != 4 || !strings.HasPrefix(got[0], "PROJECT TASK") || got[1] != root+" test 2 1m30s 2m0s 40s 2.5 GB 700 MB 0% -" || got[2] != root+" lint 3 20s 30s 7s - 8.8 GB 33% -" {
		t.Errorf("--all: %q", got)
	}
	if out, code := f.graft("", "stats", "--all", "--project", other); code != 2 {
		t.Errorf("--all with --project: exit %d\n%s", code, out)
	}
	if out, code := f.graft("", "stats", "--since", "soon"); code != 2 {
		t.Errorf("bad --since: exit %d\n%s", code, out)
	}
}

// TestStatsNestedGraft: a graft a task step starts records the run of the
// outer graft as its parent, and the breakdown of the outer task shows the
// inner graft's outermost task as a row of its own.
func TestStatsNestedGraft(t *testing.T) {
	f, statsFile := statsRepo(t, "tasks:\n  outer:\n    run: [[graft, inner]]\n  inner:\n    deps: [leaf]\n    run: ["+helperStep("ok")+"]\n  leaf:\n    run: ["+helperStep("ok")+"]\n")
	f.mustGraft("outer")
	runs := map[string]statsLine{}
	for _, l := range readStats(t, statsFile) {
		if l.Kind == "task" {
			runs[l.Task] = l
		}
	}
	outer, inner, leaf := runs["outer"], runs["inner"], runs["leaf"]
	if outer.Run == "" || outer.Parent != "" || inner.Parent != outer.Run || inner.Run == outer.Run || leaf.Run != inner.Run {
		t.Errorf("runs: outer %q (parent %q), inner %q (parent %q), leaf %q", outer.Run, outer.Parent, inner.Run, inner.Parent, leaf.Run)
	}
	if leaf.In != "inner" || inner.In != "" {
		t.Errorf("leaf ran in %q, inner in %q", leaf.In, inner.In)
	}
	out := f.mustGraft("stats", "--task", "outer")
	if !strings.Contains(out, "graft inner ") || strings.Contains(out, "graft leaf") || strings.Contains(out, "task leaf") {
		t.Errorf("the breakdown names the inner graft's outermost task alone:\n%s", out)
	}
	if out := f.mustGraft("stats", "--task", "inner"); !strings.Contains(out, "task leaf ") {
		t.Errorf("the breakdown of inner lacks its dep:\n%s", out)
	}
}

// TestStatsRerunsAreFlaky: a task that a script around graft reran after it
// failed, and that then passed, is a flaky task; two runs by hand are not a
// rerun, nor is a task that failed every time.
func TestStatsRerunsAreFlaky(t *testing.T) {
	f, _ := statsRepo(t, "tasks:\n"+
		"  retrying:\n    run:\n      - "+helperStep("touch", "m")+"\n      - {sh: 'graft flaky || { rm m; graft flaky; }'}\n"+
		"  hopeless:\n    run: [{sh: 'graft broken || graft broken'}]\n"+
		"  flaky:\n    run: ["+helperStep("failif", "m")+"]\n"+
		"  broken:\n    run: ["+helperStep("fail")+"]\n"+
		"  manual:\n    run: ["+helperStep("failif", "m")+"]\n")
	f.mustGraft("retrying")
	if _, code := f.graft("", "hopeless"); code == 0 {
		t.Fatal("hopeless passed")
	}
	f.write("m", "")
	if _, code := f.graft("", "manual"); code == 0 {
		t.Fatal("manual passed with m")
	}
	if err := os.Remove(f.path("m")); err != nil {
		t.Fatal(err)
	}
	f.mustGraft("manual")
	out := strings.Join(strings.Fields(f.mustGraft("stats", "--tests")), " ")
	if !strings.Contains(out, "flaky tasks: failed, then passed when the graft that ran them ran them again TASK RUNS FAILED flaky 1 1") {
		t.Errorf("the rerun is not a flaky task:\n%s", out)
	}
	for _, task := range []string{"broken", "manual"} {
		if strings.Contains(out, " "+task+" 1") {
			t.Errorf("%s is not a rerun to a pass:\n%s", task, out)
		}
	}
}

// TestStatsMarkTasksOverTheLimits: graft stats marks the gate over
// limits.gate and any other task over limits.task, by their median run
// without the waits, and a run over its limit says so as it ends; a task
// under its limit and a broken down task carry no mark.
func TestStatsMarkTasksOverTheLimits(t *testing.T) {
	skipWaitInShort(t)
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	f, _ := statsRepo(t, "gate:\n  - sh: sleep 0.3\ntasks:\n  slow:\n    run: [[sh, -c, 'sleep 0.5']]\n  quick:\n    run: ["+helperStep("ok")+"]\n")
	cfgDir := t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "limits: {gate: 200ms, task: 400ms}\n")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir)
	// A run over its limit says so as it ends.
	if out := f.mustGraft("gate"); !strings.Contains(out, "graft: over the limit: gate took ") || !strings.Contains(out, " without waits, above 200ms (user config limits.gate)\n") {
		t.Errorf("gate run:\n%s", out)
	}
	if out := f.mustGraft("slow"); !strings.Contains(out, "graft: over the limit: slow took ") || !strings.Contains(out, " without waits, above 400ms (user config limits.task)\n") {
		t.Errorf("slow run:\n%s", out)
	}
	if out := f.mustGraft("quick"); strings.Contains(out, "over the limit") {
		t.Errorf("quick run:\n%s", out)
	}
	out := f.mustGraft("stats")
	var marks []string
	for l := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(l, "graft: over the limit: ") {
			marks = append(marks, l)
		}
	}
	if len(marks) != 2 || !strings.Contains(marks[0], "gate takes ") || !strings.Contains(marks[0], "above 200ms (user config limits.gate)") ||
		!strings.Contains(marks[1], "slow takes ") || !strings.Contains(marks[1], "above 400ms (user config limits.task)") {
		t.Errorf("marks %q:\n%s", marks, out)
	}
	if out := f.mustGraft("stats", "--task", "slow"); strings.Contains(out, "over the limit") {
		t.Errorf("a broken down task is marked:\n%s", out)
	}
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "limits: {gate: -1s}\n")
	if out, code := f.graft("", "stats"); code == 0 || !strings.Contains(out, "limits: gate and task must be positive") {
		t.Errorf("a negative limit: exit %d\n%s", code, out)
	}
}

// TestRunOverLimitOnce: only the outermost task of a run is held to its
// limit, its deps counting in it, and a failed run is held too.
func TestRunOverLimitOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	f, _ := statsRepo(t, "tasks:\n  outer:\n    deps: [slow]\n    run: ["+helperStep("ok")+"]\n  slow:\n    run: [[sh, -c, 'sleep 0.3']]\n  broken:\n    run: [[sh, -c, 'sleep 0.3; exit 3']]\n")
	cfgDir := t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "limits: {task: 200ms}\n")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir)
	out := f.mustGraft("outer")
	if strings.Count(out, "over the limit") != 1 || !strings.Contains(out, "graft: over the limit: outer took ") {
		t.Errorf("outer:\n%s", out)
	}
	if out, code := f.graft("", "broken"); code == 0 || !strings.Contains(out, "graft: over the limit: broken took ") {
		t.Errorf("broken: exit %d\n%s", code, out)
	}
}
