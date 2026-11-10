package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/app"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/lock"
)

// TestWorktreeClean: a worktree goes, with its branch, only when no process
// works in it and merging it into the trunk, its uncommitted changes and
// untracked files included, gives the trunk's tree, the version files aside:
// content, not ancestry. Uncommitted changes the trunk lacks keep it, named;
// so do changes made less than --min-age ago. A worktree whose commit
// subjects are all in the trunk but whose content is not is shown apart, for
// a look; branches without a worktree go the same way.
func TestWorktreeClean(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process check reads /proc")
	}
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n",
		"VERSION":     "1.0.0\n",
		"shared.txt":  "one\n",
		"doc.txt":     "a\n",
	}, "origin")
	f.git("checkout", "-q", "-b", "main")
	commitSetup := func(dir, file, content, msg string) {
		t.Helper()
		if file != "" {
			writeFile(t, filepath.Join(dir, file), content)
		}
		f.mustVersionedCommit(dir, msg, false)
	}
	commitSetup(f.work, "", "", "feat: base")
	f.git("remote", "set-head", "origin", "main")

	wt := func(name string) string { return filepath.Join(f.base, "wt-"+name) }
	add := func(name string) string {
		f.git("worktree", "add", "-q", "-b", "w/"+name, wt(name), "main")
		return wt(name)
	}
	commitIn := commitSetup

	// Merged by content under another hash and another version.
	commitIn(add("merged"), "merged.txt", "m\n", "feat: merged work")
	// Not merged.
	commitIn(add("open"), "open.txt", "o\n", "feat: open work")
	// Uncommitted changes, one of them the trunk lacks.
	dirty := add("dirty")
	writeFile(t, filepath.Join(dirty, "scratch.txt"), "s\n")
	writeFile(t, filepath.Join(dirty, "doc.txt"), "b\n")
	// Uncommitted changes, a tracked file and an untracked one, that other
	// commits merged into the trunk.
	landed := add("landed")
	writeFile(t, filepath.Join(landed, "doc.txt"), "b\n")
	writeFile(t, filepath.Join(landed, "other.txt"), "three\n")
	// Merged, but a process works in it; another holds a file of one open
	// from elsewhere; a third waits between its runs and locked it.
	busy := add("busy")
	held := add("held")
	locked := add("locked")
	f.git("worktree", "lock", "--reason", "job 7 runs here until it is done", locked)
	// A commit whose subject the trunk has, with other content.
	commitIn(add("look"), "shared.txt", "two\n", "feat: shared change")
	// Branches without a worktree.
	f.git("branch", "b/old", "main")
	f.git("branch", "b/new", "main")
	f.git("switch", "-q", "b/new")
	commitSetup(f.work, "new.txt", "n\n", "feat: new work")
	f.git("switch", "-q", "main")
	commitSetup(f.work, "merged.txt", "m\n", "feat: merged work, rewritten")
	commitSetup(f.work, "other.txt", "three\n", "feat: shared change")
	commitSetup(f.work, "doc.txt", "b\n", "docs: b")

	sleep := exec.Command("sleep", "60")
	sleep.Dir = busy
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sleep.Process.Kill(); _ = sleep.Wait() })
	writeFile(t, filepath.Join(held, "notes.txt"), "n\n")
	f.git("-C", held, "add", "notes.txt")
	f.git("-C", held, "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "feat: notes")
	commitSetup(f.work, "notes.txt", "n\n", "feat: notes")
	reader := exec.Command("sh", "-c", "exec 3<notes.txt; cd / && exec sleep 60 3<&3")
	reader.Dir = held
	if err := reader.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Process.Kill(); _ = reader.Wait() })

	// Fresh worktrees wait an hour by default, and so do fresh changes in
	// one whose HEAD is old.
	old := time.Now().Add(-2 * time.Hour)
	gitDir := strings.TrimSpace(f.git("-C", landed, "rev-parse", "--path-format=absolute", "--git-dir"))
	for _, p := range []string{filepath.Join(gitDir, "HEAD"), filepath.Join(gitDir, "logs", "HEAD")} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	out := f.mustGraft("worktree", "clean")
	for _, want := range []string{
		"in work   " + wt("merged") + " (w/merged): used less than 1h0m0s ago",
		"in work   " + landed + " (w/landed): 2 uncommitted changes, changed less than 1h0m0s ago",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fresh work: no %q:\n%s", want, out)
		}
	}

	out = f.mustGraft("worktree", "clean", "--min-age", "0s")
	for _, want := range []string{
		"would remove " + wt("merged") + " (w/merged): clean, in origin/main",
		"unmerged  " + wt("open") + " (w/open): adds +1 -0 lines to origin/main",
		"in work   " + dirty + " (w/dirty): 2 uncommitted changes, not in origin/main: scratch.txt",
		"would remove " + landed + " (w/landed): 2 uncommitted changes, all in origin/main",
		"in work   " + busy + " (w/busy): a process works in it (sleep, pid ",
		"in work   " + held + " (w/held): a process works in it (sleep, pid ",
		"in work   " + locked + " (w/locked): locked: job 7 runs here until it is done (git worktree unlock to let it go)",
		"check     " + wt("look") + " (w/look): its commits are all in origin/main by subject, but it adds +1 -1 lines to origin/main: looks merged, check",
		"would remove branch b/old: in origin/main",
		"unmerged  branch b/new: adds +1 -0 lines to origin/main",
		"dry run: graft worktree clean --apply removes 3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run: no %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(wt("merged")); err != nil {
		t.Errorf("the dry run removed a worktree: %v", err)
	}

	out = f.mustGraft("worktree", "clean", "--min-age", "0s", "--apply")
	for _, want := range []string{"removed   " + wt("merged") + " (w/merged)", "removed   " + landed + " (w/landed)", "removed   branch b/old"} {
		if !strings.Contains(out, want) {
			t.Errorf("apply: no %q:\n%s", want, out)
		}
	}
	for _, gone := range []string{wt("merged"), landed} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s is still there: %v", gone, err)
		}
	}
	for _, branch := range []string{"w/merged", "w/landed", "b/old"} {
		if _, code := f.tryGit("rev-parse", "--verify", "-q", "refs/heads/"+branch); code == 0 {
			t.Errorf("branch %s is still there", branch)
		}
	}
	if _, code := f.tryGit("ls-remote", "--exit-code", "origin", "refs/heads/w/merged"); code == 0 {
		t.Error("w/merged is still on origin")
	}
	for _, kept := range []string{wt("open"), dirty, busy, held, locked, wt("look")} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s is gone: %v", kept, err)
		}
	}
}

func TestWorktreeCleanIgnoredData(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process check reads /proc")
	}
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\nworktree:\n  disposable: [rebuild]\n",
		".gitignore":  "out/\nrebuild/\nnode_modules/\nlink-data\n",
		"VERSION":     "1.0.0\n",
		"tracked.txt": "before trunk\n",
	}, "origin")
	f.git("checkout", "-q", "-b", "main")
	f.mustVersionedCommit(f.work, "feat: base", false)
	f.git("remote", "set-head", "origin", "main")
	add := func(name string) string {
		t.Helper()
		path := filepath.Join(f.base, "wt-"+name)
		f.git("worktree", "add", "-q", "-b", "w/"+name, path, "main")
		return path
	}
	data := add("data")
	writeFile(t, filepath.Join(data, "out", "notes.txt"), "keep\n")
	writeFile(t, filepath.Join(data, "rebuild", "cache.bin"), "rebuild\n")
	writeFile(t, filepath.Join(data, "node_modules", "package", "file.js"), "cache\n")
	if err := os.Symlink("out/notes.txt", filepath.Join(data, "link-data")); err != nil {
		t.Fatal(err)
	}
	rebuild := add("rebuild")
	writeFile(t, filepath.Join(rebuild, "rebuild", "cache.bin"), "rebuild\n")
	writeFile(t, filepath.Join(rebuild, "node_modules", "package", "file.js"), "cache\n")
	landed := add("landed")
	commit := func(file, body, message string) {
		t.Helper()
		writeFile(t, filepath.Join(f.work, file), body)
		f.mustVersionedCommit(f.work, message, false)
	}
	commit("tracked.txt", "in trunk\n", "feat: tracked change")
	writeFile(t, filepath.Join(landed, "tracked.txt"), "in trunk\n")
	writeFile(t, filepath.Join(landed, "out", "notes.txt"), "landed data\n")
	if ignored := f.git("-C", landed, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z"); !strings.Contains(ignored, "out/") {
		t.Fatalf("landed ignored paths = %q", ignored)
	}
	out := f.mustGraft("worktree", "clean", "--min-age", "0s")
	for _, want := range []string{
		"check     " + data + " (w/data): clean, in origin/main, but ignored data would go with it: out/ (1 file, 5 B); move it, or name it in worktree.disposable",
		"would remove " + rebuild + " (w/rebuild): clean, in origin/main",
		"check     " + landed + " (w/landed): 1 uncommitted change, all in origin/main, but ignored data would go with it: out/ (1 file, 12 B); move it, or name it in worktree.disposable",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run: no %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "link-data") {
		t.Errorf("symlink counted as ignored file data:\n%s", out)
	}
	out = f.mustGraft("worktree", "clean", "--min-age", "0s", "--apply")
	for _, kept := range []string{data, landed} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("protected worktree %s is gone: %v", kept, err)
		}
	}
	if _, err := os.Stat(rebuild); !os.IsNotExist(err) {
		t.Errorf("rebuildable worktree remains: %v\n%s", err, out)
	}
}

