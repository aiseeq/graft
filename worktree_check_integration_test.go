package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/app"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

// worktreeCheckFiles is a project ignoring out/ and *.log, with no rules.
func worktreeCheckFiles() map[string]string {
	return map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n", ".gitignore": "out/\n*.log\n", "VERSION": "1.0.0\n",
		"sub/keep": "x\n",
	}
}

func worktreeGitDirOf(t *testing.T, f *fixture, path string) string {
	t.Helper()
	return f.gitIn(path, "rev-parse", "--path-format=absolute", "--git-dir")
}

// deinitializedSubmodule leaves what git submodule deinit does: an empty folder
// under a gitlink, and the submodule's commits in the worktree's git dir.
func deinitializedSubmodule(t *testing.T, f *fixture, path string) {
	t.Helper()
	head := f.git("rev-parse", "HEAD")
	f.gitIn(path, "update-index", "--add", "--cacheinfo", "160000,"+head+",mod")
	if err := os.MkdirAll(filepath.Join(path, "mod"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(worktreeGitDirOf(t, f, path), "modules", "mod", "HEAD"), "x")
}

func requireNoAnswer(t *testing.T, what, out string, code int) {
	t.Helper()
	if code == 0 || code == app.ExitWorktreeHolds {
		t.Errorf("%s: exit %d, a caller could read that as an answer:\n%s", what, code, out)
	}
}

// The promised status is the number 3, not whatever the constant says.
func TestWorktreeCheckExitNumber(t *testing.T) {
	f, add := newWorktreeCleanFixture(t, worktreeCheckFiles())
	path := add("number")
	writeFile(t, filepath.Join(path, "out", "file"), "x")
	if out, code := f.exec(f.work, "", graftBin, "worktree", "check", path); code != 3 {
		t.Fatalf("exit %d, the README promises 3:\n%s", code, out)
	}
}

func TestWorktreeCheckEmptyLists(t *testing.T) {
	f, add := newWorktreeCleanFixture(t, worktreeCheckFiles())
	_, code, out := worktreeCheckJSON(t, f, f.work, add("lists"))
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, field := range []string{`"submodules":[]`, `"roots":[]`, `"changes":[]`, `"hidden_changes":[]`, `"nested_worktrees":[]`} {
		if !strings.Contains(out, field) {
			t.Errorf("no %s, a caller iterating it would have to handle null:\n%s", field, out)
		}
	}
}

// A hook's GIT_DIR and GIT_INDEX_FILE must not make the answer another tree's.
func TestWorktreeCheckCallerGitEnvironment(t *testing.T) {
	f, add := newWorktreeCleanFixture(t, worktreeCheckFiles())
	other := worktreeGitDirOf(t, f, add("other"))
	path := add("env")
	deinitializedSubmodule(t, f, path)
	writeFile(t, filepath.Join(path, "out", "file"), "x")
	sub := filepath.Join(path, "sub")
	f.env = append(f.env, "GIT_DIR="+other, "GIT_INDEX_FILE="+filepath.Join(other, "index"))

	answer, code, out := worktreeCheckJSON(t, f, f.work, path)
	if code != app.ExitWorktreeHolds || answer.Clear || len(answer.Submodules) != 1 || answer.Files != 1 {
		t.Errorf("answered for the tree of GIT_DIR; exit %d:\n%s", code, out)
	}
	out, code = f.exec(f.work, "", graftBin, "worktree", "check", f.work)
	requireNoAnswer(t, "the main worktree", out, code)
	out, code = f.exec(f.work, "", graftBin, "worktree", "check", sub)
	requireNoAnswer(t, "a folder inside", out, code)
}

// The deleted-branch path through check, and its git dir, not only clean's.
func TestWorktreeCheckSubmoduleCommits(t *testing.T) {
	f, add := newWorktreeCleanFixture(t, worktreeCheckFiles())
	path := add("modules")
	deinitializedSubmodule(t, f, path)
	answer, code, out := worktreeCheckJSON(t, f, f.work, path)
	if code != app.ExitWorktreeHolds || answer.Clear || len(answer.Submodules) != 1 || !strings.HasPrefix(answer.Submodules[0], "data in ") {
		t.Fatalf("the commits in the git dir are not named; exit %d:\n%s", code, out)
	}
}

// A config in the tree must not choose whose rules judge the tree, nor stop the
// answer when it is broken.
func TestWorktreeCheckTrunkFoundByMainWorktree(t *testing.T) {
	files := worktreeCheckFiles()
	files[".graft.yaml"] = "schema: 1\nversion: {mode: file}\nenvs: {local: {}}\ndeploy:\n  remote: origin\n  targets:\n    test: {env: local, run: [\"true\"]}\n"
	f, add := newWorktreeCleanFixture(t, files)
	released := add("released")
	writeFile(t, filepath.Join(released, ".graft.yaml"), "schema: 1\nversion: {mode: file}\nworktree:\n  disposable: [out/]\n")
	treeCommit(f, released, "chore: release out")
	f.git("remote", "add", "other", filepath.Join(f.base, "no-such-remote"))
	f.git("update-ref", "refs/remotes/other/main", f.gitIn(released, "rev-parse", "HEAD"))
	f.git("symbolic-ref", "refs/remotes/other/HEAD", "refs/remotes/other/main")

	path := add("remote")
	writeFile(t, filepath.Join(path, "out", "file"), "x")
	writeFile(t, filepath.Join(path, ".graft.yaml"), "schema: 1\nversion: {mode: file}\nenvs: {local: {}}\ndeploy:\n  remote: other\n  targets:\n    test: {env: local, run: [\"true\"]}\n")
	treeCommit(f, path, "chore: choose the remote")
	if answer, code, out := worktreeCheckJSON(t, f, f.work, path); code != app.ExitWorktreeHolds || answer.Clear {
		t.Errorf("the tree chose the remote whose rules release its data; exit %d:\n%s", code, out)
	}
	writeFile(t, filepath.Join(path, ".graft.yaml"), "not: [valid")
	treeCommit(f, path, "chore: break the config")
	if answer, code, out := worktreeCheckJSON(t, f, f.work, path); code != app.ExitWorktreeHolds || answer.Clear {
		t.Errorf("a broken config in the tree stopped the answer; exit %d:\n%s", code, out)
	}
	// The named ref is the caller's choice.
	out, code := f.exec(f.work, "", graftBin, "worktree", "check", "--trunk", "other/main", path)
	if code != 0 {
		t.Errorf("--trunk did not apply its rules; exit %d:\n%s", code, out)
	}
	out, code = f.exec(f.work, "", graftBin, "worktree", "check", "--trunk", "nosuch", path)
	requireNoAnswer(t, "--trunk nosuch", out, code)
	if !strings.Contains(out, "no such commit") || strings.Contains(out, "%!") {
		t.Errorf("--trunk nosuch is not said plainly:\n%s", out)
	}
}

func TestWorktreeCheckMainWorktreeRules(t *testing.T) {
	files := worktreeCheckFiles()
	f, add := newWorktreeCleanFixture(t, files)
	path := add("too-old")
	writeFile(t, filepath.Join(f.work, ".graft.yaml"), "schema: 1\ngraft: \">=999.0.0\"\nversion: {mode: file}\n")
	out, code := f.exec(f.work, "", graftBin, "worktree", "check", path)
	requireNoAnswer(t, "a project needing a newer graft", out, code)
	if !strings.Contains(out, "999.0.0") {
		t.Errorf("the version it needs is not said:\n%s", out)
	}
}

// The state of an operation and commits only HEAD reaches go with the git dir,
// and git status --porcelain shows neither.
func TestWorktreeCheckOperationAndDetachedCommits(t *testing.T) {
	f, add := newWorktreeCleanFixture(t, worktreeCheckFiles())
	rebasing := add("rebasing")
	if err := os.Mkdir(filepath.Join(worktreeGitDirOf(t, f, rebasing), "rebase-merge"), 0o755); err != nil {
		t.Fatal(err)
	}
	answer, code, out := worktreeCheckJSON(t, f, f.work, rebasing)
	if code != app.ExitWorktreeHolds || answer.Clear || answer.Operation != "rebase" {
		t.Errorf("a rebase in progress is not named; exit %d:\n%s", code, out)
	}
	if text, code := f.exec(f.work, "", graftBin, "worktree", "check", rebasing); code != app.ExitWorktreeHolds || !strings.Contains(text, "a rebase in progress") {
		t.Errorf("the plain answer does not name the rebase; exit %d:\n%s", code, text)
	}

	detached := add("detached")
	f.gitIn(detached, "checkout", "-q", "--detach")
	if answer, code, out := worktreeCheckJSON(t, f, f.work, detached); code != 0 || !answer.Clear {
		t.Errorf("a detached HEAD on a branch's commit holds nothing; exit %d:\n%s", code, out)
	}
	treeCommit(f, detached, "chore: only here")
	answer, code, out = worktreeCheckJSON(t, f, f.work, detached)
	if code != app.ExitWorktreeHolds || answer.Clear || answer.DetachedCommits != 1 {
		t.Errorf("a commit only HEAD reaches is not counted; exit %d:\n%s", code, out)
	}
	if text, code := f.exec(f.work, "", graftBin, "worktree", "check", detached); code != app.ExitWorktreeHolds || !strings.Contains(text, "1 commit only its HEAD reaches") {
		t.Errorf("the plain answer does not count the commit; exit %d:\n%s", code, text)
	}
	f.gitIn(detached, "branch", "keep-it")
	if answer, code, out := worktreeCheckJSON(t, f, f.work, detached); code != 0 || !answer.Clear {
		t.Errorf("a branch now holds the commit; exit %d:\n%s", code, out)
	}
}

// A change under assume-unchanged or skip-worktree is not in git status; what a
// sparse checkout leaves out is not on disk, and not a change.
func TestWorktreeHiddenChanges(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("clean's process check reads /proc")
	}
	for flag, name := range map[string]string{"--assume-unchanged": "assumed", "--skip-worktree": "skipped"} {
		f, add := newWorktreeCleanFixture(t, worktreeCheckFiles())
		path := add(name)
		f.gitIn(path, "update-index", flag, "VERSION")
		writeFile(t, filepath.Join(path, "VERSION"), "local edit\n")

		answer, code, out := worktreeCheckJSON(t, f, f.work, path)
		if code != app.ExitWorktreeHolds || answer.Clear || len(answer.HiddenChanges) != 1 || answer.HiddenChanges[0] != "VERSION" {
			t.Errorf("%s: the file is not named; exit %d:\n%s", flag, code, out)
		}
		out, code = f.graft("", "worktree", "clean", "--min-age", "0s", "--apply")
		if code != 0 || !strings.Contains(outputLineForPath(out, path), "check") || !strings.Contains(outputLineForPath(out, path), "VERSION") {
			t.Errorf("%s: clean does not keep and name it; exit %d:\n%s", flag, code, out)
		}
		if data, err := os.ReadFile(filepath.Join(path, "VERSION")); err != nil || string(data) != "local edit\n" {
			t.Errorf("%s: the edit is gone: %q, %v", flag, data, err)
		}
	}

	f, add := newWorktreeCleanFixture(t, worktreeCheckFiles())
	sparse := add("sparse")
	f.gitIn(sparse, "sparse-checkout", "set", "--no-cone", "/VERSION", "/.gitignore", "/.graft.yaml")
	if _, err := os.Stat(filepath.Join(sparse, "sub", "keep")); !os.IsNotExist(err) {
		t.Fatalf("the sparse checkout left the file: %v", err)
	}
	if answer, code, out := worktreeCheckJSON(t, f, f.work, sparse); code != 0 || !answer.Clear {
		t.Errorf("a sparse checkout holds nothing; exit %d:\n%s", code, out)
	}
	out, code := f.graft("", "worktree", "clean", "--min-age", "0s", "--apply")
	if code != 0 || !strings.Contains(out, "removed   "+sparse) {
		t.Errorf("clean keeps a sparse checkout; exit %d:\n%s", code, out)
	}
}

