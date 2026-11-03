package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
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
	Path       string              `json:"path"`
	Clear      bool                `json:"clear"`
	Submodules []string            `json:"submodules"`
	Roots      []worktreeCheckRoot `json:"roots"`
	// MoreRoots counts the roots past the limit, not listed in Roots.
	MoreRoots int   `json:"more_roots"`
	Files     int   `json:"files"`
	Bytes     int64 `json:"bytes"`
	// Limited: the walk stopped at its limit, so the totals are lower bounds.
	Limited bool       `json:"limited"`
	Newest  *time.Time `json:"newest,omitempty"`
}

// WorktreeCheck says whether removing one linked worktree would lose ignored
// data or submodule data, by the rules of graft worktree clean. It removes
// nothing and does not reach a remote: the rules are those of the trunk as
// last fetched. It says nothing of tracked or untracked changes, which git
// status shows.
func (a *App) WorktreeCheck(o WorktreeCheckOptions) error {
	path := o.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(a.Dir, path)
	}
	repo, err := linkedWorktreeAt(path)
	if err != nil {
		return err
	}
	cfg, err := config.Load(repo.Root)
	switch {
	case errors.Is(err, config.ErrNotFound):
		cfg = &config.Config{}
	case err != nil:
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
	submodules, err := c.heldSubmodules(repo.Root)
	if err != nil {
		return fmt.Errorf("submodules of %s not read: %w", repo.Root, err)
	}
	ignored, err := c.ignoredData(repo.Root)
	if err != nil {
		return fmt.Errorf("ignored files of %s not read: %w", repo.Root, err)
	}
	result := newWorktreeCheckResult(repo.Root, submodules, ignored)
	if err := a.printWorktreeCheck(result, ignored, o.JSON); err != nil {
		return err
	}
	if result.Clear {
		return nil
	}
	return &ExitCodeError{Code: ExitWorktreeHolds, Err: fmt.Errorf("%s holds data its removal would lose", repo.Root)}
}

// linkedWorktreeAt opens the linked worktree whose top is path: a directory
// inside one, or the main worktree, has no business being removed as a tree.
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
	return repo, nil
}

func newWorktreeCheckResult(root string, submodules []string, ignored ignoredSummary) worktreeCheckResult {
	result := worktreeCheckResult{
		Path: root, Submodules: submodules, Roots: []worktreeCheckRoot{},
		MoreRoots: ignored.more, Files: ignored.count,
		Clear: len(submodules) == 0 && ignored.count == 0,
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
	if ignored.count > 0 {
		newest := ignored.newest
		result.Newest = &newest
	}
	return result
}

func (a *App) printWorktreeCheck(result worktreeCheckResult, ignored ignoredSummary, asJSON bool) error {
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
	if len(result.Submodules) > 0 {
		parts = append(parts, "submodule "+strings.Join(result.Submodules, ", "))
	}
	if ignored.count > 0 {
		parts = append(parts, ignored.describe())
	}
	a.printf("holds     %s: %s", result.Path, strings.Join(parts, ", "))
	return nil
}