func newWorktreeCleanFixture(t *testing.T, files map[string]string) (*fixture, func(string) string) {
	t.Helper()
	f := newRepo(t, files, "origin")
	f.git("checkout", "-q", "-b", "main")
	f.mustVersionedCommit(f.work, "feat: base", false)
	f.git("remote", "set-head", "origin", "main")
	add := func(name string) string {
		t.Helper()
		path := filepath.Join(f.base, "wt-"+name)
		f.git("worktree", "add", "-q", "-b", "w/"+name, path, "main")
		return path
	}
	return f, add
}

func TestWorktreeCleanNoConfigInTrunk(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process check reads /proc")
	}
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n",
		".gitignore":  "out/\n",
		"VERSION":     "1.0.0\n",
	})
	f.git("rm", "-q", ".graft.yaml")
	f.mustVersionedCommit(f.work, "chore: remove config", false)
	writeFile(t, filepath.Join(f.work, ".graft.yaml"), "schema: 1\nversion: {mode: file}\nworktree: {disposable: [out]}\n")
	path := add("no-config")
	writeFile(t, filepath.Join(path, "out", "keep.txt"), "data\n")
	out, code := f.graft("", "worktree", "clean", "--min-age", "0s")
	if code != 0 || !strings.Contains(out, "check     "+path) || !strings.Contains(out, "out/") || !strings.Contains(out, "to check 1") {
		t.Fatalf("exit %d; trunk without config must ignore branch rules:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(path, "out", "keep.txt")); err != nil {
		t.Fatalf("ignored data is gone: %v", err)
	}
}

func TestWorktreeCleanSubmodule(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process check reads /proc")
	}
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n",
		"VERSION":     "1.0.0\n",
	}, "origin")
	f.git("checkout", "-q", "-b", "main")
	sub := filepath.Join(f.base, "sub-source")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f.run(sub, "git", "init", "-q")
	f.run(sub, "git", "config", "user.name", "Test")
	f.run(sub, "git", "config", "user.email", "test@example.invalid")
	writeFile(t, filepath.Join(sub, "base.txt"), "base\n")
	f.run(sub, "git", "add", "base.txt")
	f.run(sub, "git", "commit", "-q", "-m", "base")
	f.git("-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sm")
	f.mustVersionedCommit(f.work, "feat: submodule", false)
	f.git("remote", "set-head", "origin", "main")
	initialized := filepath.Join(f.base, "wt-initialized")
	f.git("worktree", "add", "-q", "-b", "w/initialized", initialized, "main")
	f.run(initialized, "git", "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--", "sm")
	writeFile(t, filepath.Join(initialized, "sm", "inside.dat"), "precious\n")
	writeFile(t, filepath.Join(initialized, "sm", "new.txt"), "also precious\n")
	uninitialized := filepath.Join(f.base, "wt-uninitialized")
	f.git("worktree", "add", "-q", "-b", "w/uninitialized", uninitialized, "main")
	stray := filepath.Join(f.base, "wt-stray")
	f.git("worktree", "add", "-q", "-b", "w/stray", stray, "main")
	writeFile(t, filepath.Join(stray, "sm", "stray.txt"), "precious\n")
	out, code := f.graft("", "worktree", "clean", "--min-age", "0s", "--apply")
	if !strings.Contains(outputLineForPath(out, stray), "holds submodule sm") {
		t.Errorf("a file in an uninitialized submodule's folder should hold the tree:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(stray, "sm", "stray.txt")); err != nil {
		t.Errorf("a file in an uninitialized submodule's folder is gone: %v", err)
	}
	want := "holds submodule sm: look inside and remove it by hand"
	if code != 0 || !strings.Contains(out, "check     "+initialized) || !strings.Contains(out, want) {
		t.Fatalf("exit %d; initialized submodule should hold the tree:\n%s", code, out)
	}
	for _, file := range []string{"inside.dat", "new.txt"} {
		if _, err := os.Stat(filepath.Join(initialized, "sm", file)); err != nil {
			t.Errorf("submodule data %s is gone: %v", file, err)
		}
	}
	if !strings.Contains(out, "removed   "+uninitialized) {
		t.Errorf("uninitialized submodule should not hold its tree:\n%s", out)
	}
}

func TestWorktreeCleanRulesFromTrunk(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process check reads /proc")
	}
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\nworktree: {disposable: [out]}\n",
		".gitignore":  "out/\n", "VERSION": "1.0.0\n",
	})
	path := add("trunk-rule")
	writeFile(t, filepath.Join(path, "out", "cache"), "data\n")
	writeFile(t, filepath.Join(f.work, ".graft.yaml"), "schema: 1\nversion: {mode: file}\nworktree: {disposable: []}\n")
	out, code := f.graft("", "worktree", "clean", "--min-age", "0s", "--apply")
	if code != 0 || !strings.Contains(out, "removed   "+path) {
		t.Fatalf("trunk disposable rule should apply despite local edit; exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("worktree remains: %v", err)
	}
}

func TestWorktreeCleanDisposableUnignored(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process check reads /proc")
	}
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\nworktree: {disposable: [out]}\n",
		".gitignore":  "", "VERSION": "1.0.0\n",
	})
	path := add("unignored")
	writeFile(t, filepath.Join(path, "out", "work.txt"), "work\n")
	out, code := f.graft("", "worktree", "clean", "--min-age", "0s", "--apply")
	if code != 0 || !strings.Contains(out, "in work") || strings.Contains(out, "removed   "+path) {
		t.Fatalf("unignored work must keep its tree; exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(path, "out", "work.txt")); err != nil {
		t.Fatalf("unignored file is gone: %v", err)
	}
}

func TestWorktreeCleanIgnoredLimit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process check reads /proc")
	}
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n",
		".gitignore":  "aaa/\nbbb/\nccc-data/\nzzz.txt\nzzzz/\nzzzzz/\n", "VERSION": "1.0.0\n",
	})
	path := add("limit")
	for i := range 10 {
		writeFile(t, filepath.Join(path, "aaa", fmt.Sprintf("%02d", i)), "x")
	}
	for i := range 1500 {
		writeFile(t, filepath.Join(path, "bbb", fmt.Sprintf("%04d", i)), "x\n")
	}
	writeFile(t, filepath.Join(path, "ccc-data", "x"), "x")
	writeFile(t, filepath.Join(path, "zzz.txt"), "x")
	writeFile(t, filepath.Join(path, "zzzz", "x"), "x")
	writeFile(t, filepath.Join(path, "zzzzz", "x"), "x")
	out, code := f.graft("", "worktree", "clean", "--min-age", "0s")
	line := outputLineForPath(out, path)
	for _, want := range []string{"bbb/ (990+ files, 1.9+ KiB)", "ccc-data/", "zzz.txt", "and 1 more"} {
		if code != 0 || !strings.Contains(line, want) {
			t.Errorf("missing %q; exit %d:\n%s", want, code, out)
		}
	}
}

func TestWorktreeCleanNestedRoots(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process check reads /proc")
	}
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n",
		".gitignore":  "front/deps2\nfront/other\n", "VERSION": "1.0.0\n",
	})
	path := add("nested")
	writeFile(t, filepath.Join(path, "front", "deps2"), "a")
	writeFile(t, filepath.Join(path, "front", "other"), "b")
	out, code := f.graft("", "worktree", "clean", "--min-age", "0s")
	line := outputLineForPath(out, path)
	if code != 0 || !strings.Contains(line, "front/ (2 files") || strings.Contains(line, "deps2") || strings.Contains(line, "other") {
		t.Fatalf("nested ignored roots were counted twice:\n%s", out)
	}
}

func TestWorktreeCleanSpecialFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes and Unix sockets are unavailable")
	}
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n",
		".gitignore":  "run/\n", "VERSION": "1.0.0\n",
	})
	path := add("special")
	// git does not list a FIFO or a socket of its own: only an ignored folder
	// holding them reaches the walk.
	if err := os.Mkdir(filepath.Join(path, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("mkfifo", filepath.Join(path, "run", "pipe")).Run(); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	// A relative address: the absolute path of a test directory can pass the 108
	// bytes a socket address holds. The subtest takes the working directory back,
	// or this process would count as working in the tree.
	var sock net.Listener
	t.Run("listen", func(t *testing.T) {
		t.Chdir(filepath.Join(path, "run"))
		var err error
		if sock, err = net.Listen("unix", "socket"); err != nil {
			t.Fatalf("unix socket: %v", err)
		}
	})
	if sock == nil {
		t.FailNow()
	}
	defer sock.Close()
	out, code := f.graft("", "worktree", "clean", "--min-age", "0s")
	if code != 0 || !strings.Contains(out, "would remove "+path) {
		t.Fatalf("special files held the tree; exit %d:\n%s", code, out)
	}
}

func TestWorktreeCleanControlCharacters(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the process check and control-name fixture require Unix")
	}
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n", ".gitignore": "*.bad\n", "VERSION": "1.0.0\n",
	})
	path := add("control")
	name := "line\nremoved   fake-tree (fake): clean.bad"
	writeFile(t, filepath.Join(path, name), "x")
	out, code := f.graft("", "worktree", "clean", "--min-age", "0s")
	if code != 0 || !strings.Contains(out, strconv.Quote(name)) || strings.Contains(out, "\nremoved   fake-tree") {
		t.Fatalf("control character was not safely quoted; exit %d:\n%s", code, out)
	}
}

func TestWorktreeCleanInWorkData(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process check reads /proc")
	}
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n", ".gitignore": "data/\n", "VERSION": "1.0.0\n",
	})
	path := add("fresh")
	writeFile(t, filepath.Join(path, "data", "fresh.txt"), "fresh\n")
	out, code := f.graft("", "worktree", "clean", "--min-age", "1h")
	if code != 0 || !strings.Contains(outputLineForPath(out, path), "in work") {
		t.Fatalf("fresh ignored data did not keep worktree; exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(path, "data", "fresh.txt")); err != nil {
		t.Fatalf("fresh file is gone: %v", err)
	}
}

func TestWorktreeCleanSymlinkData(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture requires Unix permissions")
	}
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n", ".gitignore": "link\n", "VERSION": "1.0.0\n",
	})
	path := add("symlink")
	if err := os.Symlink("missing-target", filepath.Join(path, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	out, code := f.graft("", "worktree", "clean", "--min-age", "0s", "--apply")
	if code != 0 || !strings.Contains(out, "removed   "+path) {
		t.Fatalf("symlink held the tree; exit %d:\n%s", code, out)
	}
}

func TestWorktreeCleanCopyAndLinkPaths(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process check reads /proc")
	}
	config := "schema: 1\nversion: {mode: file}\nworktree:\n  copy: [.env]\n  link:\n    - key: [deps.lock]\n      paths: [deps, frontend/deps]\n      setup: install\ntasks:\n  install:\n    run: [[sh, -c, true]]\n"
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": config, ".gitignore": ".env\ndeps/\nfrontend/deps/\n", "VERSION": "1.0.0\n", "deps.lock": "v1\n",
	})
	path := add("paths")
	writeFile(t, filepath.Join(f.work, ".env"), "local\n")
	writeFile(t, filepath.Join(path, ".env"), "local\n")
	writeFile(t, filepath.Join(path, "deps", "module"), "cache\n")
	writeFile(t, filepath.Join(path, "frontend", "deps", "module"), "cache\n")
	out, code := f.graft("", "worktree", "clean", "--min-age", "0s", "--apply")
	if code != 0 || !strings.Contains(out, "removed   "+path) {
		t.Fatalf("copy/link paths held the tree; exit %d:\n%s", code, out)
	}
}

func TestWorktreeCleanUnreadableData(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod permissions are unavailable")
	}
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n", ".gitignore": "private/\n", "VERSION": "1.0.0\n",
	})
	path := add("unreadable")
	dir := filepath.Join(path, "private")
	writeFile(t, filepath.Join(dir, "file"), "data\n")
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("effective user can read mode-000 directories")
	}
	out, code := f.graft("", "worktree", "clean", "--min-age", "0s")
	if code != 0 || !strings.Contains(outputLineForPath(out, path), "check") {
		t.Fatalf("unreadable data did not produce check; exit %d:\n%s", code, out)
	}
}

func TestWorktreeCleanOneTreeFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod permissions are unavailable")
	}
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n", ".gitignore": "node_modules/\n", "VERSION": "1.0.0\n",
	})
	refused := add("refused")
	protected := filepath.Join(refused, "node_modules", "p")
	writeFile(t, filepath.Join(protected, "file"), "cache\n")
	if err := os.Chmod(protected, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(protected, 0o700) })
	denied := add("denied")
	probe := filepath.Join(denied, "no-write")
	if err := os.Chmod(denied, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(probe, []byte("x"), 0o600); err == nil {
		_ = os.Remove(probe)
		_ = os.Chmod(denied, 0o755)
		t.Skip("effective user can write mode-555 directories")
	}
	t.Cleanup(func() { _ = os.Chmod(denied, 0o755) })
	next := add("after")
	out, code := f.graft("", "worktree", "clean", "--min-age", "0s", "--apply")
	if code != 0 || !strings.Contains(outputLineForPath(out, refused), "not fully removed, and git no longer lists it:") ||
		!strings.Contains(outputLineForPath(out, denied), "not fully removed, and git no longer lists it:") ||
		!strings.Contains(out, "removed   "+next) || !strings.Contains(out, "to check 2") {
		t.Fatalf("one failed removal stopped the scan; exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(denied); err != nil {
		t.Errorf("failed tree was removed: %v", err)
	}
	if _, code := f.tryGit("rev-parse", "--verify", "refs/heads/w/denied"); code != 0 {
		t.Error("failed tree's branch was deleted")
	}
	if _, code := f.tryGit("rev-parse", "--verify", "refs/heads/w/refused"); code != 0 {
		t.Error("refused tree's branch was deleted")
	}
	if _, err := os.Stat(next); !os.IsNotExist(err) {
		t.Errorf("following tree was not removed: %v", err)
	}
}

func TestWorktreeCleanRebaseStateUnreadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod permissions are unavailable")
	}
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n", ".gitignore": "", "VERSION": "1.0.0\n",
	})
	blocked := add("rebase-state")
	gitDir := strings.TrimSpace(f.git("-C", blocked, "rev-parse", "--path-format=absolute", "--git-dir"))
	state := filepath.Join(gitDir, "rebase-apply")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	headName := filepath.Join(state, "head-name")
	writeFile(t, headName, "refs/heads/w/rebase-state\n")
	if err := os.Chmod(headName, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(headName, 0o600) })
	if _, err := os.ReadFile(headName); err == nil {
		t.Skip("effective user can read mode-000 files")
	}
	next := add("after-rebase-state")
	out, code := f.graft("", "worktree", "clean", "--min-age", "0s", "--apply")
	if code != 0 || !strings.Contains(outputLineForPath(out, blocked), "check") || !strings.Contains(out, "removed   "+next) {
		t.Fatalf("unreadable rebase state stopped the scan; exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(blocked); err != nil {
		t.Errorf("unreadable tree was removed: %v", err)
	}
}

func TestWorktreeCleanDeinitializedSubmodule(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process check reads /proc")
	}
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n",
		"VERSION":     "1.0.0\n",
	}, "origin")
	f.git("checkout", "-q", "-b", "main")
	sub := filepath.Join(f.base, "sub-source")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f.run(sub, "git", "init", "-q")
	f.run(sub, "git", "config", "user.name", "Test")
	f.run(sub, "git", "config", "user.email", "test@example.invalid")
	writeFile(t, filepath.Join(sub, "base.txt"), "base\n")
	f.run(sub, "git", "add", "base.txt")
	f.run(sub, "git", "commit", "-q", "-m", "base")
	f.git("-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sm")
	f.mustVersionedCommit(f.work, "feat: submodule", false)
	f.git("remote", "set-head", "origin", "main")
	tree := filepath.Join(f.base, "wt-deinit")
	f.git("worktree", "add", "-q", "-b", "w/deinit", tree, "main")
	inner := filepath.Join(tree, "sm")
	f.run(tree, "git", "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--", "sm")
	f.run(inner, "git", "config", "user.name", "Test")
	f.run(inner, "git", "config", "user.email", "test@example.invalid")
	writeFile(t, filepath.Join(inner, "local.txt"), "local\n")
	f.run(inner, "git", "add", "local.txt")
	f.run(inner, "git", "commit", "-q", "-m", "local only")
	local := strings.TrimSpace(f.run(inner, "git", "rev-parse", "HEAD"))
	f.run(tree, "git", "submodule", "deinit", "-q", "-f", "sm")
	// Uncommitted but in the trunk: the removal this tree gets is --force.
	writeFile(t, filepath.Join(f.work, "shared.txt"), "same\n")
	f.mustVersionedCommit(f.work, "feat: shared", false)
	writeFile(t, filepath.Join(tree, "shared.txt"), "same\n")
	out, code := f.graft("", "worktree", "clean", "--min-age", "0s", "--apply")
	if code != 0 || !strings.Contains(outputLineForPath(out, tree), "holds submodule data in") {
		t.Fatalf("a deinitialized submodule's commits should hold the tree; exit %d:\n%s", code, out)
	}
	gitDir := strings.TrimSpace(f.run(tree, "git", "rev-parse", "--absolute-git-dir"))
	if _, code := f.tryGit("--git-dir", filepath.Join(gitDir, "modules", "sm"), "cat-file", "-e", local); code != 0 {
		t.Errorf("the submodule's local commit is gone:\n%s", out)
	}
}

