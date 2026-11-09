package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/stats"
)

// fakeMachine makes dir the /proc that graft measures the machine by: 22
// threads and runnable tasks ready to run, the disk and the swap calm, so
// that only the runnable count decides. It returns the variable that points
// graft at it.
func fakeMachine(t *testing.T, dir string, runnable int) string {
	t.Helper()
	writeFile(t, filepath.Join(dir, "pressure", "io"), "some avg10=0.00 avg60=0.00 avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1\n")
	writeFile(t, filepath.Join(dir, "pressure", "memory"), "some avg10=0.00 avg60=0.00 avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1\n")
	writeFile(t, filepath.Join(dir, "vmstat"), "pswpin 7\npswpout 3\n")
	writeFile(t, filepath.Join(dir, "loadavg"), "0.00 0.00 0.00 1/100 1\n")
	setRunnable(t, dir, runnable)
	return "GRAFT_PROC=" + dir
}

// setRunnable sets the tasks ready to run on the machine of fakeMachine.
func setRunnable(t *testing.T, dir string, n int) {
	t.Helper()
	var stat strings.Builder
	stat.WriteString("cpu  1 1 1 1\n")
	for i := range 22 {
		fmt.Fprintf(&stat, "cpu%d 1 1 1 1\n", i)
	}
	// procs_running counts the process that reads it.
	fmt.Fprintf(&stat, "procs_running %d\n", n+1)
	replaceFile(t, filepath.Join(dir, "stat"), stat.String())
}

func replaceFile(t *testing.T, path, contents string) {
	t.Helper()
	f, err := os.CreateTemp(filepath.Dir(path), "loadavg-")
	if err != nil {
		t.Fatal(err)
	}
	temp := f.Name()
	t.Cleanup(func() { _ = os.Remove(temp) })
	if _, err := f.WriteString(contents); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp, path); err != nil {
		t.Fatal(err)
	}
}

// TestHeavyTaskWaitsForLoad: a heavy task starts only once the load is below
// the user's wait threshold, gets the user's thread count as {threads}, and a
// task that is not heavy never waits.
func TestHeavyTaskWaitsForLoad(t *testing.T) {
	state := t.TempDir()
	out := filepath.Join(state, "out")
	f := newRepo(t, map[string]string{
		".graft.yaml": `schema: 1
version: {mode: none}
tasks:
  build:
    heavy: true
    run: [[sh, -c, 'echo threads={threads} env=$GRAFT_THREADS procs=$GOMAXPROCS admitted=$GRAFT_ADMITTED >> ` + out + `']]
  light:
    run: [[sh, -c, 'echo light >> ` + out + `']]
`,
	})
	proc := filepath.Join(state, "proc")
	cfgDir := t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "load: {threads: 6, poll: 20ms, settle: 10ms}\n")
	writeTestPressure(t, cfgDir)
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, fakeMachine(t, proc, 30), "XDG_CACHE_HOME="+t.TempDir())

	read := func() string {
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	f.mustGraft("light")
	if got := read(); got != "light\n" {
		t.Fatalf("a light task under load: %q", got)
	}

	cmd, log := f.start(f.work, "build")
	waitFor(t, func() bool { return strings.Contains(log.String(), "waiting") })
	if got := read(); got != "light\n" {
		t.Errorf("the heavy task ran at runnable 30: %q", got)
	}
	setRunnable(t, proc, 17)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("build: %v\n%s", err, log.String())
	}
	if got := read(); got != "light\nthreads=6 env=6 procs=6 admitted=1\n" {
		t.Errorf("after the load fell: %q\n%s", got, log.String())
	}
	if !strings.Contains(log.String(), "graft: build: sh -c echo threads=6 ") {
		t.Errorf("the step is echoed without its thread count:\n%s", log.String())
	}
	if !strings.Contains(log.String(), "build: runnable 30 is at or above 22 (pressure.yaml runnable), waiting") || !strings.Contains(log.String(), "build: runnable 17, starting after ") {
		t.Errorf("no wait reported:\n%s", log.String())
	}
}

