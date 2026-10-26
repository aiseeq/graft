package gitx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WorkTreeTree writes the tree git add -A would commit: the work tree with
// its changes and untracked files, ignored ones aside. It goes through a copy
// of the index, whose stat data spares hashing the unchanged files, and
// leaves the index and the work tree as they are.
func (r *Repo) WorkTreeTree() (string, error) {
	out, err := r.Git("rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp("", "graft-tree-")
	if err != nil {
		return "", fmt.Errorf("a directory for the work tree's index: %w", err)
	}
	defer os.RemoveAll(tmp)
	index := filepath.Join(tmp, "index")
	data, err := os.ReadFile(strings.TrimSpace(out))
	switch {
	case errors.Is(err, os.ErrNotExist):
		// No index yet: git add -A builds one.
	case err != nil:
		return "", fmt.Errorf("reading the index: %w", err)
	default:
		if err := os.WriteFile(index, data, 0o600); err != nil {
			return "", fmt.Errorf("copying the index: %w", err)
		}
	}
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := r.GitEnv(env, "add", "-A"); err != nil {
		return "", err
	}
	tree, err := r.GitEnv(env, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(tree), nil
}
