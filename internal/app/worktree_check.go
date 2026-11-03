package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

// ExitWorktreeHolds is the exit status of graft worktree check on a tree whose
// removal would lose data; any other failure exits as graft does.
const ExitWorktreeHolds = 3

// WorktreeCheckOptions are the arguments of graft worktree check.
type WorktreeCheckOptions struct {
	// Path is the top of the linked worktree to look at.
	Path string
	// Trunk is the ref whose rules apply, by default as graft worktree clean's.
	Trunk string
	// JSON prints the answer as one JSON object.
	JSON bool
}

// worktreeCheckRoot is one ignored path holding data.
type worktreeCheckRoot struct {
	Name      string `json:"name"`
	Directory bool   `json:"directory"`
	Files     int    `json:"files"`
	Bytes     int64  `json:"bytes"`
	// Partial: the walk stopped at the limit in this root or before it, so
	// Files and Bytes are lower bounds.
	Partial bool `json:"partial"`
}

// worktreeCheckResult is the answer of graft worktree check. Clear is what the
// caller decides on; the age of the data is the caller's to weigh, through
// Newest.
type worktreeCheckResult struct {
	Path  string `json:"path"`
	Clear bool   `json:"clear"`
	// Operation is the rebase, merge, cherry-pick or revert under way, whose
	// state is in the tree's git dir.
	Operation string `json:"operation"`
	// DetachedCommits counts the commits only the tree's HEAD or its own refs
	// reach.
	DetachedCommits int `json:"detached_commits"`
	// Changes names what git status shows, untracked files included.
	Changes []string `json:"changes"`
	// HiddenChanges names the tracked files git status does not look at.
	HiddenChanges []string `json:"hidden_changes"`
	// NestedWorktrees names the worktrees inside this one.
	NestedWorktrees []string            `json:"nested_worktrees"`
	Submodules      []string            `json:"submodules"`
	Roots           []worktreeCheckRoot `json:"roots"`
	// MoreRoots counts the roots past the limit, not listed in Roots.
	MoreRoots int   `json:"more_roots"`
	Files     int   `json:"files"`
	Bytes     int64 `json:"bytes"`
	// Limited: the walk stopped at its limit, so the totals are lower bounds
	// and the age of what it did not reach is unknown: Newest is left out.
	Limited bool       `json:"limited"`
	Newest  *time.Time `json:"newest,omitempty"`
}

// worktreeHolds is what removing a tree would lose.
type worktreeHolds struct {
	operation       string
	detachedCommits int
	changes         []string
	hidden          []string
	nested          []string
	submodules      []string
	ignored         ignoredSummary
}

// WorktreeCheck says whether removing one linked worktree would lose anything,
// by the rules of graft worktree clean for ignored data. It removes nothing and
// does not reach a remote: the rules are those of the trunk as last fetched,
// found by the main worktree's config.
func (a *App) WorktreeCheck(o WorktreeCheckOptions) error {
	if err := unbindGitFromCaller(); err != nil {
		return err
	}
	// In a partial clone, reading a blob it lacks would reach the remote.
	if err := os.Setenv("GIT_NO_LAZY_FETCH", "1"); err != nil {
		return fmt.Errorf("setting GIT_NO_LAZY_FETCH: %w", err)
	}
	path := o.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(a.Dir, path)
	}
	repo, err := linkedWorktreeAt(path)
	if err != nil {
		return err
	}
	cfg, err := mainWorktreeConfig(repo)
	if err != nil {
		return err
	}
	if err := a.requireGraft(cfg); err != nil {
		return err
	}
	c := &cleaner{app: a, repo: repo, cfg: cfg, opts: WorktreeCleanOptions{Trunk: o.Trunk}, now: time.Now(), counts: map[string]int{}}
	if c.remotes, err = repo.Remotes(); err != nil {
		return err
	}
	if c.trunk, err = trunkRef(repo, cfg, o.Trunk, c.remotes); err != nil {
		return err
	}
	if c.cfg, err = c.readTrunkConfig(); err != nil {
		return err
	}
	holds, err := c.worktreeHolds(repo)
	if err != nil {
		return err
	}
	result := newWorktreeCheckResult(repo.Root, holds)
	if err := a.printWorktreeCheck(result, holds, o.JSON); err != nil {
		return err
	}
	if result.Clear {
		return nil
	}
	return &ExitCodeError{Code: ExitWorktreeHolds, Err: fmt.Errorf("%s holds data its removal would lose", repo.Root)}
}

