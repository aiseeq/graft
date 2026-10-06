package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// workBranchRepo has a trunk that bumps the version and work branches w/*
// that neither bump nor mind being rebased.
func workBranchRepo(t *testing.T) *fixture {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion:\n  mode: file\n  skip_branches: ['w/*']\n" +
			"  sync:\n    - {path: version.go, format: regex, pattern: 'Version = \"([^\"]*)\"'}\n" +
			"push:\n  force_with_lease: ['w/*']\ngate: []\n",
		"VERSION":    "1.0.0\n",
		"version.go": "package main\n\nconst Version = \"1.0.0\"\n",
	}, "origin")
	f.git("checkout", "-q", "-b", "main")
	f.mustGraft("commit", "-m", "feat: base")
	return f
}

func TestWorkBranchLeavesVersionAndPushesWithLease(t *testing.T) {
	f := workBranchRepo(t)
	f.git("checkout", "-q", "-b", "w/one")
	f.write("a.txt", "a\n")
	out := f.mustGraft("commit", "-m", "feat: on the work branch")
	if !strings.Contains(out, "version untouched on w/one (version.skip_branches)") || f.read("VERSION") != "1.0.1\n" {
		t.Errorf("work branch bumped (VERSION %q):\n%s", f.read("VERSION"), out)
	}
	f.write("a.txt", "a, more\n")
	if out, code := f.graft("", "commit", "--minor", "-m", "feat: x"); code == 0 || !strings.Contains(out, "skip_branches") {
		t.Errorf("--minor on a work branch: %d\n%s", code, out)
	}
	f.write("a.txt", "a\n")

	// The trunk moves on; the work branch is rebased and pushed again.
	f.git("checkout", "-q", "main")
	f.write("b.txt", "b\n")
	f.mustGraft("commit", "-m", "feat: trunk moves")
	f.git("checkout", "-q", "w/one")
	f.git("-c", "core.hooksPath=/dev/null", "rebase", "-q", "main")
	f.write("a.txt", "a2\n")
	out = f.mustGraft("commit", "-m", "fix: after the rebase")
	if !strings.Contains(out, "pushed to origin") {
		t.Errorf("rebased work branch not pushed:\n%s", out)
	}
	if got, want := f.gitIn(f.remote("origin"), "rev-parse", "w/one"), f.git("rev-parse", "HEAD"); got != want {
		t.Errorf("origin w/one = %s, want %s", got, want)
	}

	// Merging: the trunk bumps once for the whole branch.
	f.git("checkout", "-q", "main")
	f.git("merge", "-q", "--ff-only", "w/one")
	out = f.mustGraft("version", "bump")
	if !strings.Contains(out, "version 1.0.2 -> 1.0.3") || !strings.Contains(f.read("version.go"), `"1.0.3"`) {
		t.Errorf("bump on merge:\n%s\n%s", out, f.read("version.go"))
	}
	f.git("checkout", "-q", "w/one")
	if out, code := f.graft("", "version", "bump"); code == 0 || !strings.Contains(out, "skip_branches") {
		t.Errorf("bump on a work branch: %d\n%s", code, out)
	}
}

func TestTrunkPushIsNotForced(t *testing.T) {
	f := workBranchRepo(t)
	f.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "--amend", "-m", "rewritten")
	f.write("c.txt", "c\n")
	out, code := f.graft("", "commit", "-m", "feat: on a rewritten trunk")
	if code == 0 || !strings.Contains(out, "push to origin FAILED") || strings.Contains(out, "force-with-lease") {
		t.Errorf("a rewritten trunk was pushed, or the retry suggests forcing it: %d\n%s", code, out)
	}
}

