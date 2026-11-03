package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	// DetachedCommits counts the commits only the tree's HEAD reaches.
	DetachedCommits int `json:"detached_commits"`
	// HiddenChanges names the tracked files git status does not look at.
	HiddenChanges []string            `json:"hidden_changes"`
	Submodules    []string            `json:"submodules"`
	Roots         []worktreeCheckRoot `json:"roots"`
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
	hidden          []string
	submodules      []string
	ignored         ignoredSummary
}

// WorktreeCheck says whether removing one linked worktree would lose data git
// status does not show, by the rules of graft worktree clean. It removes
// nothing and does not reach a remote: the rules are those of the trunk as
// last fetched, found by the main worktree's config.
func (a *App) WorktreeCheck(o WorktreeCheckOptions) error {
	if err := bindGitToPath(); err != nil {
		return err
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

// bindGitToPath frees the git this command runs from where it was called: a
// hook's GIT_DIR would answer for another tree. No lazy fetch: in a partial
// clone, reading a blob it lacks would reach the remote.
func bindGitToPath() error {
	drop, err := gitx.LocalEnvVars()
	if err != nil {
		return fmt.Errorf("listing git environment variables: %w", err)
	}
	for _, name := range drop {
		if err := os.Unsetenv(name); err != nil {
			return fmt.Errorf("unsetting %s: %w", name, err)
		}
	}
	if err := os.Setenv("GIT_NO_LAZY_FETCH", "1"); err != nil {
		return fmt.Errorf("setting GIT_NO_LAZY_FETCH: %w", err)
	}
	return nil
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
	if holds.hidden, err = hiddenChanges(repo.Root); err != nil {
		return holds, fmt.Errorf("the marked files of %s not read: %w", repo.Root, err)
	}
	if holds.submodules, err = c.heldSubmodules(repo.Root); err != nil {
		return holds, fmt.Errorf("submodules of %s not read: %w", repo.Root, err)
	}
	if holds.ignored, err = c.ignoredData(repo.Root); err != nil {
		return holds, fmt.Errorf("ignored files of %s not read: %w", repo.Root, err)
	}
	return holds, nil
}

// detachedCommits counts the commits only HEAD reaches: on a branch none, for
// the branch outlives the tree.
func detachedCommits(repo *gitx.Repo) (int, error) {
	hasHead, err := repo.HasHead()
	if err != nil || !hasHead {
		return 0, err
	}
	out, err := repo.Git("rev-list", "--count", "HEAD", "--not", "--branches", "--tags", "--remotes")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

func newWorktreeCheckResult(root string, holds worktreeHolds) worktreeCheckResult {
	ignored := holds.ignored
	result := worktreeCheckResult{
		Path: root, Operation: holds.operation, DetachedCommits: holds.detachedCommits,
		HiddenChanges: holds.hidden, Submodules: holds.submodules, Roots: []worktreeCheckRoot{},
		MoreRoots: ignored.more, Files: ignored.count,
		Clear: holds.operation == "" && holds.detachedCommits == 0 && len(holds.hidden) == 0 &&
			len(holds.submodules) == 0 && ignored.count == 0,
	}
	if result.HiddenChanges == nil {
		result.HiddenChanges = []string{}
	}
	if result.Submodules == nil {
		result.Submodules = []string{}
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
		parts = append(parts, "a "+result.Operation+" in progress")
	}
	if result.DetachedCommits > 0 {
		parts = append(parts, plural(result.DetachedCommits, "commit")+" only its HEAD reaches")
	}
	if len(result.HiddenChanges) > 0 {
		parts = append(parts, "files git status does not look at: "+strings.Join(result.HiddenChanges, ", "))
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
