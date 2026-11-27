package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gittest"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/message"
)

func newAllowEmptyCommitRepo(t *testing.T) (string, func(...string) string) {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) string { return gittest.Run(t, root, args...) }
	git("init", "-q", "-b", "main")
	git("config", "user.name", "Synthetic Test")
	git("config", "user.email", "synthetic@example.invalid")
	projectConfig := "schema: 1\nversion: {mode: none}\ncommit: {conventional: false}\ngate: []\n"
	if err := os.WriteFile(filepath.Join(root, ".graft.yaml"), []byte(projectConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "base")
	return root, git
}

func TestCommitAllowEmptyCreatesEmptyCommit(t *testing.T) {
	root, git := newAllowEmptyCommitRepo(t)
	before := git("rev-parse", "HEAD")
	if err := (&App{Dir: root, Stdout: io.Discard, Stderr: io.Discard}).Commit(context.Background(), CommitOptions{
		Message:    message.Source{Parts: []string{"chore: PROJ-123 empty marker"}},
		AllowEmpty: true,
	}); err != nil {
		t.Fatalf("Commit --allow-empty: %v", err)
	}
	if after := git("rev-parse", "HEAD"); after == before {
		t.Fatal("allow-empty did not create a commit")
	}
	if tree := git("rev-parse", "HEAD^{tree}"); tree != git("rev-parse", "HEAD^1^{tree}") {
		t.Fatalf("empty commit changed tree: current %s, parent %s", tree, git("rev-parse", "HEAD^1^{tree}"))
	}
}

func TestCommitRejectsOrdinaryCommitWhenGateRemovesAllChanges(t *testing.T) {
	root, git := newAllowEmptyCommitRepo(t)
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("candidate work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := git("rev-parse", "HEAD")
	app := &App{Dir: root, Stdout: io.Discard, Stderr: io.Discard}
	app.runCommitGate = func(_ context.Context, repo *gitx.Repo, _ *config.Config, _, _ string) error {
		_, err := repo.Git("checkout", "HEAD", "--", "tracked.txt")
		return err
	}
	err := app.Commit(context.Background(), CommitOptions{
		Message: message.Source{Parts: []string{"chore: PROJ-123 ordinary commit"}},
	})
	if err == nil || !strings.Contains(err.Error(), "nothing to commit: the gate left the work tree as HEAD has it") {
		t.Fatalf("Commit error = %v; want rejection after gate removes all changes", err)
	}
	if after := git("rev-parse", "HEAD"); after != before {
		t.Fatalf("HEAD changed after rejected empty result: %s -> %s", before, after)
	}
}