// TestHeavyTaskThreadsOverInherited: GOMAXPROCS and -p of the environment are
// the default of a command run without admission; an admitted heavy task, a
// heavy exec and the gate get their threads over them, in a graft a step
// starts too; the task's own env still wins, ${GOFLAGS} bringing the user's
// -p, not its own, and a .env key the environment sets being the
// environment's; and a task that is not heavy keeps the default.
func TestHeavyTaskThreadsOverInherited(t *testing.T) {
	state := t.TempDir()
	out := filepath.Join(state, "out")
	echo := func(name string) string {
		return `[sh, -c, 'echo ` + name + ` procs=$GOMAXPROCS flags=$GOFLAGS >> ` + out + `']`
	}
	step := func(name string) string { return "[" + echo(name) + "]" }
	inner := func(task string) string { return "[[" + yamlQuote(graftBin) + ", " + task + "]]" }
	f := newRepo(t, map[string]string{
		".graft.yaml": `schema: 1
version: {mode: none}
tasks:
  build:
    heavy: true
    run: ` + step("build") + `
  own:
    heavy: true
    env: {GOMAXPROCS: "2", GOFLAGS: -p=3}
    run: ` + step("own") + `
  ownflags:
    heavy: true
    env: {GOFLAGS: -race}
    run: ` + step("ownflags") + `
  dollar:
    heavy: true
    env: {GOFLAGS: '${GOFLAGS} -race'}
    run: ` + step("dollar") + `
  empty:
    heavy: true
    env: {GOFLAGS: ''}
    run: ` + step("empty") + `
  keys:
    heavy: true
    dotenv: [GOFLAGS, GOMAXPROCS]
    run: ` + step("keys") + `
  light:
    run: ` + step("light") + `
  underlight:
    run: ` + inner("ownflags") + `
  underheavy:
    heavy: true
    run: ` + inner("ownflags") + `
gate:
  - ` + echo("gate") + `
`,
		// The environment's values win over the file's: they are not the task's.
		".env": "GOFLAGS=-p=2\nGOMAXPROCS=2\n",
	})
	cfgDir := t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "load: {threads: 6, poll: 20ms, settle: 10ms}\n")
	writeTestPressure(t, cfgDir)
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, fakeMachine(t, filepath.Join(state, "proc"), 0), "XDG_CACHE_HOME="+t.TempDir(),
		"GOMAXPROCS=1", "GOFLAGS=-p=1")
	for _, task := range []string{"build", "own", "ownflags", "dollar", "empty", "keys", "light", "underlight", "underheavy", "gate"} {
		f.mustGraft(task)
	}
	f.mustGraft("exec", "--heavy", "--", "sh", "-c", "echo exec procs=$GOMAXPROCS flags=$GOFLAGS >> "+out)
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := "build procs=6 flags=-p=1 -p=6\n" +
		"own procs=2 flags=-p=1 -p=3\n" +
		"ownflags procs=6 flags=-p=1 -race -p=6\n" +
		"dollar procs=6 flags=-p=1 -p=1 -race -p=6\n" +
		"empty procs=6 flags=-p=1 -p=6\n" +
		"keys procs=6 flags=-p=1 -p=6\n" +
		"light procs=1 flags=-p=1\n" +
		"ownflags procs=6 flags=-p=1 -race -p=6\n" +
		"ownflags procs=6 flags=-p=1 -p=6 -race -p=6\n" +
		"gate procs=6 flags=-p=1 -p=6\n" +
		"exec procs=6 flags=-p=1 -p=6\n"
	if got := string(data); got != want {
		t.Errorf("steps saw:\n%s\nwant:\n%s", got, want)
	}
}

// TestHeavyTaskKeepsItsDotenvThreads: GOMAXPROCS of a .env key the
// environment does not set is the task's, over an admitted task's threads; a
// variable of the environment that differs in case only is not it.
func TestHeavyTaskKeepsItsDotenvThreads(t *testing.T) {
	state := t.TempDir()
	out := filepath.Join(state, "out")
	f := newRepo(t, map[string]string{
		".graft.yaml": `schema: 1
version: {mode: none}
tasks:
  keys:
    heavy: true
    dotenv: [GOMAXPROCS]
    run: [[sh, -c, 'echo procs=$GOMAXPROCS >> ` + out + `']]
`,
		".env": "GOMAXPROCS=2\n",
	})
	cfgDir := t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "load: {threads: 6, poll: 20ms, settle: 10ms}\n")
	writeTestPressure(t, cfgDir)
	// The tests' environment has no GOMAXPROCS of its own.
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, fakeMachine(t, filepath.Join(state, "proc"), 0), "XDG_CACHE_HOME="+t.TempDir())
	f.mustGraft("keys")
	want := "procs=2\n"
	if runtime.GOOS != "windows" {
		// Names are case-sensitive here, as the .env lookup reads them.
		f.env = append(f.env, "gomaxprocs=1")
		f.mustGraft("keys")
		want += want
	}
	if got := readFile(t, out); got != want {
		t.Errorf("the step saw %q, want the .env key's procs=2", got)
	}
}