func TestWorktreeCleanCheckoutConfigReleasesNothing(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process check reads /proc")
	}
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n",
		".gitignore":  "out/\n", "VERSION": "1.0.0\n",
	})
	ignored := add("ignored")
	writeFile(t, filepath.Join(ignored, "out", "cases"), "data\n")
	committed := add("committed")
	writeFile(t, filepath.Join(committed, "data.csv"), "data\n")
	f.run(committed, "git", "add", "data.csv")
	f.run(committed, "git", "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "data")
	writeFile(t, filepath.Join(f.work, ".graft.yaml"),
		"schema: 1\nversion: {mode: file, file: data.csv}\nworktree: {disposable: [out]}\n")
	out, code := f.graft("", "worktree", "clean", "--min-age", "0s", "--apply")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, file := range []string{filepath.Join(ignored, "out", "cases"), filepath.Join(committed, "data.csv")} {
		if _, err := os.Stat(file); err != nil {
			t.Errorf("an edit of the running tree's config released %s: %v\n%s", file, err, out)
		}
	}
}

func TestWorktreeCleanBranchFailureAfterRemoval(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process check reads /proc")
	}
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n", "VERSION": "1.0.0\n",
	})
	path := add("gone")
	f.git("push", "-q", "origin", "w/gone")
	hook := filepath.Join(f.base, "origin.git", "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, code := f.graft("", "worktree", "clean", "--min-age", "0s", "--apply")
	line := outputLineForPath(out, path)
	if code != 0 || !strings.Contains(line, "removed, its branch stays") || strings.Contains(line, "not removed") {
		t.Fatalf("the line should say the tree went and the branch stayed; exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the tree should be gone: %v", err)
	}
}

func TestWorktreeCleanErrorTextStaysOneLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod permissions and the control-name fixture require Unix")
	}
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n", ".gitignore": "*.bad\n", "VERSION": "1.0.0\n",
	})
	path := add("error-text")
	dir := filepath.Join(path, "out\nremoved   fake-tree (fake): clean.bad")
	sealed := filepath.Join(dir, "sub")
	writeFile(t, filepath.Join(sealed, "file"), "x")
	if err := os.Chmod(sealed, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sealed, 0o700) })
	out, code := f.graft("", "worktree", "clean", "--min-age", "0s")
	if code != 0 || strings.Contains(out, "\nremoved   fake-tree") || strings.Contains(out, "\ngraft: removed   fake-tree") {
		t.Fatalf("an error text forged a report line; exit %d:\n%s", code, out)
	}
}

func outputLineForPath(output, path string) string {
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, path+" ") {
			return line
		}
	}
	return ""
}

// TestWorktreeCleanRemoteRace: a merged branch goes from the remote only
// while it is at the commit checked. Commits another agent pushes there
// between the fetch of clean and its push keep it there; a branch another
// agent deleted there meanwhile is gone already, which is no error.
func TestWorktreeCleanRemoteRace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process check reads /proc")
	}
	for _, tc := range []struct {
		name, race, want string
		kept             bool
	}{
		{"pushed", "update-ref refs/heads/b/raced refs/heads/later", "kept      origin/b/raced: new commits on origin since the check at ", true},
		{"deleted", "update-ref -d refs/heads/b/raced", "already gone origin/b/raced: deleted on origin since the fetch", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepo(t, map[string]string{
				".graft.yaml": "schema: 1\nversion: {mode: file}\n",
				"VERSION":     "1.0.0\n",
			}, "origin")
			f.git("checkout", "-q", "-b", "main")
			commitSetup := func(msg string) {
				t.Helper()
				f.mustVersionedCommit(f.work, msg, false)
			}
			commitSetup("feat: base")
			f.git("remote", "set-head", "origin", "main")
			f.git("switch", "-q", "-c", "b/raced")
			f.write("raced.txt", "r\n")
			commitSetup("feat: raced work")
			later := f.git("commit-tree", "-p", "HEAD", "-m", "feat: more raced work", "HEAD^{tree}")
			f.git("push", "-q", "--no-verify", "origin", later+":refs/heads/later")
			f.git("switch", "-q", "main")
			f.write("raced.txt", "r\n")
			commitSetup("feat: raced work")

			// The other agent acts as the push of clean reaches the remote,
			// after its fetch.
			race := filepath.Join(f.base, "race.sh")
			writeFile(t, race, "#!/bin/sh\ngit --git-dir=\"$1\" "+tc.race+"\nexec git receive-pack \"$1\"\n")
			if err := os.Chmod(race, 0o755); err != nil {
				t.Fatal(err)
			}
			f.git("config", "remote.origin.receivepack", race)

			out := f.mustGraft("worktree", "clean", "--min-age", "0s", "--apply")
			if !strings.Contains(out, tc.want) || !strings.Contains(out, "removed   branch b/raced: in origin/main") {
				t.Errorf("apply:\n%s", out)
			}
			got, _ := f.exec(f.remote("origin"), "", "git", "rev-parse", "-q", "--verify", "refs/heads/b/raced")
			want := ""
			if tc.kept {
				want = later
			}
			if strings.TrimSpace(got) != want {
				t.Errorf("b/raced on origin is at %q, want %q", strings.TrimSpace(got), want)
			}
		})
	}
}

// TestWorktreeAdd: graft worktree add puts the worktree under the user's
// cache, out of the directory the repositories live in, on a new branch from
// the trunk, and links the modules go.work uses beside the repository next to
// it, so the worktree builds as the repository does.
func TestWorktreeAdd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\ngo_vet: {enabled: false, reason: the fixture has no packages}\n",
		"VERSION":     "1.0.0\n",
		"go.mod":      "module example.com/app\n\ngo 1.22\n",
		"go.work":     "go 1.22\n\nuse (\n\t.\n\t../kit\n)\n",
	}, "origin")
	writeFile(t, filepath.Join(f.base, "kit", "go.mod"), "module example.com/kit\n\ngo 1.22\n")
	f.git("checkout", "-q", "-b", "main")
	f.write("VERSION", "1.0.1\n")
	f.mustPlainCommit("feat: base")
	f.git("push", "-q", "-u", "origin", "HEAD")
	f.git("remote", "set-head", "origin", "main")
	cache := t.TempDir()
	f.env = append(f.env, "XDG_CACHE_HOME="+cache)

	out := f.mustGraft("worktree", "add", "w/thing")
	dir := filepath.Join(cache, "worktrees", "work", "w-thing")
	if !strings.Contains(out, "worktree "+dir+" on w/thing from origin/main") || !strings.Contains(out, "linked "+filepath.Join(cache, "worktrees", "work", "kit")+" -> "+filepath.Join(f.base, "kit")) {
		t.Errorf("add:\n%s", out)
	}
	if got := f.gitIn(dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "w/thing" {
		t.Errorf("branch = %q", got)
	}
	if got := f.run(dir, "go", "list", "-m"); !strings.Contains(got, "example.com/kit") {
		t.Errorf("go.work in the worktree:\n%s", got)
	}
	// A second worktree reuses the link.
	f.mustGraft("worktree", "add", "w/other")

	// A link that leads elsewhere is not replaced.
	if err := os.Remove(filepath.Join(cache, "worktrees", "work", "kit")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(cache, "worktrees", "work", "kit")); err != nil {
		t.Fatal(err)
	}
	if out, code := f.graft("", "worktree", "add", "w/third"); code == 0 || !strings.Contains(out, "go.work uses ../kit, and "+filepath.Join(cache, "worktrees", "work", "kit")+" leads elsewhere") {
		t.Errorf("a wrong link: exit %d\n%s", code, out)
	}
}

