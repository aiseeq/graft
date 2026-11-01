package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func releaseBranchRepo(t *testing.T) *fixture {
	t.Helper()
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: git-tag}\ngate: []\n",
		"base.txt":    "base\n",
	}, "origin")
	f.git("checkout", "-q", "-b", "main")
	f.mustPlainCommit("feat: base")
	f.git("push", "-q", "-u", "origin", "main")
	return f
}

// TestReleasePushesMergedBranchBeforeTag: main fast-forwarded to a pushed
// feature branch goes out with its tag.
func TestReleasePushesMergedBranchBeforeTag(t *testing.T) {
	f := releaseBranchRepo(t)
	f.git("checkout", "-q", "-b", "feature")
	f.write("feature.txt", "feature\n")
	f.mustGraft("commit", "-m", "feat: feature")
	f.git("checkout", "-q", "main")
	f.git("merge", "-q", "--ff-only", "feature")
	head := f.git("rev-parse", "HEAD")

	if out, code := f.graft("", "release"); code != 0 {
		t.Fatalf("graft release: exit %d\n%s", code, out)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "refs/heads/main"); got != head {
		t.Errorf("origin/main = %s, want %s", got, head)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "refs/tags/v0.1.0^{commit}"); got != head {
		t.Errorf("remote tag points to %s, want %s", got, head)
	}
}

// TestReleaseRefusesDivergedBranchWithoutTag: the branch fast-forwarded to
// pushed work, but its remote moved on: the push fails and no tag is made.
func TestReleaseRefusesDivergedBranchWithoutTag(t *testing.T) {
	f := releaseBranchRepo(t)
	f.git("checkout", "-q", "-b", "feature")
	f.write("local.txt", "local\n")
	f.mustGraft("commit", "-m", "feat: local")
	f.git("checkout", "-q", "main")
	f.git("merge", "-q", "--ff-only", "feature")

	other := filepath.Join(f.base, "other")
	f.run(f.base, "git", "clone", "--quiet", "--branch", "main", f.remote("origin"), other)
	f.run(other, "git", "config", "user.name", "Test User")
	f.run(other, "git", "config", "user.email", "test@example.invalid")
	writeFile(t, filepath.Join(other, "remote.txt"), "remote\n")
	f.run(other, "git", "add", "-A")
	f.run(other, "git", "commit", "-q", "-m", "feat: remote")
	f.run(other, "git", "push", "-q", "origin", "main")

	out, code := f.graft("", "release")
	if code == 0 || !strings.Contains(out, "push failed for 1 of 1 remotes") {
		t.Fatalf("diverged release: exit %d\n%s", code, out)
	}
	if _, code := f.tryGit("show-ref", "--verify", "--quiet", "refs/tags/v0.1.0"); code == 0 {
		t.Error("local release tag exists after the branch push failed")
	}
	if _, code := f.exec(f.remote("origin"), "", "git", "show-ref", "--verify", "--quiet", "refs/tags/v0.1.0"); code == 0 {
		t.Error("remote release tag exists after the branch push failed")
	}
}

func TestReleaseDetachedHeadStillRequiresPublishedCommit(t *testing.T) {
	f := releaseBranchRepo(t)
	f.git("checkout", "-q", "--detach", "HEAD")
	f.write("detached.txt", "detached\n")
	f.mustPlainCommit("feat: detached")

	out, code := f.graft("", "release")
	if code == 0 || !strings.Contains(out, "the commit to tag is not on origin yet: push it first (graft commit pushes it), then release") {
		t.Fatalf("detached release: exit %d\n%s", code, out)
	}
}