// TestTaskKeysTheBaseDrops: a .env key of a task the environment sets but the
// steps' base environment drops (a local git variable) reaches the step as
// the task's; a lowercase gomaxprocs of a heavy task is not its GOMAXPROCS
// where names are case-sensitive.
func TestTaskKeysTheBaseDrops(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("names differing in case are one variable on Windows")
	}
	state := t.TempDir()
	out := filepath.Join(state, "out")
	f := newRepo(t, map[string]string{
		".graft.yaml": `schema: 1
version: {mode: none}
tasks:
  keys:
    dotenv: [GIT_PREFIX]
    run: [[sh, -c, 'echo prefix=$GIT_PREFIX >> ` + out + `']]
  lower:
    heavy: true
    env: {gomaxprocs: '2'}
    run: [[sh, -c, 'echo procs=$GOMAXPROCS >> ` + out + `']]
`,
		".env": "OTHER=1\n",
	})
	cfgDir := t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "load: {threads: 6, poll: 20ms, settle: 10ms}\n")
	writeTestPressure(t, cfgDir)
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, fakeMachine(t, filepath.Join(state, "proc"), 0), "XDG_CACHE_HOME="+t.TempDir())
	f.env = append(f.env, "GIT_PREFIX=sub/")
	f.mustGraft("keys")
	f.mustGraft("lower")
	if got, want := readFile(t, out), "prefix=sub/\nprocs=6\n"; got != want {
		t.Errorf("the steps saw %q, want %q", got, want)
	}
}

// TestHeavyTaskWaitIsRecorded: the wait for the load is on the task's stats
// line and in the report's wait columns, the outer task holding its inner
// task's wait.
func TestHeavyTaskWaitIsRecorded(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\ntasks:\n  outer:\n    deps: [build]\n    run: [" + helperStep("ok") + "]\n  build:\n    heavy: true\n    deps: [prep]\n    run: [" + helperStep("ok") + "]\n  prep:\n    run: [" + helperStep("ok") + "]\n",
	})
	proc := t.TempDir()
	cfgDir, cache := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "load: {poll: 20ms, settle: 10ms}\n")
	writeTestPressure(t, cfgDir)
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, fakeMachine(t, proc, 30), "XDG_CACHE_HOME="+cache)
	cmd := exec.Command(graftBin, "outer")
	cmd.Dir = f.work
	cmd.Env = append(slices.Clone(testEnv), f.env...)
	log := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(log.String(), "waiting") })
	setRunnable(t, proc, 1)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("outer: %v\n%s", err, log.String())
	}
	waits := map[string]float64{}
	why := map[string]map[string]float64{}
	for _, l := range readStats(t, filepath.Join(cache, "graft", "stats.jsonl")) {
		if l.Kind == "task" {
			waits[l.Task], why[l.Task] = l.WaitS, l.WaitWhy
		}
	}
	// It waited at least the poll after it said so.
	if waits["build"] < 0.02 || waits["prep"] != 0 || waits["outer"] < waits["build"] {
		t.Errorf("waits: %v", waits)
	}
	// The wait is told by why, the load most of it, none of it settling,
	// which comes after the start; the outer task holds the same.
	if why["build"]["load"] < 0.02 || why["build"]["settle"] != 0 || why["prep"] != nil || why["outer"]["load"] != why["build"]["load"] {
		t.Errorf("wait_why: %v", why)
	}
	if out := f.mustGraft("stats"); !strings.Contains(out, "WAIT WHY") || !strings.Contains(out, "load ") {
		t.Errorf("no wait columns:\n%s", out)
	}
}

