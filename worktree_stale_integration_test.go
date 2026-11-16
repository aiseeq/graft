package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStaleRepo(t *testing.T) *fixture {
	t.Helper()
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\n",
		"base.txt":    "base\n",
	})
	f.git("checkout", "-q", "-b", "main")
	f.run(f.work, "git", "add", "-A")
	f.run(f.work, "git", "commit", "-q", "-m", "base")
	return f
}

func makeOld(t *testing.T, path string) {
	t.Helper()
	writeFile(t, path, "old\n")
	old := time.Now().Add(-4 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func staleCommit(t *testing.T, f *fixture, dir, message string) {
	t.Helper()
	f.env = append(f.env, "GIT_COMMITTER_DATE=2020-01-01T00:00:00Z", "GIT_AUTHOR_DATE=2020-01-01T00:00:00Z")
	if out, code := f.exec(dir, "", "git", "add", "-A"); code != 0 {
		t.Fatalf("stage stale commit: exit %d\n%s", code, out)
	}
	if out, code := f.exec(dir, "", "git", "commit", "-q", "-m", message); code != 0 {
		t.Fatalf("create stale commit: exit %d\n%s", code, out)
	}
	f.env = nil
}

func TestWorktreeStaleIncludesMainAndLinkedTrees(t *testing.T) {
	f := newStaleRepo(t)
	linked := filepath.Join(f.base, "linked")
	f.git("worktree", "add", "-q", "-b", "linked", linked, "main")
	f.git("checkout", "-q", "-b", "topic")
	writeFile(t, filepath.Join(f.work, "topic.txt"), "old topic\n")
	staleCommit(t, f, f.work, "old main worktree commit")
	makeOld(t, filepath.Join(f.work, "main-old.txt"))
	makeOld(t, filepath.Join(linked, "linked-old.txt"))
	writeFile(t, filepath.Join(linked, "fresh.txt"), "fresh\n")

	out, code := f.graft("", "worktree", "stale", "--uncommitted", "3h", "--unmerged", "24h")
	if code != 1 || !strings.Contains(out, f.work+": 1 uncommitted files") || !strings.Contains(out, "1 unmerged commits, last commit") || !strings.Contains(out, linked+": 1 uncommitted files") || strings.Contains(out, "fresh.txt") {
		t.Fatalf("main and linked changes should be listed, exit %d:\n%s", code, out)
	}

	for _, path := range []string{filepath.Join(f.work, "main-old.txt"), filepath.Join(linked, "linked-old.txt")} {
		if err := os.Chtimes(path, time.Now(), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	out, code = f.graft("", "worktree", "stale", "--uncommitted", "3h")
	if code != 0 || !strings.Contains(out, "nothing stale") {
		t.Fatalf("young files should not be stale, exit %d:\n%s", code, out)
	}
}

func TestWorktreeStaleScansCacheAndExtraCatalog(t *testing.T) {
	project := newStaleRepo(t)
	foreign := newStaleRepo(t)
	cache := filepath.Join(currentTestTempDir(), "cache", "worktrees", filepath.Base(foreign.work), "cached")
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign.git("worktree", "add", "-q", "-b", "cached", cache, "main")
	makeOld(t, filepath.Join(cache, "cached-old.txt"))
	extra := filepath.Join(project.base, "extra-catalog")
	extraTree := filepath.Join(extra, "tree")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	foreign.git("worktree", "add", "-q", "-b", "extra", extraTree, "main")
	makeOld(t, filepath.Join(extraTree, "extra-old.txt"))

	out, code := project.graft("", "worktree", "stale", "--uncommitted", "3h")
	if code != 1 || !strings.Contains(out, cache+": 1 uncommitted files") {
		t.Fatalf("graft cache worktree should be listed, exit %d:\n%s", code, out)
	}
	out, code = project.graft("", "worktree", "stale", "--uncommitted", "3h", "--in", extra)
	if code != 1 || !strings.Contains(out, cache+": 1 uncommitted files") || !strings.Contains(out, extraTree+": 1 uncommitted files") {
		t.Fatalf("cache and extra catalog worktrees should be listed, exit %d:\n%s", code, out)
	}
}

func TestWorktreeStaleDoesNotFetchOrPrune(t *testing.T) {
	f := newStaleRepo(t)
	ref := strings.TrimSpace(f.git("rev-parse", "HEAD"))
	f.git("remote", "add", "origin", filepath.Join(f.base, "missing-remote"))
	f.git("update-ref", "refs/remotes/origin/gone", ref)
	makeOld(t, filepath.Join(f.work, "old.txt"))
	out, code := f.graft("", "worktree", "stale", "--uncommitted", "3h")
	if code != 1 || !strings.Contains(out, ": 1 uncommitted files") {
		t.Fatalf("uncommitted scan should not need a remote, exit %d:\n%s", code, out)
	}
	if got := strings.TrimSpace(f.git("rev-parse", "refs/remotes/origin/gone")); got != ref {
		t.Fatalf("stale scan changed the remote tracking ref: got %s, want %s", got, ref)
	}
}

func TestWorktreeStaleChecksFilesInsideSubmodule(t *testing.T) {
	f := newStaleRepo(t)
	source := filepath.Join(f.base, "submodule-source")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	f.run(source, "git", "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(source, "file.txt"), "base\n")
	f.run(source, "git", "add", "-A")
	f.run(source, "git", "commit", "-q", "-m", "submodule base")
	if out, code := f.exec(f.work, "", "git", "-c", "protocol.file.allow=always", "submodule", "add", "-q", source, "module"); code != 0 {
		t.Fatalf("add submodule: exit %d\n%s", code, out)
	}
	f.run(f.work, "git", "add", "-A")
	f.run(f.work, "git", "commit", "-q", "-m", "add submodule")
	file := filepath.Join(f.work, "module", "file.txt")
	writeFile(t, file, "old edit\n")
	old := time.Now().Add(-4 * time.Hour)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(filepath.Join(f.work, "module"), now, now); err != nil {
		t.Fatal(err)
	}
	out, code := f.graft("", "worktree", "stale", "--uncommitted", "3h")
	if code != 1 || !strings.Contains(out, f.work+": 1 uncommitted files") {
		t.Fatalf("old submodule file should be listed by its own mtime, exit %d:\n%s", code, out)
	}
}

func TestWorktreeStaleReportsPrunableBranch(t *testing.T) {
	f := newStaleRepo(t)
	path := filepath.Join(f.base, "gone-worktree")
	f.git("worktree", "add", "-q", "-b", "stale-topic", path, "main")
	writeFile(t, filepath.Join(path, "topic.txt"), "old commit\n")
	staleCommit(t, f, path, "old topic")
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.git("worktree", "list", "--porcelain"), "prunable") {
		t.Fatal("fixture worktree is not prunable")
	}
	out, code := f.graft("", "worktree", "stale", "--unmerged", "24h")
	if code != 1 || !strings.Contains(out, "branch stale-topic (prunable worktree") || !strings.Contains(out, "1 unmerged commits") {
		t.Fatalf("prunable branch should be checked, exit %d:\n%s", code, out)
	}
}

func TestWorktreeStaleSkipsUnmergedWithoutLocalTrunk(t *testing.T) {
	f := newStaleRepo(t)
	f.git("branch", "master")
	f.git("checkout", "-q", "-b", "feature")
	linked := filepath.Join(f.base, "old-feature")
	f.git("worktree", "add", "-q", "-b", "old-feature", linked, "main")
	writeFile(t, filepath.Join(linked, "old.txt"), "old unmerged change\n")
	staleCommit(t, f, linked, "old unmerged change")
	out, code := f.graft("", "worktree", "stale", "--unmerged", "24h")
	if code != 2 || !strings.Contains(out, "unmerged check skipped") || !strings.Contains(out, "specify --trunk <ref>") || strings.Contains(out, "nothing stale") {
		t.Fatalf("unavailable trunk should be a verification error, exit %d:\n%s", code, out)
	}
	out, code = f.graft("", "worktree", "stale", "--unmerged", "24h", "--trunk", "main")
	if code != 1 || !strings.Contains(out, "1 unmerged commits") {
		t.Fatalf("explicit local trunk should enable the unmerged check, exit %d:\n%s", code, out)
	}
}

func TestWorktreeStaleUsesCleanContentRulesForVersionConflicts(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n",
		"VERSION":     "1.0.0\n",
		"base.txt":    "base\n",
	})
	f.git("checkout", "-q", "-b", "main")
	f.run(f.work, "git", "add", "-A")
	f.run(f.work, "git", "commit", "-q", "-m", "base")
	linked := filepath.Join(f.base, "version-topic")
	f.git("worktree", "add", "-q", "-b", "version-topic", linked, "main")
	writeFile(t, filepath.Join(linked, "VERSION"), "1.1.0\n")
	staleCommit(t, f, linked, "topic version")
	writeFile(t, filepath.Join(f.work, "VERSION"), "1.2.0\n")
	f.run(f.work, "git", "add", "VERSION")
	f.run(f.work, "git", "commit", "-q", "-m", "trunk version")

	cleanOut, cleanCode := f.graft("", "worktree", "clean", "--min-age", "0s")
	if cleanCode != 0 || !strings.Contains(cleanOut, "version-topic") || !strings.Contains(cleanOut, "would remove") {
		t.Fatalf("clean should consider the version-only conflict merged, exit %d:\n%s", cleanCode, cleanOut)
	}
	out, code := f.graft("", "worktree", "stale", "--unmerged", "24h")
	if code != 0 || !strings.Contains(out, "nothing stale") {
		t.Fatalf("stale should use clean's version-file exclusions, exit %d:\n%s", code, out)
	}
}

func TestWorktreeStaleIgnoresYoungUnmergedCommit(t *testing.T) {
	f := newStaleRepo(t)
	linked := filepath.Join(f.base, "young-topic")
	f.git("worktree", "add", "-q", "-b", "young-topic", linked, "main")
	writeFile(t, filepath.Join(linked, "young.txt"), "young unmerged change\n")
	f.run(linked, "git", "add", "young.txt")
	f.run(linked, "git", "commit", "-q", "-m", "young unmerged change")
	out, code := f.graft("", "worktree", "stale", "--unmerged", "24h")
	if code != 0 || !strings.Contains(out, "nothing stale") || strings.Contains(out, "young-topic") {
		t.Fatalf("young unmerged commit should not be stale, exit %d:\n%s", code, out)
	}
}

func TestWorktreeStaleUsesContentToRecognizeMergedWork(t *testing.T) {
	f := newStaleRepo(t)
	branch := filepath.Join(f.base, "rewritten")
	f.git("worktree", "add", "-q", "-b", "rewritten", branch, "main")
	writeFile(t, filepath.Join(branch, "same.txt"), "same content\n")
	staleCommit(t, f, branch, "old branch commit")
	writeFile(t, filepath.Join(f.work, "same.txt"), "same content\n")
	f.run(f.work, "git", "add", "same.txt")
	f.run(f.work, "git", "commit", "-q", "-m", "content already on trunk")
	objectsBefore := f.git("count-objects", "-v")
	out, code := f.graft("", "worktree", "stale", "--unmerged", "24h")
	if code != 0 || !strings.Contains(out, "nothing stale") {
		t.Fatalf("same content under another commit should be merged, exit %d:\n%s", code, out)
	}
	if objectsAfter := f.git("count-objects", "-v"); objectsAfter != objectsBefore {
		t.Fatalf("stale scan wrote objects to the repository:\nbefore:\n%s\nafter:\n%s", objectsBefore, objectsAfter)
	}
}

func TestWorktreeStaleUncommittedWorksWithoutAnyCommit(t *testing.T) {
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none}\n"})
	makeOld(t, filepath.Join(f.work, "old.txt"))
	out, code := f.graft("", "worktree", "stale", "--uncommitted", "3h")
	if code != 1 || !strings.Contains(out, f.work+": 1 uncommitted files") {
		t.Fatalf("file scan must not require a trunk, exit %d:\n%s", code, out)
	}
}

func TestWorktreeStaleValidatesExplicitTrunkInEmptyRepository(t *testing.T) {
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none}\n"})
	out, code := f.graft("", "worktree", "stale", "--unmerged", "24h")
	if code != 0 || !strings.Contains(out, "nothing stale") {
		t.Fatalf("without a trunk override, empty repository has nothing to check, exit %d:\n%s", code, out)
	}
	out, code = f.graft("", "worktree", "stale", "--unmerged", "24h", "--trunk", "nonexistent")
	if code != 2 || !strings.Contains(out, "nonexistent") || !strings.Contains(out, "specify --trunk <ref>") || strings.Contains(out, "nothing stale") {
		t.Fatalf("invalid explicit trunk must be a verification error even when empty, exit %d:\n%s", code, out)
	}
}
