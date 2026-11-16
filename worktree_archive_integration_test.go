package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func archiveFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f, _ := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n",
		"VERSION":     "1.0.0\n",
	})
	f.git("config", "user.name", "Archive Test")
	f.git("config", "user.email", "archive@example.test")
	path := filepath.Join(f.base, "archive-tree")
	f.git("worktree", "add", "-q", "-b", "feature/archive/idea", path, "main")
	writeFile(t, filepath.Join(path, "idea.txt"), "kept in archive\n")
	f.mustVersionedCommit(path, "feat: rejected idea", false)
	return f, path
}

func TestWorktreeArchivePushesReasonThenRemovesLocalTree(t *testing.T) {
	f, path := archiveFixture(t)
	f.git("push", "origin", "main:refs/heads/main")
	f.git("push", "origin", "feature/archive/idea:refs/heads/feature/archive/idea")
	out, code := f.graft("", "worktree", "archive", "--reason", "superseded proposal", "feature/archive/idea")
	if code != 0 || !strings.Contains(out, "remote branch feature/archive/idea remains") {
		t.Fatalf("archive failed, exit %d:\n%s", code, out)
	}
	if _, code := f.tryGit("rev-parse", "--verify", "refs/tags/archive/feature/archive/idea"); code != 0 {
		t.Fatalf("local archive tag missing:\n%s", out)
	}
	remoteTag := f.gitIn(f.remote("origin"), "show", "refs/tags/archive/feature/archive/idea")
	if !strings.Contains(remoteTag, "superseded proposal") || !strings.Contains(remoteTag, "feat: rejected idea") {
		t.Fatalf("remote annotation did not preserve reason and branch commit:\n%s", remoteTag)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("worktree still exists: %s", path)
	}
	if _, code := f.tryGit("show-ref", "--verify", "refs/heads/feature/archive/idea"); code == 0 {
		t.Fatal("local branch remains")
	}
	if remote := f.git("ls-remote", "origin", "refs/heads/feature/archive/idea"); strings.TrimSpace(remote) == "" {
		t.Fatal("remote branch was removed")
	}
	listed, code := f.graft("", "worktree", "archive", "--list", "idea")
	if code != 0 || !strings.Contains(listed, "archive/feature/archive/idea: superseded proposal") {
		t.Fatalf("archive list failed, exit %d:\n%s", code, listed)
	}
	filtered, code := f.graft("", "worktree", "archive", "--list", "no match")
	if code != 0 || !strings.Contains(filtered, "no archive tags") {
		t.Fatalf("archive list filter failed, exit %d:\n%s", code, filtered)
	}
}

func TestWorktreeArchiveRefusesUncommittedChangesAndExistingTag(t *testing.T) {
	f, path := archiveFixture(t)
	writeFile(t, filepath.Join(path, "scratch.txt"), "unsaved\n")
	out, code := f.graft("", "worktree", "archive", "--reason", "rejected", "feature/archive/idea")
	if code == 0 || !strings.Contains(out, "uncommitted change") || !strings.Contains(out, "branch and worktree remain") {
		t.Fatalf("dirty archive was not refused, exit %d:\n%s", code, out)
	}
	if _, code := f.tryGit("show-ref", "--verify", "refs/tags/archive/feature/archive/idea"); code == 0 {
		t.Fatal("dirty archive created a tag")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("dirty worktree was removed: %v", err)
	}
	if err := os.Remove(filepath.Join(path, "scratch.txt")); err != nil {
		t.Fatal(err)
	}
	f.git("tag", "archive/feature/archive/idea", "main")
	out, code = f.graft("", "worktree", "archive", "--reason", "rejected", "feature/archive/idea")
	if code == 0 || !strings.Contains(out, "already exists") {
		t.Fatalf("existing archive tag was overwritten, exit %d:\n%s", code, out)
	}
}

func TestWorktreeArchiveRefusesStagedChanges(t *testing.T) {
	f, path := archiveFixture(t)
	writeFile(t, filepath.Join(path, "idea.txt"), "staged update\n")
	f.git("-C", path, "add", "idea.txt")
	out, code := f.graft("", "worktree", "archive", "--reason", "rejected", "feature/archive/idea")
	if code == 0 || !strings.Contains(out, "uncommitted change") {
		t.Fatalf("staged archive was not refused, exit %d:\n%s", code, out)
	}
	if _, code := f.tryGit("show-ref", "--verify", "refs/tags/archive/feature/archive/idea"); code == 0 {
		t.Fatal("staged archive created a tag")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("staged worktree was removed: %v", err)
	}
}

