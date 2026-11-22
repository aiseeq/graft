package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/stats"
)

// statsLine is one line of stats.jsonl as the tests read it.
type statsLine struct {
	Time         time.Time          `json:"time"`
	Kind         string             `json:"kind"`
	Project      string             `json:"project"`
	Repo         string             `json:"repo"`
	Task         string             `json:"task"`
	Profile      string             `json:"profile"`
	CacheChanged []string           `json:"cache_changed"`
	WaitedBehind string             `json:"waited_behind"`
	ReserveCores float64            `json:"reserve_cores"`
	Top          string             `json:"top"`
	In           string             `json:"in"`
	Step         string             `json:"step"`
	Steps        int                `json:"steps"`
	WallS        float64            `json:"wall_s"`
	UserS        float64            `json:"user_s"`
	SysS         float64            `json:"sys_s"`
	MaxRSSMB     *float64           `json:"max_rss_mb"`
	TreeMB       *float64           `json:"tree_mem_mb"`
	CgroupPeakMB *float64           `json:"cgroup_peak_mb"`
	LoadMin      *float64           `json:"load_start"`
	LoadMax      *float64           `json:"load_max"`
	DBCPUS       *float64           `json:"db_cpu_s"`
	DBMemMB      *float64           `json:"db_mem_mb"`
	WaitS        float64            `json:"wait_s"`
	WaitWhy      map[string]float64 `json:"wait_why"`
	Host         string             `json:"host"`
	Here         string             `json:"here"`
	Mark         string             `json:"mark"`
	Heavy        bool               `json:"heavy"`
	Exit         int                `json:"exit"`
	Run          string             `json:"run"`
	Parent       string             `json:"parent"`
}

// statsRepo is a task repository whose graft keeps its stats in a cache of
// its own; it returns the stats file.
func statsRepo(t *testing.T, config string) (*fixture, string) {
	f := tasksRepo(t, config, nil)
	cache := filepath.Join(f.base, "cache")
	f.env = append(f.env, "XDG_CACHE_HOME="+cache, "LOCALAPPDATA="+cache)
	return f, filepath.Join(cache, "graft", "stats.jsonl")
}

func TestStatsByRepoRejectsReportModeFlags(t *testing.T) {
	f, _ := statsRepo(t, "")
	for _, args := range [][]string{
		{"--all", "--by", "repo", "--task", "build"},
		{"--all", "--by", "repo", "--tests"},
		{"--all", "--by", "repo", "--value", "wall-p50"},
	} {
		_, stderr, code := graftSeparate(f, append([]string{"stats"}, args...)...)
		if code != 2 {
			t.Errorf("graft stats %v exit = %d, want 2; stderr: %s", args, code, stderr)
		}
	}
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
		if l.MaxRSSMB == nil || *l.MaxRSSMB <= 0 {
			t.Errorf("line %d: no peak memory", i)
		}
	}
	if steps[0].Kind != "step" || !strings.Contains(steps[0].Step, "alloc 64") || steps[1].Kind != "step" || !strings.HasSuffix(steps[1].Step, " ok") {
		t.Errorf("step lines: %+v", steps)
	}
	if *steps[0].MaxRSSMB < 64 {
		t.Errorf("alloc 64 peaked at %.1f MB", *steps[0].MaxRSSMB)
	}
	if total.Kind != "task" || total.Step != "" || total.Steps != 2 {
		t.Errorf("task line: %+v", total)
	}
	if total.WallS < steps[0].WallS+steps[1].WallS-0.002 ||
		!near(total.UserS, steps[0].UserS+steps[1].UserS) || !near(total.SysS, steps[0].SysS+steps[1].SysS) {
		t.Errorf("task line does not sum its steps: %+v\nsteps: %+v", total, steps)
	}
	if *total.MaxRSSMB != max(*steps[0].MaxRSSMB, *steps[1].MaxRSSMB) {
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
	hostLine, err := json.Marshal(map[string]any{"time": now.Add(-time.Hour), "kind": "task", "project": root, "task": "remote", "top": "remote", "host": "hostA", "wall_s": 1, "user_s": 1, "exit": 0})
	if err != nil {
		t.Fatal(err)
	}
	b.Write(hostLine)
	b.WriteByte('\n')
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
	if !strings.Contains(out, "CPU, memory and IO for records with a host are from the sending command, not work on the host.") {
		t.Errorf("host resource note missing:\n%s", out)
	}
	if strings.Contains(f.mustGraft("stats", "--project", other), "CPU, memory and IO for records with a host") {
		t.Error("host resource note shown without a host record in the report")
	}
	want := []string{
		"TASK RUNS WALL MED WALL MAX CPU MED TREE MEM MEM MAX FAILED SAVED",
		"test 2 1m30s 2m0s 40s 2.5 GB 700 MB 0% -",
		"lint 3 20s 30s 7s - 8.8 GB 33% -",
		"remote 1 1s 1s 1s - - 0% -",
	}
	if got := rows(out); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("graft stats:\n%s", out)
	}
	if !strings.Contains(out, "1 malformed line") {
		t.Errorf("the torn line is not reported:\n%s", out)
	}
	month := f.mustGraft("stats", "--since", "30d")
	if got := rows(month); len(got) != 4 || got[2] != "lint 4 15s 30s 6s - 19.5 GB 25% -" || got[3] != "remote 1 1s 1s 1s - - 0% -" {
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
	if len(got) != 5 || !strings.HasPrefix(got[0], "PROJECT TASK") || got[1] != root+" test 2 1m30s 2m0s 40s 2.5 GB 700 MB 0% -" || got[2] != root+" lint 3 20s 30s 7s - 8.8 GB 33% -" || got[4] != root+" remote 1 1s 1s 1s - - 0% -" {
		t.Errorf("--all: %q", got)
	}
	if out, code := f.graft("", "stats", "--all", "--project", other); code != 2 {
		t.Errorf("--all with --project: exit %d\n%s", code, out)
	}
	if out, code := f.graft("", "stats", "--since", "soon"); code != 2 {
		t.Errorf("bad --since: exit %d\n%s", code, out)
	}
}

