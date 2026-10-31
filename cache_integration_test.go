package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCacheMissNamesTheChangedPart(t *testing.T) {
	input := filepath.Join(t.TempDir(), "input")
	writeFile(t, input, "one\n")
	f, statsFile := statsRepo(t, "tasks:\n  verify:\n    cache:\n      files: ["+yamlQuote(input)+"]\n    run: ["+helperStep("ok")+"]\n")
	if _, code := f.graft("", "verify"); code != 0 {
		t.Fatal("first run failed")
	}
	writeFile(t, input, "two\n")
	if _, code := f.graft("", "verify"); code != 0 {
		t.Fatal("second run failed")
	}
	for _, line := range readStats(t, statsFile) {
		if line.Kind == "cache-miss" && line.Task == "verify" {
			if !slices.Contains(line.CacheChanged, "inputs") {
				t.Fatalf("cache miss did not name inputs: %+v", line)
			}
			return
		}
	}
	t.Fatal("no cache-miss stats record")
}

// cacheConfig is a task with cache, a dep, an inner task and inputs from
// outside the tree. .graft.yaml itself is excluded from the tree, so a
// change to a step, dep or inner task must count through the definition.
func cacheConfig(ext, cmdOut, out, check, prep, inner string) string {
	return `cache_exclude: ['docs/**', .graft.yaml]
dotenv: .env
tasks:
  verify:
    cache:
      env: [CACHE_ENV]
      files: [` + yamlQuote(ext) + `]
      commands: [[` + yamlQuote(helperBin) + `, cat, ` + yamlQuote(cmdOut) + `]]
    deps: [prep]
    env: {MODE: '${MODE_SRC}'}
    dotenv: [DOTKEY]
    run:
      - ` + helperStep("append", out, check) + `
      - task: inner
  prep:
    run: [` + helperStep("append", out, prep) + `]
  inner:
    run: [` + helperStep("append", out, inner) + `]
  flaky:
    cache: tree
    run: [` + helperStep("failif", ext) + `]
`
}

// TestTaskCacheSkipsOnlyAnUnchangedRun: a task with cache that passed is
// skipped, deps and inner tasks with it, while the tree, the definition and
// the declared inputs are what they were; a change to any of them runs it.
func TestTaskCacheSkipsOnlyAnUnchangedRun(t *testing.T) {
	state := t.TempDir()
	ext, cmdOut, out := filepath.Join(state, "ext.txt"), filepath.Join(state, "cmd.txt"), filepath.Join(state, "out.txt")
	writeFile(t, ext, "ext 1\n")
	writeFile(t, cmdOut, "tool 1\n")
	f, statsFile := statsRepo(t, cacheConfig(ext, cmdOut, out, "check", "prep", "inner"))
	f.write(".gitignore", ".env\n")
	f.write(".env", "DOTKEY=one\n")
	f.write("a.txt", "a\n")
	f.env = append(f.env, "CACHE_ENV=e1", "MODE_SRC=m1")
	f.git("add", "-A")
	f.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "init")

	runs := 0
	expect := func(what string, ran bool, args ...string) {
		t.Helper()
		got, code := f.graft("", append([]string{"verify"}, args...)...)
		if code != 0 {
			t.Fatalf("%s: exit %d\n%s", what, code, got)
		}
		lines := 0
		if data, err := os.ReadFile(out); err == nil {
			lines = strings.Count(string(data), "\n")
		}
		switch {
		case ran && lines != runs+3:
			t.Errorf("%s: want a run of check, prep and inner, %d lines after %d\n%s", what, lines, runs, got)
		case !ran && (lines != runs || !strings.Contains(got, "graft: verify: passed on this tree ")):
			t.Errorf("%s: want a skip, %d lines after %d\n%s", what, lines, runs, got)
		}
		runs = lines
	}
	expect("first run", true)
	expect("nothing changed", false)

	f.write("a.txt", "a2\n")
	expect("a tracked file", true)
	expect("after it", false)
	f.write("b.txt", "b\n")
	expect("an untracked file", true)
	f.write("docs/guide.md", "g\n")
	expect("an excluded path", false)
	f.write(".env", "DOTKEY=two\n")
	expect("a .env value the task gets", true)
	f.write(".env", "DOTKEY=two\nOTHER=x\n")
	expect("a .env value it does not get", false)
	f.env = append(f.env, "MODE_SRC=m2")
	expect("a value of the task's env", true)
	f.env = append(f.env, "CACHE_ENV=e2")
	expect("cache.env", true)
	writeFile(t, ext, "ext 2\n")
	expect("cache.files", true)
	writeFile(t, cmdOut, "tool 2\n")
	expect("cache.commands output", true)
	f.write(".graft.yaml", "schema: 1\nversion:\n  mode: git-tag\n"+cacheConfig(ext, cmdOut, out, "check2", "prep", "inner"))
	expect("a step", true)
	f.write(".graft.yaml", "schema: 1\nversion:\n  mode: git-tag\n"+cacheConfig(ext, cmdOut, out, "check2", "prep2", "inner"))
	expect("a dep", true)
	f.write(".graft.yaml", "schema: 1\nversion:\n  mode: git-tag\n"+cacheConfig(ext, cmdOut, out, "check2", "prep2", "inner2"))
	expect("an inner task", true)
	expect("unchanged again", false)
	expect("--no-cache", true, "--no-cache")

	// A failure is not kept: the task runs until it passes.
	if _, code := f.graft("", "flaky"); code == 0 {
		t.Fatal("flaky passed with the file present")
	}
	if got, code := f.graft("", "flaky"); code == 0 || strings.Contains(got, "passed on this tree") {
		t.Errorf("a failed run was kept:\n%s", got)
	}

	// The skips are in the stats with the time they saved.
	var hits int
	for _, l := range readStats(t, statsFile) {
		if l.Kind == "cache-hit" {
			hits++
			if l.Task != "verify" || l.WallS <= 0 {
				t.Errorf("cache-hit line: %+v", l)
			}
		}
	}
	if hits != 5 {
		t.Errorf("want 5 cache-hit lines, got %d", hits)
	}
	if report := f.mustGraft("stats"); !strings.Contains(report, "SAVED") || !strings.Contains(report, "5 runs skipped by the cache") {
		t.Errorf("graft stats:\n%s", report)
	}
}