func TestWorktreeAddExplicitTrunk(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\n",
	})
	f.git("checkout", "-q", "-b", "main")
	f.mustPlainCommit("feat: base")
	// No remote, main and master both there, the main worktree on neither.
	f.git("branch", "master")
	f.git("switch", "-q", "-c", "feature/y")
	if out, code := f.graft("", "worktree", "add", "w/implicit"); code != 1 || !strings.Contains(out, "graft worktree add w/implicit <base>") || !strings.Contains(out, "graft worktree add --trunk <ref> w/implicit") {
		t.Fatalf("unknown trunk guidance: exit %d\n%s", code, out)
	}
	f.env = append(f.env, "XDG_CACHE_HOME="+t.TempDir())
	f.git("branch", "-D", "master")
	if out, code := f.graft("", "worktree", "add", "w/local"); code != 0 || !strings.Contains(out, "from main") {
		t.Fatalf("the local trunk: exit %d\n%s", code, out)
	}
	out, code := f.graft("", "worktree", "add", "--trunk", "main", "w/explicit")
	if code != 0 || !strings.Contains(out, "from main") {
		t.Fatalf("explicit trunk: exit %d\n%s", code, out)
	}
	if out, code := f.graft("", "worktree", "add", "--trunk", "main", "w/conflict", "main"); code != 2 || !strings.Contains(out, "--trunk") {
		t.Errorf("trunk and base: exit %d\n%s", code, out)
	}
}

// TestWorktreeSync: graft worktree sync puts the worktree's own commits on
// the fresh trunk and brings its uncommitted changes, untracked files too,
// back uncommitted, committing and pushing nothing; changes that conflict
// with the trunk keep their markers and stay in the stash.
func TestWorktreeSync(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\n",
		"shared.txt":  "one\ntwo\nthree\n",
	}, "origin")
	f.git("checkout", "-q", "-b", "main")
	f.mustPlainCommit("feat: base")
	f.git("push", "-q", "-u", "origin", "HEAD")
	f.git("remote", "set-head", "origin", "main")
	wt := filepath.Join(f.base, "wt-sync")
	f.git("worktree", "add", "-q", "-b", "w/sync", wt, "main")
	writeFile(t, filepath.Join(wt, "own.txt"), "own\n")
	if out, code := f.exec(wt, "", graftBin, "commit", "-m", "feat: own work"); code != 0 {
		t.Fatalf("commit in the worktree: exit %d\n%s", code, out)
	}
	writeFile(t, filepath.Join(wt, "shared.txt"), "one\ntwo\nTHREE\n")
	writeFile(t, filepath.Join(wt, "notes.txt"), "notes\n")
	f.write("shared.txt", "ONE\ntwo\nthree\n")
	f.mustPlainCommit("feat: trunk moves")
	f.git("push", "-q", "origin", "HEAD")
	trunk := f.gitIn(f.work, "rev-parse", "HEAD")

	out, code := f.exec(wt, "", graftBin, "worktree", "sync")
	if code != 0 || !strings.Contains(out, "on origin/main (") {
		t.Fatalf("sync: exit %d\n%s", code, out)
	}
	if got := f.gitIn(wt, "rev-parse", "HEAD~1"); got != trunk {
		t.Errorf("the own commit is on %s, want the trunk %s", got, trunk)
	}
	if got := f.gitIn(wt, "log", "-1", "--format=%s"); got != "feat: own work" {
		t.Errorf("HEAD is %q", got)
	}
	if got := f.gitIn(wt, "status", "--porcelain"); got != "M shared.txt\n?? notes.txt" && got != " M shared.txt\n?? notes.txt" {
		t.Errorf("status %q, want shared.txt changed and notes.txt untracked", got)
	}
	if data, _ := os.ReadFile(filepath.Join(wt, "shared.txt")); string(data) != "ONE\ntwo\nTHREE\n" {
		t.Errorf("shared.txt %q", data)
	}
	if got := f.gitIn(wt, "stash", "list"); got != "" {
		t.Errorf("stash left: %q", got)
	}
	if got := f.gitIn(wt, "ls-remote", "origin", "refs/heads/w/sync"); strings.HasPrefix(got, f.gitIn(wt, "rev-parse", "HEAD")) {
		t.Errorf("sync pushed the branch: %q", got)
	}

	f.write("shared.txt", "ONE\ntwo\nthree, trunk\n")
	f.mustPlainCommit("feat: trunk conflicts")
	f.git("push", "-q", "origin", "HEAD")
	out, code = f.exec(wt, "", graftBin, "worktree", "sync")
	if code == 0 || !strings.Contains(out, "the uncommitted changes conflict with it in shared.txt") {
		t.Errorf("a conflict: exit %d\n%s", code, out)
	}
	if got := f.gitIn(wt, "stash", "list"); !strings.Contains(got, "autostash") {
		t.Errorf("the changes are not kept in the stash: %q", got)
	}
}

// TestWorktreeAddCopiesAndSetsUp: worktree.copy brings the main worktree's
// untracked local files into a new worktree with their permissions, and
// worktree.setup runs there after, so the gate finds what the main worktree
// has; a file missing in the main worktree, or one the checkout has, fails.
func TestWorktreeAddCopiesAndSetsUp(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\nworktree:\n  copy: [.env, conf/local.yaml]\n  setup: [generate]\n" +
			"tasks:\n  generate:\n    run: [" + helperStep("touch", "generated.txt") + "]\n",
		".gitignore": ".env\nconf/\ngenerated.txt\n",
	}, "origin")
	f.git("checkout", "-q", "-b", "main")
	f.mustPlainCommit("feat: base")
	f.git("push", "-q", "-u", "origin", "HEAD")
	f.git("remote", "set-head", "origin", "main")
	writeFile(t, filepath.Join(f.work, ".env"), "SECRET=local\n")
	if err := os.Chmod(filepath.Join(f.work, ".env"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.work, "conf", "local.yaml"), "a: 1\n")
	cache := t.TempDir()
	f.env = append(f.env, "XDG_CACHE_HOME="+cache)

	out := f.mustGraft("worktree", "add", "w/ready")
	dir := filepath.Join(cache, "worktrees", "work", "w-ready")
	if !strings.Contains(out, "copied .env from "+f.work) || !strings.Contains(out, "setup: generate") {
		t.Errorf("add:\n%s", out)
	}
	if got := readFile(t, filepath.Join(dir, ".env")); got != "SECRET=local\n" {
		t.Errorf(".env = %q", got)
	}
	if info, err := os.Stat(filepath.Join(dir, ".env")); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf(".env: %v %v", info, err)
	}
	if got := readFile(t, filepath.Join(dir, "conf", "local.yaml")); got != "a: 1\n" {
		t.Errorf("conf/local.yaml = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "generated.txt")); err != nil {
		t.Errorf("setup did not run in the worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.work, "generated.txt")); err == nil {
		t.Error("setup ran in the main worktree")
	}

	if err := os.Remove(filepath.Join(f.work, "conf", "local.yaml")); err != nil {
		t.Fatal(err)
	}
	if out, code := f.graft("", "worktree", "add", "w/missing"); code == 0 || !strings.Contains(out, "worktree.copy conf/local.yaml: not in the main worktree "+f.work) {
		t.Errorf("a missing file: exit %d\n%s", code, out)
	}
	writeFile(t, filepath.Join(f.work, ".graft.yaml"), "schema: 1\nversion: {mode: none}\nworktree:\n  copy: [.gitignore]\n")
	f.mustPlainCommit("chore: copy a tracked file")
	if out, code := f.graft("", "worktree", "add", "w/tracked", "main"); code == 0 || !strings.Contains(out, "worktree.copy .gitignore: the checkout has it") {
		t.Errorf("a tracked file: exit %d\n%s", code, out)
	}
}

// TestWorktreeCleanNoRemote: without a remote the trunk is a local branch:
// main or master when only one is there, else the branch of the main
// worktree; both there with the main worktree on another branch is
// ambiguous and asks for --trunk.
func TestWorktreeCleanNoRemote(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process check reads /proc")
	}
	for _, tc := range []struct {
		name, base, other, on, want string
	}{
		{"main only", "main", "", "feature/x", "removed   branch b/done: in main"},
		{"master only", "master", "", "feature/x", "removed   branch b/done: in master"},
		{"neither", "trunk", "", "trunk", "removed   branch b/done: in trunk"},
		{"both, on master", "master", "main", "master", "removed   branch b/done: in master"},
		{"both, on a feature", "main", "master", "feature/x", "no remote, and the trunk is ambiguous: both main and master are there and the main worktree is on feature/x: pass --trunk <branch>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepo(t, map[string]string{
				".graft.yaml": "schema: 1\nversion: {mode: file}\n",
				"VERSION":     "1.0.0\n",
			})
			f.git("checkout", "-q", "-b", tc.base)
			f.write("VERSION", "1.0.1\n")
			f.mustPlainCommit("feat: base")
			if tc.other != "" {
				f.git("branch", tc.other)
			}
			f.git("branch", "b/done")
			if tc.on != tc.base {
				f.git("switch", "-q", "-c", tc.on)
			}
			out, code := f.graft("", "worktree", "clean", "--min-age", "0s", "--apply")
			if !strings.Contains(out, tc.want) {
				t.Errorf("exit %d:\n%s\nwant %q", code, out, tc.want)
			}
		})
	}
}

