package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNestedGraftWaitCountsInParent(t *testing.T) {
	config := "schema: 1\nversion: {mode: none}\n" +
		"go_fix: {enabled: false}\ngo_fmt: {enabled: false, reason: test}\ngo_vet: {enabled: false, reason: test}\n" +
		"gate:\n  - task: work\ntasks:\n" +
		"  outer:\n    run: ['" + graftBin + " middle']\n" +
		"  middle:\n    run: ['" + graftBin + " gate']\n" +
		"  work:\n    heavy: true\n    run: [" + helperStep("ok") + "]\n"
	f := newRepo(t, map[string]string{".graft.yaml": config})
	project := filepath.Join(f.base, "projectA")
	if err := os.Rename(f.work, project); err != nil {
		t.Fatal(err)
	}
	f.work = project
	state := t.TempDir()
	cache := filepath.Join(state, "cache")
	statsFile := filepath.Join(cache, "graft", "stats.jsonl")
	proc, cfg := filepath.Join(state, "proc"), filepath.Join(state, "config")
	writeFile(t, filepath.Join(cfg, "graft", "config.yaml"), "load: {poll: 20ms, settle: 10ms}\n")
	writeTestPressure(t, cfg)
	f.env = append(f.env, "XDG_CACHE_HOME="+cache, "XDG_CONFIG_HOME="+cfg, fakeMachine(t, proc, 30))
	cmd := exec.Command(graftBin, "outer")
	cmd.Dir = f.work
	cmd.Env = append(slices.Clone(testEnv), f.env...)
	log := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.After(10 * time.Second)
	for !strings.Contains(log.String(), "waiting") {
		select {
		case err := <-done:
			t.Fatalf("outer exited before admission: %v\n%s", err, log.String())
		case <-deadline:
			stopTestChild(cmd)
			<-done
			t.Fatalf("outer did not wait:\n%s", log.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
	setRunnable(t, proc, 1)
	if err := <-done; err != nil {
		t.Fatalf("outer: %v\n%s", err, log.String())
	}
	rows := map[string]statsLine{}
	for _, rec := range readStats(t, statsFile) {
		if rec.Kind == "task" && (rec.Task == "outer" || rec.Task == "middle" || rec.Task == "gate") {
			rows[rec.Task] = rec
		}
	}
	child, middle, outer := rows["gate"], rows["middle"], rows["outer"]
	if child.WaitS < 0.1 || middle.WaitS != child.WaitS || outer.WaitS != child.WaitS {
		t.Fatalf("nested waits: outer=%+v middle=%+v gate=%+v\n%s", outer, middle, child, log.String())
	}
	if child.Parent != middle.Run || middle.Parent != outer.Run || outer.WallS-outer.WaitS >= outer.WallS-0.1 {
		t.Fatalf("nested runs or without waits wrong: outer=%+v middle=%+v gate=%+v", outer, middle, child)
	}
}

func TestParallelNestedGraftWaitUnion(t *testing.T) {
	script := graftBin + " work & " + graftBin + " work & wait"
	config := "schema: 1\nversion: {mode: none}\ntasks:\n" +
		"  outer:\n    run:\n      - sh: " + yamlQuote(script) + "\n" +
		"  work:\n    heavy: true\n    run: [" + helperStep("ok") + "]\n"
	f := newRepo(t, map[string]string{".graft.yaml": config})
	state := t.TempDir()
	cache, proc, cfg := filepath.Join(state, "cache"), filepath.Join(state, "proc"), filepath.Join(state, "config")
	writeFile(t, filepath.Join(cfg, "graft", "config.yaml"), "load: {poll: 20ms, settle: 10ms}\n")
	writeTestPressure(t, cfg)
	f.env = append(f.env, "XDG_CACHE_HOME="+cache, "XDG_CONFIG_HOME="+cfg, fakeMachine(t, proc, 30))
	cmd, log := f.start(f.work, "outer")
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.After(15 * time.Second)
	for strings.Count(log.String(), "waiting") < 2 {
		select {
		case err := <-done:
			t.Fatalf("outer ended before both children waited: %v\n%s", err, log.String())
		case <-deadline:
			stopTestChild(cmd)
			<-done
			t.Fatalf("children did not both wait:\n%s", log.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
	setRunnable(t, proc, 1)
	if err := <-done; err != nil {
		t.Fatalf("outer: %v\n%s", err, log.String())
	}
	var outer statsLine
	var children []statsLine
	for _, rec := range readStats(t, filepath.Join(cache, "graft", "stats.jsonl")) {
		if rec.Kind == "task" && rec.Task == "outer" {
			outer = rec
		} else if rec.Kind == "task" && rec.Task == "work" {
			children = append(children, rec)
		}
	}
	if len(children) != 2 {
		t.Fatalf("expected two children, got %+v\n%s", children, log.String())
	}
	first, second := children[0], children[1]
	if first.Parent != outer.Run || second.Parent != outer.Run ||
		outer.WaitS < max(first.WaitS, second.WaitS)-0.1 ||
		outer.WaitS > max(first.WaitS, second.WaitS)+1 ||
		first.WaitS+second.WaitS-outer.WaitS < 1 {
		t.Fatalf("overlapping waits: outer=%+v first=%+v second=%+v\n%s", outer, first, second, log.String())
	}
}
