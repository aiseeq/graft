package checks

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

var addedHunk = regexp.MustCompile(`^@@ -[0-9]+(?:,[0-9]+)? \+([0-9]+)(?:,[0-9]+)? @@`)

// NewIgnoredSources checks only rules added to staged .gitignore files.
func NewIgnoredSources(repo *gitx.Repo, cfg config.IgnoredSources, staged []StagedFile) ([]Finding, error) {
	added := make(map[string]map[string]bool)
	var mismatches []Finding
	changed := false
	for _, f := range staged {
		if path.Base(f.Path) != ".gitignore" {
			continue
		}
		changed = true
		excluded, err := excludedIgnoreFile(repo.Root, f.Path, cfg.Exceptions)
		if err != nil {
			return nil, err
		}
		if excluded {
			continue
		}
		same, err := sameStagedIgnore(repo, f)
		if err != nil {
			return nil, err
		}
		if !same {
			mismatches = append(mismatches, Finding{Path: f.Path, Reason: ".gitignore staged differs from the work tree", IgnoredSource: true})
			continue
		}
		lines, err := addedIgnoreLines(repo, f.Path)
		if err != nil {
			return nil, err
		}
		if len(lines) > 0 {
			added[f.Path] = lines
		}
	}
	if len(mismatches) > 0 {
		return mismatches, nil
	}
	if !changed {
		deleted, err := repo.Git("diff", "--cached", "--name-only", "--no-renames", "--diff-filter=D", "-z", "--no-ext-diff")
		if err != nil {
			return nil, fmt.Errorf("listing deleted ignore files: %w", err)
		}
		for name := range strings.SplitSeq(deleted, "\x00") {
			if path.Base(name) == ".gitignore" {
				changed = true
				break
			}
		}
	}
	if !changed {
		return nil, nil
	}
	mismatches, err := mismatchedIgnoreFiles(repo, cfg)
	if err != nil || len(mismatches) > 0 {
		return mismatches, err
	}
	if len(added) == 0 {
		return nil, nil
	}
	return ignoredSources(repo, cfg, added)
}

func sameStagedIgnore(repo *gitx.Repo, f StagedFile) (bool, error) {
	filename := filepath.Join(repo.Root, filepath.FromSlash(f.Path))
	info, err := os.Lstat(filename)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", f.Path, err)
	}
	if !info.Mode().IsRegular() || f.Symlink || f.Content == nil {
		return false, nil
	}
	disk, err := os.ReadFile(filename)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", f.Path, err)
	}
	return bytes.Equal(disk, f.Content), nil
}

func addedIgnoreLines(repo *gitx.Repo, name string) (map[string]bool, error) {
	diff, err := repo.Git("diff", "--cached", "--no-renames", "--no-ext-diff", "--no-textconv", "--text", "--no-color", "--unified=0", "--", ":(literal)"+name)
	if err != nil {
		return nil, fmt.Errorf("reading staged ignore changes for %s: %w", name, err)
	}
	lines := make(map[string]bool)
	line := 0
	for text := range strings.SplitSeq(diff, "\n") {
		if match := addedHunk.FindStringSubmatch(text); match != nil {
			line, err = strconv.Atoi(match[1])
			if err != nil {
				return nil, fmt.Errorf("invalid diff hunk for %s: %w", name, err)
			}
			continue
		}
		if line > 0 && strings.HasPrefix(text, "+") {
			lines[strconv.Itoa(line)] = true
			line++
		}
	}
	return lines, nil
}