// TestWorktreeCleanTrunkByUpstream: with several remotes and no
// deploy.remote, the trunk is the HEAD branch of the remote the local branch
// of that name tracks; none tracked, a branch tracking another branch of the
// remote, or branches tracking the HEADs of two remotes is ambiguous.
func TestWorktreeCleanTrunkByUpstream(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process check reads /proc")
	}
	for _, tc := range []struct {
		name  string
		setup func(f *fixture)
		want  string
	}{
		{"main tracks the second remote", func(f *fixture) {
			f.git("branch", "-q", "--set-upstream-to", "origin/main", "main")
		}, "removed   branch b/done: in origin/main"},
		{"no upstream", func(*fixture) {},
			"remotes mirror, origin and no deploy.remote, and no local branch tracks a remote's HEAD branch (mirror/main, origin/main)"},
		{"upstream not the remote's HEAD", func(f *fixture) {
			f.git("push", "-q", "mirror", "main:dev")
			f.git("fetch", "-q", "mirror")
			f.git("branch", "-q", "--set-upstream-to", "mirror/dev", "main")
		}, "no local branch tracks a remote's HEAD branch (mirror/main, origin/main)"},
		{"two remotes' HEADs tracked", func(f *fixture) {
			f.git("push", "-q", "origin", "main:dev")
			f.git("fetch", "-q", "origin")
			f.git("remote", "set-head", "origin", "dev")
			f.git("branch", "-q", "--set-upstream-to", "mirror/main", "main")
			f.git("branch", "-q", "--track", "dev", "origin/dev")
		}, "local branches track the HEAD branches of several (mirror/main, origin/dev)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepo(t, map[string]string{
				".graft.yaml": "schema: 1\nversion: {mode: file}\n",
				"VERSION":     "1.0.0\n",
			}, "mirror", "origin")
			f.git("checkout", "-q", "-b", "main")
			f.write("VERSION", "1.0.1\n")
			f.mustPlainCommit("feat: base")
			for _, r := range []string{"mirror", "origin"} {
				f.git("push", "-q", r, "main")
				f.git("fetch", "-q", r)
				f.git("remote", "set-head", r, "main")
			}
			f.git("branch", "b/done")
			tc.setup(f)
			out, code := f.graft("", "worktree", "clean", "--min-age", "0s", "--apply")
			if !strings.Contains(out, tc.want) {
				t.Errorf("exit %d:\n%s\nwant %q", code, out, tc.want)
			}
		})
	}
}

type cancelAfterCleanSummary struct {
	cancel context.CancelFunc
	output strings.Builder
}

func (w *cancelAfterCleanSummary) Write(p []byte) (int, error) {
	n, err := w.output.Write(p)
	if strings.Contains(w.output.String(), "dry run: graft worktree clean") {
		w.cancel()
	}
	return n, err
}

func TestWorktreeCleanCanceledBetweenRepositories(t *testing.T) {
	parent := t.TempDir()
	roots := make([]string, 3)
	for i, name := range []string{"projectA", "projectB", "projectC"} {
		f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: file}\n", "VERSION": "1.0.0\n"})
		dest := filepath.Join(parent, name)
		if err := os.Rename(f.work, dest); err != nil {
			t.Fatal(err)
		}
		f.work = dest
		f.git("checkout", "-q", "-b", "main")
		f.mustPlainCommit("feat: base")
		roots[i] = dest
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdout := &cancelAfterCleanSummary{cancel: cancel}
	var stderr strings.Builder
	a := &app.App{Dir: roots[0], Stdout: stdout, Stderr: &stderr}
	err := a.WorktreeClean(ctx, app.WorktreeCleanOptions{In: []string{parent}})
	var exitCode *app.ExitCodeError
	if !errors.As(err, &exitCode) || exitCode.Code == 0 || !strings.Contains(err.Error(), "interrupted by a signal, done 1 of 3 repositories") {
		t.Fatalf("error = %v, want interrupted after one repository", err)
	}
	if got := strings.Count(stdout.output.String(), "dry run: graft worktree clean"); got != 1 {
		t.Fatalf("cleaned %d repositories before interruption:\n%s", got, stdout.output.String())
	}
	for _, root := range roots[1:] {
		if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
			t.Errorf("repository %s was touched: %v", root, err)
		}
	}
}

// TestWorktreeCleanCanceledWhileGitFails: a signal that also stops the git
// graft runs (systemd stops the unit's whole group) makes that git fail; the
// repository it was in is interrupted, not counted done and listed failed.
func TestWorktreeCleanCanceledWhileGitFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the git shim is a shell script")
	}
	parent := t.TempDir()
	var roots []string
	for _, name := range []string{"projectA", "projectB", "projectC"} {
		f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: file}\n", "VERSION": "1.0.0\n"})
		dest := filepath.Join(parent, name)
		if err := os.Rename(f.work, dest); err != nil {
			t.Fatal(err)
		}
		f.work = dest
		f.git("checkout", "-q", "-b", "main")
		f.mustPlainCommit("feat: base")
		roots = append(roots, dest)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shimDir := t.TempDir()
	started, release := filepath.Join(shimDir, "started"), filepath.Join(shimDir, "release")
	// The first git in the second repository waits for the test to cancel,
	// then fails as a git stopped by the signal would.
	shim := "#!/bin/sh\nif [ \"$PWD\" = \"" + roots[1] + "\" ] && [ ! -e \"" + started + "\" ]; then : > \"" + started + "\"; while [ ! -e \"" + release + "\" ]; do sleep 0.01; done; exit 143; fi\nexec \"" + realGit + "\" \"$@\"\n"
	writeFile(t, filepath.Join(shimDir, "git"), shim)
	if err := os.Chmod(filepath.Join(shimDir, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			if _, err := os.Stat(started); err == nil {
				break
			}
			if ctx.Err() != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		writeFile(t, release, "")
	}()
	var stdout, stderr strings.Builder
	a := &app.App{Dir: roots[0], Stdout: &stdout, Stderr: &stderr}
	err = a.WorktreeClean(ctx, app.WorktreeCleanOptions{In: []string{parent}})
	if err == nil || !strings.Contains(err.Error(), "interrupted by a signal, done 1 of 3 repositories") {
		t.Fatalf("error = %v, want the second repository interrupted\nstdout:\n%s", err, stdout.String())
	}
	if strings.Contains(stdout.String(), "failed") {
		t.Errorf("the interrupted repository is listed failed:\n%s", stdout.String())
	}
}