// TestTaskCacheIsSharedByWorktrees: a worktree holding the same content
// skips what another one passed.
func TestTaskCacheIsSharedByWorktrees(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.txt")
	f, _ := statsRepo(t, "tasks:\n  verify:\n    cache: tree\n    run: ["+helperStep("append", out, "check")+"]\n")
	f.write("a.txt", "a\n")
	f.git("add", "-A")
	f.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "init")
	f.mustGraft("verify")
	other := filepath.Join(f.base, "other")
	f.git("worktree", "add", "-q", "--detach", other)
	got, code := f.exec(other, "", graftBin, "verify")
	if code != 0 || !strings.Contains(got, "passed on this tree") {
		t.Errorf("the other worktree: exit %d\n%s", code, got)
	}
	if data, _ := os.ReadFile(out); string(data) != "check\n" {
		t.Errorf("runs: %q", data)
	}
}

// TestTaskCacheRefusesSideEffects: a task that deploys, releases, pushes or
// acts on services must run every time.
func TestTaskCacheRefusesSideEffects(t *testing.T) {
	for _, step := range []string{"[graft, deploy, prod]", "[git, push]", "[docker, push, app]", "{sh: 'graft release'}"} {
		f := tasksRepo(t, "tasks:\n  ship:\n    cache: tree\n    run: ["+step+"]\n", nil)
		if out, code := f.graft("", "tasks"); code == 0 || !strings.Contains(out, "tasks.ship.cache") {
			t.Errorf("%s: exit %d\n%s", step, code, out)
		}
	}
}

// TestTaskCacheInTheGate: the gate of a commit skips a check that passed on
// the tree being committed; a task that reads the commit message runs for a
// new message.
func TestTaskCacheInTheGate(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.txt")
	f, _ := statsRepo(t, "cache_exclude: [notes.txt]\ngate:\n  - task: verify\n  - task: message\ntasks:\n"+
		"  verify:\n    cache: tree\n    run: ["+helperStep("append", out, "verify")+"]\n"+
		"  message:\n    cache: tree\n    run: ["+helperStep("copyenv", "GRAFT_COMMIT_MESSAGE_FILE", out+".msg")+"]\n")
	f.write("a.txt", "a\n")
	f.mustGraft("verify")
	got := f.mustGraft("commit", "-m", "feat: one")
	if !strings.Contains(got, "graft: verify: passed on this tree") || strings.Contains(got, "graft: message: passed") {
		t.Errorf("first commit:\n%s", got)
	}
	f.write("notes.txt", "n\n")
	got = f.mustGraft("commit", "-m", "feat: two")
	if !strings.Contains(got, "graft: verify: passed on this tree") || strings.Contains(got, "graft: message: passed") {
		t.Errorf("second commit, a new message on the same tree:\n%s", got)
	}
	if data, _ := os.ReadFile(out); string(data) != "verify\n" {
		t.Errorf("verify ran %q", data)
	}
	if data, _ := os.ReadFile(out + ".msg"); !strings.Contains(string(data), "feat: two") {
		t.Errorf("message saw %q", data)
	}
}
