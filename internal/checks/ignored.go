package checks

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

// IgnoredSources reports ignored source files and the git rule hiding each one.
func IgnoredSources(repo *gitx.Repo, cfg config.IgnoredSources) ([]Finding, error) {
	return ignoredSources(repo, cfg, nil)
}

func ignoredSources(repo *gitx.Repo, cfg config.IgnoredSources, added map[string]map[string]bool) ([]Finding, error) {
	listed, err := repo.GitStrict("ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	if err != nil {
		return nil, fmt.Errorf("listing ignored files: %w", err)
	}
	paths, err := collectIgnoredSources(repo, cfg, listed)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, nil
	}
	out, err := repo.GitInput(strings.NewReader(strings.Join(paths, "\x00")+"\x00"), nil, "check-ignore", "-v", "-z", "--non-matching", "--stdin")
	if err != nil && gitx.ExitCode(err) != 1 {
		return nil, fmt.Errorf("checking ignore rules: %w", err)
	}
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	if len(fields) != 4*len(paths) {
		return nil, fmt.Errorf("unexpected git check-ignore output: %d fields for %d paths", len(fields), len(paths))
	}
	findings := make([]Finding, 0, len(paths))
	for n := 0; n < len(fields); n += 4 {
		if fields[n] == "" || strings.HasPrefix(fields[n+2], "!") {
			continue
		}
		if added != nil && !added[fields[n]][fields[n+1]] {
			continue
		}
		if exactIgnoredFile(fields[n], fields[n+2], fields[n+3]) {
			continue
		}
		findings = append(findings, Finding{Path: fields[n+3], Reason: fmt.Sprintf("ignored source (%s:%s: %s)", fields[n], fields[n+1], fields[n+2]), IgnoredSource: true})
	}
	return findings, nil
}

func exactIgnoredFile(source, pattern, file string) bool {
	if strings.ContainsAny(pattern, "*?[\\") || strings.HasSuffix(pattern, "/") {
		return false
	}
	return path.Join(path.Dir(source), strings.TrimPrefix(pattern, "/")) == file
}

func collectIgnoredSources(repo *gitx.Repo, cfg config.IgnoredSources, listed string) ([]string, error) {
	var paths []string
	seen := make(map[string]bool)
	for entry := range strings.SplitSeq(listed, "\x00") {
		if entry == "" {
			continue
		}
		entry = strings.TrimSuffix(entry, "/")
		err := filepath.WalkDir(filepath.Join(repo.Root, filepath.FromSlash(entry)), func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, err := filepath.Rel(repo.Root, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				skip, err := skipIgnoredDir(repo.Root, rel, p, cfg.Exceptions)
				if err != nil {
					return err
				}
				if skip {
					return filepath.SkipDir
				}
				return nil
			}
			if config.Matches(cfg.Exceptions, rel) || path.Base(rel) == "next-env.d.ts" {
				return nil
			}
			ext := strings.TrimPrefix(strings.ToLower(path.Ext(rel)), ".")
			if slices.Contains(cfg.Extensions, ext) && !seen[rel] {
				paths = append(paths, rel)
				seen[rel] = true
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walking ignored path %q: %w", entry, err)
		}
	}
	return paths, nil
}

func skipIgnoredDir(root, rel, p string, exceptions []config.Exception) (bool, error) {
	if excludedIgnoredDir(root, rel, exceptions) {
		return true, nil
	}
	_, err := os.Lstat(filepath.Join(p, ".git"))
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func excludedIgnoreFile(root, name string, exceptions []config.Exception) (bool, error) {
	if config.Matches(exceptions, name) {
		return true, nil
	}
	dir := path.Dir(name)
	if dir == "." {
		return false, nil
	}
	rel := ""
	for part := range strings.SplitSeq(dir, "/") {
		rel = path.Join(rel, part)
		skip, err := skipIgnoredDir(root, rel, filepath.Join(root, filepath.FromSlash(rel)), exceptions)
		if err != nil || skip {
			return skip, err
		}
	}
	return false, nil
}

func excludedIgnoredDir(root, rel string, exceptions []config.Exception) bool {
	if config.Matches(exceptions, rel) || config.Matches(exceptions, rel+"/") {
		return true
	}
	name := path.Base(rel)
	if slices.Contains([]string{"node_modules", ".next", ".nuxt", ".svelte-kit", ".turbo", "__pycache__", ".venv", ".git"}, name) {
		return true
	}
	if slices.Contains([]string{"out", "dist", "build"}, name) {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(path.Dir(rel)), "package.json"))
		return err == nil && info.Mode().IsRegular()
	}
	return false
}