func TestWorktreeArchiveRefusesForeignAnnotatedTagAtBranchTip(t *testing.T) {
	f, path := archiveFixture(t)
	f.git("tag", "-a", "archive/feature/archive/idea", "-m", "foreign reason", "feature/archive/idea")
	out, code := f.graft("", "worktree", "archive", "--reason", "foreign reason", "feature/archive/idea")
	if code == 0 || !strings.Contains(out, "not created by this archive") || !strings.Contains(out, "--name") || !strings.Contains(out, "delete the tag manually") {
		t.Fatalf("foreign annotated tag was accepted or lacked recovery guidance, exit %d:\n%s", code, out)
	}
	if got := strings.TrimSpace(f.git("ls-remote", "origin", "refs/tags/archive/feature/archive/idea")); got != "" {
		t.Fatalf("foreign tag was pushed to origin: %s", got)
	}
	if _, code := f.tryGit("show-ref", "--verify", "refs/heads/feature/archive/idea"); code != 0 {
		t.Fatal("branch was removed after refusing foreign tag")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("worktree was removed after refusing foreign tag: %v", err)
	}
}

func TestWorktreeArchiveRefusesOwnTagWhenReasonDiffers(t *testing.T) {
	f, path := archiveFixture(t)
	f.git("tag", "-a", "archive/feature/archive/idea", "-m", "original reason", "-m", "Archived-by: graft worktree archive", "feature/archive/idea")
	out, code := f.graft("", "worktree", "archive", "--reason", "different reason", "feature/archive/idea")
	if code == 0 || !strings.Contains(out, "different --reason") {
		t.Fatalf("archive tag with a different reason was accepted, exit %d:\n%s", code, out)
	}
	if got := strings.TrimSpace(f.git("ls-remote", "origin", "refs/tags/archive/feature/archive/idea")); got != "" {
		t.Fatalf("tag with a different reason was pushed to origin: %s", got)
	}
	if _, code := f.tryGit("show-ref", "--verify", "refs/heads/feature/archive/idea"); code != 0 {
		t.Fatal("branch was removed after refusing reason mismatch")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("worktree was removed after refusing reason mismatch: %v", err)
	}
}

func TestWorktreeArchiveWithoutRemotesKeepsCustomTagNameLocally(t *testing.T) {
	f, path := archiveFixture(t)
	f.git("remote", "remove", "origin")
	out, code := f.exec(path, "", graftBin, "worktree", "archive", "--name", "reviewed/idea", "--reason", "local review only", "feature/archive/idea")
	if code != 0 || !strings.Contains(out, "tag is local only") || !strings.Contains(out, "no remote branch feature/archive/idea to remove") {
		t.Fatalf("local-only archive failed, exit %d:\n%s", code, out)
	}
	annotation := f.git("cat-file", "tag", "refs/tags/archive/reviewed/idea")
	if !strings.Contains(annotation, "local review only") {
		t.Fatalf("local archive tag lacks reason:\n%s", annotation)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("worktree still exists: %s", path)
	}
}

