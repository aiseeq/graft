package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// withTestEnv runs fn with extra appended to testEnv, restoring it even when
// fn stops the test.
func withTestEnv(extra []string, fn func()) {
	prior := testEnv
	defer func() { testEnv = prior }()
	testEnv = append(slices.Clip(testEnv), extra...)
	fn()
}

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
	if code == 0 || !strings.Contains(out, "branch main was not pushed to origin, no tag is made; if the remote rejected it, git fetch") {
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

// TestReleaseBehindBranchTagsWithoutPush: a branch behind its remote already
// has HEAD there; release tags it without pushing the branch.
func TestReleaseBehindBranchTagsWithoutPush(t *testing.T) {
	f := releaseBranchRepo(t)
	head := f.git("rev-parse", "HEAD")

	other := filepath.Join(f.base, "other")
	f.run(f.base, "git", "clone", "--quiet", "--branch", "main", f.remote("origin"), other)
	f.run(other, "git", "config", "user.name", "Test User")
	f.run(other, "git", "config", "user.email", "test@example.invalid")
	writeFile(t, filepath.Join(other, "remote.txt"), "remote\n")
	f.run(other, "git", "add", "-A")
	f.run(other, "git", "commit", "-q", "-m", "feat: remote")
	f.run(other, "git", "push", "-q", "origin", "main")
	remoteMain := f.gitIn(f.remote("origin"), "rev-parse", "refs/heads/main")
	f.git("fetch", "-q", "origin")

	if out, code := f.graft("", "release"); code != 0 {
		t.Fatalf("graft release behind its remote: exit %d\n%s", code, out)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "refs/heads/main"); got != remoteMain {
		t.Errorf("origin/main = %s, want it untouched at %s", got, remoteMain)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "refs/tags/v0.1.0^{commit}"); got != head {
		t.Errorf("remote tag points to %s, want %s", got, head)
	}
}

// TestReleaseLeavesBranchAbsentFromRemote: a local branch on pushed work is
// tagged, but release does not create it on the remote.
func TestReleaseLeavesBranchAbsentFromRemote(t *testing.T) {
	f := releaseBranchRepo(t)
	f.git("checkout", "-q", "-b", "local-prep")
	head := f.git("rev-parse", "HEAD")

	if out, code := f.graft("", "release"); code != 0 {
		t.Fatalf("graft release on a local branch: exit %d\n%s", code, out)
	}
	if _, code := f.exec(f.remote("origin"), "", "git", "show-ref", "--verify", "--quiet", "refs/heads/local-prep"); code == 0 {
		t.Error("release created local-prep on the remote")
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "refs/tags/v0.1.0^{commit}"); got != head {
		t.Errorf("remote tag points to %s, want %s", got, head)
	}
}

// twoRemoteRepo has main pushed to origin and mirror.
func twoRemoteRepo(t *testing.T) *fixture {
	t.Helper()
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: git-tag}\ngate: []\n",
		"base.txt":    "base\n",
	}, "origin", "mirror")
	f.git("checkout", "-q", "-b", "main")
	f.mustPlainCommit("feat: base")
	f.git("push", "-q", "-u", "origin", "main")
	f.git("push", "-q", "mirror", "main")
	return f
}

// TestReleaseAsksRemoteWithoutTrackingCopy: the clone has no copy of
// mirror's main, but mirror has main; release pushes it there too, so the tag
// does not point past mirror's main.
func TestReleaseAsksRemoteWithoutTrackingCopy(t *testing.T) {
	f := twoRemoteRepo(t)
	f.git("update-ref", "-d", "refs/remotes/mirror/main")
	f.git("checkout", "-q", "-b", "feature")
	f.write("feature.txt", "feature\n")
	f.mustGraft("commit", "-m", "feat: feature")
	f.git("checkout", "-q", "main")
	f.git("merge", "-q", "--ff-only", "feature")
	head := f.git("rev-parse", "HEAD")

	if out, code := f.graft("", "release"); code != 0 {
		t.Fatalf("graft release: exit %d\n%s", code, out)
	}
	for _, r := range []string{"origin", "mirror"} {
		if got := f.gitIn(f.remote(r), "rev-parse", "refs/heads/main"); got != head {
			t.Errorf("%s main = %s, want %s", r, got, head)
		}
	}
}