func TestStatsProjectPrefilterCLICompatibility(t *testing.T) {
	f, statsFile := statsRepo(t, "")
	root := realRoot(t, f)
	other := filepath.Join(f.base, "other")
	now := time.Now().Truncate(time.Second)
	old := now.Add(-2 * time.Hour)
	appendRecord := func(dst *strings.Builder, record stats.Record, spacedProject bool) {
		t.Helper()
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if spacedProject {
			line = []byte(strings.Replace(string(line), `"project":`, `"project": `, 1))
		}
		dst.Write(line)
		dst.WriteByte('\n')
	}
	var journal strings.Builder
	appendRecord(&journal, stats.Record{Time: old, Kind: stats.KindTask, Project: other, Task: "foreign", Top: "foreign", WallS: 1}, false)
	appendRecord(&journal, stats.Record{Time: old.Add(time.Hour), Kind: stats.KindTask, Project: root, Task: "build", Top: "build", WallS: 2}, true)
	appendRecord(&journal, stats.Record{Time: old.Add(time.Hour + time.Minute), Kind: stats.KindStep, Project: root, Task: "child", Top: "child", Step: "compile", Run: "shared-run", Exit: 1}, false)
	appendRecord(&journal, stats.Record{Time: old.Add(time.Minute), Kind: stats.KindTask, Project: other, Task: "child", Top: "child", In: "outer", Run: "shared-run", WallS: 1}, false)
	journal.WriteString("{torn line\n")
	writeFile(t, statsFile, journal.String())

	testJournal := strings.Builder{}
	for i, run := range []stats.PackageRun{
		{Time: old, Project: other, Task: "foreign", Top: "foreign", Runner: "go", Package: "example/foreign", Result: stats.ResultPass, WallS: 1},
		{Time: old.Add(time.Hour), Project: root, Task: "build", Top: "build", Runner: "go", Package: "example/root", Result: stats.ResultPass, WallS: 2},
	} {
		line, err := json.Marshal(run)
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			line = []byte(strings.Replace(string(line), `"project":`, `"project": `, 1))
		}
		testJournal.Write(line)
		testJournal.WriteByte('\n')
	}
	testJournal.WriteString("{torn line\n")
	writeFile(t, stats.TestsPath(statsFile), testJournal.String())

	headerTime := func(output, marker string) string {
		t.Helper()
		for line := range strings.SplitSeq(output, "\n") {
			if strings.Contains(line, marker) {
				_, value, ok := strings.Cut(line, " since ")
				if ok {
					return value
				}
			}
		}
		t.Fatalf("no %q report header in:\n%s", marker, output)
		return ""
	}
	rootTasks := f.mustGraft("stats", "--since", "7d")
	allTasks := f.mustGraft("stats", "--all", "--since", "7d")
	if got, want := headerTime(rootTasks, "tasks in "), headerTime(allTasks, "tasks in "); got != want {
		t.Errorf("project task header start = %q, all-project start = %q", got, want)
	}
	if !strings.Contains(rootTasks, "1 malformed line(s) left out") {
		t.Errorf("malformed line without a project needle was not counted:\n%s", rootTasks)
	}
	if !strings.Contains(rootTasks, "build") || !strings.Contains(f.mustGraft("stats", "--task", "build", "--since", "7d"), "build") {
		t.Errorf("root record with whitespace after project colon was lost:\n%s", rootTasks)
	}

	rootTests := f.mustGraft("stats", "--tests", "--since", "7d")
	allTests := f.mustGraft("stats", "--tests", "--all", "--since", "7d")
	if got, want := headerTime(rootTests, "tests in "), headerTime(allTests, "tests in "); got != want {
		t.Errorf("project test header start = %q, all-project start = %q", got, want)
	}
	if strings.Count(rootTests, "malformed line(s) left out") != 2 {
		t.Errorf("tests report did not count malformed lines in both journals:\n%s", rootTests)
	}
	if !strings.Contains(rootTests, "example/root") {
		t.Errorf("test record with whitespace after project colon was lost:\n%s", rootTests)
	}

	failures := f.mustGraft("stats", "--failures", "--task", "child", "--in", "outer", "--since", "7d")
	if !strings.Contains(failures, "child") || !strings.Contains(failures, "compile") {
		t.Errorf("--failures --in lost a root failure matched by another project's taskInputs:\n%s", failures)
	}
}