func TestWorktreeArchivePartialPushCanResume(t *testing.T) {
	f, path := archiveFixture(t)
	second := filepath.Join(f.base, "second.git")
	if err := os.CopyFS(second, os.DirFS(bareTemplate)); err != nil {
		t.Fatal(err)
	}
	f.addRemote("second", second)
	f.git("push", "origin", "main:refs/heads/main")
	f.git("push", "second", "main:refs/heads/main")
	deny := filepath.Join(second, "hooks", "pre-receive")
	writeFile(t, deny, "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(deny, 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"worktree", "archive", "--trunk", "main", "--reason", "superseded proposal\n# not a comment  ", "feature/archive/idea"}
	out, code := f.graft("", args...)
	if code == 0 || !strings.Contains(out, "present on origin") || !strings.Contains(out, "missing on second") || !strings.Contains(out, "continue with: graft worktree archive") {
		t.Fatalf("partial push state or retry command missing, exit %d:\n%s", code, out)
	}
	localTag, code := f.tryGit("rev-parse", "refs/tags/archive/feature/archive/idea")
	if code != 0 {
		t.Fatalf("partial push removed the local tag:\n%s", out)
	}
	if got := strings.TrimSpace(f.git("ls-remote", "origin", "refs/tags/archive/feature/archive/idea")); got == "" {
		t.Fatal("first remote lacks the accepted tag")
	}
	if got := strings.TrimSpace(f.git("ls-remote", "second", "refs/tags/archive/feature/archive/idea")); got != "" {
		t.Fatal("refusing remote unexpectedly has the tag")
	}
	if err := os.Remove(deny); err != nil {
		t.Fatal(err)
	}
	out, code = f.graft("", args...)
	if code != 0 || !strings.Contains(out, "remote branch feature/archive/idea remains") {
		t.Fatalf("archive continuation failed, exit %d:\n%s", code, out)
	}
	first := strings.Fields(f.git("ls-remote", "origin", "refs/tags/archive/feature/archive/idea"))[0]
	secondOID := strings.Fields(f.git("ls-remote", "second", "refs/tags/archive/feature/archive/idea"))[0]
	if first != secondOID || first != strings.TrimSpace(localTag) {
		t.Fatalf("continuation produced different tag objects: local=%s origin=%s second=%s", strings.TrimSpace(localTag), first, secondOID)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("worktree remains after successful continuation: %s", path)
	}
}

func TestWorktreeArchiveRefusesRemoteOnlyTagEvenWhenObjectMatches(t *testing.T) {
	f, path := archiveFixture(t)
	f.env = append(f.env, "GIT_COMMITTER_DATE=2026-01-01T12:00:00+0000")
	f.git("tag", "-a", "archive/feature/archive/idea", "-m", "same", "feature/archive/idea")
	f.git("push", "origin", "refs/tags/archive/feature/archive/idea")
	f.git("tag", "-d", "archive/feature/archive/idea")
	out, code := f.graft("", "worktree", "archive", "--reason", "same", "feature/archive/idea")
	if code == 0 || !strings.Contains(out, "already exists on remote") {
		t.Fatalf("remote-only tag was not refused before archive, exit %d:\n%s", code, out)
	}
	if _, code := f.tryGit("show-ref", "--verify", "refs/tags/archive/feature/archive/idea"); code == 0 {
		t.Fatal("local archive tag was created for a remote-only tag")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("worktree was removed despite remote-only tag: %v", err)
	}
}

func TestWorktreeArchiveProtectsMainWorktreeAndTrunk(t *testing.T) {
	t.Run("main worktree branch from a linked caller", func(t *testing.T) {
		f, _ := archiveFixture(t)
		f.git("switch", "-q", "-c", "backup")
		caller := filepath.Join(f.base, "caller-tree")
		f.git("worktree", "add", "-q", "-b", "caller", caller, "main")
		out, code := f.exec(caller, "", graftBin, "worktree", "archive", "--reason", "rejected", "backup")
		if code == 0 || !strings.Contains(out, "main worktree") {
			t.Fatalf("main worktree branch was not protected, exit %d:\n%s", code, out)
		}
		if _, code := f.tryGit("show-ref", "--verify", "refs/tags/archive/backup"); code == 0 {
			t.Fatal("archive tag was created for main worktree branch")
		}
	})
	t.Run("trunk checked out in a linked worktree", func(t *testing.T) {
		f, _ := archiveFixture(t)
		f.git("push", "origin", "main:refs/heads/main")
		trunkTree := filepath.Join(f.base, "trunk-tree")
		f.git("switch", "-q", "-c", "backup")
		f.git("worktree", "add", "-q", trunkTree, "main")
		out, code := f.graft("", "worktree", "archive", "--reason", "rejected", "main")
		if code == 0 || !strings.Contains(out, "trunk branch") {
			t.Fatalf("trunk branch was not protected, exit %d:\n%s", code, out)
		}
		if _, code := f.tryGit("show-ref", "--verify", "refs/tags/archive/main"); code == 0 {
			t.Fatal("archive tag was created for trunk")
		}
		if _, err := os.Stat(trunkTree); err != nil {
			t.Fatalf("trunk worktree was removed: %v", err)
		}
	})
}

func TestWorktreeArchiveLeakStopsPushAndKeepsBranchAndTree(t *testing.T) {
	f := leakRepo(t, "/", "")
	f.git("config", "user.name", "Archive Test")
	f.git("config", "user.email", "archive@example.test")
	f.git("push", "origin", "HEAD:refs/heads/main")
	f.git("remote", "set-head", "origin", "main")
	path := filepath.Join(f.base, "archive-leak-tree")
	f.git("worktree", "add", "-q", "-b", "rejected", path, "HEAD")
	capture := filepath.Join(f.base, "archive-pre-push-stdin")
	installGlobalPrePush(f, capture)
	hooks, _ := f.exec(f.work, "", "git", "hook", "list", "pre-push")
	out, code := f.graft("", "worktree", "archive", "--reason", "rejected because AcmePay is involved", "rejected")
	if code == 0 || !strings.Contains(out, "private names or terms found in tag messages") || !strings.Contains(out, "local tag removed") {
		captured, _ := os.ReadFile(capture)
		t.Fatalf("leaking tag push was not refused, exit %d; hooks=%q stdin=%q:\n%s", code, hooks, captured, out)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("worktree was removed after refused push: %v", err)
	}
	if _, code := f.tryGit("show-ref", "--verify", "refs/heads/rejected"); code != 0 {
		t.Fatal("branch was removed after refused push")
	}
	if _, code := f.tryGit("show-ref", "--verify", "refs/tags/archive/rejected"); code == 0 {
		t.Fatal("local tag remains after refused push")
	}
}
