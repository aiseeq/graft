package main

import (
	"path/filepath"
	"runtime"
	"testing"
)

// TestWorktreeLinkSharedSetupNeverWritesIntoMain: a setup task named by two
// groups, run by worktree add for the group whose key differs, or as a
// dependency of another task, first removes the links of the group whose
// key matched, so what it installs lands in the new worktree, not in the
// main worktree's directories.
func TestWorktreeLinkSharedSetupNeverWritesIntoMain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the install step is sh")
	}
	f, cache := linkRepo(t)
	f.git("checkout", "-q", "-b", "w/one")
	writeFile(t, filepath.Join(f.work, ".graft.yaml"), "schema: 1\nversion: {mode: none}\nworktree:\n  link:\n"+
		"    - key: [deps.lock]\n      paths: [deps, sub/deps]\n      setup: install\n"+
		"    - key: [e2e/deps.lock]\n      paths: [e2e/deps]\n      setup: install\n"+
		"tasks:\n  install:\n    run: [[sh, -c, 'for d in deps sub/deps e2e/deps; do mkdir -p $d && echo new > $d/mod.txt; done']]\n"+
		"  verify:\n    deps: [install]\n    run: [[sh, -c, 'true']]\n")
	writeFile(t, filepath.Join(f.work, "e2e", "deps.lock"), "e 3\n")
	f.mustPlainCommit("feat: new e2e deps")
	f.git("checkout", "-q", "main")
	writeFile(t, filepath.Join(f.work, ".graft.yaml"), readFile(t, filepath.Join(f.work, ".graft.yaml")))

	out := f.mustGraft("worktree", "add", "w/one")
	wt := filepath.Join(cache, "worktrees", "work", "w-one")
	for _, d := range []string{"deps", "sub/deps", "e2e/deps"} {
		if got := readFile(t, filepath.Join(f.work, filepath.FromSlash(d), "mod.txt")); got != d+"\n" {
			t.Errorf("the main worktree's %s changed: %q\n%s", d, got, out)
		}
		if linksToMain(t, f, wt, d) {
			t.Errorf("%s is still a link after install ran:\n%s", d, out)
		}
	}

	// A setup run as another task's dependency unlinks too.
	f.git("checkout", "-q", "-b", "w/dep", "main")
	f.git("checkout", "-q", "main")
	writeFile(t, filepath.Join(f.work, ".graft.yaml"), readFile(t, filepath.Join(wt, ".graft.yaml")))
	f.mustPlainCommit("chore: install task")
	f.git("branch", "-q", "-f", "w/dep", "main")
	f.mustGraft("worktree", "add", "w/dep")
	wt = filepath.Join(cache, "worktrees", "work", "w-dep")
	if !linksToMain(t, f, wt, "deps") {
		t.Fatalf("deps is not linked with the keys the same")
	}
	if out, code := f.exec(wt, "", graftBin, "verify"); code != 0 {
		t.Fatalf("verify: exit %d\n%s", code, out)
	}
	if linksToMain(t, f, wt, "deps") || readFile(t, filepath.Join(f.work, "deps", "mod.txt")) != "deps\n" {
		t.Errorf("install as a dependency wrote through the link")
	}
}
