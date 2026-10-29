package gitx

import (
	"errors"
	"testing"
)

func TestOpenTellsNoRepositoryFromAFailingGit(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	if _, err := Open(dir); !errors.Is(err, ErrNotRepo) {
		t.Errorf("outside a repository: %v", err)
	}
	t.Setenv("PATH", "")
	if _, err := Open(dir); err == nil || errors.Is(err, ErrNotRepo) {
		t.Errorf("no git is not outside a repository: %v", err)
	}
}
