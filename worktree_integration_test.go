package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestWorktreeClean: a worktree goes, with its branch, only when its tree is
// clean, no process works in it and merging it into the trunk gives the
// trunk's tree, the version files aside: content, not ancestry. A worktree
// whose commit subjects are all in the trunk but whose content is not is
// shown apart, for a look; branches without a worktree go the same way.
func TestWorktreeClean(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process check reads /proc")
	}
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n",
		"VERSION":     "1.0.0\n",
		"shared.txt":  "one\n",
	}, "origin")
	f.git("checkout", "-q", "-b", "main")
	f.mustGraft("commit", "-m", "feat: base")
	f.git("remote", "set-head", "origin", "main")

	wt := func(name string) string { return filepath.Join(f.base, "wt-"+name) }
	add := func(name string) string {
		f.git("worktree", "add", "-q", "-b", "w/"+name, wt(name), "main")
		return wt(name)
	}
	commitIn := func(dir, file, content, msg string) {
		writeFile(t, filepath.Join(dir, file), content)
		f.exec(dir, "", graftBin, "commit", "-m", msg)
	}

	// Merged by content under another hash and another version.
	commitIn(add("merged"), "merged.txt", "m\n", "feat: merged work")
	// Not merged.
	commitIn(add("open"), "open.txt", "o\n", "feat: open work")
	// Merged, but with an uncommitted file.
	dirty := add("dirty")
	writeFile(t, filepath.Join(dirty, "scratch.txt"), "s\n")
	// Merged, but a process works in it.
	busy := add("busy")
	// A commit whose subject the trunk has, with other content.
	commitIn(add("look"), "shared.txt", "two\n", "feat: shared change")
	// Branches without a worktree.
	f.git("branch", "b/old", "main")
	f.git("branch", "b/new", "main")
	f.git("switch", "-q", "b/new")
	f.write("new.txt", "n\n")
	f.mustGraft("commit", "-m", "feat: new work")
	f.git("switch", "-q", "main")
	f.write("merged.txt", "m\n")
	f.mustGraft("commit", "-m", "feat: merged work, rewritten")
	f.write("other.txt", "three\n")
	f.mustGraft("commit", "-m", "feat: shared change")

	sleep := exec.Command("sleep", "60")
	sleep.Dir = busy
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sleep.Process.Kill(); _ = sleep.Wait() })

	// Fresh worktrees wait an hour by default.
	out := f.mustGraft("worktree", "clean")
	if !strings.Contains(out, "in work   "+wt("merged")+" (w/merged): used less than 1h0m0s ago") {
		t.Errorf("a fresh worktree:\n%s", out)
	}

	out = f.mustGraft("worktree", "clean", "--min-age", "0s")
	for _, want := range []string{
		"would remove " + wt("merged") + " (w/merged): clean, in origin/main",
		"unmerged  " + wt("open") + " (w/open): adds +1 -0 lines to origin/main",
		"in work   " + dirty + " (w/dirty): 1 uncommitted change",
		"in work   " + busy + " (w/busy): a process works in it (sleep, pid ",
		"check     " + wt("look") + " (w/look): its commits are all in origin/main by subject, but it adds +1 -1 lines to origin/main: looks merged, check",
		"would remove branch b/old: in origin/main",
		"unmerged  branch b/new: adds +1 -0 lines to origin/main",
		"dry run: graft worktree clean --apply removes 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run: no %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(wt("merged")); err != nil {
		t.Errorf("the dry run removed a worktree: %v", err)
	}

	out = f.mustGraft("worktree", "clean", "--min-age", "0s", "--apply")
	if !strings.Contains(out, "removed   "+wt("merged")+" (w/merged)") || !strings.Contains(out, "removed   branch b/old") {
		t.Errorf("apply:\n%s", out)
	}
	if _, err := os.Stat(wt("merged")); !os.IsNotExist(err) {
		t.Errorf("the merged worktree is still there: %v", err)
	}
	for _, branch := range []string{"w/merged", "b/old"} {
		if _, code := f.tryGit("rev-parse", "--verify", "-q", "refs/heads/"+branch); code == 0 {
			t.Errorf("branch %s is still there", branch)
		}
	}
	if _, code := f.tryGit("ls-remote", "--exit-code", "origin", "refs/heads/w/merged"); code == 0 {
		t.Error("w/merged is still on origin")
	}
	for _, kept := range []string{wt("open"), dirty, busy, wt("look")} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s is gone: %v", kept, err)
		}
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
	f.mustGraft("commit", "-m", "feat: base")
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