// git ls-files warns of a folder it cannot read and exits 0.
func TestWorktreeUnreadableOutsideIgnored(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() == 0 {
		t.Skip("needs a folder this user cannot read, and clean's process check reads /proc")
	}
	f, add := newWorktreeCleanFixture(t, worktreeCheckFiles())
	path := add("unreadable-untracked")
	locked := filepath.Join(path, "untracked")
	writeFile(t, filepath.Join(locked, "data.log"), "x")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	out, code := f.exec(f.work, "", graftBin, "worktree", "check", path)
	requireNoAnswer(t, "an unread folder", out, code)
	if !strings.Contains(out, "not read") {
		t.Errorf("the unread folder is not said:\n%s", out)
	}
	out, code = f.graft("", "worktree", "clean", "--min-age", "0s")
	if code != 0 || !strings.Contains(outputLineForPath(out, path), "check") {
		t.Errorf("clean would remove what it could not read; exit %d:\n%s", code, out)
	}
}

func TestWorktreeCheckUnreadableSubmodule(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a folder this user cannot read")
	}
	f, add := newWorktreeCleanFixture(t, worktreeCheckFiles())
	path := add("unreadable-submodule")
	head := f.git("rev-parse", "HEAD")
	f.gitIn(path, "update-index", "--add", "--cacheinfo", "160000,"+head+",mod")
	mod := filepath.Join(path, "mod")
	if err := os.Mkdir(mod, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(mod, 0o755) })
	out, code := f.exec(f.work, "", graftBin, "worktree", "check", path)
	requireNoAnswer(t, "an unread submodule folder", out, code)
	if !strings.Contains(out, "submodules of") {
		t.Errorf("the submodule folder is not said:\n%s", out)
	}
}

