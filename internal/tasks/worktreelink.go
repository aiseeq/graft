package tasks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// unlinkWorktreeLinks removes, before the task name runs in a worktree other
// than the main one, the links to the main worktree's directories of the
// worktree.link groups whose setup the task is: so the task installs into
// this worktree and never writes into the main worktree's directories. A
// directory, or a link elsewhere, stays.
func (r *Runner) unlinkWorktreeLinks(name string) error {
	var paths []string
	for _, g := range r.Config.Worktree.Link {
		if g.Setup == name {
			paths = append(paths, g.Paths...)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	repo, err := r.repo()
	if err != nil {
		return err
	}
	if filepath.Clean(repo.GitDir) == filepath.Clean(repo.CommonDir) {
		return nil
	}
	main := filepath.Dir(repo.CommonDir)
	realMain, err := filepath.EvalSymlinks(main)
	if err != nil {
		return fmt.Errorf("worktree.link: the main worktree: %w", err)
	}
	for _, p := range paths {
		at := filepath.Join(r.Root, filepath.FromSlash(p))
		info, err := os.Lstat(at)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("worktree.link %s: checking the link before %s: %w", p, name, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		to, err := os.Readlink(at)
		if err != nil {
			return fmt.Errorf("worktree.link %s: reading the link before %s: %w", p, name, err)
		}
		rel := filepath.FromSlash(p)
		if to != filepath.Join(main, rel) && to != filepath.Join(realMain, rel) {
			continue
		}
		if err := os.Remove(at); err != nil {
			return fmt.Errorf("worktree.link %s: removing the link before %s: %w", p, name, err)
		}
		fmt.Fprintf(r.Stderr, "graft: %s: unlinked %s from the main worktree, to install it here\n", name, p)
	}
	return nil
}