// TestDeployPrioritySkipsTheQueue: a heavy task run for a deploy does not
// queue behind a heavy task waiting for a busy machine, and starts when the
// deploy's budget for waiting is spent.
func TestDeployPrioritySkipsTheQueue(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\ntasks:\n  build:\n    heavy: true\n    run: [" + helperStep("ok") + "]\n",
	})
	cfgDir := t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "load: {poll: 20ms, settle: 1h}\n")
	writeTestPressure(t, cfgDir)
	cache := t.TempDir()
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, fakeMachine(t, t.TempDir(), 30), "XDG_CACHE_HOME="+cache)
	queued := exec.Command(graftBin, "build")
	queued.Dir = f.work
	queued.Env = append(slices.Clone(testEnv), f.env...)
	if err := queued.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queued.Process.Kill(); _ = queued.Wait() })
	waitFor(t, func() bool {
		notes, _ := filepath.Glob(filepath.Join(cache, "graft", "load.lock.*.lholder"))
		return len(notes) > 0
	})
	// Queued, it would wait as long as the load stays up.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	priority := exec.CommandContext(ctx, graftBin, "build")
	priority.Dir = f.work
	budget := filepath.Join(t.TempDir(), "budget")
	writeFile(t, budget, fmt.Sprint(int64(300*time.Millisecond)))
	priority.Env = append(slices.Clone(testEnv), append(f.env, "GRAFT_LOAD_PRIORITY=graft deploy prod",
		"GRAFT_LOAD_BUDGET="+budget, "GRAFT_LOAD_THEN=start")...)
	out, err := priority.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "not queueing behind other heavy tasks: it runs for graft deploy prod") ||
		!strings.Contains(string(out), "all the wait graft deploy prod allows") {
		t.Errorf("the priority task did not skip the queue and start when its budget was spent: %v\n%s", err, out)
	}
	if left, err := os.ReadFile(budget); err != nil || strings.TrimSpace(string(left)) != "0" {
		t.Errorf("budget left: %s %v", left, err)
	}
}

// TestExecHeavyWaitsForLoad: graft exec --heavy runs a command that is no
// task in the directory it was started in, after the machine admits it,
// with {threads} and GRAFT_THREADS; its exit status is graft's, and the
// stats record it as the task exec of the work tree. Without --heavy it
// does not wait.
func TestExecHeavyWaitsForLoad(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none}\n", "sub/keep": ""})
	proc := t.TempDir()
	cfgDir, cache := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "load: {threads: 6, poll: 20ms, settle: 10ms}\n")
	writeTestPressure(t, cfgDir)
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, fakeMachine(t, proc, 30), "XDG_CACHE_HOME="+cache)
	sub := filepath.Join(f.work, "sub")

	if out, code := f.exec(sub, "", graftBin, "exec", "--", "sh", "-c", "echo light in $(basename $PWD)"); code != 0 || !strings.Contains(out, "light in sub") || strings.Contains(out, "waiting") {
		t.Fatalf("exec without --heavy: exit %d\n%s", code, out)
	}
	cmd, log := f.start(sub, "exec", "--heavy", "--", "sh", "-c", "echo threads={threads} env=$GRAFT_THREADS in $(basename $PWD); exit 3")
	waitFor(t, func() bool { return strings.Contains(log.String(), "exec: runnable 30 is at or above 22") })
	if strings.Contains(log.String(), "threads=") {
		t.Fatalf("ran at runnable 30:\n%s", log.String())
	}
	setRunnable(t, proc, 17)
	err := cmd.Wait()
	if code := cmd.ProcessState.ExitCode(); err == nil || code != 3 {
		t.Errorf("exit %d (%v), want the command's 3", code, err)
	}
	if !strings.Contains(log.String(), "threads=6 env=6 in sub\n") {
		t.Errorf("no thread count, or not in sub:\n%s", log.String())
	}
	var tasks []string
	for _, l := range readStats(t, filepath.Join(cache, "graft", "stats.jsonl")) {
		if l.Kind == "task" {
			tasks = append(tasks, fmt.Sprintf("%s %s %d wait=%v", l.Task, l.Top, l.Exit, l.WaitS > 0))
		}
	}
	if want := "exec exec 0 wait=false,exec exec 3 wait=true"; strings.Join(tasks, ",") != want {
		t.Errorf("task lines %q, want %q", strings.Join(tasks, ","), want)
	}
	if out, code := f.graft("", "exec", "--heavy", "sh"); code != 125 || !strings.Contains(out, "graft exec [--heavy") || !strings.Contains(out, "-- <command>") {
		t.Errorf("without --: exit %d\n%s", code, out)
	}
}

