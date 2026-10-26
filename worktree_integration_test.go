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
	writeFile(t, filepath.Join(held, "notes.txt"), "n\n")
	f.git("-C", held, "add", "notes.txt")
	f.git("-C", held, "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "feat: notes")
	f.write("notes.txt", "n\n")
	f.mustGraft("commit", "-m", "feat: notes")
	reader := exec.Command("sh", "-c", "exec 3<notes.txt; cd / && exec sleep 60 3<&3")
	reader.Dir = held
	if err := reader.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Process.Kill(); _ = reader.Wait() })

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
		"in work   " + held + " (w/held): a process works in it (sleep, pid ",
		"in work   " + locked + " (w/locked): locked: job 7 runs here until it is done (git worktree unlock to let it go)",
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
	for _, kept := range []string{wt("open"), dirty, busy, held, locked, wt("look")} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s is gone: %v", kept, err)
		}
	}
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
			f.mustGraft("commit", "-m", "feat: base")
			f.git("remote", "set-head", "origin", "main")
			f.git("switch", "-q", "-c", "b/raced")
			f.write("raced.txt", "r\n")
			f.mustGraft("commit", "-m", "feat: raced work")
			later := f.git("commit-tree", "-p", "HEAD", "-m", "feat: more raced work", "HEAD^{tree}")
			f.git("push", "-q", "--no-verify", "origin", later+":refs/heads/later")
			f.git("switch", "-q", "main")
			f.write("raced.txt", "r\n")
			f.mustGraft("commit", "-m", "feat: raced work")

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

func TestWorktreeAddExplicitTrunk(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\n",
	})
	f.git("checkout", "-q", "-b", "main")
	f.mustGraft("commit", "-m", "feat: base")
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
	f.mustGraft("commit", "-m", "feat: base")
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
	f.mustGraft("commit", "-m", "feat: trunk moves")
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
	f.mustGraft("commit", "-m", "feat: trunk conflicts")
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
	f.mustGraft("commit", "-m", "feat: base")
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
	f.mustGraft("commit", "-m", "chore: copy a tracked file")
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
			f.mustGraft("commit", "-m", "feat: base")
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
