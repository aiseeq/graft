package main

import (
	"strings"
	"testing"
)

func sparseCommitRepo(t *testing.T, gate string) *fixture {
	t.Helper()
	f := newRepo(t, map[string]string{
		".graft.yaml":     "schema: 1\nversion: {mode: none}\ngate:\n" + gate,
		".gitignore":      "other/*.log\n",
		"keep/inside.txt": "base\n",
		"other/o.txt":     "outside sparse checkout\n",
	})
	f.git("-c", "core.hooksPath=/dev/null", "add", "-A")
	f.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "chore: base")
	f.git("sparse-checkout", "set", "--cone", "keep")
	return f
}

func checkSparseIndex(t *testing.T, f *fixture) {
	t.Helper()
	if got := f.git("status", "--short"); got != "" {
		t.Errorf("sparse checkout has work-tree changes after commit:\n%s", got)
	}
	if got := f.git("ls-files", "-t", "other/o.txt"); got != "S other/o.txt" {
		t.Errorf("other/o.txt index flag = %q, want S", got)
	}
	if _, code := f.tryGit("cat-file", "-e", "HEAD:other/o.txt"); code != 0 {
		t.Errorf("other/o.txt is missing from HEAD")
	}
}

func sparseHeadMovesRepo(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newRepo(t, map[string]string{
		".graft.yaml":     "schema: 1\nversion: {mode: none}\ngate:\n  - [git, merge, -q, --ff-only, side]\n",
		"keep/inside.txt": "base\n",
		"other/o.txt":     "outside sparse checkout\n",
	})
	f.git("-c", "core.hooksPath=/dev/null", "add", "-A")
	f.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "chore: base")
	branch := f.git("symbolic-ref", "--short", "HEAD")
	f.git("checkout", "-q", "-b", "side")
	f.write("side.txt", "merged outside sparse checkout\n")
	f.git("-c", "core.hooksPath=/dev/null", "add", "-A")
	f.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "feat: side")
	tip := f.git("rev-parse", "HEAD")
	f.git("checkout", "-q", branch)
	f.git("sparse-checkout", "set", "--cone", "keep")
	return f, tip
}

func TestCommitKeepsSparseIndexFlagsAcrossCommits(t *testing.T) {
	f := sparseCommitRepo(t, "  - [sh, -c, 'true']\n")
	if got := f.git("ls-files", "-t", "other/o.txt"); got != "S other/o.txt" {
		t.Fatalf("test setup did not mark other/o.txt skipped: %q", got)
	}
	if got := f.git("ls-files", "--error-unmatch", "keep/inside.txt"); strings.TrimSpace(got) == "" {
		t.Fatal("test setup did not keep the in-scope file")
	}

	f.write("keep/inside.txt", "first change\n")
	f.mustGraft("commit", "-m", "feat: first")
	checkSparseIndex(t, f)

	f.write("keep/inside.txt", "second change\n")
	f.mustGraft("commit", "-m", "feat: second")
	checkSparseIndex(t, f)
}

func TestRestageOverMovedHeadKeepsSparseIndexFlags(t *testing.T) {
	f, tip := sparseHeadMovesRepo(t)
	if got := f.git("ls-files", "-t", "other/o.txt"); got != "S other/o.txt" {
		t.Fatalf("other/o.txt is not outside the sparse checkout: %q", got)
	}
	f.write("keep/inside.txt", "changed\n")
	out, code := f.graft("", "commit", "-m", "feat: mine")
	if code == 0 || !strings.Contains(out, "HEAD moved from ") {
		t.Fatalf("commit did not refuse the moved HEAD (code %d):\n%s", code, out)
	}
	if got := f.git("rev-parse", "HEAD"); got != tip {
		t.Fatalf("HEAD = %s, want merged commit %s", got, tip)
	}
	if got := f.git("ls-files", "-t", "other/o.txt"); got != "S other/o.txt" {
		t.Errorf("other/o.txt index flag after restaging = %q, want S", got)
	}
	if got := f.git("status", "--short"); strings.Contains(got, "other/o.txt") {
		t.Errorf("sparse other/o.txt appears in status after restaging:\n%s", got)
	}
}

