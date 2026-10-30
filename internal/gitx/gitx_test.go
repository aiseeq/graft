package gitx

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenTellsNoRepositoryFromAFailingGit(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	if _, err := Open(dir); !errors.Is(err, ErrNotRepo) {
		t.Errorf("outside a repository: %v", err)
	}
	bare := filepath.Join(dir, "bare.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", bare).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	if _, err := Open(bare); !errors.Is(err, ErrNotRepo) {
		t.Errorf("a bare repository has no work tree: %v", err)
	}
	t.Setenv("PATH", "")
	if _, err := Open(dir); err == nil || errors.Is(err, ErrNotRepo) {
		t.Errorf("no git is not outside a repository: %v", err)
	}
}

func TestRepoStateCacheTracksRepositoryMutations(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	repo, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if repo.ObjectFormat != "sha1" {
		t.Fatalf("repository object format: %q", repo.ObjectFormat)
	}

	if hasHead, err := repo.HasHead(); err != nil || hasHead {
		t.Fatalf("empty repository has HEAD: %t, %v", hasHead, err)
	}
	if !repo.headKnown {
		t.Fatal("missing HEAD result was not cached")
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Git("add", "-A"); err != nil {
		t.Fatal(err)
	}

	if !repo.headKnown || repo.hasHead {
		t.Fatal("git add changed the cached HEAD result")
	}
	first, err := repo.Git("write-tree")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Git("write-tree"); err != nil || !repo.treeSet {
		t.Fatalf("write-tree result was not cached: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Git("add", "-A"); err != nil {
		t.Fatal(err)
	}
	if repo.treeSet {
		t.Fatal("git add left the cached index tree valid")
	}
	second, err := repo.Git("write-tree")
	if err != nil || strings.TrimSpace(first) == strings.TrimSpace(second) {
		t.Fatalf("write-tree after git add: %q, %v", second, err)
	}
	if out, err := exec.Command("git", "-C", dir, "config", "user.name", "Test").CombinedOutput(); err != nil {
		t.Fatalf("git config user.name: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "config", "user.email", "test@example.invalid").CombinedOutput(); err != nil {
		t.Fatalf("git config user.email: %v\n%s", err, out)
	}
	if _, err := repo.WorkTreeID(); err != nil || !repo.headTreeSet {
		t.Fatalf("cache initial HEAD tree: %v", err)
	}
	if _, err := repo.Git("commit", "-m", "base"); err != nil {
		t.Fatal(err)
	}
	if repo.headKnown || repo.treeSet || repo.headTreeSet {
		t.Fatal("git commit left cached HEAD, index tree, or HEAD entries valid")
	}
	if hasHead, err := repo.HasHead(); err != nil || !hasHead {
		t.Fatalf("committed repository has HEAD: %t, %v", hasHead, err)
	}
	headOID, err := repo.Git("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || strings.TrimSpace(headOID) == "" || repo.headOID != strings.TrimSpace(headOID) {
		t.Fatalf("cache HEAD object ID: %q, %v", headOID, err)
	}
	cachedHeadOID, err := repo.Git("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || strings.TrimSpace(cachedHeadOID) != strings.TrimSpace(headOID) {
		t.Fatalf("cached HEAD object ID: %q, %v", cachedHeadOID, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "next"), []byte("next\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Git("add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Git("commit", "-m", "next"); err != nil {
		t.Fatal(err)
	}
	if repo.headOID != "" || repo.headKnown {
		t.Fatal("git commit left cached HEAD state valid")
	}
	newHeadOID, err := repo.Git("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || strings.TrimSpace(newHeadOID) == strings.TrimSpace(headOID) {
		t.Fatalf("HEAD after commit: %q, %v", newHeadOID, err)
	}
	commitID, err := repo.Git("rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	commitID = strings.TrimSpace(commitID)
	if exists, err := repo.RevExists(commitID); err != nil || !exists {
		t.Fatalf("full object ID resolves: %t, %v", exists, err)
	}
	if !repo.refSet["commit-id:"+commitID] {
		t.Fatal("full object ID result was not cached")
	}
	if _, err := repo.Git("checkout", "--detach"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Branch(); err == nil || !repo.branchSet || repo.branch != "" {
		t.Fatalf("detached branch result was not cached: branch %q, error %v", repo.branch, err)
	}
	if _, err := repo.Branch(); err == nil {
		t.Fatal("cached detached branch result succeeded")
	}
	if _, err := repo.Git("checkout", "-b", "feature"); err != nil {
		t.Fatal(err)
	}
	if branch, err := repo.Branch(); err != nil || branch != "feature" {
		t.Fatalf("branch after checkout: %q, %v", branch, err)
	}
	if _, err := repo.Git("branch", "cached-ref"); err != nil {
		t.Fatal(err)
	}
	if exists, err := repo.RefExists("refs/heads/cached-ref"); err != nil || !exists {
		t.Fatalf("new branch exists: %t, %v", exists, err)
	}
	if _, err := repo.Git("branch", "-D", "cached-ref"); err != nil {
		t.Fatal(err)
	}
	if exists, err := repo.RefExists("refs/heads/cached-ref"); err != nil || exists {
		t.Fatalf("deleted branch exists: %t, %v", exists, err)
	}
	remotes, err := repo.Remotes()
	if err != nil || len(remotes) != 0 || !repo.remoteSet {
		t.Fatalf("empty remote list: %v, %v", remotes, err)
	}
	if _, err := repo.Git("remote", "add", "origin", "https://example.invalid/project.git"); err != nil {
		t.Fatal(err)
	}
	remotes, err = repo.Remotes()
	if err != nil || len(remotes) != 1 || remotes[0] != "origin" {
		t.Fatalf("remote list after adding a remote: %v, %v", remotes, err)
	}
	remotes[0] = "changed"
	remotes, err = repo.Remotes()
	if err != nil || len(remotes) != 1 || remotes[0] != "origin" {
		t.Fatalf("caller changed cached remotes: %v, %v", remotes, err)
	}
	repo.headKnown, repo.branchSet, repo.treeSet = true, true, true
	repo.Invalidate()
	if repo.headKnown || repo.headOID != "" || repo.branchSet || repo.treeSet || repo.remoteSet || repo.refSet != nil {
		t.Fatal("Invalidate left cached repository state")
	}
}
