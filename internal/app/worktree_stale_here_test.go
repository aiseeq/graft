package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/gitx"
)

func TestWorktreeStaleHereLimitsToCurrentWorktree(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "cache"))
	main := initStaleTestRepo(t, filepath.Join(base, "projectB"))
	worktree := filepath.Join(base, "cache", "worktrees", "projectB", "feature")
	gitStaleTest(t, main, "worktree", "add", "-b", "feature", worktree)
	writeOldStaleTestFile(t, filepath.Join(main, "old-main.txt"))

	var out bytes.Buffer
	app := &App{Dir: worktree, Stdout: &out}
	err := app.WorktreeStale(context.Background(), WorktreeStaleOptions{Uncommitted: time.Hour, UncommittedSet: true, Here: true})
	if err != nil {
		t.Fatalf("--here with changes only in the main worktree: %v", err)
	}
	if got := out.String(); got != "graft: nothing stale\n" {
		t.Fatalf("--here output = %q, want nothing stale", got)
	}

	writeOldStaleTestFile(t, filepath.Join(worktree, "old-worktree.txt"))
	out.Reset()
	err = app.WorktreeStale(context.Background(), WorktreeStaleOptions{Uncommitted: time.Hour, UncommittedSet: true, Here: true})
	var codeErr *ExitCodeError
	if !errors.As(err, &codeErr) || codeErr.Code != 1 {
		t.Fatalf("--here stale result = %v, want exit 1", err)
	}
	if got := out.String(); !strings.Contains(got, "stale     "+worktree+":") {
		t.Fatalf("--here output = %q, want current worktree path", got)
	}

	if err := os.Remove(filepath.Join(worktree, "old-worktree.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "new-worktree.txt"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	err = app.WorktreeStale(context.Background(), WorktreeStaleOptions{Uncommitted: time.Hour, UncommittedSet: true, Here: true})
	if err != nil || out.String() != "graft: nothing stale\n" {
		t.Fatalf("--here with a young change: output %q, error %v", out.String(), err)
	}
}

func TestWorktreeStaleCountsChangedAndAddedFilesUnderCopyDirectory(t *testing.T) {
	base := t.TempDir()
	main := initStaleTestRepo(t, filepath.Join(base, "project"))
	if err := os.WriteFile(filepath.Join(main, ".graft.yaml"), []byte("schema: 1\nversion: {mode: none}\nworktree:\n  copy: [data/]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, ".gitignore"), []byte("data/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitStaleTest(t, main, "add", ".graft.yaml", ".gitignore")
	gitStaleTest(t, main, "commit", "-m", "configure copied data")
	worktree := filepath.Join(base, "feature")
	gitStaleTest(t, main, "worktree", "add", "-b", "feature", worktree)
	if err := os.MkdirAll(filepath.Join(main, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(worktree, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeOldStaleTestFile(t, filepath.Join(main, "data", "same.txt"))
	writeOldStaleTestFile(t, filepath.Join(worktree, "data", "same.txt"))
	if count, _, err := staleFileCount(worktree, time.Hour); err != nil || count != 0 {
		t.Fatalf("matching copied files: count %d, error %v; want 0", count, err)
	}
	changed := filepath.Join(worktree, "data", "same.txt")
	if err := os.WriteFile(changed, []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(changed, old, old); err != nil {
		t.Fatal(err)
	}
	writeOldStaleTestFile(t, filepath.Join(worktree, "data", "added.txt"))
	if count, _, err := staleFileCount(worktree, time.Hour); err != nil || count != 2 {
		t.Fatalf("changed and added copied files: count %d, error %v; want 2", count, err)
	}
}

func TestWorktreeStaleIgnoresDisposableCopiedFile(t *testing.T) {
	base := t.TempDir()
	main := initStaleTestRepo(t, filepath.Join(base, "project"))
	if err := os.WriteFile(filepath.Join(main, ".graft.yaml"), []byte("schema: 1\nversion: {mode: none}\nworktree:\n  copy: [.env]\n  disposable: [.env]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, ".gitignore"), []byte(".env\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitStaleTest(t, main, "add", ".graft.yaml", ".gitignore")
	gitStaleTest(t, main, "commit", "-m", "configure copied disposable file")
	worktree := filepath.Join(base, "feature")
	gitStaleTest(t, main, "worktree", "add", "-b", "feature", worktree)
	writeOldStaleTestFile(t, filepath.Join(main, ".env"))
	writeOldStaleTestFile(t, filepath.Join(worktree, ".env"))
	changed := filepath.Join(worktree, ".env")
	if err := os.WriteFile(changed, []byte("changed env\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(changed, old, old); err != nil {
		t.Fatal(err)
	}
	if count, _, err := staleFileCount(worktree, time.Hour); err != nil || count != 0 {
		t.Fatalf("disposable copied .env: count %d, error %v; want 0", count, err)
	}
}

func TestWorktreeStaleMainConfigErrorIsANoteAndContinues(t *testing.T) {
	base := t.TempDir()
	main := initStaleTestRepo(t, filepath.Join(base, "project"))
	if err := os.WriteFile(filepath.Join(main, ".graft.yaml"), []byte("schema: 1\nversion: {mode: none}\nworktree:\n  copy: [.env]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitStaleTest(t, main, "add", ".graft.yaml")
	gitStaleTest(t, main, "commit", "-m", "configure copied file")
	aPath, bPath := filepath.Join(base, "a"), filepath.Join(base, "b")
	gitStaleTest(t, main, "worktree", "add", "-b", "feature/a", aPath)
	gitStaleTest(t, main, "worktree", "add", "-b", "feature/b", bPath)
	if err := os.WriteFile(filepath.Join(main, ".graft.yaml"), []byte("schema: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeOldStaleTestFile(t, filepath.Join(aPath, "tracked.txt"))
	repoA, err := gitx.Open(aPath)
	if err != nil {
		t.Fatal(err)
	}
	repoB, err := gitx.Open(bPath)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	app := &App{Stdout: &out}
	err = app.reportStaleWorktrees(context.Background(), WorktreeStaleOptions{Uncommitted: time.Hour, UncommittedSet: true}, []staleWorktree{
		{path: aPath, branch: "feature/a", repo: repoA},
		{path: bPath, branch: "feature/b", repo: repoB},
	})
	var codeErr *ExitCodeError
	if !errors.As(err, &codeErr) || codeErr.Code != 2 {
		t.Fatalf("stale report error = %v, want incomplete-check exit 2", err)
	}
	for _, want := range []string{aPath, bPath, "uncommitted check skipped", "1 uncommitted files"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stale output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestWorktreeStaleHereLimitsUnmergedToCurrentWorktree(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "cache"))
	trunk := initStaleTestRepo(t, filepath.Join(base, "project"))
	neighbor := filepath.Join(base, "cache", "worktrees", "project", "feature")
	gitStaleTest(t, trunk, "worktree", "add", "-b", "feature", neighbor)
	if err := os.WriteFile(filepath.Join(neighbor, "feature.txt"), []byte("feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitStaleTest(t, neighbor, "add", "feature.txt")
	gitStaleTest(t, neighbor, "commit", "-m", "unmerged feature")

	o := WorktreeStaleOptions{Unmerged: 0, UnmergedSet: true, Trunk: "main"}
	var out bytes.Buffer
	if err := (&App{Dir: trunk, Stdout: &out}).WorktreeStale(context.Background(), withHere(o)); err != nil {
		t.Fatalf("trunk --here --unmerged: %v", err)
	}
	if got := out.String(); got != "graft: nothing stale\n" {
		t.Fatalf("trunk --here output = %q, want nothing stale", got)
	}

	out.Reset()
	err := (&App{Dir: trunk, Stdout: &out}).WorktreeStale(context.Background(), o)
	var codeErr *ExitCodeError
	if !errors.As(err, &codeErr) || codeErr.Code != 1 {
		t.Fatalf("trunk default unmerged result = %v, want exit 1", err)
	}
	if !strings.Contains(out.String(), "stale     "+neighbor+":") {
		t.Fatalf("trunk default output = %q, want neighboring worktree", out.String())
	}

	out.Reset()
	if err := (&App{Dir: neighbor, Stdout: &out}).WorktreeStale(context.Background(), withHere(o)); !errors.As(err, &codeErr) || codeErr.Code != 1 {
		t.Fatalf("feature --here --unmerged result = %v, want exit 1", err)
	}
	if !strings.Contains(out.String(), "stale     "+neighbor+":") {
		t.Fatalf("feature --here output = %q, want current worktree", out.String())
	}
}

func withHere(o WorktreeStaleOptions) WorktreeStaleOptions {
	o.Here = true
	return o
}

func TestWorktreeStaleHereOutsideGitUsesExitTwo(t *testing.T) {
	var out bytes.Buffer
	err := (&App{Dir: t.TempDir(), Stdout: &out}).WorktreeStale(context.Background(), WorktreeStaleOptions{UncommittedSet: true, Here: true})
	var codeErr *ExitCodeError
	if !errors.As(err, &codeErr) || codeErr.Code != 2 || !strings.Contains(err.Error(), "--here requires a Git worktree") {
		t.Fatalf("outside Git --here error = %v, want explanatory exit 2", err)
	}
}

func TestWorktreeStaleDefaultStillScansOtherProjects(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "cache"))
	projectA := initStaleTestRepo(t, filepath.Join(base, "projectA"))
	projectB := initStaleTestRepo(t, filepath.Join(base, "projectB"))
	worktree := filepath.Join(base, "cache", "worktrees", "projectB", "feature")
	gitStaleTest(t, projectB, "worktree", "add", "-b", "feature", worktree)
	writeOldStaleTestFile(t, filepath.Join(projectB, "old-main.txt"))
	writeOldStaleTestFile(t, filepath.Join(worktree, "old-worktree.txt"))

	var out bytes.Buffer
	err := (&App{Dir: projectA, Stdout: &out}).WorktreeStale(context.Background(), WorktreeStaleOptions{Uncommitted: time.Hour, UncommittedSet: true})
	var codeErr *ExitCodeError
	if !errors.As(err, &codeErr) || codeErr.Code != 1 {
		t.Fatalf("default scan result = %v, want exit 1", err)
	}
	for _, path := range []string{projectB, worktree} {
		if !strings.Contains(out.String(), "stale     "+path+":") {
			t.Errorf("default scan output %q does not name %s", out.String(), path)
		}
	}
}

func initStaleTestRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	gitStaleTest(t, dir, "init", "-b", "main")
	gitStaleTest(t, dir, "config", "user.name", "Stale Test")
	gitStaleTest(t, dir, "config", "user.email", "stale@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("tracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitStaleTest(t, dir, "add", "tracked.txt")
	gitStaleTest(t, dir, "commit", "-m", "initial")
	return dir
}

func gitStaleTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	argv := append([]string{"-c", "core.hooksPath=/dev/null", "-C", dir}, args...)
	cmd := exec.Command("git", argv...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func writeOldStaleTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, fmt.Appendf(nil, "old %s\n", filepath.Base(path)), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}
