package checks

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

// mismatchedIgnoreFiles checks even ignored and untracked .gitignore files:
// Git can read them while deciding which files a staged rule hides.
func mismatchedIgnoreFiles(repo *gitx.Repo, cfg config.IgnoredSources) ([]Finding, error) {
	skipWorktree, err := skipWorktreeFiles(repo)
	if err != nil {
		return nil, err
	}
	indexed, err := indexedIgnoreFiles(repo, cfg.Exceptions)
	if err != nil {
		return nil, err
	}
	var findings []Finding
	seen := make(map[string]bool)
	err = filepath.WalkDir(repo.Root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(repo.Root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "." {
				return nil
			}
			skip, err := skipIgnoredDir(repo.Root, rel, p, cfg.Exceptions)
			if err != nil {
				return err
			}
			if skip {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != ".gitignore" || config.Matches(cfg.Exceptions, rel) {
			return nil
		}
		seen[rel] = true
		f, ok := indexed[rel]
		reason := reasonIgnoreDiffers
		switch {
		case ok:
			reason, err = ignoreMismatch(repo, f)
			if err != nil {
				return err
			}
		case d.Type()&fs.ModeSymlink != 0:
			reason = reasonIgnoreSymlink
		}
		if reason != "" {
			findings = append(findings, Finding{Path: rel, Reason: reason, IgnoredSource: true})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("checking worktree ignore files: %w", err)
	}
	// An indexed .gitignore not seen is missing, even with its whole directory,
	// unless Git marked it skip-worktree and will read it from the index.
	for name := range indexed {
		if !seen[name] && !skipWorktree[name] && !config.Matches(cfg.Exceptions, name) {
			findings = append(findings, Finding{Path: name, Reason: reasonIgnoreDiffers, IgnoredSource: true})
		}
	}
	slices.SortFunc(findings, func(a, b Finding) int { return strings.Compare(a.Path, b.Path) })
	return findings, nil
}

func skipWorktreeFiles(repo *gitx.Repo) (map[string]bool, error) {
	listed, err := repo.GitStrict("ls-files", "-v", "-z", "--", ":(glob)**/.gitignore")
	if err != nil {
		return nil, fmt.Errorf("listing skip-worktree files: %w", err)
	}
	skipped := make(map[string]bool)
	for entry := range strings.SplitSeq(listed, "\x00") {
		// S, or s when also assume-unchanged.
		if len(entry) >= 3 && (entry[0] == 'S' || entry[0] == 's') && entry[1] == ' ' {
			skipped[entry[2:]] = true
		}
	}
	return skipped, nil
}

func indexedIgnoreFiles(repo *gitx.Repo, exceptions []config.Exception) (map[string]StagedFile, error) {
	index, err := repo.GitStrict("ls-files", "--stage", "-z", "--", ":(glob)**/.gitignore")
	if err != nil {
		return nil, fmt.Errorf("listing indexed ignore files: %w", err)
	}
	indexed := make(map[string]StagedFile)
	var names, oids []string
	for entry := range strings.SplitSeq(index, "\x00") {
		if entry == "" {
			continue
		}
		meta, name, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[2] != "0" {
			return nil, fmt.Errorf("unexpected indexed ignore file %q", entry)
		}
		excluded, err := excludedIgnoreFile(repo.Root, name, exceptions)
		if err != nil {
			return nil, err
		}
		if excluded {
			continue
		}
		names = append(names, name)
		oids = append(oids, fields[1])
		indexed[name] = StagedFile{Path: name, Symlink: fields[0] == "120000"}
	}
	blobs, err := repo.ReadBlobs(oids, maxScanBytes)
	if err != nil {
		return nil, fmt.Errorf("reading indexed ignore files: %w", err)
	}
	for n, name := range names {
		f := indexed[name]
		f.Blob = blobs[n]
		indexed[name] = f
	}
	return indexed, nil
}