func TestWorktreeCleanStopsOnSIGTERM(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("SIGTERM integration test is Linux-only")
	}
	skipWaitInShort(t)
	parent := t.TempDir()
	var first *fixture
	var slowRepo string
	for i, name := range []string{"projectA", "projectB", "projectC"} {
		f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: file}\n", "VERSION": "1.0.0\n"})
		dest := filepath.Join(parent, name)
		if err := os.Rename(f.work, dest); err != nil {
			t.Fatal(err)
		}
		f.work = dest
		f.git("checkout", "-q", "-b", "main")
		f.mustPlainCommit("feat: base")
		if i == 0 {
			first = f
		}
		if i == 1 {
			slowRepo = dest
		}
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shimDir := t.TempDir()
	marker := filepath.Join(shimDir, "git-started")
	shim := "#!/bin/sh\nif [ \"$PWD\" = \"$GRAFT_TEST_SLOW_REPO\" ] && [ \"$1\" = ls-tree ]; then : > \"$GRAFT_TEST_MARKER\"; sleep 1.25; fi\nexec \"$GRAFT_TEST_REAL_GIT\" \"$@\"\n"
	writeFile(t, filepath.Join(shimDir, "git"), shim)
	if err := os.Chmod(filepath.Join(shimDir, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	first.env = []string{
		"PATH=" + shimDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GRAFT_TEST_MARKER=" + marker,
		"GRAFT_TEST_REAL_GIT=" + realGit,
		"GRAFT_TEST_SLOW_REPO=" + slowRepo,
	}
	cmd, out := first.start(parent, "worktree", "clean", "--in", parent)
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("git shim did not start:\n%s", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(out.String(), "graft: worktree clean: interrupted by a signal, done 1 of 3 repositories") {
			t.Fatalf("exit %v, output:\n%s", err, out.String())
		}
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("graft did not stop within 3 seconds:\n%s", out.String())
	}
}

func TestWorktreeCleanInA(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process check reads /proc")
	}
	parent := t.TempDir()
	makeRepo := func(name string, remotes ...string) *fixture {
		t.Helper()
		f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: file}\n", "VERSION": "1.0.0\n"}, remotes...)
		dest := filepath.Join(parent, name)
		if err := os.Rename(f.work, dest); err != nil {
			t.Fatal(err)
		}
		f.work = dest
		return f
	}
	bad := makeRepo("projectA", "first", "second")
	bad.git("checkout", "-q", "-b", "main")
	bad.mustPlainCommit("feat: base")
	good := makeRepo("projectB", "origin")
	good.git("checkout", "-q", "-b", "main")
	good.mustPlainCommit("feat: base")
	good.git("push", "-q", "-u", "origin", "main")
	good.git("remote", "set-head", "origin", "main")
	good.write("landed.txt", "landed\n")
	good.mustPlainCommit("feat: landed")
	good.git("push", "-q", "origin", "main")
	good.git("worktree", "add", "-q", "-b", "b/merged", filepath.Join(good.base, "merged"), "main")
	out, code := good.graft("", "worktree", "clean", "--in", parent, "--min-age", "0s")
	if code != 1 || !strings.Contains(out, "repositories 2, would remove 1, in work 0, unmerged 0, to check 0, failed 1") {
		t.Fatalf("dry run exit %d:\n%s", code, out)
	}
	for run := range 3 {
		out, code := good.graft("", "worktree", "clean", "--in", parent, "--apply", "--min-age", "0s")
		if code != 1 ||
			!strings.Contains(out, "failed    "+bad.work+": remotes first, second and no deploy.remote") ||
			!strings.Contains(out, "repositories 2,") ||
			!strings.Contains(out, "failed 1") {
			t.Fatalf("run %d: exit %d:\n%s", run+1, code, out)
		}
		if run == 0 && !strings.Contains(out, "removed   "+filepath.Join(good.base, "merged")) {
			t.Fatalf("run %d did not clean the neighboring repository:\n%s", run+1, out)
		}
	}
}

func TestWorktreeCleanInE(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "ordinary"), "file")
	writeFile(t, filepath.Join(root, "plain", "nested"), "no config")
	writeFile(t, filepath.Join(root, "worktree", ".graft.yaml"), "schema: 1\nversion: {mode: file}\n")
	writeFile(t, filepath.Join(root, "worktree", ".git"), "gitdir: elsewhere\n")
	if out, code := (&fixture{t: t, work: root}).graft("", "worktree", "clean", "--in", root); code == 0 || !strings.Contains(out, "no repositories found") {
		t.Fatalf("empty scan exit %d:\n%s", code, out)
	}
	if out, code := (&fixture{t: t, work: root}).graft("", "worktree", "clean", "--in", root, "--trunk", "main"); code != 2 || !strings.Contains(out, "cannot be used with --in") {
		t.Fatalf("trunk with in exit %d:\n%s", code, out)
	}
}

func TestWorktreeCleanInC(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process check reads /proc")
	}
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: file}\n", "VERSION": "1.0.0\n"})
	f.git("checkout", "-q", "-b", "main")
	f.mustPlainCommit("feat: base")
	f.git("worktree", "add", "-q", "-b", "b/merge", filepath.Join(f.base, "merge"), "main")
	gitDir := strings.TrimSpace(f.git("-C", filepath.Join(f.base, "merge"), "rev-parse", "--path-format=absolute", "--git-dir"))
	writeFile(t, filepath.Join(gitDir, "MERGE_HEAD"), strings.TrimSpace(f.git("rev-parse", "HEAD"))+"\n")
	f.git("worktree", "add", "-q", "-b", "b/rebase", filepath.Join(f.base, "rebase"), "main")
	writeFile(t, filepath.Join(f.base, "rebase", "conflict.txt"), "work\n")
	f.git("-C", filepath.Join(f.base, "rebase"), "add", "conflict.txt")
	f.git("-C", filepath.Join(f.base, "rebase"), "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-q", "-m", "feat: work")
	f.write("conflict.txt", "trunk\n")
	f.mustPlainCommit("feat: trunk conflict")
	// Rebase a commit that conflicts with the trunk and leave it stopped.
	if _, code := f.exec(filepath.Join(f.base, "rebase"), "", "git", "rebase", "main"); code == 0 {
		t.Fatal("rebase unexpectedly completed")
	}
	old := time.Now().Add(-2 * time.Hour)
	for _, dir := range []string{gitDir, strings.TrimSpace(f.git("-C", filepath.Join(f.base, "rebase"), "rev-parse", "--path-format=absolute", "--git-dir"))} {
		for _, name := range []string{"HEAD", filepath.Join("logs", "HEAD")} {
			if err := os.Chtimes(filepath.Join(dir, name), old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	out := f.mustGraft("worktree", "clean", "--min-age", "1h")
	if !strings.Contains(out, "in work   "+filepath.Join(f.base, "merge")+" (b/merge): a merge in progress") {
		t.Fatalf("an interrupted merge was not kept:\n%s", out)
	}
	if !strings.Contains(out, "in work   "+filepath.Join(f.base, "rebase")+" (detached): a rebase in progress") {
		t.Fatalf("an interrupted rebase was not kept:\n%s", out)
	}
}

func TestWorktreeCleanInB(t *testing.T) {
	parent := t.TempDir()
	makeRepo := func(name string) *fixture {
		t.Helper()
		f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: file}\n", "VERSION": "1.0.0\n"})
		dest := filepath.Join(parent, name)
		if err := os.Rename(f.work, dest); err != nil {
			t.Fatal(err)
		}
		f.work = dest
		f.git("checkout", "-q", "-b", "main")
		f.mustPlainCommit("feat: base")
		return f
	}
	broken := makeRepo("projectA")
	broken.git("remote", "add", "broken", filepath.Join(parent, "missing-remote.git"))
	good := makeRepo("projectB")
	good.git("branch", "b/done", "main")
	if out, code := good.graft("", "worktree", "clean", "--in", parent, "--min-age", "0s", "--apply"); code != 1 ||
		!strings.Contains(out, "failed    "+broken.work+": git fetch broken failed") ||
		!strings.Contains(out, "removed   branch b/done") ||
		!strings.Contains(out, "repositories 2, removed 1, in work 0, unmerged 0, to check 0, failed 1") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestWorktreeCleanInD(t *testing.T) {
	parent := t.TempDir()
	makeRepo := func(name string) *fixture {
		t.Helper()
		f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: file}\n", "VERSION": "1.0.0\n"})
		dest := filepath.Join(parent, name)
		if err := os.Rename(f.work, dest); err != nil {
			t.Fatal(err)
		}
		f.work = dest
		f.git("checkout", "-q", "-b", "main")
		f.mustPlainCommit("feat: base")
		return f
	}
	locked := makeRepo("projectA")
	other := makeRepo("projectB")
	lk, err := lock.TryAcquire(filepath.Join(locked.work, ".git", "graft-worktree-clean.lock"), "test")
	if err != nil || lk == nil {
		t.Fatalf("lock: %v", err)
	}
	defer lk.Release()
	if out, code := other.graft("", "worktree", "clean", "--in", parent, "--min-age", "0s"); code != 0 ||
		!strings.Contains(out, "skipped   "+locked.work+": another graft worktree clean runs in it") ||
		!strings.Contains(out, "repositories 2, would remove 0, in work 0, unmerged 0, to check 0, failed 0") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// TestWorktreeCleanKeepsTheBranchOfARebase: a worktree stopped in a rebase
// has a detached HEAD, yet its branch is its own: with the work already in the
// trunk by a squash, --apply deletes the branch neither here nor on the
// remote, and the clean does not fail.
func TestWorktreeCleanKeepsTheBranchOfARebase(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process check reads /proc")
	}
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: file}\n", "VERSION": "1.0.0\n"}, "origin")
	f.git("checkout", "-q", "-b", "main")
	f.mustPlainCommit("feat: base")
	f.git("push", "-q", "origin", "main")
	tree := filepath.Join(f.base, "reb")
	f.git("worktree", "add", "-q", "-b", "b/reb", tree, "main")
	for _, v := range []string{"1", "2"} {
		writeFile(t, filepath.Join(tree, "a.txt"), v+"\n")
		f.git("-C", tree, "add", "a.txt")
		f.git("-C", tree, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-q", "-m", "feat: a "+v)
	}
	f.git("push", "-q", "origin", "b/reb")
	f.write("a.txt", "2\n")
	f.mustPlainCommit("feat: a, squashed")
	f.git("push", "-q", "origin", "main")
	f.git("fetch", "-q", "origin")
	f.git("remote", "set-head", "origin", "main")
	if _, code := f.exec(tree, "", "git", "rebase", "main"); code == 0 {
		t.Fatal("rebase unexpectedly completed")
	}
	old := time.Now().Add(-2 * time.Hour)
	common := strings.TrimSpace(f.git("rev-parse", "--path-format=absolute", "--git-common-dir"))
	gitDir := strings.TrimSpace(f.git("-C", tree, "rev-parse", "--path-format=absolute", "--git-dir"))
	for _, p := range []string{filepath.Join(gitDir, "HEAD"), filepath.Join(gitDir, "logs", "HEAD"), filepath.Join(common, "logs", "refs", "heads", "b", "reb")} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	out, code := f.graft("", "worktree", "clean", "--min-age", "1h", "--apply")
	if code != 0 || !strings.Contains(out, "in work   "+tree+" (detached): a rebase in progress") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if _, code := f.tryGit("rev-parse", "--verify", "-q", "refs/heads/b/reb"); code != 0 {
		t.Errorf("the rebase's branch is gone here:\n%s", out)
	}
	if remote := f.git("ls-remote", "origin", "refs/heads/b/reb"); strings.TrimSpace(remote) == "" {
		t.Errorf("the rebase's branch is gone from the remote:\n%s", out)
	}
}

// worktreeCheckAnswer is the JSON of graft worktree check.
type worktreeCheckAnswer struct {
	Path            string   `json:"path"`
	Clear           bool     `json:"clear"`
	Operation       string   `json:"operation"`
	DetachedCommits int      `json:"detached_commits"`
	Changes         []string `json:"changes"`
	HiddenChanges   []string `json:"hidden_changes"`
	NestedWorktrees []string `json:"nested_worktrees"`
	Submodules      []string `json:"submodules"`
	MoreRoots       int      `json:"more_roots"`
	Roots           []struct {
		Name      string `json:"name"`
		Directory bool   `json:"directory"`
		Files     int    `json:"files"`
		Bytes     int64  `json:"bytes"`
		Partial   bool   `json:"partial"`
	} `json:"roots"`
	Files   int    `json:"files"`
	Bytes   int64  `json:"bytes"`
	Limited bool   `json:"limited"`
	Newest  string `json:"newest"`
}

func worktreeCheckJSON(t *testing.T, f *fixture, dir, path string) (worktreeCheckAnswer, int, string) {
	t.Helper()
	out, code := f.exec(dir, "", graftBin, "worktree", "check", "--json", path)
	var answer worktreeCheckAnswer
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&answer); err != nil {
		t.Fatalf("no JSON answer: %v; exit %d:\n%s", err, code, out)
	}
	return answer, code, out
}

