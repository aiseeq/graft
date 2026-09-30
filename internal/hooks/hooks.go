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

// InstallHint tells how to get graft onto PATH: the version the project
// needs (X.Y.Z from its graft: key), or the latest one when it names none.
func InstallHint(minVersion string) string {
	if minVersion == "" {
		return "go install github.com/aiseeq/graft@latest"
	}
	return "go install github.com/aiseeq/graft@v" + minVersion
}

// shimMarker is the line that tells graft's shims from somebody else's hooks.
const shimMarker = "# Installed by graft init. The hook logic lives in graft itself."

// Shim is the hook file content: a POSIX sh stub that hands over to graft.
// Git for Windows runs hooks through its own sh, so the same file works there.
// A missing graft fails the hook rather than letting the commit through.
func Shim(name, minVersion string) []byte {
	return []byte(`#!/bin/sh
` + shimMarker + `
if ! command -v graft >/dev/null 2>&1; then
	echo "graft: not found in PATH, the ` + name + ` hook cannot run." >&2
	echo "Install it: ` + InstallHint(minVersion) + `" >&2
	exit 1
fi
exec graft hook ` + name + ` "$@"
`)
}

// isShim reports whether a hook file is one graft wrote, of any version.
func isShim(content []byte, name string) bool {
	text := string(bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n")))
	return strings.HasPrefix(text, "#!/bin/sh\n"+shimMarker+"\n") &&
		strings.HasSuffix(text, "\nexec graft hook "+name+` "$@"`+"\n")
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
func Install(repo *gitx.Repo, dir, minVersion string) ([]Change, error) {
	hooksDir := filepath.Join(repo.Root, filepath.FromSlash(dir))
	current, err := hooksPath(repo)
	if err != nil {
		return nil, err
	}
	if err := checkHooksPath(repo, current, dir, hooksDir); err != nil {
		return nil, err
	}
	if err := checkForeignShims(dir, hooksDir); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating the hooks directory: %w", err)
	}
	var changes []Change
	for _, name := range Names {
		changed, err := writeShim(filepath.Join(hooksDir, name), Shim(name, minVersion))
		if err != nil {
			return nil, err
		}
		if changed != "" {
			changes = append(changes, Change{What: changed + " " + filepath.ToSlash(filepath.Join(dir, name))})
		}
	}

	if current == dir {
		return changes, nil
	}
	if _, err := repo.Git("config", "core.hooksPath", dir); err != nil {
		return nil, err
	}
	if current == "" {
		return append(changes, Change{What: "set core.hooksPath to " + dir}), nil
	}
	return append(changes, Change{What: fmt.Sprintf("rewrote core.hooksPath from %q to %q", current, dir)}), nil
}

// checkHooksPath refuses to repoint core.hooksPath away from another hooks
// directory, and to set it while the default directory holds live hooks.
func checkHooksPath(repo *gitx.Repo, current, dir, hooksDir string) error {
	if current == "" {
		active, err := activeDefaultHooks(repo)
		if err != nil {
			return err
		}
		if len(active) > 0 {
			return fmt.Errorf("setting core.hooksPath would stop these hooks from running: %s; move their checks into the gate in .graft.yaml and delete them", strings.Join(active, ", "))
		}
		return nil
	}
	if current == dir {
		return nil
	}
	resolved := current
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(repo.Root, filepath.FromSlash(resolved))
	}
	if !samePath(resolved, hooksDir) {
		return fmt.Errorf("core.hooksPath is %q, not %q: move those hooks or unset it (git config --unset core.hooksPath) first", current, dir)
	}
	return nil
}

// checkForeignShims refuses to overwrite hooks graft did not write.
func checkForeignShims(dir, hooksDir string) error {
	var foreign []string
	for _, name := range Names {
		existing, err := os.ReadFile(filepath.Join(hooksDir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("checking existing hooks: %w", err)
		}
		// A shim of another graft version, or with CRLF line endings (an
		// autocrlf checkout), is ours and gets rewritten.
		if !isShim(existing, name) {
			foreign = append(foreign, filepath.ToSlash(filepath.Join(dir, name)))
		}
	}
	if len(foreign) > 0 {
		return fmt.Errorf("hooks not written by graft are in the way: %s; move their checks into the gate in .graft.yaml and delete them", strings.Join(foreign, ", "))
	}
	return nil
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
		return "rewrote", os.WriteFile(path, content, 0o755)
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

// activeDefaultHooks lists the hooks git runs from the default hooks directory
// while core.hooksPath is unset; the .sample files git creates do not run.
func activeDefaultHooks(repo *gitx.Repo) ([]string, error) {
	dir := filepath.Join(repo.CommonDir, "hooks")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing default hooks: %w", err)
	}
	active := []string{}
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".sample") {
			continue
		}
		active = append(active, filepath.Join(dir, e.Name()))
	}
	return active, nil
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
