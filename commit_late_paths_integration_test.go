package main

import (
	"strings"
	"testing"
)

// lateWritesGate is a gate whose steps write as a formatter does to a path
// changed before the commit, and as another writer does to a new path and to
// a path the commit did not change.
func lateWritesGate() string {
	return gateStep("append", "mine.txt", "formatted") +
		gateStep("touch", "late.txt") +
		gateStep("append", "clean.txt", "late edit")
}

// lateWritesRepo is a repository with one commit made outside graft, so the
// gate's writes are the only thing on top of the change under test.
func lateWritesRepo(t *testing.T) *fixture {
	t.Helper()
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\ngate:\n" + lateWritesGate(),
		"clean.txt":   "clean\n",
	})
	f.git("-c", "core.hooksPath=/dev/null", "add", "-A")
	f.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "chore: base")
	return f
}

// checkLateWritesLeftOut checks that the commit has mine.txt as the gate left
// it and neither the new path nor the clean one, which stay in the tree as
// they are, each named in out.
func checkLateWritesLeftOut(t *testing.T, f *fixture, out, mine string) {
	t.Helper()
	if got := f.git("show", "HEAD:mine.txt"); got != mine+"\nformatted" {
		t.Errorf("HEAD:mine.txt = %q, want the change with the gate's write", got)
	}
	if _, code := f.tryGit("cat-file", "-e", "HEAD:late.txt"); code == 0 {
		t.Errorf("late.txt, written while the gate ran, is in the commit")
	}
	if got := f.git("show", "HEAD:clean.txt"); got != "clean" {
		t.Errorf("HEAD:clean.txt = %q, want the committed content without the late edit", got)
	}
	// Not f.git: trimming would take the status column of the first line.
	status := f.run(f.work, "git", "status", "--porcelain")
	for _, want := range []string{"?? late.txt", " M clean.txt"} {
		if !strings.Contains(status, want) {
			t.Errorf("status lacks %q: the late write is not left in the tree as it is:\n%s", want, status)
		}
	}
	if strings.Contains(status, "mine.txt") {
		t.Errorf("mine.txt is not committed whole:\n%s", status)
	}
	for _, want := range []string{"left out late.txt:", "left out clean.txt:", "mine.txt changed while the gate ran"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "left out mine.txt") {
		t.Errorf("mine.txt, changed when the commit started, is left out:\n%s", out)
	}
}

// TestCommitLeavesOutPathsWrittenDuringTheGate: graft commit commits the
// paths that were changed when it started, as the gate left them (a
// formatter's write goes in); a path created or changed only while the gate
// ran is another writer's and stays in the tree, out of the commit.
func TestCommitLeavesOutPathsWrittenDuringTheGate(t *testing.T) {
	f := lateWritesRepo(t)
	f.write("mine.txt", "mine\n")
	out := f.mustGraft("commit", "-m", "feat: mine")
	checkLateWritesLeftOut(t, f, out, "mine")

	// The next commit starts with them changed: they go in.
	out = f.mustGraft("commit", "-m", "feat: the late ones")
	if _, code := f.tryGit("cat-file", "-e", "HEAD:late.txt"); code != 0 {
		t.Errorf("late.txt, changed when the next commit started, is not in it:\n%s", out)
	}
	if got := f.git("show", "HEAD:clean.txt"); !strings.HasPrefix(got, "clean\nlate edit") {
		t.Errorf("HEAD:clean.txt = %q, want the late edit in the next commit", got)
	}
}

// TestAmendLeavesOutPathsWrittenDuringTheGate: graft amend stages as graft
// commit does.
func TestAmendLeavesOutPathsWrittenDuringTheGate(t *testing.T) {
	f := lateWritesRepo(t)
	f.write("mine.txt", "first\n")
	f.git("-c", "core.hooksPath=/dev/null", "add", "-A")
	f.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "fix: local")
	f.write("mine.txt", "amended\n")
	out := f.mustGraft("amend")
	checkLateWritesLeftOut(t, f, out, "amended")
	if got := f.git("log", "--format=%s"); got != "fix: local\nchore: base" {
		t.Errorf("amend did not rewrite the last commit:\n%s", got)
	}
}
