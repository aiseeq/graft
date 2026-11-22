package app

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/message"
)

func syncStaleRemoteBranch(t *testing.T, f *syncPushFixture) {
	t.Helper()
	f.git(f.work, "checkout", "-q", "-b", "feature", "--track", "origin/main")
	f.commit(f.work, "feature-work")
	f.git(f.work, "push", "-q", "-u", "origin", "feature")
	f.git(f.work, "checkout", "-q", "main")
	f.commit(f.work, "trunk-advance")
	f.git(f.work, "push", "-q", "origin", "main")
	f.git(f.work, "checkout", "-q", "feature")
}

func TestWorktreeSyncStaleRemoteCommitCanBePushed(t *testing.T) {
	f := newSyncPushFixture(t, false)
	syncStaleRemoteBranch(t, f)
	var out bytes.Buffer
	a := &App{Dir: f.work, Stdout: &out, Stderr: io.Discard}
	if err := a.WorktreeSync(context.Background(), WorktreeSyncOptions{Trunk: "origin/main"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "the next graft commit will not overwrite it") || !strings.Contains(out.String(), "graft worktree sync --push") || !strings.Contains(out.String(), "graft merge feature") {
		t.Fatalf("sync output lacks recovery steps: %s", out.String())
	}
	if err := os.WriteFile(filepath.Join(f.work, "post-sync-commit"), []byte("post-sync-commit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var commitOut bytes.Buffer
	err := (&App{Dir: f.work, Stdout: &commitOut, Stderr: io.Discard}).Commit(context.Background(), CommitOptions{Message: message.Source{Parts: []string{"feat: post sync"}}})
	if err == nil || !strings.Contains(err.Error(), "graft worktree sync --push") || strings.Contains(err.Error(), "retry with: git push") {
		t.Fatalf("graft commit output = %q, error = %v", commitOut.String(), err)
	}
	if err := (&App{Dir: f.work, Stdout: io.Discard, Stderr: io.Discard}).WorktreeSync(context.Background(), WorktreeSyncOptions{Push: true, Trunk: "origin/main"}); err != nil {
		t.Fatalf("sync --push: %v", err)
	}
	if got, want := f.git(f.work, "rev-parse", "HEAD"), f.git(f.remote, "rev-parse", "refs/heads/feature"); got != want {
		t.Fatalf("remote feature tip = %s, want local HEAD %s", want, got)
	}
}

func TestCommitPushKeepsGenericHintForForeignRemoteCommit(t *testing.T) {
	f := newSyncPushFixture(t, false)
	syncStaleRemoteBranch(t, f)
	foreign := f.root + "/foreign"
	f.git(f.root, "clone", "-q", f.remote, foreign)
	f.git(foreign, "checkout", "-q", "-b", "feature", "--track", "origin/feature")
	f.git(foreign, "config", "user.name", "Other")
	f.git(foreign, "config", "user.email", "other@example.invalid")
	if err := os.WriteFile(filepath.Join(f.work, "duplicate"), []byte("same patch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git(f.work, "add", "duplicate")
	f.git(f.work, "commit", "-q", "-m", "local duplicate patch")
	if err := os.WriteFile(filepath.Join(foreign, "duplicate"), []byte("same patch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git(foreign, "add", "duplicate")
	f.git(foreign, "commit", "-q", "-m", "foreign duplicate patch")
	f.git(foreign, "push", "-q", "origin", "HEAD:feature")
	if err := os.WriteFile(filepath.Join(f.work, "local-change"), []byte("local-change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var commitOut bytes.Buffer
	err := (&App{Dir: f.work, Stdout: &commitOut, Stderr: io.Discard}).Commit(context.Background(), CommitOptions{Message: message.Source{Parts: []string{"feat: local after sync"}}})
	const previousHint = "push failed for 1 of 1 remotes (origin); local refs are kept, retry with: git push origin HEAD"
	if err == nil || err.Error() != previousHint {
		t.Fatalf("foreign commit output = %q, error = %v", commitOut.String(), err)
	}
}

func TestWorktreeSyncDoesNotCallForeignTipOldHistory(t *testing.T) {
	f := newSyncPushFixture(t, false)
	syncStaleRemoteBranch(t, f)
	foreign := f.root + "/foreign"
	f.git(f.root, "clone", "-q", f.remote, foreign)
	f.git(foreign, "checkout", "-q", "-b", "feature", "--track", "origin/feature")
	f.git(foreign, "config", "user.name", "Other")
	f.git(foreign, "config", "user.email", "other@example.invalid")
	f.commit(foreign, "foreign-change")
	f.git(foreign, "push", "-q", "origin", "HEAD:feature")
	var out bytes.Buffer
	err := (&App{Dir: f.work, Stdout: &out, Stderr: io.Discard}).WorktreeSync(context.Background(), WorktreeSyncOptions{Trunk: "origin/main"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "holds the old history") || strings.Contains(out.String(), "graft worktree sync --push") {
		t.Fatalf("sync advised overwriting a foreign remote tip: %s", out.String())
	}
}

func TestCommitAndSyncDoNotAdviseForDifferentUpstreamBranch(t *testing.T) {
	f := newSyncPushFixture(t, false)
	syncStaleRemoteBranch(t, f)
	f.git(f.work, "push", "-q", "origin", "refs/heads/feature:refs/heads/other")
	f.git(f.work, "branch", "--set-upstream-to=origin/other", "feature")
	var syncOut bytes.Buffer
	if err := (&App{Dir: f.work, Stdout: &syncOut, Stderr: io.Discard}).WorktreeSync(context.Background(), WorktreeSyncOptions{Trunk: "origin/main"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(syncOut.String(), "graft worktree sync --push") || strings.Contains(syncOut.String(), "holds the old history") {
		t.Fatalf("sync advised an unusable upstream: %s", syncOut.String())
	}
	if err := os.WriteFile(filepath.Join(f.work, "after-sync"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := (&App{Dir: f.work, Stdout: io.Discard, Stderr: io.Discard}).Commit(context.Background(), CommitOptions{Message: message.Source{Parts: []string{"feat: after sync"}}})
	if err == nil || !strings.Contains(err.Error(), "retry with: git push origin HEAD") || strings.Contains(err.Error(), "graft worktree sync --push") {
		t.Fatalf("commit advice for mismatched upstream = %v", err)
	}
}

func TestCommitAdviceNamesEveryStaleRemote(t *testing.T) {
	f := newSyncPushFixture(t, false)
	second := filepath.Join(f.root, "second.git")
	f.git(f.root, "init", "--bare", "-q", "-b", "main", second)
	f.git(f.work, "remote", "add", "second", second)
	if err := os.WriteFile(filepath.Join(f.work, ".graft.yaml"), []byte("schema: 1\nversion: {mode: none}\npush:\n  remotes: [origin, second]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git(f.work, "add", ".graft.yaml")
	f.git(f.work, "commit", "-q", "-m", "configure push remotes")
	f.git(f.work, "push", "-q", "origin", "main")
	f.git(f.work, "push", "-q", "second", "main")
	syncStaleRemoteBranch(t, f)
	f.git(f.work, "push", "-q", "second", "feature")
	f.git(f.work, "push", "-q", "second", "main")
	f.git(f.work, "branch", "--set-upstream-to=second/feature", "feature")
	oldTip := f.git(f.work, "rev-parse", "refs/remotes/origin/feature")
	var syncOut bytes.Buffer
	if err := (&App{Dir: f.work, Stdout: &syncOut, Stderr: io.Discard}).WorktreeSync(context.Background(), WorktreeSyncOptions{Trunk: "origin/main"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(syncOut.String(), "graft worktree sync --push") {
		t.Fatalf("sync did not identify upstream old history: %s", syncOut.String())
	}
	if err := os.WriteFile(filepath.Join(f.work, "post-sync"), []byte("post-sync\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := (&App{Dir: f.work, Stdout: io.Discard, Stderr: io.Discard}).Commit(context.Background(), CommitOptions{Message: message.Source{Parts: []string{"feat: post sync"}}})
	if err == nil {
		t.Fatal("commit unexpectedly pushed to stale remotes")
	}
	wantUpstream := "second: run `graft worktree sync --push`"
	wantOther := "origin: run `git push --force-with-lease=refs/heads/feature:" + oldTip + " origin HEAD:refs/heads/feature`"
	if !strings.Contains(err.Error(), wantUpstream) || !strings.Contains(err.Error(), wantOther) {
		t.Fatalf("multi-remote advice = %v; want %q and %q", err, wantUpstream, wantOther)
	}
	if err := (&App{Dir: f.work, Stdout: io.Discard, Stderr: io.Discard}).WorktreeSync(context.Background(), WorktreeSyncOptions{Push: true, Trunk: "origin/main"}); err != nil {
		t.Fatalf("sync --push upstream: %v", err)
	}
	f.git(f.work, "push", "--force-with-lease=refs/heads/feature:"+oldTip, "origin", "HEAD:refs/heads/feature")
	for _, remote := range []struct{ name, path string }{{"origin", f.remote}, {"second", second}} {
		if got, want := f.git(f.work, "rev-parse", "HEAD"), f.git(remote.path, "rev-parse", "refs/heads/feature"); got != want {
			t.Errorf("%s feature = %s, want HEAD %s", remote.name, want, got)
		}
	}
}

func TestCommitAdviceKeepsRetryForForeignRemoteInMixedFailure(t *testing.T) {
	f := newSyncPushFixture(t, false)
	second := filepath.Join(f.root, "second.git")
	f.git(f.root, "init", "--bare", "-q", "-b", "main", second)
	f.git(f.work, "remote", "add", "second", second)
	if err := os.WriteFile(filepath.Join(f.work, ".graft.yaml"), []byte("schema: 1\nversion: {mode: none}\npush:\n  remotes: [origin, second]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git(f.work, "add", ".graft.yaml")
	f.git(f.work, "commit", "-q", "-m", "configure push remotes")
	f.git(f.work, "push", "-q", "origin", "main")
	f.git(f.work, "push", "-q", "second", "main")
	syncStaleRemoteBranch(t, f)
	f.git(f.work, "push", "-q", "second", "feature")
	f.git(f.work, "push", "-q", "second", "main")
	f.git(f.work, "branch", "--set-upstream-to=second/feature", "feature")
	oldOriginTip := f.git(f.work, "rev-parse", "refs/remotes/origin/feature")
	foreign := filepath.Join(f.root, "foreign")
	f.git(f.root, "clone", "-q", second, foreign)
	f.git(foreign, "checkout", "-q", "-b", "feature", "--track", "origin/feature")
	f.git(foreign, "config", "user.name", "Other")
	f.git(foreign, "config", "user.email", "other@example.invalid")
	f.commit(foreign, "foreign-change")
	f.git(foreign, "push", "-q", "origin", "HEAD:feature")
	foreignTip := f.git(foreign, "rev-parse", "HEAD")
	if err := (&App{Dir: f.work, Stdout: io.Discard, Stderr: io.Discard}).WorktreeSync(context.Background(), WorktreeSyncOptions{Trunk: "origin/main"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.work, "local-change"), []byte("local-change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := (&App{Dir: f.work, Stdout: io.Discard, Stderr: io.Discard}).Commit(context.Background(), CommitOptions{Message: message.Source{Parts: []string{"feat: mixed remote failure"}}})
	if err == nil {
		t.Fatal("commit unexpectedly pushed to both remotes")
	}
	wantOrigin := "origin: run `git push --force-with-lease=refs/heads/feature:" + oldOriginTip + " origin HEAD:refs/heads/feature`"
	wantSecond := "second: retry with `git push second HEAD`"
	if !strings.Contains(err.Error(), wantOrigin) || !strings.Contains(err.Error(), wantSecond) {
		t.Fatalf("mixed remote advice = %v; want %q and %q", err, wantOrigin, wantSecond)
	}
	f.git(f.work, "push", "--force-with-lease=refs/heads/feature:"+oldOriginTip, "origin", "HEAD:refs/heads/feature")
	if got := f.git(second, "rev-parse", "refs/heads/feature"); got != foreignTip {
		t.Fatalf("second feature tip = %s, want preserved foreign tip %s", got, foreignTip)
	}
}

func TestWorktreeSyncStaleUpstreamHintRequiresUpstreamAndDivergence(t *testing.T) {
	f := newSyncPushFixture(t, false)
	repo, err := gitx.Open(f.work)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := worktreeSyncStaleUpstreamHint(&App{Stdout: &out}, repo); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("hint without divergence = %q", out.String())
	}
	f.git(f.work, "checkout", "-q", "-b", "no-upstream")
	if err := worktreeSyncStaleUpstreamHint(&App{Stdout: &out}, repo); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("hint without upstream = %q", out.String())
	}
}
