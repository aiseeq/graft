package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WorktreeSyncOptions are the arguments of graft worktree sync.
type WorktreeSyncOptions struct {
	// Trunk is the ref to sync onto, by default as for graft worktree clean.
	Trunk string
}

// WorktreeSync moves the work of the current worktree onto the fresh trunk:
// it fetches every remote, rebases the branch's own commits onto the trunk
// and brings the uncommitted changes back uncommitted (git rebase
// --autostash), so syncing needs no temporary commit past the hooks. Nothing
// is committed or pushed. Changes that conflict with the trunk are left with
// their conflict markers and kept in the stash too.
func (a *App) WorktreeSync(_ context.Context, o WorktreeSyncOptions) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	remotes, err := repo.Remotes()
	if err != nil {
		return err
	}
	for _, r := range remotes {
		if _, err := repo.Git("fetch", "--quiet", "--prune", r); err != nil {
			return fmt.Errorf("git fetch %s failed, the trunk may be stale: %w", r, err)
		}
	}
	trunk, err := trunkRef(repo, cfg, o.Trunk, remotes)
	if err != nil {
		return err
	}
	if _, err := repo.Git("rebase", "--autostash", trunk); err != nil {
		for _, dir := range []string{"rebase-merge", "rebase-apply"} {
			if _, serr := os.Stat(filepath.Join(repo.GitDir, dir)); serr == nil {
				return fmt.Errorf("a commit of the branch conflicts with %s: resolve and git rebase --continue, or git rebase --abort; the uncommitted changes come back when the rebase ends: %w", trunk, err)
			}
		}
		return fmt.Errorf("git rebase --autostash %s failed, the worktree is as it was: %w", trunk, err)
	}
	out, err := repo.Git("diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return err
	}
	if conflicts := strings.Fields(out); len(conflicts) > 0 {
		return errors.New("on " + trunk + " now, but the uncommitted changes conflict with it in " + strings.Join(conflicts, ", ") +
			": resolve the markers, then git stash drop (the changes are kept in the stash too)")
	}
	head, err := repo.Git("rev-parse", "--short", "HEAD")
	if err != nil {
		return err
	}
	a.printf("on %s (%s), the uncommitted changes back in the worktree; nothing committed or pushed", trunk, strings.TrimSpace(head))
	return nil
}