// A .git leading to a neighbour's git dir would answer for the neighbour.
func TestWorktreeCheckForeignGitDir(t *testing.T) {
	f, add := newWorktreeCleanFixture(t, worktreeCheckFiles())
	neighbour := worktreeGitDirOf(t, f, add("neighbour"))
	path := add("foreign")
	writeFile(t, filepath.Join(path, ".git"), "gitdir: "+neighbour+"\n")
	out, code := f.exec(f.work, "", graftBin, "worktree", "check", path)
	requireNoAnswer(t, "a .git leading to a neighbour", out, code)
	if !strings.Contains(out, "leads to") {
		t.Errorf("the mismatch is not said:\n%s", out)
	}
}

func TestWorktreeCheckEmptyPathAndRelativeToC(t *testing.T) {
	f, add := newWorktreeCleanFixture(t, worktreeCheckFiles())
	path := add("relative")
	writeFile(t, filepath.Join(path, "out", "file"), "x")
	out, code := f.exec(path, "", graftBin, "worktree", "check", "")
	requireNoAnswer(t, "an empty path", out, code)

	rel, err := filepath.Rel(f.work, path)
	if err != nil {
		t.Fatal(err)
	}
	out, code = f.exec(t.TempDir(), "", graftBin, "-C", f.work, "worktree", "check", rel)
	if code != app.ExitWorktreeHolds || !strings.Contains(out, path) {
		t.Errorf("a relative path is not read from -C; exit %d:\n%s", code, out)
	}
}

