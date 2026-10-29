package gitx

import (
	"errors"
	"os/exec"
	"path/filepath"
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
