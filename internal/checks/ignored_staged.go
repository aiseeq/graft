package checks

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

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
		lines, err := addedIgnoreLines(repo, f)
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

// addedIgnoreLines are the numbers of the lines of the staged f whose text
// is not a line of f in HEAD: the rules this commit brings. The staged and
// HEAD contents are compared here, not git diff's text, whose hunks and
// context follow the user's diff settings (interHunkContext,
// suppressBlankEmpty). A rule only moved is not new.
func addedIgnoreLines(repo *gitx.Repo, f StagedFile) (map[string]bool, error) {
	old := make(map[string]bool)
	listed, err := repo.Git("ls-tree", "HEAD", "--", ":(literal)"+f.Path)
	// No HEAD yet (the first commit) fails ls-tree: nothing is old, every
	// rule is checked.
	if err == nil && strings.TrimSpace(listed) != "" {
		data, err := repo.ShowFile("HEAD", f.Path)
		if err != nil {
			return nil, err
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			old[strings.TrimSuffix(line, "\r")] = true
		}
	}
	lines := make(map[string]bool)
	for i, line := range strings.Split(string(f.Content), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) != "" && !old[line] {
			lines[strconv.Itoa(i+1)] = true
		}
	}
	return lines, nil
}
