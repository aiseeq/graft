// Package hooks installs graft's git hooks and tells a hand-written commit
// from one git's own machinery is producing.
package hooks

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aiseeq/graft/internal/gitx"
)

// Names of the hooks graft installs.
const (
	PreCommit = "pre-commit"
	PostMerge = "post-merge"
)

// Names lists every hook graft installs.
var Names = []string{PreCommit, PostMerge}

// InstallHint tells how to get graft onto PATH.
const InstallHint = "go install github.com/aiseeq/graft@latest (or make install in a graft checkout)"

// Shim is the hook file content: a POSIX sh stub that hands over to graft.
// Git for Windows runs hooks through its own sh, so the same file works there.
// A missing graft fails the hook rather than letting the commit through.
func Shim(name string) []byte {
	return []byte(`#!/bin/sh
# Installed by graft init. The hook logic lives in graft itself.
if ! command -v graft >/dev/null 2>&1; then
	echo "graft: not found in PATH, the ` + name + ` hook cannot run." >&2
	echo "Install it: ` + InstallHint + `" >&2
	exit 1
fi
exec graft hook ` + name + ` "$@"
`)
}

// Context is the kind of commit git is about to create.
type Context string

// Commit contexts.
const (
	Normal     Context = "normal"
	Merge      Context = "merge"
	Rebase     Context = "rebase"
	CherryPick Context = "cherry-pick"
	Revert     Context = "revert"
)

// Detect inspects the per-worktree git directory for an operation in progress.
func Detect(gitDir string) (Context, error) {
	checks := []struct {
		name string
		ctx  Context
	}{
		{"MERGE_HEAD", Merge},
		{"rebase-merge", Rebase},
		{"rebase-apply", Rebase},
		{"CHERRY_PICK_HEAD", CherryPick},
		{"REVERT_HEAD", Revert},
	}
	for _, c := range checks {
		_, err := os.Stat(filepath.Join(gitDir, c.name))
		if err == nil {
			return c.ctx, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return Normal, nil
}

// Change is one thing Install did or would do.
type Change struct {
	What string
}

// Install writes the hook shims into dir (relative to the work tree root) and
// points core.hooksPath at it. It is idempotent. It refuses to overwrite a hook
// that is not a graft shim and to repoint core.hooksPath away from a different
// hooks directory: both hold somebody's hooks that would silently stop running.
func Install(repo *gitx.Repo, dir string) ([]Change, error) {
	hooksDir := filepath.Join(repo.Root, filepath.FromSlash(dir))
	var changes []Change

	current, err := hooksPath(repo)
	if err != nil {
		return nil, err
	}
	if current != "" && current != dir {
		resolved := current
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(repo.Root, filepath.FromSlash(resolved))
		}
		if !samePath(resolved, hooksDir) {
			return nil, fmt.Errorf("core.hooksPath is %q, not %q: move those hooks or unset it (git config --unset core.hooksPath) first", current, dir)
		}
	}

	var foreign []string
	for _, name := range Names {
		existing, err := os.ReadFile(filepath.Join(hooksDir, name))
		// A shim that only differs by CRLF line endings (autocrlf checkout)
		// is ours and gets rewritten: sh would choke on the carriage returns.
		if err == nil && !bytes.Equal(bytes.ReplaceAll(existing, []byte("\r\n"), []byte("\n")), Shim(name)) {
			foreign = append(foreign, filepath.ToSlash(filepath.Join(dir, name)))
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if len(foreign) > 0 {
		return nil, fmt.Errorf("hooks not written by graft are in the way: %s; move their checks into the gate in .graft.yaml and delete them", strings.Join(foreign, ", "))
	}

	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return nil, err
	}
	for _, name := range Names {
		changed, err := writeShim(filepath.Join(hooksDir, name), Shim(name))
		if err != nil {
			return nil, err
		}
		if changed != "" {
			changes = append(changes, Change{What: changed + " " + filepath.ToSlash(filepath.Join(dir, name))})
		}
	}

	if current != dir {
		if _, err := repo.Git("config", "core.hooksPath", dir); err != nil {
			return nil, err
		}
		if current == "" {
			changes = append(changes, Change{What: "set core.hooksPath to " + dir})
		} else {
			changes = append(changes, Change{What: fmt.Sprintf("rewrote core.hooksPath from %q to %q", current, dir)})
		}
	}
	return changes, nil
}

// writeShim writes the shim unless it is already in place with the executable
// bit set; a clone made on a filesystem without modes loses the bit.
func writeShim(path string, content []byte) (string, error) {
	existing, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "wrote", os.WriteFile(path, content, 0o755)
	case err != nil:
		return "", err
	case !bytes.Equal(existing, content):
		return "rewrote (line endings)", os.WriteFile(path, content, 0o755)
	}
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return "", err
	case info.Mode().Perm()&0o111 == 0o111:
		return "", nil
	default:
		return "made executable", os.Chmod(path, info.Mode().Perm()|0o755)
	}
}

func hooksPath(repo *gitx.Repo) (string, error) {
	out, err := repo.Git("config", "--get", "core.hooksPath")
	if err != nil {
		if gitx.ExitCode(err) == 1 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func samePath(a, b string) bool {
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	if errA == nil && errB == nil {
		return os.SameFile(ia, ib)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
