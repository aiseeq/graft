package main

import (
	"runtime"
	"strings"
	"testing"
)

// TestCommitLeavesOutWhatTheGateWritesUnderAStartPath: a path changed when
// the commit started that the gate replaces with a directory goes in as the
// gate left it, removed; the files under the new directory, written only
// while the gate ran, stay out of the commit and in the tree.
func TestCommitLeavesOutWhatTheGateWritesUnderAStartPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\ngate:\n  - [sh, -c, 'rm x && mkdir x && echo late > x/y']\n",
		"x":           "base\n",
	})
	f.git("-c", "core.hooksPath=/dev/null", "add", "-A")
	f.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "chore: base")
	f.write("x", "mine\n")
	out := f.mustGraft("commit", "-m", "feat: mine")
	if got := f.git("ls-tree", "-r", "--name-only", "HEAD"); got != ".graft.yaml" {
		t.Errorf("HEAD has %q, want x removed as the gate left it and nothing under x/\n%s", got, out)
	}
	for _, want := range []string{"x changed while the gate ran", "left out x/y:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if status := f.run(f.work, "git", "status", "--porcelain"); !strings.Contains(status, "?? x/") {
		t.Errorf("x/y is not left in the tree, untracked:\n%s", status)
	}
}

// TestAmendCommitsTheGatesWritesToTheAmendedCommit: amend starts from the
// whole amended commit, against its parent: a gate step that formats a file
// of that commit, clean in the work tree, puts its write in.
func TestAmendCommitsTheGatesWritesToTheAmendedCommit(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\ngate:\n" + gateStep("append", "h.txt", "formatted"),
		"clean.txt":   "clean\n",
	})
	f.git("-c", "core.hooksPath=/dev/null", "add", "-A")
	f.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "chore: base")
	f.write("h.txt", "raw\n")
	f.git("-c", "core.hooksPath=/dev/null", "add", "-A")
	f.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "fix: local")
	f.write("mine.txt", "mine\n")
	out := f.mustGraft("amend")
	if got := f.git("show", "HEAD:h.txt"); got != "raw\nformatted" {
		t.Errorf("HEAD:h.txt = %q, want the amended commit's file as the gate left it\n%s", got, out)
	}
	if !strings.Contains(out, "h.txt changed while the gate ran") || strings.Contains(out, "left out h.txt") {
		t.Errorf("output does not say h.txt went in as the gate left it:\n%s", out)
	}
	if got := f.git("log", "--format=%s"); got != "fix: local\nchore: base" {
		t.Errorf("amend did not rewrite the last commit:\n%s", got)
	}
	if status := f.run(f.work, "git", "status", "--porcelain"); status != "" {
		t.Errorf("the tree is not committed whole:\n%s", status)
	}
}

// TestCommitWhoseGateUndoesItsChangesHasNothingToCommit: when the gate puts
// every path back as HEAD has it, graft says there is nothing to commit, and
// the version stays as it is.
func TestCommitWhoseGateUndoesItsChangesHasNothingToCommit(t *testing.T) {
	f := fileModeRepo(t, gateStep("touch", "empty.txt"))
	f.write("empty.txt", "")
	f.mustGraft("commit", "-m", "feat: x")
	f.write("empty.txt", "not empty\n")
	out, code := f.graft("", "commit", "-m", "feat: again")
	if code == 0 || !strings.Contains(out, "nothing to commit") {
		t.Errorf("exit %d, want nothing to commit:\n%s", code, out)
	}
	if n := f.commitCount(); n != 1 {
		t.Errorf("%d commits, want the first only\n%s", n, out)
	}
	if v := f.read("VERSION"); v != "1.2.4\n" {
		t.Errorf("VERSION = %q, want the first commit's bump only", v)
	}
}