func releaseAuthorsRepo(t *testing.T, authors string) *fixture {
	t.Helper()
	conf := "schema: 1\nversion: {mode: git-tag}\ngate: []\n"
	if authors == "" {
		conf += "release: {}\n"
	} else {
		conf += "release:\n  authors: " + authors + "\n"
	}
	f := newRepo(t, map[string]string{".graft.yaml": conf, "base.txt": "base\n"}, "origin")
	f.git("checkout", "-q", "-b", "main")
	withTestEnv([]string{"GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_EMAIL=test@example.com"}, func() {
		f.mustPlainCommit("feat: base")
	})
	f.git("push", "-q", "-u", "origin", "main")
	return f
}

func (f *fixture) commitWithAuthor(email string) string {
	f.t.Helper()
	f.write("change.txt", "change\n")
	f.git("add", "-A")
	prior := f.env
	f.env = append(f.env, "GIT_AUTHOR_EMAIL="+email, "GIT_COMMITTER_EMAIL=test@example.com")
	out, code := f.exec(f.work, "", "git", "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "feat: change")
	f.env = prior
	if code != 0 {
		f.t.Fatalf("git commit: exit %d\n%s", code, out)
	}
	f.git("push", "-q", "origin", "HEAD")
	return f.git("rev-parse", "HEAD")
}

func (f *fixture) commitStagedAs(email, message string) {
	f.t.Helper()
	prior := f.env
	f.env = append(f.env, "GIT_AUTHOR_EMAIL="+email, "GIT_COMMITTER_EMAIL=test@example.com")
	out, code := f.exec(f.work, "", "git", "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", message)
	f.env = prior
	if code != 0 {
		f.t.Fatalf("git commit failed: %s", out)
	}
}

func TestReleaseChecksAuthorsBeforeTag(t *testing.T) {
	f := releaseAuthorsRepo(t, "[test@example.com]")
	if out, code := f.graft("", "release"); code != 0 {
		t.Fatalf("initial release tag: exit %d\n%s", code, out)
	}
	sha := f.commitWithAuthor("other@example.com")
	out, code := f.graft("", "release")
	if code == 0 || !strings.Contains(out, sha[:12]+" author other@example.com") || !strings.HasSuffix(strings.TrimSpace(out), "rewrite the author or add the address to release.authors") {
		t.Fatalf("release with foreign author: exit %d\n%s", code, out)
	}
	if _, code := f.tryGit("show-ref", "--verify", "--quiet", "refs/tags/v0.1.1"); code == 0 {
		t.Fatal("new release tag exists after author refusal")
	}
}

func TestReleaseAllowsConfiguredAuthors(t *testing.T) {
	f := releaseAuthorsRepo(t, "[test@example.com, dev@example.org]")
	f.commitWithAuthor("dev@example.org")
	if out, code := f.graft("", "release"); code != 0 {
		t.Fatalf("release with allowed authors: exit %d\n%s", code, out)
	}
	if _, code := f.tryGit("show-ref", "--verify", "--quiet", "refs/tags/v0.1.0"); code != 0 {
		t.Fatal("release tag is missing")
	}
}

func TestReleaseWithoutAuthorList(t *testing.T) {
	f := releaseAuthorsRepo(t, "")
	f.commitWithAuthor("other@example.com")
	if out, code := f.graft("", "release"); code != 0 {
		t.Fatalf("release without author list: exit %d\n%s", code, out)
	}
}

func TestReleaseDoesNotRecheckHotfixCommitsAlreadyTagged(t *testing.T) {
	f := releaseAuthorsRepo(t, "")
	if out, code := f.graft("", "release", "--version", "1.0.0"); code != 0 {
		t.Fatalf("release v1.0.0: exit %d\n%s", code, out)
	}
	f.git("checkout", "-q", "-b", "hotfix/one")
	f.write("hotfix.txt", "hotfix\n")
	f.git("add", "-A")
	prior := f.env
	f.env = append(f.env, "GIT_AUTHOR_EMAIL=other@example.com", "GIT_COMMITTER_EMAIL=test@example.com")
	out, code := f.exec(f.work, "", "git", "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "fix: hotfix")
	f.env = prior
	if code != 0 {
		t.Fatalf("hotfix commit: exit %d\n%s", code, out)
	}
	f.git("push", "-q", "-u", "origin", "HEAD")
	if out, code := f.graft("", "release", "--version", "1.0.1"); code != 0 {
		t.Fatalf("release v1.0.1: exit %d\n%s", code, out)
	}
	f.git("checkout", "-q", "main")
	f.write("main.txt", "main\n")
	f.git("add", "-A")
	f.commitStagedAs("test@example.com", "feat: main work")
	f.git("push", "-q", "origin", "HEAD")
	if out, code := f.graft("", "release", "--version", "1.1.0"); code != 0 {
		t.Fatalf("release v1.1.0: exit %d\n%s", code, out)
	}
	withTestEnv([]string{"GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_EMAIL=test@example.com"}, func() {
		f.git("merge", "-q", "--no-ff", "hotfix/one", "-m", "merge: hotfix")
	})
	f.write(".graft.yaml", "schema: 1\nversion: {mode: git-tag}\ngate: []\nrelease:\n  authors: [test@example.com]\n")
	f.git("add", "-A")
	f.commitStagedAs("test@example.com", "chore: configure release authors")
	f.git("push", "-q", "origin", "HEAD")
	if out, code := f.graft("", "release", "--version", "1.1.1"); code != 0 {
		t.Fatalf("release after merging released hotfix: exit %d\n%s", code, out)
	}
	if _, code := f.tryGit("show-ref", "--verify", "--quiet", "refs/tags/v1.1.1"); code != 0 {
		t.Fatal("release tag v1.1.1 is missing")
	}
}

func TestReleaseAuthorCheckStartsAfterPreviousTag(t *testing.T) {
	f := releaseAuthorsRepo(t, "")
	f.commitWithAuthor("other@example.com")
	if out, code := f.graft("", "release"); code != 0 {
		t.Fatalf("initial release: exit %d\n%s", code, out)
	}
	f.write(".graft.yaml", "schema: 1\nversion: {mode: git-tag}\ngate: []\nrelease:\n  authors: [test@example.com]\n")
	f.git("add", "-A")
	f.commitStagedAs("test@example.com", "chore: configure release authors")
	f.git("push", "-q", "origin", "HEAD")
	if out, code := f.graft("", "release"); code != 0 {
		t.Fatalf("release after previous tag: exit %d\n%s", code, out)
	}
	if _, code := f.tryGit("show-ref", "--verify", "--quiet", "refs/tags/v0.1.1"); code != 0 {
		t.Fatal("second release tag is missing")
	}
}

// TestReleasePartialBranchPushMakesNoTag: origin takes the branch, mirror
// rejects it; no tag is made anywhere.
func TestReleasePartialBranchPushMakesNoTag(t *testing.T) {
	f := twoRemoteRepo(t)
	f.git("checkout", "-q", "-b", "feature")
	f.write("feature.txt", "feature\n")
	f.mustGraft("commit", "-m", "feat: feature")
	f.git("checkout", "-q", "main")
	f.git("merge", "-q", "--ff-only", "feature")

	other := filepath.Join(f.base, "other")
	f.run(f.base, "git", "clone", "--quiet", "--branch", "main", f.remote("mirror"), other)
	f.run(other, "git", "config", "user.name", "Test User")
	f.run(other, "git", "config", "user.email", "test@example.invalid")
	writeFile(t, filepath.Join(other, "remote.txt"), "remote\n")
	f.run(other, "git", "add", "-A")
	f.run(other, "git", "commit", "-q", "-m", "feat: remote")
	f.run(other, "git", "push", "-q", "origin", "main")

	out, code := f.graft("", "release")
	if code == 0 || !strings.Contains(out, "branch main was not pushed to mirror, no tag is made") {
		t.Fatalf("partial release: exit %d\n%s", code, out)
	}
	if _, code := f.tryGit("show-ref", "--verify", "--quiet", "refs/tags/v0.1.0"); code == 0 {
		t.Error("local release tag exists after a branch push failed")
	}
	for _, r := range []string{"origin", "mirror"} {
		if _, code := f.exec(f.remote(r), "", "git", "show-ref", "--verify", "--quiet", "refs/tags/v0.1.0"); code == 0 {
			t.Errorf("%s has the release tag after a branch push failed", r)
		}
	}
}
