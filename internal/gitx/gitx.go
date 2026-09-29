// Package gitx runs git for graft. Every call goes through the git binary: the
// behaviour users see (hooks, config, credentials, index locking) is exactly the
// one plain git gives them.
package gitx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Repo is a work tree with its git directories resolved to absolute paths.
type Repo struct {
	// Root is the top level of the work tree.
	Root string
	// GitDir is the per-worktree git directory (MERGE_HEAD, rebase state).
	GitDir string
	// CommonDir is shared by all worktrees of the repository (refs, config).
	CommonDir string
}

// ErrNotRepo is returned by Open outside a git work tree.
var ErrNotRepo = errors.New("not inside a git work tree")

// Open resolves the repository that contains dir.
func Open(dir string) (*Repo, error) {
	out, err := run(dir, nil, nil, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-dir", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotRepo, err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		return nil, fmt.Errorf("unexpected git rev-parse output: %q", out)
	}
	return &Repo{
		Root:      filepath.FromSlash(lines[0]),
		GitDir:    filepath.FromSlash(lines[1]),
		CommonDir: filepath.FromSlash(lines[2]),
	}, nil
}

// Git runs git in the work tree root and returns its stdout.
func (r *Repo) Git(args ...string) (string, error) {
	return run(r.Root, nil, nil, args...)
}

// GitEnv runs git with extra environment entries (KEY=VALUE).
func (r *Repo) GitEnv(env []string, args ...string) (string, error) {
	return run(r.Root, env, nil, args...)
}

// GitInput runs git with stdin fed from input.
func (r *Repo) GitInput(input io.Reader, env []string, args ...string) (string, error) {
	return run(r.Root, env, input, args...)
}

// Command builds a git command in the work tree root for callers that need to
// stream its output themselves.
func (r *Repo) Command(args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Root
	return cmd
}

// Error carries the stderr of a failed git call.
type Error struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		return fmt.Sprintf("git %s: %v", strings.Join(e.Args, " "), e.Err)
	}
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, msg)
}

func (e *Error) Unwrap() error { return e.Err }

func run(dir string, env []string, input io.Reader, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Stdin = input
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), &Error{Args: args, Stderr: stderr.String(), Err: err}
	}
	return stdout.String(), nil
}

// ExitCode reports the exit status of a failed git call, or -1 when the
// process did not run at all.
func ExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// Branch returns the short name of the checked-out branch. A detached HEAD is
// an error: there is no branch to push and no branch name to take a key from.
func (r *Repo) Branch() (string, error) {
	out, err := r.Git("symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		if ExitCode(err) == 1 {
			return "", errors.New("HEAD is detached: check out a branch first")
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// HasHead reports whether HEAD points at a commit (false on an unborn branch).
func (r *Repo) HasHead() (bool, error) {
	return r.RevExists("HEAD")
}

// RevExists reports whether rev resolves to a commit.
func (r *Repo) RevExists(rev string) (bool, error) {
	_, err := r.Git("rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err == nil {
		return true, nil
	}
	if ExitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

// RefExists reports whether the fully qualified ref exists.
func (r *Repo) RefExists(ref string) (bool, error) {
	_, err := r.Git("rev-parse", "--verify", "--quiet", ref)
	if err == nil {
		return true, nil
	}
	if ExitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

// Remotes lists the configured remotes in git's order.
func (r *Repo) Remotes() ([]string, error) {
	out, err := r.Git("remote")
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

// IsClean reports whether the work tree and the index match HEAD, untracked
// files included.
func (r *Repo) IsClean() (bool, error) {
	out, err := r.Git("status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}

// RemotesContaining returns the remotes whose remote-tracking branches already
// contain rev.
func (r *Repo) RemotesContaining(rev string) ([]string, error) {
	out, err := r.Git("for-each-ref", "--contains", rev, "--format=%(refname)", "refs/remotes/")
	if err != nil {
		return nil, err
	}
	remotes, err := r.Remotes()
	if err != nil {
		return nil, err
	}
	var found []string
	for _, remote := range remotes {
		prefix := "refs/remotes/" + remote + "/"
		for _, ref := range splitLines(out) {
			if strings.HasPrefix(ref, prefix) && ref != prefix+"HEAD" {
				found = append(found, remote)
				break
			}
		}
	}
	return found, nil
}

// LocalEnvVars lists the variables git sets for hooks that bind a process to
// this repository (GIT_DIR, GIT_INDEX_FILE, ...). Child processes that may run
// git against other repositories must not inherit them.
func LocalEnvVars() ([]string, error) {
	out, err := run("", nil, nil, "rev-parse", "--local-env-vars")
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

// ShowFile returns the content of path at rev.
func (r *Repo) ShowFile(rev, path string) ([]byte, error) {
	out, err := r.Git("cat-file", "blob", rev+":"+filepath.ToSlash(path))
	if err != nil {
		return nil, fmt.Errorf("%s is not in %s: %w", path, rev, err)
	}
	return []byte(out), nil
}

func splitLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