func TestStatsAllByRepoCombinesCopiesFromRecords(t *testing.T) {
	f, statsFile := statsRepo(t, "")
	now := time.Now().Truncate(time.Second)
	mem := func(value float64) *float64 { return &value }
	records := []stats.Record{
		{Time: now.Add(-time.Minute), Kind: stats.KindTask, Project: "/src/projectA", Repo: "/src/projectA/.git", Profile: "projectA:build", Task: "build", Top: "build", Run: "main", WallS: 10, UserS: 4, TreeMemMB: mem(100)},
		{Time: now.Add(-time.Minute), Kind: stats.KindTask, Project: "/home/test/.cache/worktrees/projectA/topic", Repo: "/src/projectA/.git", Profile: "projectA:build", Task: "build", Top: "build", Run: "worktree", WallS: 20, UserS: 8, TreeMemMB: mem(300)},
		{Time: now.Add(-time.Minute), Kind: stats.KindTask, Project: "/tmp/graft-merge-123/projectA", Repo: "/src/projectA/.git", Profile: "projectA:build", Task: "build", Top: "build", Run: "merge", WallS: 100, UserS: 40, TreeMemMB: mem(900)},
		{Time: now.Add(-time.Minute), Kind: stats.KindTask, Project: "/src/projectB", Repo: "/src/projectB/.git", Profile: "projectB:test", Task: "test", Top: "test", Run: "other", WallS: 5, UserS: 2, TreeMemMB: mem(50)},
	}
	var journal strings.Builder
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		journal.Write(line)
		journal.WriteByte('\n')
	}
	writeFile(t, statsFile, journal.String())
	outputRows := func(out string) []string {
		var result []string
		for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
			if !strings.HasPrefix(line, "graft:") {
				result = append(result, strings.Join(strings.Fields(line), " "))
			}
		}
		return result
	}

	defaultOutput := f.mustGraft("stats", "--all", "--since", "7d")
	defaultRows := outputRows(defaultOutput)
	if len(defaultRows) != 5 || !strings.HasPrefix(defaultRows[0], "PROJECT TASK") {
		t.Fatalf("default --all output changed; want header plus four path rows, got:\n%s", defaultOutput)
	}
	grouped := f.mustGraft("stats", "--all", "--by", "repo", "--since", "7d")
	groupedRows := outputRows(grouped)
	if len(groupedRows) != 3 || groupedRows[0] != "PROJECT TASK RUNS WALL MED WALL MAX CPU MED TREE MEM MEM MAX FAILED SAVED" {
		t.Fatalf("--by repo should produce two repository rows:\n%s", grouped)
	}
	if !strings.Contains(groupedRows[1], "projectA build 3 20s 1m40s") || !strings.Contains(groupedRows[1], "900 MB") {
		t.Errorf("projectA copies were not summarized from their combined records:\n%s", grouped)
	}
	if !strings.Contains(groupedRows[2], "projectB test 1 5s 5s") {
		t.Errorf("projectB row missing:\n%s", grouped)
	}
	if out, code := f.graft("", "stats", "--by", "repo", "--since", "7d"); code != 2 || !strings.Contains(out, "requires --all") {
		t.Errorf("--by repo without --all: exit %d\n%s", code, out)
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

// TestProjectLimitsOverrideUserConfig checks source and project precedence.
func TestProjectLimitsOverrideUserConfig(t *testing.T) {
	f, _ := statsRepo(t, "limits:\n  gate: 200ms\n  task: 400ms\ngate:\n  - sh: sleep 0.3\ntasks:\n  slow:\n    run: [[sh, -c, 'sleep 0.5']]\n")
	cfgDir := t.TempDir()
	writeUserConfig(t, cfgDir, "limits: {gate: 50ms, task: 50ms}\n")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir)
	if out := f.mustGraft("gate"); !strings.Contains(out, "above 200ms (project .graft.yaml limits.gate)") {
		t.Errorf("gate run:\n%s", out)
	}
	if out := f.mustGraft("slow"); !strings.Contains(out, "above 400ms (project .graft.yaml limits.task)") {
		t.Errorf("task run:\n%s", out)
	}
	// A light exec is held to the project's limits as graft stats is.
	if out := f.mustGraft("exec", "--", "sh", "-c", "sleep 0.5"); !strings.Contains(out, "above 400ms (project .graft.yaml limits.task)") {
		t.Errorf("light exec:\n%s", out)
	}
	out := f.mustGraft("stats")
	if !strings.Contains(out, "above 200ms (project .graft.yaml limits.gate)") || !strings.Contains(out, "above 400ms (project .graft.yaml limits.task)") {
		t.Errorf("stats:\n%s", out)
	}
	// A project whose config no longer parses keeps the report of every
	// project going: its rows fall back to the user's limits, with a warning.
	if err := os.WriteFile(filepath.Join(f.work, ".graft.yaml"), []byte("schema: 1\nlimits: {gate: soon}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := f.graft("", "stats", "--all")
	if code != 0 || !strings.Contains(out, "its limits are the user's") || !strings.Contains(out, "(user config limits.gate)") {
		t.Errorf("stats --all with a broken project config, exit %d:\n%s", code, out)
	}
}

// limits.gate and any other task over limits.task, by their median run
// without the waits, and a run over its limit says so as it ends; a task
// under its limit and a broken down task carry no mark.
func TestStatsMarkTasksOverTheLimits(t *testing.T) {
	skipWaitInShort(t)

	f, _ := statsRepo(t, "gate:\n  - sh: sleep 0.3\ntasks:\n  slow:\n    run: [[sh, -c, 'sleep 0.5']]\n  quick:\n    run: ["+helperStep("ok")+"]\n")
	cfgDir := t.TempDir()
	writeUserConfig(t, cfgDir, "limits: {gate: 200ms, task: 400ms}\n")
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
	writeUserConfig(t, cfgDir, "limits: {gate: -1s}\n")
	if out, code := f.graft("", "stats"); code == 0 || !strings.Contains(out, "limits: gate and task must be positive") {
		t.Errorf("a negative limit: exit %d\n%s", code, out)
	}
}

// TestRunOverLimitOnce: only the outermost task of a run is held to its
// limit, its deps counting in it, and a failed run is held too.
func TestRunOverLimitOnce(t *testing.T) {
	f, _ := statsRepo(t, "tasks:\n  outer:\n    deps: [slow]\n    run: ["+helperStep("ok")+"]\n  slow:\n    run: [[sh, -c, 'sleep 0.3']]\n  broken:\n    run: [[sh, -c, 'sleep 0.3; exit 3']]\n")
	cfgDir := t.TempDir()
	writeUserConfig(t, cfgDir, "limits: {task: 200ms}\n")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir)
	out := f.mustGraft("outer")
	if strings.Count(out, "over the limit") != 1 || !strings.Contains(out, "graft: over the limit: outer took ") {
		t.Errorf("outer:\n%s", out)
	}
	if out, code := f.graft("", "broken"); code == 0 || !strings.Contains(out, "graft: over the limit: broken took ") {
		t.Errorf("broken: exit %d\n%s", code, out)
	}
}

func TestStatsValueCommand(t *testing.T) {
	f, path := statsRepo(t, "tasks: {}\n")
	now := time.Now()
	root := realRoot(t, f)
	recs := []stats.Record{
		{Time: now, Kind: stats.KindTask, Project: root, Profile: "work:gate", Task: "gate", Top: "commit", WallS: 10, WaitS: 2, Exit: 0},
		{Time: now, Kind: stats.KindTask, Project: filepath.Join(f.base, "linked"), Profile: "work:gate", Task: "gate", Top: "gate", WallS: 20, Exit: 0},
		{Time: now, Kind: stats.KindTask, Project: root, Profile: "work:gate", Task: "gate", Top: "gate", WallS: 30, Exit: 0},
		{Time: now, Kind: stats.KindTask, Project: root, Profile: "work:gate", Task: "gate", WallS: 100, Exit: 1},
		{Time: now, Kind: stats.KindTask, Project: root, Profile: "work:dep", Task: "dep", In: "outer", WaitS: 100},
		{Time: now, Kind: stats.KindTask, Project: root, Profile: "work:outer", Task: "outer", WaitS: 1},
		{Time: now, Kind: stats.KindTask, Project: root, Profile: "work:outer", Task: "outer", WaitS: 2},
		{Time: now, Kind: stats.KindTask, Project: root, Profile: "work:outer", Task: "outer", WaitS: 3},
		{Time: now, Kind: stats.KindTask, Project: root, Profile: "work:glint", Task: "glint", In: "smoke", WallS: 12, Exit: 0},
		{Time: now, Kind: stats.KindTask, Project: root, Profile: "work:glint", Task: "glint", In: "deploy", WallS: 22, Exit: 0},
		{Time: now, Kind: stats.KindTask, Project: root, Profile: "work:glint", Task: "glint", WallS: 32, Exit: 0},
	}
	var data strings.Builder
	for _, rec := range recs {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		data.Write(line)
		data.WriteByte('\n')
	}
	writeFile(t, path, data.String())
	if out, code := f.graft("", "stats", "--value", "wall-p50", "--task", "gate", "--since", "24h"); code != 0 || out != "20\n" {
		t.Errorf("project wall-p50 including linked tree and commit gate: exit %d, %q; want 0, %q", code, out, "20\n")
	}
	if out, code := f.graft("", "stats", "--all", "--value", "wall-p95", "--task", "gate", "--since", "24h"); code != 0 || out != "30\n" {
		t.Errorf("wall-p95: exit %d, %q; want 0, %q", code, out, "30\n")
	}
	if out, code := f.graft("", "stats", "--value", "wall-p50", "--task", "glint", "--in", "smoke", "--min-runs", "1", "--since", "24h"); code != 0 || out != "12\n" {
		t.Errorf("wall-p50 in smoke: exit %d, %q; want 0, %q", code, out, "12\n")
	}
	if out, code := f.graft("", "stats", "--all", "--task", "glint", "--in", "smoke", "--since", "24h"); code != 0 || !strings.Contains(out, "glint") || strings.Contains(out, "deploy") {
		t.Errorf("task report in smoke: exit %d, %q; want only glint in smoke", code, out)
	}
	if out, code := f.graft("", "stats", "--all", "--value", "wait-p95", "--since", "24h"); code != 0 || out != "3\n" {
		t.Errorf("outer wait-p95: exit %d, %q; want 0, %q", code, out, "3\n")
	}
	if out, code := f.graft("", "stats", "--all", "--value", "wall-p50", "--task", "absent", "--since", "24h"); code != 3 || !strings.Contains(out, "0 runs, need 3") {
		t.Errorf("too few runs: exit %d, %q; want code 3 and the threshold message", code, out)
	}
	if out, code := f.graft("", "stats", "--value=", "--since", "24h"); code != 2 || !strings.Contains(out, "--value: want") {
		t.Errorf("empty --value: exit %d, %q; want a usage error", code, out)
	}
	if out, code := f.graft("", "stats", "--in", "smoke", "--since", "24h"); code != 2 || !strings.Contains(out, "--in requires --task") {
		t.Errorf("--in without --task: exit %d, %q; want a usage error", code, out)
	}
	if out, code := f.graft("", "stats", "--value", "wait-p95", "--task", "glint", "--in", "smoke", "--since", "24h"); code != 2 || !strings.Contains(out, "cannot be used with --in") {
		t.Errorf("wait-p95 with --in: exit %d, %q; want a usage error", code, out)
	}
	if out, code := f.graft("", "stats", "--tests", "--task", "glint", "--in", "smoke", "--since", "24h"); code != 2 || !strings.Contains(out, "--tests cannot be used with --in") {
		t.Errorf("--tests with --in: exit %d, %q; want a usage error", code, out)
	}
}

func TestStatsWindowAndWaitBreakdown(t *testing.T) {
	f, path := statsRepo(t, "tasks: {}\n")
	root := realRoot(t, f)
	stamp := func(s string) time.Time {
		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	recs := []stats.Record{
		{Time: stamp("2026-11-09T23:59:59Z"), Kind: stats.KindTask, Project: root, Here: "outside-before", Task: "x", WaitS: 70},
		{Time: stamp("2026-11-10T00:00:00Z"), Kind: stats.KindTask, Project: root, Here: "queue", Task: "x", WaitS: 10},
		{Time: stamp("2026-11-11T12:00:00Z"), Kind: stats.KindTask, Project: root, Here: "queue", Task: "x", WaitS: 20},
		{Time: stamp("2026-11-11T13:00:00Z"), Kind: stats.KindTask, Project: root, Here: "local", Task: "x", WaitS: 5},
		{Time: stamp("2026-11-11T14:01:00Z"), Kind: stats.KindTask, Project: root, Here: "outer-queue", Task: "outer", Run: "outer-run", WallS: 3000, WaitS: 10},
		{Time: stamp("2026-11-11T13:30:00Z"), Kind: stats.KindTask, Project: root, Here: "outer-queue", Task: "dep", In: "outer", Run: "outer-run", WaitS: 5},
		{Time: stamp("2026-11-11T14:00:00Z"), Kind: stats.KindTask, Project: root, Here: "queue", Task: "child", Parent: "outer-run", WaitS: 100},
		{Time: stamp("2026-11-12T00:00:00Z"), Kind: stats.KindTask, Project: root, Here: "outside-until", Task: "x", WaitS: 50},
	}
	var data strings.Builder
	for _, rec := range recs {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		data.Write(line)
		data.WriteByte('\n')
	}
	writeFile(t, path, data.String())
	out, code := f.graft("", "stats", "--all", "--since", "2026-11-10T00:00:00Z", "--until", "2026-11-12T00:00:00Z", "--by", "here")
	if code != 0 {
		t.Fatalf("stats window: exit %d, %s", code, out)
	}
	gotRows := map[string][]string{}
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 && (fields[0] == "queue" || fields[0] == "local" || fields[0] == "outer-queue") {
			gotRows[fields[0]] = fields[1:]
		}
		if strings.HasPrefix(line, "nested (already in parents)") {
			gotRows["nested"] = fields[len(fields)-3:]
		}
	}
	for key, want := range map[string][]string{"queue": {"30.000", "2", "2"}, "outer-queue": {"10.000", "1", "1"}, "local": {"5.000", "1", "1"}, "nested": {"105.000", "2", "2"}} {
		if strings.Join(gotRows[key], ",") != strings.Join(want, ",") {
			t.Errorf("%s stats: got %v, want %v; output:\n%s", key, gotRows[key], want, out)
		}
	}
	for _, excluded := range []string{"outside-before", "outside-until"} {
		if strings.Contains(out, excluded) {
			t.Errorf("out-of-window group %q included:\n%s", excluded, out)
		}
	}
	if strings.Index(out, "queue") > strings.Index(out, "local") {
		t.Errorf("groups are not sorted by wait sum:\n%s", out)
	}
	if _, code := f.graft("", "stats", "--all", "--since", "2026-11-12T00:00:00Z", "--until", "2026-11-10T00:00:00Z"); code != 2 {
		t.Errorf("until <= since: exit %d, want usage error", code)
	}
	if _, code := f.graft("", "stats", "--all", "--since", "1d", "--until", "1d", "--by", "here"); code != 2 {
		t.Errorf("equal relative window: exit %d, want usage error", code)
	}
	if _, code := f.graft("", "stats", "--all", "--since", "1d", "--until", "", "--by", "here"); code != 2 {
		t.Errorf("empty --until: exit %d, want usage error", code)
	}
}

func TestStatsReportDoesNotDoubleCountNestedWait(t *testing.T) {
	f, path := statsRepo(t, "tasks: {}\n")
	root := realRoot(t, f)
	recs := []stats.Record{
		{Time: time.Date(2026, 11, 11, 11, 1, 0, 0, time.UTC), Kind: stats.KindTask, Project: root, Task: "same", Here: "queue", Profile: "p", WallS: 120, WaitS: 10, WaitWhy: map[string]float64{"queue": 10}, Run: "r"},
		{Time: time.Date(2026, 11, 11, 11, 0, 0, 0, time.UTC), Kind: stats.KindTask, Project: root, Task: "same", Here: "queue", Profile: "p", WallS: 8, WaitS: 5, WaitWhy: map[string]float64{"queue": 5}, Run: "c", Parent: "r"},
		{Time: time.Date(2026, 11, 11, 12, 3, 0, 0, time.UTC), Kind: stats.KindTask, Project: root, Task: "outer", Here: "queue", Profile: "p", WallS: 120, WaitS: 10, WaitWhy: map[string]float64{"queue": 10}, Run: "in-run"},
		{Time: time.Date(2026, 11, 11, 12, 2, 0, 0, time.UTC), Kind: stats.KindTask, Project: root, Task: "dep", In: "outer", Here: "queue", Profile: "p", WallS: 8, WaitS: 5, WaitWhy: map[string]float64{"queue": 5}, Run: "in-run"},
	}
	var data strings.Builder
	for _, rec := range recs {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		data.Write(line)
		data.WriteByte('\n')
	}
	writeFile(t, path, data.String())
	out, code := f.graft("", "stats", "--all", "--since", "2026-11-10T00:00:00Z")
	if code != 0 {
		t.Fatalf("stats report: exit %d, %s", code, out)
	}
	rows := map[string][]string{}
	for line := range strings.SplitSeq(out, "\n") {
		lineFields := strings.Fields(line)
		if len(lineFields) > 1 {
			rows[lineFields[1]] = lineFields
		}
	}
	for _, task := range []string{"same", "outer"} {
		fields := rows[task]
		if len(fields) < 9 || strings.Join(fields[5:9], " ") != "10s 10s queue 10s" {
			t.Errorf("nested wait leaked into %s row: fields %v\n%s", task, fields, out)
		}
	}
	if strings.Contains(strings.Join(rows["dep"], " "), "queue 5s") {
		t.Errorf("dep wait leaked into old report: %v\n%s", rows["dep"], out)
	}
}

func TestStatsOrphanedNestedWaitCountsAsTopLevel(t *testing.T) {
	f, path := statsRepo(t, "tasks: {}\n")
	root := realRoot(t, f)
	recs := []stats.Record{
		{Time: time.Date(2026, 11, 11, 11, 0, 0, 0, time.UTC), Kind: stats.KindTask, Project: root, Task: "child", Here: "queue", Profile: "p", WallS: 8, WaitS: 5, WaitWhy: map[string]float64{"queue": 5}, Run: "c", Parent: "r"},
		{Time: time.Date(2026, 11, 11, 11, 30, 0, 0, time.UTC), Kind: stats.KindTask, Project: root, Task: "dep", In: "outer", Here: "queue", Profile: "p", WallS: 9, WaitS: 7, WaitWhy: map[string]float64{"queue": 7}, Run: "orphan-run"},
		{Time: time.Date(2026, 11, 12, 0, 0, 1, 0, time.UTC), Kind: stats.KindTask, Project: root, Task: "outer", Here: "queue", Profile: "p", WallS: 20, WaitS: 10, WaitWhy: map[string]float64{"queue": 10}, Run: "r"},
	}
	var data strings.Builder
	for _, rec := range recs {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		data.Write(line)
		data.WriteByte('\n')
	}
	writeFile(t, path, data.String())
	args := []string{"stats", "--all", "--since", "2026-11-10T00:00:00Z", "--until", "2026-11-12T00:00:00Z"}
	if out, code := f.graft("", args...); code != 0 || !strings.Contains(out, "queue 5s") {
		t.Errorf("orphan wait in old report: exit %d, %s", code, out)
	}
	if out, code := f.graft("", append(args, "--by", "here")...); code != 0 || !strings.Contains(out, "queue") || !strings.Contains(out, "12.000") || strings.Contains(out, "nested (already in parents)") {
		t.Errorf("orphan wait by here: exit %d, %s", code, out)
	}
	if out, code := f.graft("", append(args, "--by", "profile-task")...); code != 0 || !strings.Contains(out, "p/child") || !strings.Contains(out, "5.000") || !strings.Contains(out, "p/dep") || !strings.Contains(out, "7.000") || strings.Contains(out, "nested (already in parents)") {
		t.Errorf("orphan wait by profile-task: exit %d, %s", code, out)
	}
}

func TestStatsParentMustContainChildInterval(t *testing.T) {
	f, path := statsRepo(t, "tasks: {}\n")
	root := realRoot(t, f)
	stamp := func(s string) time.Time {
		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	recs := []stats.Record{
		{Time: stamp("2026-11-11T10:00:00Z"), Kind: stats.KindTask, Project: root, Task: "sibling", Here: "queue", Profile: "p", WallS: 10, WaitS: 2, WaitWhy: map[string]float64{"queue": 2}, Run: "outer-run"},
		{Time: stamp("2026-11-11T11:00:00Z"), Kind: stats.KindTask, Project: root, Task: "child", Here: "queue", Profile: "p", WallS: 20, WaitS: 5, WaitWhy: map[string]float64{"queue": 5}, Run: "child-run", Parent: "outer-run"},
		{Time: stamp("2026-11-12T00:00:01Z"), Kind: stats.KindTask, Project: root, Task: "outer", Here: "outer", Profile: "p", WallS: 50000, WaitS: 10, WaitWhy: map[string]float64{"queue": 10}, Run: "outer-run"},
	}
	var data strings.Builder
	for _, rec := range recs {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		data.Write(line)
		data.WriteByte('\n')
	}
	writeFile(t, path, data.String())

	window := []string{"--all", "--since", "2026-11-10T00:00:00Z", "--until", "2026-11-12T00:00:00Z"}
	byArgs := append(append([]string(nil), window...), "--by", "here")
	out, code := f.graft("", append([]string{"stats"}, byArgs...)...)
	if code != 0 || !strings.Contains(out, "queue  7.000") || strings.Contains(out, "nested (already in parents)") {
		t.Errorf("sibling must not hide child wait when outer is outside the window: exit %d, %s", code, out)
	}
	out, code = f.graft("", append([]string{"stats"}, window...)...)
	if code != 0 || !strings.Contains(out, "child") || !strings.Contains(out, "queue 5s") {
		t.Errorf("old report must include orphaned-in-window child wait: exit %d, %s", code, out)
	}

	fullWindow := []string{"--all", "--since", "2026-11-10T00:00:00Z", "--until", "2026-11-13T00:00:00Z"}
	out, code = f.graft("", append(append([]string{"stats"}, fullWindow...), "--by", "here")...)
	if code != 0 || !strings.Contains(out, "outer") || !strings.Contains(out, "10.000") || !hasNestedWaitLine(out, "5.000") {
		t.Errorf("containing outer must account for child exactly once: exit %d, %s", code, out)
	}
	out, code = f.graft("", append([]string{"stats"}, fullWindow...)...)
	if code != 0 || !strings.Contains(out, "outer") || strings.Contains(out, "queue 5s") {
		t.Errorf("old report must suppress child wait when containing outer is selected: exit %d, %s", code, out)
	}
}

func hasNestedWaitLine(output, wait string) bool {
	for line := range strings.SplitSeq(output, "\n") {
		if strings.HasPrefix(line, "nested (already in parents)") && strings.Contains(line, wait) {
			return true
		}
	}
	return false
}
