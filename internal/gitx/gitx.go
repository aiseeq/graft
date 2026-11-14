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
	"slices"
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

// Open resolves the repository that contains dir. Only git saying there is
// no work tree (no repository, a bare one, inside .git) is ErrNotRepo; git
// missing or refusing the repository (a dubious owner, a broken one) is an
// error of its own.
func Open(dir string) (*Repo, error) {
	// LC_ALL=C: the messages that tell there is no work tree are English.
	out, err := run(dir, []string{"LC_ALL=C"}, nil, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-dir", "--git-common-dir")
	if gerr, ok := errors.AsType[*Error](err); ok && (strings.Contains(gerr.Stderr, "not a git repository") || strings.Contains(gerr.Stderr, "must be run in a work tree")) {
		return nil, fmt.Errorf("%w: %w", ErrNotRepo, err)
	}
	if err != nil {
		return nil, err
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

// MainWorktree returns the first worktree listed by git. Git lists the main
// checkout first, regardless of where the repository's common directory is.
func (r *Repo) MainWorktree() (string, error) {
	out, err := r.Git("worktree", "list", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("listing worktrees: %w", err)
	}
	if out == "" {
		return "", errors.New("git worktree list returned no entries")
	}
	first := strings.SplitN(strings.TrimRight(out, "\n"), "\n\n", 2)[0]
	var worktree string
	for line := range strings.SplitSeq(first, "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case line == "bare":
			return "", errors.New("first worktree entry is bare")
		case strings.HasPrefix(line, "worktree "):
			worktree = strings.TrimPrefix(line, "worktree ")
		}
	}
	if worktree == "" {
		return "", fmt.Errorf("unexpected first git worktree list entry: %q", first)
	}
	worktree = filepath.FromSlash(worktree)
	if filepath.Clean(worktree) == filepath.Clean(r.CommonDir) {
		// Older Git versions report the common git directory as the first
		// worktree for repositories initialized with --separate-git-dir and
		// for submodules. The main checkout is the current root when it owns
		// the common directory; linked worktrees retain its path in config.
		if filepath.Clean(r.GitDir) == filepath.Clean(r.CommonDir) {
			return r.Root, nil
		}
		configured, configErr := r.Git("config", "--path", "--get", "core.worktree")
		if configErr != nil || strings.TrimSpace(configured) == "" {
			return "", fmt.Errorf("git worktree list reports the common directory %s and the main checkout is not configured", r.CommonDir)
		}
		configured = filepath.FromSlash(strings.TrimSpace(configured))
		if !filepath.IsAbs(configured) {
			configured = filepath.Join(r.CommonDir, configured)
		}
		return filepath.Clean(configured), nil
	}
	return worktree, nil
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

// InOther runs git in dir, a work tree other than the one a hook may run in,
// with stdin fed from input: without the variables git sets for the hook
// (LocalEnvVars), which would bind it to the hook's repository and index.
func InOther(dir string, input io.Reader, args ...string) (string, error) {
	drop, err := LocalEnvVars()
	if err != nil {
		return "", fmt.Errorf("listing git environment variables: %w", err)
	}
	return runEnviron(dir, Without(os.Environ(), drop), input, args...)
}

// InOtherStrict is InOther for a command that warns and exits 0 when it could
// not read all it was asked to, as git ls-files does: a warning is an error.
func InOtherStrict(dir string, args ...string) (string, error) {
	drop, err := LocalEnvVars()
	if err != nil {
		return "", fmt.Errorf("listing git environment variables: %w", err)
	}
	stdout, stderr, err := runCaptured(dir, Without(os.Environ(), drop), nil, args...)
	if err != nil {
		return stdout, err
	}
	if strings.TrimSpace(stderr) != "" {
		return stdout, &Error{Args: args, Stderr: stderr, Err: errors.New("it could not read everything")}
	}
	return stdout, nil
}

func run(dir string, env []string, input io.Reader, args ...string) (string, error) {
	var environ []string
	if len(env) > 0 {
		environ = append(os.Environ(), env...)
	}
	return runEnviron(dir, environ, input, args...)
}

// runEnviron runs git with the environment environ, nil inheriting this
// process's.
func runEnviron(dir string, environ []string, input io.Reader, args ...string) (string, error) {
	stdout, _, err := runCaptured(dir, environ, input, args...)
	return stdout, err
}

func runCaptured(dir string, environ []string, input io.Reader, args ...string) (string, string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = environ
	cmd.Stdin = input
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), stderr.String(), &Error{Args: args, Stderr: stderr.String(), Err: err}
	}
	return stdout.String(), stderr.String(), nil
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

// ErrDetachedHead means HEAD does not point at a branch.
var ErrDetachedHead = errors.New("HEAD is detached")

// Branch returns the short name of the checked-out branch. A detached HEAD is
// an error: there is no branch to push and no branch name to take a key from.
func (r *Repo) Branch() (string, error) {
	out, err := r.Git("symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		if ExitCode(err) == 1 {
			return "", fmt.Errorf("%w: check out a branch first", ErrDetachedHead)
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
	return r.gitTest("rev-parse", "--verify", "--quiet", rev+"^{commit}")
}

// IsAncestor reports whether ancestor is reachable from rev (a commit is its
// own ancestor).
func (r *Repo) IsAncestor(ancestor, rev string) (bool, error) {
	return r.gitTest("merge-base", "--is-ancestor", ancestor, rev)
}

// RefExists reports whether the fully qualified ref exists.
func (r *Repo) RefExists(ref string) (bool, error) {
	return r.gitTest("rev-parse", "--verify", "--quiet", ref)
}

// gitTest runs a git command that answers by its status: 0 yes, 1 no, any
// other a failure.
func (r *Repo) gitTest(args ...string) (bool, error) {
	_, err := r.Git(args...)
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

// ChangedFiles lists the paths the work tree and the index change against
// HEAD, untracked files included; a rename lists both its paths.
func (r *Repo) ChangedFiles() ([]string, error) {
	out, err := r.Git("status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	var files []string
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		files = append(files, e[3:])
		if e[0] == 'R' || e[0] == 'C' || e[1] == 'R' || e[1] == 'C' {
			i++
			if i < len(entries) && entries[i] != "" {
				files = append(files, entries[i])
			}
		}
	}
	return files, nil
}

// StagedFiles lists the paths that differ in the index from base, or, when
// base is "", from HEAD (the empty tree when HEAD does not exist). A rename
// contributes both paths.
func (r *Repo) StagedFiles(base string) ([]string, error) {
	args := []string{"diff", "--cached", "--name-only", "--no-renames", "-z"}
	if base != "" {
		args = append(args, base, "--")
	}
	out, err := r.Git(args...)
	if err != nil {
		return nil, err
	}
	var files []string
	for name := range strings.SplitSeq(out, "\x00") {
		if name != "" {
			files = append(files, name)
		}
	}
	return files, nil
}

// ParentTree returns the tree of rev's first parent, or the empty tree when rev
// is a root commit.
func (r *Repo) ParentTree(rev string) (string, error) {
	parent, err := r.Parent(rev)
	if err != nil {
		return "", err
	}
	var tree string
	if parent != "" {
		tree, err = r.Git("rev-parse", parent+"^{tree}")
	} else {
		tree, err = r.GitInput(strings.NewReader(""), nil, "hash-object", "-t", "tree", "--stdin")
	}
	return strings.TrimSpace(tree), err
}

// Parent is the full hash of rev's first parent, "" for a root commit.
func (r *Repo) Parent(rev string) (string, error) {
	out, err := r.Git("rev-list", "--parents", "-n", "1", rev)
	if err != nil {
		return "", err
	}
	ids := strings.Fields(out)
	if len(ids) == 0 {
		return "", fmt.Errorf("git rev-list --parents %s: no commit", rev)
	}
	if len(ids) == 1 {
		return "", nil
	}
	return ids[1], nil
}

// RemoteBranch is a branch on a remote, from its remote-tracking ref.
type RemoteBranch struct {
	Remote string
	Branch string
}

// RemoteBranchesContaining returns the remote-tracking branches that already
// contain rev, the remotes' HEAD aliases left out.
func (r *Repo) RemoteBranchesContaining(rev string) ([]RemoteBranch, error) {
	out, err := r.Git("for-each-ref", "--contains", rev, "--format=%(refname)", "refs/remotes/")
	if err != nil {
		return nil, err
	}
	remotes, err := r.Remotes()
	if err != nil {
		return nil, err
	}
	var found []RemoteBranch
	for _, ref := range splitLines(out) {
		for _, remote := range remotes {
			branch, ok := strings.CutPrefix(ref, "refs/remotes/"+remote+"/")
			if ok && branch != "HEAD" {
				found = append(found, RemoteBranch{Remote: remote, Branch: branch})
				break
			}
		}
	}
	return found, nil
}

// RemotesContaining returns the remotes whose remote-tracking branches already
// contain rev.
func (r *Repo) RemotesContaining(rev string) ([]string, error) {
	branches, err := r.RemoteBranchesContaining(rev)
	if err != nil {
		return nil, err
	}
	remotes, err := r.Remotes()
	if err != nil {
		return nil, err
	}
	var found []string
	for _, remote := range remotes {
		if slices.ContainsFunc(branches, func(b RemoteBranch) bool { return b.Remote == remote }) {
			found = append(found, remote)
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

// SameEnvName reports whether a and b name one environment variable.
func SameEnvName(a, b string) bool {
	return a == b
}

// Without is environ but the variables named in drop (SameEnvName).
func Without(environ, drop []string) []string {
	var kept []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.ContainsFunc(drop, func(d string) bool { return SameEnvName(d, name) }) {
			kept = append(kept, kv)
		}
	}
	return kept
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
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// OpenObjects resolves the repository dir is in for commands that read its
// objects and refs only: a work tree as Open does, or else a bare repository
// (or the inside of .git), whose Root is then its git directory.
func OpenObjects(dir string) (*Repo, error) {
	repo, err := Open(dir)
	if !errors.Is(err, ErrNotRepo) {
		return repo, err
	}
	out, bareErr := run(dir, []string{"LC_ALL=C"}, nil, "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir")
	if bareErr != nil {
		return nil, errors.Join(err, bareErr)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		return nil, fmt.Errorf("unexpected git rev-parse output: %q", out)
	}
	gitDir := filepath.FromSlash(lines[0])
	return &Repo{Root: gitDir, GitDir: gitDir, CommonDir: filepath.FromSlash(lines[1])}, nil
}