// Past the limit the roots are only named or counted, and the age is unknown: a
// date from what the walk reached would look like the newest.
func TestWorktreeCheckPastTheLimit(t *testing.T) {
	f, add := newWorktreeCleanFixture(t, worktreeCheckFiles())
	path := add("past-limit")
	for i := range 1001 {
		writeFile(t, filepath.Join(path, "out", strconv.Itoa(i)), "x")
	}
	for i := range 7 {
		writeFile(t, filepath.Join(path, "z"+strconv.Itoa(i)+".log"), "x")
	}
	answer, code, out := worktreeCheckJSON(t, f, f.work, path)
	if code != app.ExitWorktreeHolds || !answer.Limited || answer.Newest != "" {
		t.Fatalf("the limit is not said or a date stands for the unknown; exit %d:\n%s", code, out)
	}
	if len(answer.Roots) != 5 || answer.MoreRoots != 3 {
		t.Fatalf("five roots named and three counted expected; exit %d:\n%s", code, out)
	}
	for _, root := range answer.Roots {
		if !root.Partial {
			t.Errorf("root %s past the limit is not partial:\n%s", root.Name, out)
		}
	}
}

// In a partial clone a blob it lacks would be fetched from the remote: the check
// says it cannot tell instead.
func TestWorktreeCheckPartialCloneStaysOffline(t *testing.T) {
	f, _ := newWorktreeCleanFixture(t, worktreeCheckFiles())
	remote := f.git("remote", "get-url", "origin")
	f.gitIn(remote, "config", "uploadpack.allowFilter", "true")
	f.gitIn(remote, "config", "uploadpack.allowAnySHA1InWant", "true")
	clone := filepath.Join(f.base, "partial")
	if out, err := gitx.InOther(f.base, nil, "clone", "-q", "-b", "main", "--filter=blob:none", "file://"+remote, clone); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	f.gitIn(clone, "remote", "set-head", "origin", "main")
	path := filepath.Join(f.base, "partial-tree")
	f.gitIn(clone, "worktree", "add", "-q", "-b", "w/partial", path)

	// The trunk moves on: its new config is known by hash only.
	f.write(".graft.yaml", "schema: 1\nversion: {mode: file}\nworktree:\n  disposable: [out/]\n")
	f.mustVersionedCommit(f.work, "chore: release out", false)
	f.gitIn(clone, "fetch", "-q", "origin")
	objects := func() string { return f.gitIn(clone, "count-objects", "-v") }
	before := objects()
	out, code := f.exec(f.base, "", graftBin, "worktree", "check", path)
	requireNoAnswer(t, "a config blob the clone lacks", out, code)
	if !strings.Contains(out, "reading .graft.yaml") {
		t.Errorf("the missing config is not said:\n%s", out)
	}
	if after := objects(); after != before {
		t.Errorf("the check fetched objects:\n%s\nthen\n%s", before, after)
	}
}