// linkedWorktreeAt opens the linked worktree whose top is path: a directory
// inside one, or the main worktree, has no business being removed as a tree,
// and one whose .git leads to another tree's git dir would answer for that one.
func linkedWorktreeAt(path string) (*gitx.Repo, error) {
	repo, err := gitx.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	asked, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", path, err)
	}
	top, err := filepath.EvalSymlinks(repo.Root)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", repo.Root, err)
	}
	if asked != top {
		return nil, fmt.Errorf("%s is not the top of a work tree: that is %s", path, repo.Root)
	}
	if filepath.Clean(repo.GitDir) == filepath.Clean(repo.CommonDir) {
		return nil, fmt.Errorf("%s is the main worktree of its repository", path)
	}
	data, err := os.ReadFile(filepath.Join(repo.GitDir, "gitdir"))
	if err != nil {
		return nil, fmt.Errorf("%s: its git dir does not say whose it is: %w", path, err)
	}
	link := filepath.FromSlash(strings.TrimSpace(string(data)))
	if !filepath.IsAbs(link) {
		link = filepath.Join(repo.GitDir, link)
	}
	if back, err := filepath.EvalSymlinks(filepath.Dir(link)); err != nil || back != top {
		return nil, fmt.Errorf("%s: its .git leads to the git dir of %s", path, filepath.Dir(link))
	}
	return repo, nil
}

// mainWorktreeConfig is the config that finds the trunk: a branch's own config
// would choose whose rules judge it.
func mainWorktreeConfig(repo *gitx.Repo) (*config.Config, error) {
	main, err := mainWorktreePath(repo)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(main)
	if errors.Is(err, config.ErrNotFound) {
		return &config.Config{}, nil
	}
	return cfg, err
}

func (c *cleaner) worktreeHolds(repo *gitx.Repo) (worktreeHolds, error) {
	var holds worktreeHolds
	var err error
	if holds.operation, err = gitOperation(repo, repo.Root); err != nil {
		return holds, fmt.Errorf("the state of %s not read: %w", repo.Root, err)
	}
	if holds.detachedCommits, err = detachedCommits(repo); err != nil {
		return holds, fmt.Errorf("the commits of %s not read: %w", repo.Root, err)
	}
	if holds.changes, err = repo.ChangedFiles(); err != nil {
		return holds, fmt.Errorf("the changes of %s not read: %w", repo.Root, err)
	}
	if holds.hidden, err = hiddenChanges(repo.Root); err != nil {
		return holds, fmt.Errorf("the marked files of %s not read: %w", repo.Root, err)
	}
	trees, err := listWorktrees(repo)
	if err != nil {
		return holds, fmt.Errorf("the worktrees of %s not read: %w", repo.Root, err)
	}
	if holds.nested, err = nestedWorktrees(trees, repo.Root); err != nil {
		return holds, err
	}
	if holds.submodules, err = c.heldSubmodules(repo.Root); err != nil {
		return holds, fmt.Errorf("submodules of %s not read: %w", repo.Root, err)
	}
	if holds.ignored, err = c.ignoredData(repo.Root); err != nil {
		return holds, fmt.Errorf("ignored files of %s not read: %w", repo.Root, err)
	}
	return holds, nil
}

// detachedCommits counts the commits only HEAD and the tree's own refs reach:
// on a branch none, for the branch outlives the tree. A commit it cannot read
// is an error, not none.
func detachedCommits(repo *gitx.Repo) (int, error) {
	tips, err := perWorktreeTips(repo)
	if err != nil {
		return 0, err
	}
	head, err := headTip(repo)
	if err != nil {
		return 0, err
	}
	if head != "" {
		tips = append(tips, head)
	}
	return commitsOnlyFrom(repo, tips)
}

