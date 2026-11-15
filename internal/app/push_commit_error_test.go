package app

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

func TestReleaseBranchPushErrorKeepsPushErrorType(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	remote := filepath.Join(root, "origin.git")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit(root, "init", "--bare", "-q", remote)
	runGit(work, "init", "-q", "-b", "main")
	runGit(work, "config", "user.name", "Release Test")
	runGit(work, "config", "user.email", "release@example.test")
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(work, "add", "file.txt")
	runGit(work, "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "base")
	runGit(work, "remote", "add", "origin", remote)
	runGit(work, "push", "-q", "-u", "origin", "main")
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("release\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(work, "add", "file.txt")
	runGit(work, "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "release")
	runGit(work, "tag", "-a", "v0.8.0", "-m", "v0.8.0")
	hook := filepath.Join(remote, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nwhile read old new ref; do case $ref in refs/heads/*) echo rejected >&2; exit 1;; esac; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo, err := gitx.Open(work)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	a := &App{Stderr: &stderr}
	plan := commitPlan{
		branch:  "main",
		remotes: []string{"origin"},
		release: true,
		bump:    bumpPlan{tag: "v0.8.0"},
	}
	err = a.pushCommit(repo, &config.Config{}, plan, []string{"HEAD", "refs/tags/v0.8.0"})
	if err == nil {
		t.Fatal("pushCommit succeeded despite the rejecting branch hook")
	}
	var pushErr *PushError
	if !errors.As(err, &pushErr) {
		t.Fatalf("errors.As(*PushError) = false: %v", err)
	}
	if !strings.Contains(err.Error(), "local branch and release commit are kept; the local tag v0.8.0 was removed") || strings.Contains(err.Error(), "local refs are kept") {
		t.Fatalf("release branch error has confusing local-ref state: %v", err)
	}
}