// treeCommit commits all a tree holds, what it ignores aside, past the hook.
func treeCommit(f *fixture, path, message string, flags ...string) {
	f.t.Helper()
	f.gitIn(path, "add", "-A")
	args := append([]string{"-c", "core.hooksPath=/dev/null", "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-q", "--allow-empty", "-m", message}, flags...)
	f.gitIn(path, args...)
}

// What git status shows is lost too, whatever it is set to hide.
func TestWorktreeCheckChanges(t *testing.T) {
	f, add := newWorktreeCleanFixture(t, worktreeCheckFiles())
	edited := add("edited-file")
	writeFile(t, filepath.Join(edited, "VERSION"), "edited\n")
	answer, code, out := worktreeCheckJSON(t, f, f.work, edited)
	if code != app.ExitWorktreeHolds || len(answer.Changes) != 1 || answer.Changes[0] != "VERSION" {
		t.Errorf("a changed file is not named; exit %d:\n%s", code, out)
	}
	if text, code := f.exec(f.work, "", graftBin, "worktree", "check", edited); code != app.ExitWorktreeHolds || !strings.Contains(text, "1 uncommitted change") {
		t.Errorf("the plain answer does not count the change; exit %d:\n%s", code, text)
	}

	f.git("config", "status.showUntrackedFiles", "no")
	untracked := add("untracked")
	writeFile(t, filepath.Join(untracked, "new.txt"), "x")
	answer, code, out = worktreeCheckJSON(t, f, f.work, untracked)
	if code != app.ExitWorktreeHolds || len(answer.Changes) != 1 || answer.Changes[0] != "new.txt" {
		t.Errorf("an untracked file under status.showUntrackedFiles=no is not named; exit %d:\n%s", code, out)
	}
}

