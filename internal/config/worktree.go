package config

import (
	"fmt"
	"path"
	"strings"
)

// Worktree is what graft worktree add puts in a new worktree besides the
// checkout, so that the gate and the tasks run there as in the main one.
type Worktree struct {
	// Copy are files of the main worktree git does not track, the local
	// setup (.env), copied into the new worktree.
	Copy []string `yaml:"copy"`
	// Setup are tasks run in the new worktree after the copy, such as
	// generating the files the project needs and does not track.
	Setup []string `yaml:"setup"`
}

func (c *Config) validateWorktree() error {
	for _, p := range c.Worktree.Copy {
		if p == "" || path.IsAbs(p) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") ||
			p == ".git" || strings.HasPrefix(p, ".git/") || strings.Contains(p, `\`) {
			return fmt.Errorf("worktree.copy: %q is not a clean path inside the repository, written with /", p)
		}
	}
	for _, t := range c.Worktree.Setup {
		if _, ok := c.Tasks[t]; !ok {
			return fmt.Errorf("worktree.setup: %s is no task of tasks", t)
		}
	}
	return nil
}
