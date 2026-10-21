// Package lintcache gives each work tree a golangci-lint cache of its own.
// golangci-lint keys a package's results by its content but keeps the
// absolute paths of the tree that produced them: a sibling worktree with the
// same content is handed findings in the other tree, whose //nolint comments
// it reads there, and once that tree is gone or changed, the comments are
// lost and suppressed findings fail the lint
// (https://github.com/golangci/golangci-lint/issues/6695).
package lintcache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Env is the variable golangci-lint takes its cache directory from.
const Env = "GOLANGCI_LINT_CACHE"

// rootFile names, inside a cache, the work tree it belongs to.
const rootFile = "graft-root"

// base holds the caches of all work trees.
func base() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("the user's cache directory: %w", err)
	}
	return filepath.Join(cache, "graft", "golangci-lint"), nil
}

// Dir is the cache of the work tree at root, created on first use. It lives
// in the user's cache, not in the tree: watchers of the tree would see it
// churn on every run.
func Dir(root string) (string, error) {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("the work tree %s: %w", root, err)
	}
	b, err := base()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(real))
	dir := filepath.Join(b, hex.EncodeToString(sum[:8]))
	file := filepath.Join(dir, rootFile)
	if _, err := os.Stat(file); err == nil {
		return dir, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("the lint cache: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("the lint cache: %w", err)
	}
	if err := os.WriteFile(file, []byte(real+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("the lint cache: %w", err)
	}
	return dir, nil
}

// Owned tells whether dir is a cache graft gave some work tree: a graft
// run from a step of another tree inherits it and gives its own instead.
func Owned(dir string) (bool, error) {
	b, err := base()
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(b, dir)
	// No relative path: on another volume, not under the caches.
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel), nil
}

// Prune removes the caches of work trees that are gone and returns those
// trees; a cache that cannot be read or removed is an error, after the
// others.
func Prune() ([]string, error) {
	b, err := base()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(b)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the lint caches: %w", err)
	}
	var gone []string
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(b, e.Name())
		data, err := os.ReadFile(filepath.Join(dir, rootFile))
		if errors.Is(err, os.ErrNotExist) {
			// Not one of graft's: left alone.
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("the lint cache %s: %w", dir, err))
			continue
		}
		root := strings.TrimSpace(string(data))
		if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, fmt.Errorf("the lint cache of %s: %w", root, err))
			continue
		}
		gone = append(gone, root)
	}
	return gone, errors.Join(errs...)
}