func TestWorktreeCheckOperations(t *testing.T) {
	f, add := newWorktreeCleanFixture(t, worktreeCheckFiles())
	for _, op := range []struct{ marker, name string }{
		{"rebase-merge/x", "rebase"},
		{"rebase-apply/x", "rebase"},
		{"rebase-apply/applying", "am"},
		{"MERGE_HEAD", "merge"},
		{"CHERRY_PICK_HEAD", "cherry-pick"},
		{"REVERT_HEAD", "revert"},
		{"sequencer/todo", "cherry-pick or revert series"},
		{"BISECT_LOG", "bisect"},
	} {
		path := add(strings.ReplaceAll(strings.ReplaceAll(op.marker, "/", "-"), "_", "-"))
		writeFile(t, filepath.Join(worktreeGitDirOf(t, f, path), filepath.FromSlash(op.marker)), "x")
		out, code := f.exec(f.work, "", graftBin, "worktree", "check", "--json", path)
		if code != app.ExitWorktreeHolds || !strings.Contains(out, `"operation":"`+op.name+`"`) {
			t.Errorf("%s is not named %q; exit %d:\n%s", op.marker, op.name, code, out)
		}
		out, code = f.graft("", "worktree", "clean", "--min-age", "0s")
		if code != 0 || !strings.Contains(outputLineForPath(out, path), "a "+op.name+" in progress") {
			t.Errorf("%s: clean does not keep it; exit %d:\n%s", op.marker, code, out)
		}
	}
}

func TestWorktreeCheckCommitsThatStay(t *testing.T) {
	f, add := newWorktreeCleanFixture(t, worktreeCheckFiles())
	for name, keep := range map[string]func(path string){
		"a tag":    func(path string) { f.gitIn(path, "tag", "kept-by-tag") },
		"a remote": func(path string) { f.gitIn(path, "update-ref", "refs/remotes/origin/kept", "HEAD") },
	} {
		path := add(strings.ReplaceAll(name, " ", "-"))
		f.gitIn(path, "checkout", "-q", "--detach")
		treeCommit(f, path, "chore: here for "+name)
		if answer, code, out := worktreeCheckJSON(t, f, f.work, path); code != app.ExitWorktreeHolds || answer.DetachedCommits != 1 {
			t.Errorf("%s: before it the commit is only here; exit %d:\n%s", name, code, out)
		}
		keep(path)
		if answer, code, out := worktreeCheckJSON(t, f, f.work, path); code != 0 || !answer.Clear {
			t.Errorf("%s holds the commit; exit %d:\n%s", name, code, out)
		}
	}
}

// A branch deleted under a tree leaves its commits to the tree's reflog; a new
// orphan branch has nothing to lose; a HEAD leading nowhere is no answer.
func TestWorktreeCheckHeadStates(t *testing.T) {
	f, add := newWorktreeCleanFixture(t, worktreeCheckFiles())
	deleted := add("deleted")
	treeCommit(f, deleted, "chore: on the branch")
	f.gitIn(deleted, "update-ref", "-d", "refs/heads/w/deleted")
	f.gitIn(deleted, "rm", "-rfq", ".")
	if answer, code, out := worktreeCheckJSON(t, f, f.work, deleted); code != app.ExitWorktreeHolds || answer.DetachedCommits != 1 {
		t.Errorf("the commit of a deleted branch is not counted; exit %d:\n%s", code, out)
	}
	out, code := f.graft("", "worktree", "clean", "--min-age", "0s")
	if code != 0 || !strings.Contains(outputLineForPath(out, deleted), "no commit yet") {
		t.Errorf("clean does not say what it holds; exit %d:\n%s", code, out)
	}

	orphan := filepath.Join(f.base, "wt-orphan")
	f.git("worktree", "add", "-q", "--orphan", "-b", "w/orphan", orphan)
	if answer, code, out := worktreeCheckJSON(t, f, f.work, orphan); code != 0 || !answer.Clear {
		t.Errorf("a new orphan branch holds nothing; exit %d:\n%s", code, out)
	}

	missing := strings.Repeat("ab", 20)
	garbage := add("garbage-head")
	writeFile(t, filepath.Join(worktreeGitDirOf(t, f, garbage), "HEAD"), missing+"\n")
	out, code = f.exec(f.work, "", graftBin, "worktree", "check", garbage)
	requireNoAnswer(t, "a HEAD with no such object", out, code)

	broken := add("broken-ref")
	writeFile(t, filepath.Join(f.work, ".git", "refs", "heads", "w", "broken-ref"), missing+"\n")
	out, code = f.exec(f.work, "", graftBin, "worktree", "check", broken)
	requireNoAnswer(t, "a branch at no such object", out, code)
}