// TestExecExitStatus: graft exec exits with the command's status, 128 and
// the signal for one a signal ended, and its own failures as env does: 125
// graft, 126 a command that cannot be started, 127 one not found.
func TestExecExitStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none}\n", "plain": "echo\n"})
	f.env = append(f.env, "XDG_CACHE_HOME="+t.TempDir())
	for _, c := range []struct {
		name string
		args []string
		code int
		says string
	}{
		{"the command's", []string{"exec", "--", "sh", "-c", "exit 3"}, 3, "exit status 3"},
		{"a signal", []string{"exec", "--", "sh", "-c", "kill -TERM $$"}, 128 + 15, "signal: terminated"},
		{"not found", []string{"exec", "--heavy", "--", "no-such-command-here"}, 127, "graft: exec: start no-such-command-here: "},
		{"not executable", []string{"exec", "--", "./plain"}, 126, "graft: exec: start ./plain: "},
		{"usage", []string{"exec", "sh"}, 125, "(see graft help)"},
		{"-C to nowhere", []string{"-C", "no-such-dir", "exec", "--", "true"}, 125, "no-such-dir"},
		{"-C after exec", []string{"exec", "-C", ".git", "--", "true"}, 125, "-C goes before the command"},
	} {
		if out, code := f.graft("", c.args...); code != c.code || !strings.Contains(out, c.says) {
			t.Errorf("%s: exit %d, want %d and %q:\n%s", c.name, code, c.code, c.says, out)
		}
	}
	writeFile(t, filepath.Join(f.work, ".graft.yaml"), "schema: 1\nno_such_key: 1\n")
	if out, code := f.graft("", "exec", "--heavy", "--", "true"); code != 125 || !strings.Contains(out, "graft:") {
		t.Errorf("a config graft cannot read: exit %d\n%s", code, out)
	}
}

// TestGateQueuesForTheMachine: the gate as a whole waits for a busy machine
// as a heavy task does, its tasks not heavy; a graft that a step of an
// admitted heavy task starts does not queue again.
func TestGateQueuesForTheMachine(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\ngate:\n  - task: lint\ntasks:\n  lint:\n    run: [" + helperStep("ok") + "]\n  build:\n    heavy: true\n    run: [" + helperStep("ok") + "]\n",
	})
	proc := t.TempDir()
	cfgDir := t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "load: {poll: 20ms, settle: 10ms}\n")
	writeTestPressure(t, cfgDir)
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, fakeMachine(t, proc, 30), "XDG_CACHE_HOME="+t.TempDir())
	cmd, log := f.start(f.work, "gate")
	waitFor(t, func() bool {
		return strings.Contains(log.String(), "gate: runnable 30 is at or above 22 (pressure.yaml runnable), waiting")
	})
	if strings.Contains(log.String(), "gate 1/1") {
		t.Fatalf("the gate ran at runnable 30:\n%s", log.String())
	}
	setRunnable(t, proc, 17)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("gate: %v\n%s", err, log.String())
	}
	setRunnable(t, proc, 30)
	f.env = append(f.env, "GRAFT_ADMITTED=1")
	if out, code := f.graft("", "build"); code != 0 || strings.Contains(out, "waiting") {
		t.Errorf("an admitted graft queued again: exit %d\n%s", code, out)
	}
}

// TestLightGateDoesNotWaitForTheMachine: a gate whose last runs cost next to
// nothing runs on a busy machine at once, its heavy task with it; a change of .graft.yaml sends it
// back to the queue until it has runs again.
func TestLightGateDoesNotWaitForTheMachine(t *testing.T) {
	config := "schema: 1\nversion: {mode: none}\ngate:\n  - task: lightstep\ntasks:\n  lightstep:\n    heavy: true\n    run:\n" +
		"      - [" + yamlQuote(helperBin) + ", ok]\n"
	f := newRepo(t, map[string]string{".graft.yaml": config})
	proc := filepath.Join(t.TempDir(), "proc")
	f.env = append(f.env, "XDG_CACHE_HOME="+t.TempDir(), fakeMachine(t, proc, 1))
	for range stats.LightGateRuns {
		f.mustGraft("gate")
	}

	setRunnable(t, proc, 30)
	g := startGraft(t, f, "gate")
	select {
	case <-g.done:
	case <-time.After(20 * time.Second):
		g.kill()
		t.Fatalf("the light gate did not end on a busy machine:\n%s", g.out.String())
	}
	if out := g.out.String(); !strings.Contains(out, "not waiting for the machine") || strings.Contains(out, ", waiting\n") {
		t.Errorf("a light gate on a busy machine:\n%s", out)
	}

	f.write(".graft.yaml", config+"# changed\n")
	waiting := startWaitingForMachine(t, f, "gate")
	setRunnable(t, proc, 1)
	if out := waiting.end(t); strings.Contains(out, "not waiting for the machine") {
		t.Errorf("a gate just reconfigured was taken for light:\n%s", out)
	}
}
