package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// linkRepo is a repository on main whose .graft.yaml links two groups of
// untracked dependency directories into new worktrees: deps and sub/deps by
// deps.lock, e2e/deps by e2e/deps.lock, both installed by the task install,
// which appends a line to installs.log in the tree it runs in. The main
// worktree has the directories, a file in each.
func linkRepo(t *testing.T) (f *fixture, cache string) {
	t.Helper()
	f = newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\nworktree:\n  link:\n" +
			"    - key: [deps.lock]\n      paths: [deps, sub/deps]\n      setup: install\n" +
			"    - key: [e2e/deps.lock]\n      paths: [e2e/deps]\n      setup: install\n" +
			"tasks:\n  install:\n    run: [" + helperStep("append", "installs.log", "installed") + "]\n",
		".gitignore":    "deps/\nsub/deps/\ne2e/deps/\ninstalls.log\n",
		"deps.lock":     "a 1\n",
		"e2e/deps.lock": "e 1\n",
	}, "origin")
	f.git("checkout", "-q", "-b", "main")
	f.mustPlainCommit("feat: base")
	f.git("push", "-q", "-u", "origin", "HEAD")
	f.git("remote", "set-head", "origin", "main")
	for _, d := range []string{"deps", "sub/deps", "e2e/deps"} {
		writeFile(t, filepath.Join(f.work, filepath.FromSlash(d), "mod.txt"), d+"\n")
	}
	cache = t.TempDir()
	f.env = append(f.env, "XDG_CACHE_HOME="+cache)
	return f, cache
}

// linksToMain tells whether path in the worktree is a link to the same path
// of the main worktree.
func linksToMain(t *testing.T, f *fixture, wt, path string) bool {
	t.Helper()
	p := filepath.Join(wt, filepath.FromSlash(path))
	info, err := os.Lstat(p)
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeSymlink == 0 && runtime.GOOS != "windows" {
		return false
	}
	got, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(f.work, filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	return got == want && got != p
}

func installs(t *testing.T, dir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "installs.log"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "installed")
}

// TestWorktreeLinkSameKey: a new worktree whose key files match the main
// worktree's gets every path of the group as a link to the main worktree's
// directory, and nothing is installed.
func TestWorktreeLinkSameKey(t *testing.T) {
	f, cache := linkRepo(t)
	out := f.mustGraft("worktree", "add", "w/same")
	wt := filepath.Join(cache, "worktrees", "work", "w-same")
	for _, p := range []string{"deps", "sub/deps", "e2e/deps"} {
		if !linksToMain(t, f, wt, p) {
			t.Errorf("%s is not a link to the main worktree's:\n%s", p, out)
		}
	}
	if got := readFile(t, filepath.Join(wt, "sub", "deps", "mod.txt")); got != "sub/deps\n" {
		t.Errorf("sub/deps/mod.txt through the link = %q", got)
	}
	if n := installs(t, wt); n != 0 {
		t.Errorf("install ran %d times with the keys the same:\n%s", n, out)
	}
	if !strings.Contains(out, "linked deps") {
		t.Errorf("add does not say what it linked:\n%s", out)
	}
}

// TestWorktreeLinkKeyDiffers: a group whose key differs from the main
// worktree's is not linked and its setup task runs instead, once for every
// group that names it; a group whose key matches is still linked.
func TestWorktreeLinkKeyDiffers(t *testing.T) {
	f, cache := linkRepo(t)
	f.git("checkout", "-q", "-b", "w/both")
	writeFile(t, filepath.Join(f.work, "deps.lock"), "a 2\n")
	writeFile(t, filepath.Join(f.work, "e2e", "deps.lock"), "e 2\n")
	f.mustPlainCommit("feat: new deps")
	f.git("checkout", "-q", "-b", "w/one", "main")
	writeFile(t, filepath.Join(f.work, "e2e", "deps.lock"), "e 3\n")
	f.mustPlainCommit("feat: new e2e deps")
	f.git("checkout", "-q", "main")

	out := f.mustGraft("worktree", "add", "w/both")
	wt := filepath.Join(cache, "worktrees", "work", "w-both")
	for _, p := range []string{"deps", "sub/deps", "e2e/deps"} {
		if linksToMain(t, f, wt, p) {
			t.Errorf("%s is linked though its key differs:\n%s", p, out)
		}
	}
	if n := installs(t, wt); n != 1 {
		t.Errorf("install ran %d times for two groups that name it, want once:\n%s", n, out)
	}
	if n := installs(t, f.work); n != 0 {
		t.Errorf("install ran in the main worktree:\n%s", out)
	}

	out = f.mustGraft("worktree", "add", "w/one")
	wt = filepath.Join(cache, "worktrees", "work", "w-one")
	if !linksToMain(t, f, wt, "deps") || !linksToMain(t, f, wt, "sub/deps") {
		t.Errorf("the group with the same key is not linked:\n%s", out)
	}
	if linksToMain(t, f, wt, "e2e/deps") {
		t.Errorf("e2e/deps is linked though its key differs:\n%s", out)
	}
	if n := installs(t, wt); n != 1 {
		t.Errorf("install ran %d times, want once:\n%s", n, out)
	}
}

