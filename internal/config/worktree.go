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
	// Link groups directories shared with the main worktree while their key files match.
	Link []WorktreeLink `yaml:"link"`
	// Disposable are ignored paths that can be regenerated and need not keep a worktree.
	Disposable []string `yaml:"disposable"`
}

// WorktreeLink describes dependency directories and the files that identify them.
type WorktreeLink struct {
	Key   []string `yaml:"key"`
	Paths []string `yaml:"paths"`
	Setup string   `yaml:"setup"`
}

func (c *Config) validateWorktree() error {
	for _, p := range c.Worktree.Copy {
		if !cleanWorktreePath(p) {
			return fmt.Errorf("worktree.copy: %q is not a clean path inside the repository, written with /", p)
		}
	}
	for _, t := range c.Worktree.Setup {
		if _, ok := c.Tasks[t]; !ok {
			return fmt.Errorf("worktree.setup: %s is no task of tasks", t)
		}
	}
	for _, pattern := range c.Worktree.Disposable {
		if err := validateDisposablePattern(pattern); err != nil {
			return fmt.Errorf("worktree.disposable: %q is not a clean path pattern inside the repository: %w", pattern, err)
		}
	}
	for i, group := range c.Worktree.Link {
		prefix := fmt.Sprintf("worktree.link[%d]", i)
		if len(group.Paths) == 0 || len(group.Key) == 0 || group.Setup == "" {
			return fmt.Errorf("%s: key, paths and setup are required", prefix)
		}
		for _, p := range append(append([]string{}, group.Key...), group.Paths...) {
			if !cleanWorktreePath(p) {
				return fmt.Errorf("%s: %q is not a clean path inside the repository, written with /", prefix, p)
			}
		}
		if _, ok := c.Tasks[group.Setup]; !ok {
			return fmt.Errorf("%s setup: %s is no task of tasks", prefix, group.Setup)
		}
	}
	return nil
}

func validateDisposablePattern(pattern string) error {
	if pattern == "" || strings.Contains(pattern, `\`) || path.IsAbs(pattern) || path.Clean(pattern) != strings.TrimSuffix(pattern, "/") {
		return fmt.Errorf("invalid path")
	}
	pattern = strings.TrimSuffix(pattern, "/")
	if pattern == "" || pattern == "." || pattern == ".." || strings.HasPrefix(pattern, "../") || strings.Contains(pattern, "/../") || strings.HasPrefix(pattern, ".git/") || pattern == ".git" {
		return fmt.Errorf("invalid path")
	}
	for component := range strings.SplitSeq(pattern, "/") {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("invalid path")
		}
		if _, err := path.Match(component, ""); err != nil {
			return fmt.Errorf("pattern %q: %w", pattern, err)
		}
	}
	_, err := path.Match(pattern, "")
	if err != nil {
		return fmt.Errorf("pattern %q: %w", pattern, err)
	}
	return nil
}

// DisposablePath reports whether path is covered by one of the configured patterns.
func (w Worktree) DisposablePath(name string) (bool, error) {
	for _, pattern := range w.Disposable {
		pattern = strings.TrimSuffix(pattern, "/")
		if strings.Contains(pattern, "/") {
			// The pattern may name a directory above name, which git lists on
			// its own when only some of that directory is ignored.
			for prefix := name; prefix != "."; prefix = path.Dir(prefix) {
				ok, err := path.Match(pattern, prefix)
				if err != nil {
					return false, fmt.Errorf("pattern %q: %w", pattern, err)
				}
				if ok {
					return true, nil
				}
			}
			continue
		}
		for component := range strings.SplitSeq(name, "/") {
			ok, err := path.Match(pattern, component)
			if err != nil {
				return false, fmt.Errorf("pattern %q: %w", pattern, err)
			}
			if ok {
				return true, nil
			}
		}
	}
	return false, nil
}

func cleanWorktreePath(p string) bool {
	return p != "" && !path.IsAbs(p) && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../") &&
		p != ".git" && !strings.HasPrefix(p, ".git/") && !strings.Contains(p, `\`)
}
