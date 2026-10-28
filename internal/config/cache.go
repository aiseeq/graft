package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// TaskCache makes a task that passed skip while nothing it depends on has
// changed: the work tree, its definition and these inputs from outside the
// tree.
type TaskCache struct {
	// Env are environment variables whose values the result depends on.
	Env []string `yaml:"env"`
	// Files are files outside the tree, or ignored by git, whose content the
	// result depends on.
	Files []string `yaml:"files"`
	// Commands print what else the result depends on (a toolchain version,
	// an image id); a command that fails runs the task.
	Commands []Command `yaml:"commands"`
}

// CacheTree is the scalar form: the tree and the definition only.
const CacheTree = "tree"

// UnmarshalYAML accepts "tree" or a map of inputs.
func (c *TaskCache) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		if node.Value != CacheTree {
			return fmt.Errorf("cache: want %q or a map of env, files and commands, got %q", CacheTree, node.Value)
		}
		*c = TaskCache{}
		return nil
	}
	type plain TaskCache
	var p plain
	if err := node.Decode(&p); err != nil {
		return err
	}
	*c = TaskCache(p)
	return nil
}

// VersionFiles are the files graft commit bumps: the version file and the
// files synced with it, in mode file.
func (c *Config) VersionFiles() []string {
	if c.Version.Mode != ModeFile {
		return nil
	}
	files := []string{c.Version.File}
	for _, f := range c.Version.Sync {
		files = append(files, f.Path)
	}
	return files
}

// CacheExcluded tells whether path is left out of the tree a task cache
// keys on: cache_exclude and the version files, which graft commit changes
// itself.
func (c *Config) CacheExcluded(path string) bool {
	return slices.Contains(c.VersionFiles(), path) || slices.ContainsFunc(c.cacheExclude, func(re *regexp.Regexp) bool { return re.MatchString(path) })
}

func (c *Config) validateCache() error {
	for _, glob := range c.CacheExclude {
		re, err := CompileGlob(glob)
		if err != nil {
			return fmt.Errorf("cache_exclude: %w", err)
		}
		c.cacheExclude = append(c.cacheExclude, re)
	}
	for _, name := range keysOf(c.Tasks) {
		t := c.Tasks[name]
		if t.Cache == nil {
			continue
		}
		if effect := c.sideEffect(name, map[string]bool{}); effect != "" {
			return fmt.Errorf("tasks.%s.cache: %s; cache only checks that change nothing outside the work tree", name, effect)
		}
		for _, k := range t.Cache.Env {
			if k == "" || strings.ContainsAny(k, "=$ ") {
				return fmt.Errorf("tasks.%s.cache.env: %q is not a variable name", name, k)
			}
		}
		for _, cmd := range t.Cache.Commands {
			if len(cmd.Argv) == 0 {
				return errors.New("tasks." + name + ".cache.commands: an empty command")
			}
		}
	}
	return nil
}

// sideEffects are commands that act outside the work tree: their tasks must
// run every time.
var sideEffects = map[string][]string{
	"graft":  {"deploy", "release", "commit", "amend", "start", "stop", "restart", "testdb"},
	"git":    {"push", "commit", "tag", "merge", "rebase", "reset"},
	"docker": {"push"},
}

// sideEffect names what in task name, its deps or its inner tasks acts
// outside the work tree, or "" when nothing does that graft can see.
func (c *Config) sideEffect(name string, seen map[string]bool) string {
	if seen[name] {
		return ""
	}
	seen[name] = true
	t := c.Tasks[name]
	for _, d := range t.Deps {
		if e := c.sideEffect(d, seen); e != "" {
			return e
		}
	}
	for _, s := range t.Run {
		switch {
		case s.Install != nil:
			return fmt.Sprintf("task %s installs files outside the work tree", name)
		case s.ServiceAction != "":
			return fmt.Sprintf("task %s acts on services", name)
		case s.Task != "":
			if e := c.sideEffect(s.Task, seen); e != "" {
				return e
			}
			continue
		}
		words := s.Argv
		if s.Shell != "" {
			words = strings.Fields(s.Shell)
		}
		for i := 0; i+1 < len(words); i++ {
			if slices.Contains(sideEffects[words[i]], words[i+1]) {
				return fmt.Sprintf("task %s runs %s %s", name, words[i], words[i+1])
			}
		}
	}
	return ""
}