func TestWorktreeOwnRefs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("clean's process check reads /proc")
	}
	f, add := newWorktreeCleanFixture(t, worktreeCheckFiles())
	path := add("own-ref")
	treeCommit(f, path, "chore: under its ref")
	f.gitIn(path, "update-ref", "refs/worktree/kept", "HEAD")
	f.gitIn(path, "reset", "-q", "--hard", "HEAD~1")
	answer, code, out := worktreeCheckJSON(t, f, f.work, path)
	if code != app.ExitWorktreeHolds || answer.DetachedCommits != 1 {
		t.Errorf("a commit under refs/worktree is not counted; exit %d:\n%s", code, out)
	}
	out, code = f.graft("", "worktree", "clean", "--min-age", "0s")
	if code != 0 || !strings.Contains(outputLineForPath(out, path), "refs/worktree") {
		t.Errorf("clean would remove it; exit %d:\n%s", code, out)
	}
}

func TestWorktreeCheckRelativeGitdirAndNoMainConfig(t *testing.T) {
	f, add := newWorktreeCleanFixture(t, worktreeCheckFiles())
	path := add("relative-link")
	admin := worktreeGitDirOf(t, f, path)
	rel, err := filepath.Rel(admin, filepath.Join(path, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(admin, "gitdir"), rel+"\n")
	if answer, code, out := worktreeCheckJSON(t, f, f.work, path); code != 0 || !answer.Clear {
		t.Errorf("a relative gitdir is a tree's own; exit %d:\n%s", code, out)
	}
	if err := os.Remove(filepath.Join(f.work, ".graft.yaml")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(path, "out", "file"), "x")
	if answer, code, out := worktreeCheckJSON(t, f, f.work, path); code != app.ExitWorktreeHolds || answer.Clear {
		t.Errorf("no config in the main worktree stopped the answer; exit %d:\n%s", code, out)
	}
}

// Removing a tree takes a tree inside it, whatever rules let its folder go.
func TestWorktreeNested(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("clean's process check reads /proc")
	}
	files := worktreeCheckFiles()
	files[".graft.yaml"] = "schema: 1\nversion: {mode: file}\nworktree:\n  disposable: [/trees/]\n"
	files[".gitignore"] = "trees/\n"
	f, add := newWorktreeCleanFixture(t, files)
	outer := add("outer")
	inner := filepath.Join(outer, "trees", "inner")
	f.git("worktree", "add", "-q", "-b", "w/inner", inner, "main")
	answer, code, out := worktreeCheckJSON(t, f, f.work, outer)
	if code != app.ExitWorktreeHolds || len(answer.NestedWorktrees) != 1 {
		t.Errorf("the tree inside is not named; exit %d:\n%s", code, out)
	}
	out, code = f.graft("", "worktree", "clean", "--min-age", "0s", "--apply")
	if code != 0 || !strings.Contains(outputLineForPath(out, outer), "holds the worktree") {
		t.Errorf("clean does not keep it; exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(outer); err != nil {
		t.Errorf("clean took the outer tree: %v", err)
	}
}

// A sparse definition must not make a change on disk look merged: git add -A
// leaves such a file out.
func TestWorktreeCleanSparseChange(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("clean's process check reads /proc")
	}
	f, add := newWorktreeCleanFixture(t, worktreeCheckFiles())
	path := add("sparse-change")
	f.gitIn(path, "sparse-checkout", "set", "--no-cone", "/VERSION", "/.gitignore", "/.graft.yaml")
	writeFile(t, filepath.Join(path, "sub", "keep"), "PRECIOUS\n")
	out, code := f.graft("", "worktree", "clean", "--min-age", "0s", "--apply")
	if data, err := os.ReadFile(filepath.Join(path, "sub", "keep")); err != nil || string(data) != "PRECIOUS\n" {
		t.Fatalf("the change is gone: %q, %v; exit %d:\n%s", data, err, code, out)
	}
}