// TestWorktreeLinkMissingInMain: a path the main worktree does not have is
// not linked; its group's setup runs.
func TestWorktreeLinkMissingInMain(t *testing.T) {
	f, cache := linkRepo(t)
	if err := os.RemoveAll(filepath.Join(f.work, "e2e", "deps")); err != nil {
		t.Fatal(err)
	}
	out := f.mustGraft("worktree", "add", "w/nomain")
	wt := filepath.Join(cache, "worktrees", "work", "w-nomain")
	if linksToMain(t, f, wt, "e2e/deps") {
		t.Errorf("e2e/deps linked to nothing:\n%s", out)
	}
	if n := installs(t, wt); n != 1 {
		t.Errorf("install ran %d times, want once:\n%s", n, out)
	}
}

// TestWorktreeLinkSetupUnlinksFirst: the setup task of a group, run in a
// worktree where the group's paths are links, removes the links first, so
// it installs into the worktree and never into the main worktree's
// directories.
func TestWorktreeLinkSetupUnlinksFirst(t *testing.T) {
	f, cache := linkRepo(t)
	f.mustGraft("worktree", "add", "w/later")
	wt := filepath.Join(cache, "worktrees", "work", "w-later")
	if out, code := f.exec(wt, "", graftBin, "install"); code != 0 {
		t.Fatalf("install: exit %d\n%s", code, out)
	}
	for _, p := range []string{"deps", "sub/deps", "e2e/deps"} {
		if linksToMain(t, f, wt, p) {
			t.Errorf("%s is still a link after its setup task ran", p)
		}
	}
	if got := readFile(t, filepath.Join(f.work, "deps", "mod.txt")); got != "deps\n" {
		t.Errorf("the main worktree's deps changed: %q", got)
	}
}

// TestWorktreeLinkGateWarnsOfAStaleLink: a branch that changes a key file
// after the worktree linked the group gets a warning from the gate that
// names the path, the key and the setup task, and the gate still runs.
func TestWorktreeLinkGateWarnsOfAStaleLink(t *testing.T) {
	f, cache := linkRepo(t)
	f.mustGraft("worktree", "add", "w/stale")
	wt := filepath.Join(cache, "worktrees", "work", "w-stale")
	writeFile(t, filepath.Join(wt, "deps.lock"), "a 9\n")
	out, code := f.exec(wt, "", graftBin, "gate")
	if code != 0 {
		t.Fatalf("gate: exit %d\n%s", code, out)
	}
	for _, want := range []string{"worktree.link deps", "deps.lock", "graft install"} {
		if !strings.Contains(out, want) {
			t.Errorf("the gate's warning lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "e2e/deps") {
		t.Errorf("a group whose key is the same is warned about:\n%s", out)
	}

	// In the main worktree nothing is a link: no warning.
	out, code = f.exec(f.work, "", graftBin, "gate")
	if code != 0 || strings.Contains(out, "worktree.link") {
		t.Errorf("gate in the main worktree: exit %d\n%s", code, out)
	}
}

// TestWorktreeLinkConfigErrors: a group without paths, key or setup, a path
// the checkout tracks, and a setup that is not a task are errors.
func TestWorktreeLinkConfigErrors(t *testing.T) {
	for _, c := range []struct{ name, link, want string }{
		{"no paths", "    - key: [deps.lock]\n      setup: install\n", "worktree.link"},
		{"no key", "    - paths: [deps]\n      setup: install\n", "worktree.link"},
		{"no setup", "    - key: [deps.lock]\n      paths: [deps]\n", "worktree.link"},
		{"unknown setup", "    - key: [deps.lock]\n      paths: [deps]\n      setup: nope\n", "nope"},
		{"tracked path", "    - key: [deps.lock]\n      paths: [deps.lock]\n      setup: install\n", "deps.lock"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, _ := linkRepo(t)
			writeFile(t, filepath.Join(f.work, ".graft.yaml"), "schema: 1\nversion: {mode: none}\nworktree:\n  link:\n"+c.link+
				"tasks:\n  install:\n    run: ["+helperStep("append", "installs.log", "installed")+"]\n")
			f.mustPlainCommit("chore: link")
			if out, code := f.graft("", "worktree", "add", "w/bad", "main"); code == 0 || !strings.Contains(out, c.want) {
				t.Errorf("exit %d, want an error naming %q:\n%s", code, c.want, out)
			}
		})
	}
}
