package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

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
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "load: {threads: 6, poll: 20ms, settle: 10ms}\n")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, "GRAFT_LOADAVG="+loadavg, "XDG_CACHE_HOME="+t.TempDir())

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

	cmd := exec.Command(graftBin, "build")
	cmd.Dir = f.work
	cmd.Env = append(slices.Clone(testEnv), f.env...)
	log := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(log.String(), "waiting") })
	if got := read(); got != "light\n" {
		t.Errorf("the heavy task ran at load 30.50: %q", got)
	}
	writeFile(t, loadavg, "17.00 20.00 10.00 3/400 1234\n")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("build: %v\n%s", err, log.String())
	}
	if got := read(); got != "light\nthreads=6 env=6\n" {
		t.Errorf("after the load fell: %q\n%s", got, log.String())
	}
	if !strings.Contains(log.String(), "graft: build: sh -c echo threads=6 ") {
		t.Errorf("the step is echoed without its thread count:\n%s", log.String())
	}
	if !strings.Contains(log.String(), "build: load 30.50 is above 22, waiting") || !strings.Contains(log.String(), "build: load 17.00, starting after ") {
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
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "load: {poll: 20ms, settle: 10ms}\n")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, "GRAFT_LOADAVG="+loadavg, "XDG_CACHE_HOME="+cache)
	cmd := exec.Command(graftBin, "outer")
	cmd.Dir = f.work
	cmd.Env = append(slices.Clone(testEnv), f.env...)
	log := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(log.String(), "waiting") })
	writeFile(t, loadavg, "1.00 20.00 10.00 3/400 1234\n")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("outer: %v\n%s", err, log.String())
	}
	waits := map[string]float64{}
	for _, l := range readStats(t, filepath.Join(cache, "graft", "stats.jsonl")) {
		if l.Kind == "task" {
			waits[l.Task] = l.WaitS
		}
	}
	// It waited at least the poll after it said so.
	if waits["build"] < 0.02 || waits["prep"] != 0 || waits["outer"] < waits["build"] {
		t.Errorf("waits: %v", waits)
	}
	if out := f.mustGraft("stats"); !strings.Contains(out, "WAIT MED") {
		t.Errorf("no wait columns:\n%s", out)
	}
}

// TestDeployPrioritySkipsTheQueue: a heavy task run for a deploy does not
// queue behind a heavy task that holds the queue while it settles.
func TestDeployPrioritySkipsTheQueue(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\ntasks:\n  build:\n    heavy: true\n    run: [" + helperStep("ok") + "]\n",
	})
	loadavg := filepath.Join(t.TempDir(), "loadavg")
	writeFile(t, loadavg, "18.00 20.00 10.00 3/400 1234\n")
	cfgDir := t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "load: {poll: 20ms, settle: 1h}\n")
	cache := t.TempDir()
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, "GRAFT_LOADAVG="+loadavg, "XDG_CACHE_HOME="+cache)
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
	// Queued, it would wait the hour the holder settles.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	priority := exec.CommandContext(ctx, graftBin, "build")
	priority.Dir = f.work
	priority.Env = append(slices.Clone(testEnv), append(f.env, "GRAFT_LOAD_PRIORITY=graft deploy prod")...)
	out, err := priority.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "not queueing behind other heavy tasks: it runs for graft deploy prod") {
		t.Errorf("the priority task did not skip the queue: %v\n%s", err, out)
	}
}