// Clean run from a linked tree must not let that tree choose whose rules judge
// its neighbours.
func TestWorktreeCleanTrunkFoundByMainWorktree(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("clean's process check reads /proc")
	}
	deploy := func(remote string) string {
		return "schema: 1\nversion: {mode: file}\nenvs: {local: {}}\ndeploy:\n  remote: " + remote + "\n  targets:\n    test: {env: local, run: [\"true\"]}\n"
	}
	files := worktreeCheckFiles()
	files[".graft.yaml"] = deploy("origin")
	f, add := newWorktreeCleanFixture(t, files)
	released := add("released")
	writeFile(t, filepath.Join(released, ".graft.yaml"), "schema: 1\nversion: {mode: file}\nworktree:\n  disposable: [out/]\n")
	treeCommit(f, released, "chore: release out")
	other := filepath.Join(f.base, "other.git")
	f.gitIn(f.base, "init", "-q", "--bare", other)
	f.git("remote", "add", "other", other)
	f.gitIn(released, "push", "-q", "other", "HEAD:refs/heads/main")
	f.git("fetch", "-q", "other")
	f.git("remote", "set-head", "other", "main")

	neighbour := add("neighbour")
	writeFile(t, filepath.Join(neighbour, "out", "file"), "x")
	chooser := add("chooser")
	writeFile(t, filepath.Join(chooser, ".graft.yaml"), deploy("other"))
	treeCommit(f, chooser, "chore: choose")
	out, code := f.exec(chooser, "", graftBin, "worktree", "clean", "--min-age", "0s", "--apply")
	if _, err := os.Stat(filepath.Join(neighbour, "out", "file")); err != nil {
		t.Fatalf("the data went by the rules the linked tree chose: %v; exit %d:\n%s", err, code, out)
	}
}

// git lists an unreadable tree as prunable, and git worktree prune has no way to
// spare it.
func TestWorktreeCleanUnreadableTreeNotPruned(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() == 0 {
		t.Skip("needs a folder this user cannot read, and clean's process check reads /proc")
	}
	f, add := newWorktreeCleanFixture(t, worktreeCheckFiles())
	locked := add("locked-up")
	gone := add("really-gone")
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	out, code := f.graft("", "worktree", "clean", "--min-age", "0s", "--apply")
	if code != 0 || !strings.Contains(outputLineForPath(out, locked), "check") || strings.Contains(out, "pruned") {
		t.Fatalf("clean pruned a tree that is there; exit %d:\n%s", code, out)
	}
	if err := os.Chmod(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.git("worktree", "list"), locked) {
		t.Fatalf("git forgot the tree:\n%s", out)
	}
}

// Clean run from a hook of a linked tree judges the neighbours by their own git
// dir, not the hook's.
func TestWorktreeCleanFromAHook(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("clean's process check reads /proc")
	}
	f, add := newWorktreeCleanFixture(t, worktreeCheckFiles())
	caller := add("caller")
	rebasing := add("neighbour-rebasing")
	writeFile(t, filepath.Join(worktreeGitDirOf(t, f, rebasing), "rebase-merge", "x"), "x")
	admin := worktreeGitDirOf(t, f, caller)
	f.env = append(f.env, "GIT_DIR="+admin, "GIT_INDEX_FILE="+filepath.Join(admin, "index"))
	out, code := f.exec(caller, "", graftBin, "worktree", "clean", "--min-age", "0s", "--apply")
	if code != 0 || !strings.Contains(outputLineForPath(out, rebasing), "a rebase in progress") {
		t.Fatalf("the rebase next door went unseen; exit %d:\n%s", code, out)
	}
}
