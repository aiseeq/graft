package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCachePaths: cache.paths keys a task on part of the tree: the
// union of its own paths and those of its deps and inner tasks.
func TestCachePaths(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.txt")
	config := `cache_exclude: [.graft.yaml]
tasks:
  shell:
    cache:
      paths: ['**/*.sh', .shellcheckrc]
    deps: [prep]
    run:
      - ` + helperStep("append", out, "shell") + `
      - task: inner
  prep:
    cache:
      paths: ['lib/**']
    run: [` + helperStep("append", out, "prep") + `]
  inner:
    cache:
      paths: [tools.cfg]
    run: [` + helperStep("append", out, "inner") + `]
  whole:
    cache: tree
    run: [` + helperStep("append", out, "whole") + `]
`
	f, _ := statsRepo(t, config)
	f.write("scripts/a.sh", "echo a\n")
	f.write("lib/x.txt", "x\n")
	f.write("tools.cfg", "t\n")
	f.write("main.go", "package main\n")
	f.git("add", "-A")
	f.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "init")

	lines := func() int {
		data, err := os.ReadFile(out)
		if err != nil {
			return 0
		}
		return strings.Count(string(data), "\n")
	}
	expect := func(what, task string, ran bool) {
		t.Helper()
		before := lines()
		got, code := f.graft("", task)
		if code != 0 {
			t.Fatalf("%s: exit %d\n%s", what, code, got)
		}
		skipped := strings.Contains(got, "graft: "+task+": passed on ")
		if ran == skipped || ran != (lines() > before) {
			t.Errorf("%s: %s: want ran=%v, lines %d -> %d\n%s", what, task, ran, before, lines(), got)
		}
	}
	expect("first", "shell", true)
	expect("first", "whole", true)
	expect("unchanged", "shell", false)

	f.write("main.go", "package main\n\nfunc main() {}\n")
	expect("a file outside the paths", "shell", false)
	expect("the same file for a tree task", "whole", true)

	f.write("scripts/a.sh", "echo a2\n")
	expect("a file under its own paths", "shell", true)
	expect("after it", "shell", false)

	f.write("scripts/new.sh", "echo n\n")
	expect("an untracked file under the paths", "shell", true)

	f.write(".shellcheckrc", "disable=SC1090\n")
	expect("a literal path", "shell", true)

	f.write("lib/x.txt", "x2\n")
	expect("a file under a dep's paths", "shell", true)

	f.write("tools.cfg", "t2\n")
	expect("a file under an inner task's paths", "shell", true)

	if err := os.Remove(filepath.Join(f.work, "scripts", "new.sh")); err != nil {
		t.Fatal(err)
	}
	expect("a removed file under the paths", "shell", true)
	expect("unchanged again", "shell", false)
}

// TestCachePathsNeedThemOnEveryDep: a dep or inner task whose inputs
// are the whole tree cannot be narrowed by its caller's paths.
func TestCachePathsNeedThemOnEveryDep(t *testing.T) {
	for name, config := range map[string]string{
		"dep without cache":    "tasks:\n  shell:\n    cache: {paths: ['**/*.sh']}\n    deps: [plain]\n    run: [[true]]\n  plain:\n    run: [[true]]\n",
		"dep with cache: tree": "tasks:\n  shell:\n    cache: {paths: ['**/*.sh']}\n    deps: [plain]\n    run: [[true]]\n  plain:\n    cache: tree\n    run: [[true]]\n",
		"inner without paths":  "tasks:\n  shell:\n    cache: {paths: ['**/*.sh']}\n    run: [{task: plain}]\n  plain:\n    run: [[true]]\n",
		"empty paths":          "tasks:\n  shell:\n    cache: {paths: []}\n    run: [[true]]\n",
		"bad glob":             "tasks:\n  shell:\n    cache: {paths: ['../x.sh']}\n    run: [[true]]\n",
	} {
		f := tasksRepo(t, config, nil)
		if out, code := f.graft("", "tasks"); code == 0 || !strings.Contains(out, "tasks.shell.cache") {
			t.Errorf("%s: want a config error naming tasks.shell.cache, exit %d\n%s", name, code, out)
		}
	}
}
