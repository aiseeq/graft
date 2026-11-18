package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

const archivedPushMessage = "ERROR: This repository was archived so it is read-only."

type cleanReadOnlyFixture struct {
	root, work, remote, hookCount string
	app                           *App
	cleaner                       *cleaner
	output                        bytes.Buffer
}

func newCleanReadOnlyFixture(t *testing.T, hookMessage string, branches ...string) *cleanReadOnlyFixture {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	remote := filepath.Join(root, "remote.git")
	hookCount := filepath.Join(root, "hook-count")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(remote, 0o700); err != nil {
		t.Fatal(err)
	}
	git(work, "init", "-q", "-b", "main")
	git(work, "config", "user.name", "Test")
	git(work, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(work, "file"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(work, "add", "file")
	git(work, "commit", "-q", "-m", "base")
	git(remote, "init", "-q", "--bare", "-b", "main")
	git(work, "remote", "add", "origin", remote)
	git(work, "push", "-q", "origin", "main")
	for _, branch := range branches {
		git(work, "checkout", "-q", "-b", branch, "main")
		if err := os.WriteFile(filepath.Join(work, "file"), fmt.Appendf(nil, "%s\n", branch), 0o600); err != nil {
			t.Fatal(err)
		}
		git(work, "commit", "-qam", branch)
		git(work, "checkout", "-q", "main")
		git(work, "merge", "-q", "--ff-only", branch)
	}
	pushArgs := []string{"push", "-q", "origin", "main"}
	pushArgs = append(pushArgs, branches...)
	git(work, pushArgs...)
	if err := os.WriteFile(hookCount, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	hook := fmt.Sprintf("#!/bin/sh\nprintf 'called\\n' >> %q\nprintf '%%s\\n' %q >&2\nexit 1\n", hookCount, hookMessage)
	if err := os.WriteFile(filepath.Join(remote, "hooks", "pre-receive"), []byte(hook), 0o700); err != nil {
		t.Fatal(err)
	}
	repo, err := gitx.Open(work)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Git("fetch", "--quiet", "origin"); err != nil {
		t.Fatal(err)
	}
	f := &cleanReadOnlyFixture{root: root, work: work, remote: remote, hookCount: hookCount}
	f.app = &App{Dir: work, Stdout: &f.output, Stderr: &bytes.Buffer{}}
	f.cleaner = &cleaner{
		ctx: context.Background(), app: f.app, repo: repo, cfg: &config.Config{},
		opts: WorktreeCleanOptions{Apply: true}, trunk: "origin/main",
		remotes: []string{"origin"}, now: time.Now(), counts: map[string]int{},
	}
	return f
}

func (f *cleanReadOnlyFixture) hookCalls(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(f.hookCount)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		return 0
	}
	return strings.Count(string(data), "called\n")
}

func TestWorktreeCleanArchivedRemoteKeepsBranchWithoutFailing(t *testing.T) {
	f := newCleanReadOnlyFixture(t, archivedPushMessage, "merged")
	if _, err := f.cleaner.repo.Git("push", "--porcelain", "origin", "--delete", "merged"); err == nil || !strings.Contains(err.Error(), archivedPushMessage) {
		t.Fatalf("git push error = %v, want archived hook response", err)
	}
	if err := os.WriteFile(f.hookCount, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.cleaner.deleteRemoteCopy("origin", "merged"); err != nil {
		t.Fatalf("deleteRemoteCopy returned an error: %v", err)
	}
	if !strings.Contains(f.output.String(), "kept      origin/merged: origin is read-only (archived); the branch stays") {
		t.Fatalf("output lacks kept reason: %s", f.output.String())
	}
	f.cleaner.summary()
	if !strings.Contains(f.output.String(), "kept 1") {
		t.Fatalf("summary lacks kept count: %s", f.output.String())
	}
	if f.hookCalls(t) != 1 {
		t.Fatalf("hook calls = %d, want 1", f.hookCalls(t))
	}
	if out, err := f.cleaner.repo.Git("ls-remote", "--exit-code", "origin", "refs/heads/merged"); err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("remote branch missing: output %q, error %v", out, err)
	}
}

func TestWorktreeCleanArchivedRemoteSkipsFurtherPushes(t *testing.T) {
	f := newCleanReadOnlyFixture(t, archivedPushMessage, "merged-a", "merged-b")
	for _, branch := range []string{"merged-a", "merged-b"} {
		if err := f.cleaner.deleteRemoteCopy("origin", branch); err != nil {
			t.Fatalf("deleteRemoteCopy(%q): %v", branch, err)
		}
	}
	for _, branch := range []string{"main", "nonexistent"} {
		if err := f.cleaner.deleteRemoteCopy("origin", branch); err != nil {
			t.Fatalf("deleteRemoteCopy(%q): %v", branch, err)
		}
	}
	if calls := f.hookCalls(t); calls != 1 {
		t.Fatalf("hook calls = %d, want 1", calls)
	}
	for _, branch := range []string{"merged-a", "merged-b"} {
		if !strings.Contains(f.output.String(), "kept      origin/"+branch+": origin is read-only (archived); the branch stays") {
			t.Errorf("output lacks kept line for %s: %s", branch, f.output.String())
		}
	}
	if strings.Contains(f.output.String(), "origin/main:") || strings.Contains(f.output.String(), "origin/nonexistent:") {
		t.Fatalf("output reports an ineligible branch as kept: %s", f.output.String())
	}
}

func TestWorktreeCleanOtherRemotePushFailureStillFails(t *testing.T) {
	f := newCleanReadOnlyFixture(t, "policy: repository was archived so it is read-only? no, access denied", "merged")
	err := f.cleaner.deleteRemoteCopy("origin", "merged")
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("deleteRemoteCopy error = %v, want policy denial", err)
	}
	if strings.Contains(f.output.String(), "kept") {
		t.Fatalf("policy denial was reported as kept: %s", f.output.String())
	}
	if f.hookCalls(t) != 1 {
		t.Fatalf("hook calls = %d, want 1", f.hookCalls(t))
	}
}
