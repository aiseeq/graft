// Package hooks installs graft's git hooks and tells a hand-written commit
// from one git's own machinery is producing.
package hooks

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/aiseeq/graft/v2/internal/gitx"
)

// Names of the hooks graft installs.
const (
	PreCommit        = "pre-commit"
	PrepareCommitMsg = "prepare-commit-msg"
	PostMerge        = "post-merge"
)

// Names lists every hook graft installs.
var Names = []string{PreCommit, PrepareCommitMsg, PostMerge}

// InstallHint gets graft onto PATH: the latest, whatever version a project
// needs at least, as graft versions are not pinned. It installs into ~/bin,
// where every graft of the user is: go install would put a second one in
// GOBIN, and the hooks would run whichever comes first on PATH.
const InstallHint = "in a checkout of github.com/aiseeq/graft: git pull && go run . install"

// shimMarker is the line that tells graft's shims from somebody else's hooks.
const shimMarker = "# Installed by graft init. The hook logic lives in graft itself."

// Shim is the hook file content: a POSIX sh stub that hands over to graft.
// Git for Windows runs hooks through its own sh, so the same file works there.
// A missing graft fails the hook rather than letting the commit through.
func Shim(name string) []byte {
	return []byte(`#!/bin/sh
` + shimMarker + `
if ! command -v graft >/dev/null 2>&1; then
	echo "graft: not found in PATH, the ` + name + ` hook cannot run." >&2
	echo "Install it, ` + InstallHint + `" >&2
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

// Outdated lists what graft init would change: core.hooksPath not pointing
// at dir as written in the config, and shims that are missing or differ from
// the ones this graft writes. Line endings do not count: a
// Windows checkout may convert them.
func Outdated(repo *gitx.Repo, dir string) ([]string, error) {
	var stale []string
	current, err := hooksPath(repo)
	if err != nil {
		return nil, err
	}
	if current != dir {
		stale = append(stale, fmt.Sprintf("core.hooksPath is %q, not %q", current, dir))
	}
	for _, name := range Names {
		path := filepath.Join(repo.Root, filepath.FromSlash(dir), name)
		existing, err := os.ReadFile(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			stale = append(stale, filepath.ToSlash(filepath.Join(dir, name))+" is missing")
		case err != nil:
			return nil, err
		case !bytes.Equal(bytes.ReplaceAll(existing, []byte("\r\n"), []byte("\n")), Shim(name)):
			stale = append(stale, filepath.ToSlash(filepath.Join(dir, name))+" differs from the shim of this graft")
		}
	}
	return stale, nil
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
		changed, err := writeShim(filepath.Join(hooksDir, name), Shim(name))
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

// GlobalHooksSince is the first git that runs hooks from its config.
var GlobalHooksSince = [2]int{2, 54}

// GitRunsConfigHooks says whether the git on PATH runs hooks from its config,
// and which version it is.
func GitRunsConfigHooks() (bool, string, error) {
	out, err := exec.Command("git", "version").Output()
	if err != nil {
		return false, "", fmt.Errorf("git version: %w", err)
	}
	return runsConfigHooks(string(out))
}

// runsConfigHooks reads the output of git version.
func runsConfigHooks(out string) (bool, string, error) {
	text := strings.TrimSpace(out)
	fields := strings.Fields(text)
	if len(fields) < 3 {
		return false, text, fmt.Errorf("git version: unexpected output %q", text)
	}
	parts := strings.SplitN(fields[2], ".", 3)
	if len(parts) < 2 {
		return false, text, fmt.Errorf("git version: unexpected output %q", text)
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	if errMajor != nil || errMinor != nil {
		return false, text, fmt.Errorf("git version: unexpected output %q", text)
	}
	return major > GlobalHooksSince[0] || major == GlobalHooksSince[0] && minor >= GlobalHooksSince[1], fields[2], nil
}

// InstallGlobal registers graft's hooks with Git's global hook config: the
// leak check before a push, and the pre-commit of projects that have not run
// graft init.
func InstallGlobal() ([]Change, error) {
	var changes []Change
	if err := installConfigHook("hook.graft-leaks", "graft hook pre-push", "pre-push", &changes); err != nil {
		return nil, err
	}
	if err := installConfigHook("hook.graft-commit", GlobalPreCommitCommand, "pre-commit", &changes); err != nil {
		return nil, err
	}
	return changes, nil
}

// GlobalPreCommitCommand runs graft only in a work tree with .graft.yaml at
// its top: a commit anywhere else, such as a test's scratch repository with a
// stub graft on its PATH, starts no graft at all.
const GlobalPreCommitCommand = `top=$(git rev-parse --show-toplevel 2>/dev/null) && test -f "$top/.graft.yaml" || exit 0; exec graft hook global-pre-commit`

func installConfigHook(name, command, event string, changes *[]Change) error {
	key := name + ".command"
	old, err := exec.Command("git", "config", "--global", "--get", key).Output()
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return fmt.Errorf("git config --global --get %s: %w", key, err)
		}
	}
	previous := strings.TrimSpace(string(old))
	if previous != command {
		cmd := exec.Command("git", "config", "--global", "--replace-all", key, command)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git config --global %s: %w: %s", key, err, strings.TrimSpace(string(out)))
		}
		if previous == "" {
			*changes = append(*changes, Change{What: "set " + key + " to " + command})
		} else {
			*changes = append(*changes, Change{What: fmt.Sprintf("rewrote %s from %q to %q", key, previous, command)})
		}
	}
	eventKey := name + ".event"
	events, err := exec.Command("git", "config", "--global", "--get-all", eventKey).Output()
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return fmt.Errorf("git config --global --get-all %s: %w", eventKey, err)
		}
	}
	for existing := range strings.FieldsSeq(string(events)) {
		if existing == event {
			return nil
		}
	}
	cmd := exec.Command("git", "config", "--global", "--add", eventKey, event)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git config --global %s: %w: %s", eventKey, err, strings.TrimSpace(string(out)))
	}
	*changes = append(*changes, Change{What: "added " + event + " to " + eventKey})
	return nil
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

// LocalShimActive reports whether git runs graft's shim for the hook name
// from the repository's core.hooksPath, which graft init points at its
// hooks directory.
func LocalShimActive(repo *gitx.Repo, name string) (bool, error) {
	dir, err := hooksPath(repo)
	if err != nil || dir == "" {
		return false, err
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repo.Root, filepath.FromSlash(dir))
	}
	path := filepath.Join(dir, name)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat hook %s: %w", path, err)
	}
	// git skips a hook without the executable bit, outside Windows.
	if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		return false, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read hook %s: %w", path, err)
	}
	return isShim(content, name), nil
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