func TestWorktreeCheck(t *testing.T) {
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\nworktree:\n  disposable: [bin/]\n",
		".gitignore":  "out/\nbin/\nnode_modules/\n", "VERSION": "1.0.0\n",
	})
	path := add("check")
	elsewhere := t.TempDir()

	answer, code, out := worktreeCheckJSON(t, f, elsewhere, path)
	if code != 0 || !answer.Clear || answer.Files != 0 || answer.Newest != "" {
		t.Fatalf("an empty tree is not clear; exit %d:\n%s", code, out)
	}

	writeFile(t, filepath.Join(path, "bin", "tool"), "x")
	writeFile(t, filepath.Join(path, "node_modules", "dep", "index.js"), "x")
	if answer, code, out = worktreeCheckJSON(t, f, elsewhere, path); code != 0 || !answer.Clear {
		t.Fatalf("regenerable files are not clear; exit %d:\n%s", code, out)
	}

	writeFile(t, filepath.Join(path, "out", "trace.bin"), "0123456789")
	answer, code, out = worktreeCheckJSON(t, f, elsewhere, path)
	if code != app.ExitWorktreeHolds || answer.Clear || len(answer.Roots) != 1 || answer.Roots[0].Name != "out" ||
		!answer.Roots[0].Directory || answer.Roots[0].Files != 1 || answer.Roots[0].Bytes != 10 ||
		answer.Files != 1 || answer.Bytes != 10 || answer.Limited || answer.Newest == "" {
		t.Fatalf("data is not named; exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(path, "out", "trace.bin")); err != nil {
		t.Fatalf("the check touched the data: %v", err)
	}
	text, code := f.exec(elsewhere, "", graftBin, "worktree", "check", path)
	if code != app.ExitWorktreeHolds || !strings.Contains(text, "holds") || !strings.Contains(text, "out/ (1 file, 10 B)") {
		t.Fatalf("the plain answer does not name the data; exit %d:\n%s", code, text)
	}
}

func TestWorktreeCheckRefuses(t *testing.T) {
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n", ".gitignore": "out/\n", "VERSION": "1.0.0\n",
		"sub/keep": "x\n",
	})
	path := add("refuse")
	for name, target := range map[string]string{
		"the main worktree": f.work,
		"a folder inside":   filepath.Join(path, "sub"),
		"no repository":     t.TempDir(),
		"no such folder":    filepath.Join(path, "gone"),
	} {
		out, code := f.exec(t.TempDir(), "", graftBin, "worktree", "check", "--json", target)
		if code == 0 || code == app.ExitWorktreeHolds {
			t.Errorf("%s: exit %d, a caller could read that as an answer:\n%s", name, code, out)
		}
	}
	if out, code := f.graft("", "worktree", "check"); code == 0 || code == app.ExitWorktreeHolds {
		t.Errorf("no path: exit %d:\n%s", code, out)
	}
}

func TestWorktreeCheckRulesFromTrunk(t *testing.T) {
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n", ".gitignore": "out/\n", "VERSION": "1.0.0\n",
	})
	path := add("rules")
	writeFile(t, filepath.Join(path, "out", "trace.bin"), "x")
	writeFile(t, filepath.Join(path, ".graft.yaml"), "schema: 1\nversion: {mode: file}\nworktree:\n  disposable: [out/]\n")
	treeCommit(f, path, "chore: release out")
	if answer, code, out := worktreeCheckJSON(t, f, f.work, path); code != app.ExitWorktreeHolds || answer.Clear {
		t.Fatalf("the tree's own config released its data; exit %d:\n%s", code, out)
	}
	if err := os.Remove(filepath.Join(path, ".graft.yaml")); err != nil {
		t.Fatal(err)
	}
	treeCommit(f, path, "chore: no config")
	if answer, code, out := worktreeCheckJSON(t, f, f.work, path); code != app.ExitWorktreeHolds || answer.Clear {
		t.Fatalf("a tree without a config got no answer; exit %d:\n%s", code, out)
	}
}

func TestWorktreeCheckOffline(t *testing.T) {
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n", ".gitignore": "out/\n", "VERSION": "1.0.0\n",
	})
	path := add("offline")
	f.git("remote", "set-url", "origin", filepath.Join(f.base, "no-such-remote"))
	if answer, code, out := worktreeCheckJSON(t, f, f.work, path); code != 0 || !answer.Clear {
		t.Fatalf("the check needed the remote; exit %d:\n%s", code, out)
	}
}

func TestWorktreeCheckSubmoduleAndLimit(t *testing.T) {
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n", ".gitignore": "out/\n", "VERSION": "1.0.0\n",
	})
	sub := add("submodule")
	head := strings.TrimSpace(f.git("rev-parse", "HEAD"))
	if out, err := gitx.InOther(sub, nil, "update-index", "--add", "--cacheinfo", "160000,"+head+",mod"); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	writeFile(t, filepath.Join(sub, "mod", "file"), "x")
	answer, code, out := worktreeCheckJSON(t, f, f.work, sub)
	if code != app.ExitWorktreeHolds || answer.Clear || len(answer.Submodules) != 1 || answer.Submodules[0] != "mod" {
		t.Fatalf("the submodule is not named; exit %d:\n%s", code, out)
	}

	many := add("limit")
	for i := range 1001 {
		writeFile(t, filepath.Join(many, "out", strconv.Itoa(i)), "x")
	}
	answer, code, out = worktreeCheckJSON(t, f, f.work, many)
	if code != app.ExitWorktreeHolds || !answer.Limited || answer.Files != 1000 || len(answer.Roots) != 1 || !answer.Roots[0].Partial {
		t.Fatalf("the limit is not said; exit %d:\n%s", code, out)
	}
}

func TestWorktreeCheckUnreadable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a folder this user cannot read")
	}
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n", ".gitignore": "out/\n", "VERSION": "1.0.0\n",
	})
	path := add("unreadable")
	locked := filepath.Join(path, "out", "locked")
	writeFile(t, filepath.Join(locked, "file"), "x")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	out, code := f.exec(f.work, "", graftBin, "worktree", "check", "--json", path)
	if code == 0 || code == app.ExitWorktreeHolds || !strings.Contains(out, "not read") {
		t.Fatalf("an unread folder must be neither clear nor an answer; exit %d:\n%s", code, out)
	}
}
