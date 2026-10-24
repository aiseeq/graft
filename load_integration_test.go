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
)

// calmMachine is the environment of a machine whose disk and swap are calm,
// so that only the faked load decides.
func calmMachine(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	io, vmstat := filepath.Join(dir, "io"), filepath.Join(dir, "vmstat")
	writeFile(t, io, "some avg10=0.00 avg60=0.00 avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1\n")
	writeFile(t, vmstat, "pswpin 7\npswpout 3\n")
	return []string{"GRAFT_PRESSURE_IO=" + io, "GRAFT_VMSTAT=" + vmstat}
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
    run: [[sh, -c, 'echo threads={threads} env=$GRAFT_THREADS >> ` + out + `']]
  light:
    run: [[sh, -c, 'echo light >> ` + out + `']]
`,
	})
	loadavg := filepath.Join(state, "loadavg")
	writeFile(t, loadavg, "30.50 20.00 10.00 3/400 1234\n")
	cfgDir := t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "load: {threads: 6, poll: 20ms, settle: 10ms, swap_window: 10ms}\n")
	f.env = append(append(f.env, "XDG_CONFIG_HOME="+cfgDir, "GRAFT_LOADAVG="+loadavg, "XDG_CACHE_HOME="+t.TempDir()), calmMachine(t)...)

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
		t.Errorf("the heavy task ran at load 30.50: %q", got)
	}
	replaceFile(t, loadavg, "17.00 20.00 10.00 3/400 1234\n")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("build: %v\n%s", err, log.String())
	}
	if got := read(); got != "light\nthreads=6 env=6\n" {
		t.Errorf("after the load fell: %q\n%s", got, log.String())
	}
	if !strings.Contains(log.String(), "graft: build: sh -c echo threads=6 ") {
		t.Errorf("the step is echoed without its thread count:\n%s", log.String())
	}
	if !strings.Contains(log.String(), "build: load 30.50 is at or above 22 (user config load.wait_above), waiting") || !strings.Contains(log.String(), "build: load 17.00, starting after ") {
		t.Errorf("no wait reported:\n%s", log.String())
	}
}

// TestHeavyTaskWaitIsRecorded: the wait for the load is on the task's stats
// line and in the report's wait columns, the outer task holding its inner
// task's wait.
func TestHeavyTaskWaitIsRecorded(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\ntasks:\n  outer:\n    deps: [build]\n    run: [" + helperStep("ok") + "]\n  build:\n    heavy: true\n    deps: [prep]\n    run: [" + helperStep("ok") + "]\n  prep:\n    run: [" + helperStep("ok") + "]\n",
	})
	loadavg := filepath.Join(t.TempDir(), "loadavg")
	writeFile(t, loadavg, "30.50 20.00 10.00 3/400 1234\n")
	cfgDir, cache := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "load: {poll: 20ms, settle: 10ms, swap_window: 10ms}\n")
	f.env = append(append(f.env, "XDG_CONFIG_HOME="+cfgDir, "GRAFT_LOADAVG="+loadavg, "XDG_CACHE_HOME="+cache), calmMachine(t)...)
	cmd := exec.Command(graftBin, "outer")
	cmd.Dir = f.work
	cmd.Env = append(slices.Clone(testEnv), f.env...)
	log := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(log.String(), "waiting") })
	replaceFile(t, loadavg, "1.00 20.00 10.00 3/400 1234\n")
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
	// The wait is told by why: the load most of it, the settling after it;
	// the outer task holds the same.
	if why["build"]["load"] < 0.02 || why["build"]["settle"] <= 0 || why["prep"] != nil || why["outer"]["load"] != why["build"]["load"] {
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
	loadavg := filepath.Join(t.TempDir(), "loadavg")
	writeFile(t, loadavg, "30.00 20.00 10.00 3/400 1234\n")
	cfgDir := t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "load: {poll: 20ms, settle: 1h, swap_window: 10ms}\n")
	cache := t.TempDir()
	f.env = append(append(f.env, "XDG_CONFIG_HOME="+cfgDir, "GRAFT_LOADAVG="+loadavg, "XDG_CACHE_HOME="+cache), calmMachine(t)...)
	queued := exec.Command(graftBin, "build")
	queued.Dir = f.work
	queued.Env = append(slices.Clone(testEnv), f.env...)
	if err := queued.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queued.Process.Kill(); _ = queued.Wait() })
	waitFor(t, func() bool {
		notes, _ := filepath.Glob(filepath.Join(cache, "graft", "load.lock.*.holder"))
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
	loadavg := filepath.Join(t.TempDir(), "loadavg")
	writeFile(t, loadavg, "30.50 20.00 10.00 3/400 1234\n")
	cfgDir, cache := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "load: {threads: 6, poll: 20ms, settle: 10ms, swap_window: 10ms}\n")
	f.env = append(append(f.env, "XDG_CONFIG_HOME="+cfgDir, "GRAFT_LOADAVG="+loadavg, "XDG_CACHE_HOME="+cache), calmMachine(t)...)
	sub := filepath.Join(f.work, "sub")

	if out, code := f.exec(sub, "", graftBin, "exec", "--", "sh", "-c", "echo light in $(basename $PWD)"); code != 0 || !strings.Contains(out, "light in sub") || strings.Contains(out, "waiting") {
		t.Fatalf("exec without --heavy: exit %d\n%s", code, out)
	}
	cmd, log := f.start(sub, "exec", "--heavy", "--", "sh", "-c", "echo threads={threads} env=$GRAFT_THREADS in $(basename $PWD); exit 3")
	waitFor(t, func() bool { return strings.Contains(log.String(), "exec: load 30.50 is at or above 22") })
	if strings.Contains(log.String(), "threads=") {
		t.Fatalf("ran at load 30.50:\n%s", log.String())
	}
	writeFile(t, loadavg, "17.00 20.00 10.00 3/400 1234\n")
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
	if out, code := f.graft("", "exec", "--heavy", "sh"); code != 2 || !strings.Contains(out, "graft exec [--heavy] -- <command>") {
		t.Errorf("without --: exit %d\n%s", code, out)
	}
}