func TestCommitKeepsIgnoredFileOutsideSparseCone(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml":     "schema: 1\nversion: {mode: none}\n",
		".gitignore":      "other/*.log\n",
		"keep/inside.txt": "base\n",
		"other/o.txt":     "tracked outside sparse checkout\n",
	})
	f.git("-c", "core.hooksPath=/dev/null", "add", "-A")
	f.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "chore: base")
	f.git("sparse-checkout", "set", "--cone", "keep")
	f.write("other/local.log", "keep this ignored file\n")
	f.write("keep/inside.txt", "changed\n")
	f.mustGraft("commit", "-m", "feat: change")
	if got := f.read("other/local.log"); got != "keep this ignored file\n" {
		t.Errorf("ignored file outside the cone = %q, want its contents preserved", got)
	}
}

func TestCommitKeepsManualSkipWorktreeAcrossCommits(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml":     "schema: 1\nversion: {mode: none}\n",
		"keep/inside.txt": "base\n",
		"other/p.txt":     "committed content\n",
	})
	f.git("-c", "core.hooksPath=/dev/null", "add", "-A")
	f.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "chore: base")
	f.git("update-index", "--skip-worktree", "other/p.txt")
	f.write("other/p.txt", "local hidden change\n")
	f.write("keep/inside.txt", "first change\n")
	f.mustGraft("commit", "-m", "feat: first")

	f.write("keep/inside.txt", "second change\n")
	f.mustGraft("commit", "-m", "feat: second")
	if got := f.git("ls-files", "-t", "other/p.txt"); got != "S other/p.txt" {
		t.Errorf("manual skip-worktree flag = %q, want S", got)
	}
	if got := f.git("show", "HEAD:other/p.txt"); got != "committed content" {
		t.Errorf("HEAD:other/p.txt = %q, want the original committed content", got)
	}
}

func TestCommitKeepsSparseFlagsWhenGateChangesKeptPath(t *testing.T) {
	f := sparseCommitRepo(t, "  - [sh, -c, 'echo gate >> keep/inside.txt']\n")
	f.write("other/local.log", "ignored data\n")
	f.write("keep/inside.txt", "mine\n")
	f.mustGraft("commit", "-m", "feat: gate edit")
	if got := f.git("ls-files", "-t", "other/o.txt"); got != "S other/o.txt" {
		t.Errorf("other/o.txt index flag after kept-path staging = %q, want S", got)
	}
	if got := f.git("show", "HEAD:keep/inside.txt"); got != "mine\ngate" {
		t.Errorf("HEAD:keep/inside.txt = %q, want gate's appended content", got)
	}
	if got := f.read("other/local.log"); got != "ignored data\n" {
		t.Errorf("ignored file outside the cone = %q, want its contents preserved", got)
	}
}

func TestRestageOverMovedHeadKeepsManualSkipWorktreeChangeOut(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml":     "schema: 1\nversion: {mode: none}\ngate:\n  - [git, merge, -q, --ff-only, side]\n",
		"keep/inside.txt": "base\n",
		"other/p.txt":     "committed content\n",
	})
	f.git("-c", "core.hooksPath=/dev/null", "add", "-A")
	f.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "chore: base")
	branch := f.git("symbolic-ref", "--short", "HEAD")
	f.git("checkout", "-q", "-b", "side")
	f.write("side.txt", "side\n")
	f.git("-c", "core.hooksPath=/dev/null", "add", "-A")
	f.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "feat: side")
	f.git("checkout", "-q", branch)
	f.git("update-index", "--skip-worktree", "other/p.txt")
	f.write("other/p.txt", "local hidden change\n")
	f.write("keep/inside.txt", "changed\n")
	out, code := f.graft("", "commit", "-m", "feat: mine")
	if code == 0 || !strings.Contains(out, "HEAD moved from ") {
		t.Fatalf("commit did not refuse the moved HEAD (code %d):\n%s", code, out)
	}
	if got := f.git("show", ":other/p.txt"); got != "committed content" {
		t.Errorf("index other/p.txt after restaging = %q, want the committed content", got)
	}
	if got := f.git("ls-files", "-t", "other/p.txt"); got != "S other/p.txt" {
		t.Errorf("manual skip-worktree flag after restaging = %q, want S", got)
	}
	f.write(".graft.yaml", "schema: 1\nversion: {mode: none}\n")
	f.mustGraft("commit", "-m", "feat: mine")
	if got := f.git("show", "HEAD:other/p.txt"); got != "committed content" {
		t.Errorf("HEAD:other/p.txt = %q, want the committed content, not the hidden change", got)
	}
}