// headTip is what HEAD stands for: on a branch that is gone, a deleted one or
// an unborn one, its last commit by the reflog, the only trace of it left, or
// nothing when it never had one.
func headTip(repo *gitx.Repo) (string, error) {
	out, err := repo.Git("symbolic-ref", "-q", "HEAD")
	if err != nil {
		if gitx.ExitCode(err) == 1 {
			return "HEAD", nil
		}
		return "", err
	}
	exists, err := repo.RefExists(strings.TrimSpace(out))
	if err != nil {
		return "", err
	}
	if exists {
		return "HEAD", nil
	}
	data, err := os.ReadFile(filepath.Join(repo.GitDir, "logs", "HEAD"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the reflog: %w", err)
	}
	// Deleting the branch logs a line ending at zeros: the commit is before it.
	lines := strings.Split(string(data), "\n")
	for _, line := range slices.Backward(lines) {
		if fields := strings.Fields(line); len(fields) >= 2 && strings.Trim(fields[1], "0") != "" {
			return fields[1], nil
		}
	}
	return "", nil
}

func newWorktreeCheckResult(root string, holds worktreeHolds) worktreeCheckResult {
	ignored := holds.ignored
	result := worktreeCheckResult{
		Path: root, Operation: holds.operation, DetachedCommits: holds.detachedCommits,
		Changes: orEmpty(holds.changes), HiddenChanges: orEmpty(holds.hidden), NestedWorktrees: orEmpty(holds.nested),
		Submodules: orEmpty(holds.submodules), Roots: []worktreeCheckRoot{},
		MoreRoots: ignored.more, Files: ignored.count,
		Clear: holds.operation == "" && holds.detachedCommits == 0 && len(holds.changes) == 0 && len(holds.hidden) == 0 &&
			len(holds.nested) == 0 && len(holds.submodules) == 0 && ignored.count == 0,
	}
	for _, r := range ignored.roots {
		partial := r.cut || r.omitted
		result.Roots = append(result.Roots, worktreeCheckRoot{Name: r.name, Directory: r.directory, Files: r.count, Bytes: r.size, Partial: partial})
		result.Bytes += r.size
		result.Limited = result.Limited || partial
	}
	if ignored.count > 0 && !result.Limited {
		newest := ignored.newest
		result.Newest = &newest
	}
	return result
}

// orEmpty makes an empty list [] in JSON, not null.
func orEmpty(list []string) []string {
	return append([]string{}, list...)
}

func (a *App) printWorktreeCheck(result worktreeCheckResult, holds worktreeHolds, asJSON bool) error {
	if asJSON {
		data, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("encoding the answer: %w", err)
		}
		fmt.Fprintln(a.Stdout, string(data))
		return nil
	}
	if result.Clear {
		a.printf("clear     %s: nothing its removal would lose", result.Path)
		return nil
	}
	var parts []string
	if result.Operation != "" {
		parts = append(parts, operationInProgress(result.Operation))
	}
	if result.DetachedCommits > 0 {
		parts = append(parts, plural(result.DetachedCommits, "commit")+" only its HEAD reaches")
	}
	if len(result.Changes) > 0 {
		parts = append(parts, plural(len(result.Changes), "uncommitted change"))
	}
	if len(result.HiddenChanges) > 0 {
		parts = append(parts, "files git status does not look at: "+strings.Join(result.HiddenChanges, ", "))
	}
	if len(result.NestedWorktrees) > 0 {
		parts = append(parts, "the worktree "+strings.Join(result.NestedWorktrees, ", "))
	}
	if len(result.Submodules) > 0 {
		parts = append(parts, "submodule "+strings.Join(result.Submodules, ", "))
	}
	if holds.ignored.count > 0 {
		parts = append(parts, holds.ignored.describe())
	}
	a.printf("holds     %s: %s", result.Path, strings.Join(parts, ", "))
	return nil
}
