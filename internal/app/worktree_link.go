package app

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

// unlinkSetupLinks removes the links of the worktree.link groups whose setup
// is task, so the task installs into this worktree and never writes into
// the main worktree's directories; a real directory stays.
func unlinkSetupLinks(repo *gitx.Repo, cfg *config.Config, task string) error {
	for _, group := range cfg.Worktree.Link {
		if group.Setup != task {
			continue
		}
		for _, p := range group.Paths {
			at := filepath.Join(repo.Root, filepath.FromSlash(p))
			info, err := os.Lstat(at)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("worktree.link %s: checking link before %s: %w", p, task, err)
			}
			if info.Mode()&os.ModeSymlink == 0 {
				continue
			}
			if err := os.Remove(at); err != nil {
				return fmt.Errorf("worktree.link %s: removing link before %s: %w", p, task, err)
			}
		}
	}
	return nil
}

// warnStaleWorktreeLinks warns of every path of a worktree.link group that
// links to the main worktree while a key file of the group differs from the
// main worktree's: the branch needs the group's setup.
func (a *App) warnStaleWorktreeLinks(repo *gitx.Repo, cfg *config.Config) {
	if len(cfg.Worktree.Link) == 0 {
		return
	}
	trees, err := listWorktrees(repo)
	if err != nil {
		a.warn("worktree.link: listing worktrees: %v", err)
		return
	}
	if len(trees) == 0 || filepath.Clean(trees[0].path) == filepath.Clean(repo.Root) {
		return
	}
	main := trees[0].path
	for _, group := range cfg.Worktree.Link {
		var differing string
		for _, key := range group.Key {
			mainData, mainErr := os.ReadFile(filepath.Join(main, filepath.FromSlash(key)))
			workData, workErr := os.ReadFile(filepath.Join(repo.Root, filepath.FromSlash(key)))
			if mainErr != nil || workErr != nil || !bytes.Equal(mainData, workData) {
				differing = key
				break
			}
		}
		if differing == "" {
			continue
		}
		for _, p := range group.Paths {
			at := filepath.Join(repo.Root, filepath.FromSlash(p))
			mainPath := filepath.Join(main, filepath.FromSlash(p))
			if _, err := os.Lstat(at); os.IsNotExist(err) {
				continue
			} else if err != nil {
				a.warn("worktree.link %s: checking link: %v", p, err)
				continue
			}
			got, err := filepath.EvalSymlinks(at)
			if err != nil {
				a.warn("worktree.link %s: resolving link: %v", p, err)
				continue
			}
			want, err := filepath.EvalSymlinks(mainPath)
			if err != nil {
				a.warn("worktree.link %s: resolving main worktree path: %v", p, err)
				continue
			}
			if filepath.Clean(got) == filepath.Clean(want) {
				a.warn("worktree.link %s: a link to the main worktree, but %s differs from it: run graft %s", p, differing, group.Setup)
			}
		}
	}
}