// TestCommitLockScope commits in two worktrees at once: a slow gate in the
// first holds the commit lock while the second tries to commit.
func TestCommitLockScope(t *testing.T) {
	for _, scope := range []string{"repository", "worktree"} {
		t.Run(scope, func(t *testing.T) {
			state := t.TempDir()
			f := newRepo(t, map[string]string{
				".graft.yaml": "schema: 1\nversion:\n  mode: none\nlock:\n  timeout: 300ms\n  scope: " + scope + "\ngate:\n" +
					gateStep("log", filepath.Join(state, "gate.log"), "1500"),
			}, "origin")
			f.mustGraft("commit", "-m", "feat: base")
			// The base commit ran the gate too; only the first commit's run counts.
			if err := os.Remove(filepath.Join(state, "gate.log")); err != nil {
				t.Fatal(err)
			}
			second := filepath.Join(f.base, "second")
			f.git("worktree", "add", "-q", "-b", "w/two", second)

			f.write("a.txt", "a\n")
			var wg sync.WaitGroup
			var firstOut string
			var firstCode int
			wg.Go(func() { firstOut, firstCode = f.graft("", "commit", "-m", "feat: first") })
			// The first gate has started, so the first commit holds its lock.
			waitForFile(t, filepath.Join(state, "gate.log"))
			writeFile(t, filepath.Join(second, "b.txt"), "b\n")
			out, code := f.exec(second, "", graftBin, "commit", "-m", "feat: second")
			wg.Wait()
			if firstCode != 0 {
				t.Fatalf("first commit: %d\n%s", firstCode, firstOut)
			}
			switch scope {
			case "repository":
				if code == 0 || !strings.Contains(out, "still held") {
					t.Errorf("second worktree did not wait for the repository lock: %d\n%s", code, out)
				}
			case "worktree":
				if code != 0 {
					t.Errorf("second worktree blocked: %d\n%s", code, out)
				}
			}
		})
	}
}

func TestWorktreeLockScopeRefusesTagOnCommit(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion:\n  mode: file\n  tag_on_commit: true\nlock:\n  scope: worktree\ngate: []\n",
		"VERSION":     "1.0.0\n",
	})
	if out, code := f.graft("", "tasks"); code == 0 || !strings.Contains(out, "tag_on_commit needs the repository lock") {
		t.Errorf("%d\n%s", code, out)
	}
}

func TestGateSeesTheCommitMessage(t *testing.T) {
	state := t.TempDir()
	got := filepath.Join(state, "message")
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion:\n  mode: none\nticket:\n  pattern: 'PROJ-[0-9]+'\ngate:\n" +
			gateStep("copyenv", "GRAFT_COMMIT_MESSAGE_FILE", got),
	}, "origin")
	f.mustGraft("commit", "-m", "feat: names the flag", "-m", "anchor 7, frames 10-20")
	if msg := readFile(t, got); msg != "feat: names the flag\n\nanchor 7, frames 10-20\n\nPROJ-42\n" {
		t.Errorf("gate saw %q", msg)
	}
	if left, _ := filepath.Glob(filepath.Join(f.work, ".git", "GRAFT_COMMIT_MESSAGE.*")); len(left) > 0 {
		t.Errorf("message file left behind: %v", left)
	}

	// graft amend keeps the message, and the gate sees that one.
	f.git("checkout", "-q", "-b", "local-only")
	f.write("a.txt", "a\n")
	f.git("-c", "core.hooksPath=/dev/null", "add", "-A")
	f.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "fix: local\n\nbody")
	f.write("a.txt", "a2\n")
	f.mustGraft("amend")
	if msg := readFile(t, got); msg != "fix: local\n\nbody\n" {
		t.Errorf("amend gate saw %q", msg)
	}
}

func TestAmendRewritesOnlyLeaseBranches(t *testing.T) {
	f := workBranchRepo(t)
	f.write("t.txt", "trunk\n")
	if out, code := f.graft("", "amend"); code == 0 || !strings.Contains(out, "already on origin/main") {
		t.Errorf("amended a published trunk commit: %d\n%s", code, out)
	}
	if err := os.Remove(filepath.Join(f.work, "t.txt")); err != nil {
		t.Fatal(err)
	}
	f.git("checkout", "-q", "-b", "w/one")
	f.write("a.txt", "a\n")
	f.mustGraft("commit", "-m", "feat: work")
	// The work branch is pushed and the trunk has not moved: rebasing it
	// changes nothing, and the merge folds the version bump in with amend.
	f.git("checkout", "-q", "-B", "merge", "origin/w/one")
	f.mustGraft("version", "bump")
	out := f.mustGraft("amend")
	if !strings.Contains(out, "amended the last commit") || f.git("show", "HEAD:VERSION") != "1.0.2" {
		t.Errorf("amend on a pushed work branch commit:\n%s\nVERSION %s", out, f.git("show", "HEAD:VERSION"))
	}
}
