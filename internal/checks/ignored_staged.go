package checks

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

// NewIgnoredSources checks only rules added to staged .gitignore files.
func NewIgnoredSources(repo *gitx.Repo, cfg config.IgnoredSources, staged []StagedFile) ([]Finding, error) {
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
	return ignoredSources(repo, cfg, func(files []string) (map[string]bool, error) {
		return ignoredByHead(repo, files)
	})
}

// ignoredByHead are those of files that HEAD's ignore rules hide already:
// its .gitignore files and the repository's info/exclude, written into an
// empty repository and asked with git check-ignore --no-index, which needs
// no file on disk. The commit's rules are what they hide on top: no line of
// a diff is read, so a rule repeated, moved or negated counts by what git
// does with it. Without HEAD (the first commit) nothing was hidden.
func ignoredByHead(repo *gitx.Repo, files []string) (hidden map[string]bool, err error) {
	tmp, err := os.MkdirTemp("", "graft-ignore-head-")
	if err != nil {
		return nil, fmt.Errorf("making a directory for HEAD's ignore rules: %w", err)
	}
	defer func() {
		if rerr := os.RemoveAll(tmp); rerr != nil && err == nil {
			err = fmt.Errorf("removing %s: %w", tmp, rerr)
		}
	}()
	// No template: an info/exclude of the user's template is not HEAD's.
	if _, err := gitx.InOther(tmp, nil, "init", "-q", "--template="); err != nil {
		return nil, fmt.Errorf("making a repository for HEAD's ignore rules: %w", err)
	}
	if err := writeHeadIgnores(repo, tmp); err != nil {
		return nil, err
	}
	exclude, err := repo.Git("rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
	if err != nil {
		return nil, fmt.Errorf("locating info/exclude: %w", err)
	}
	data, err := os.ReadFile(strings.TrimSpace(exclude))
	switch {
	case err == nil:
		if err := os.MkdirAll(filepath.Join(tmp, ".git", "info"), 0o700); err != nil {
			return nil, fmt.Errorf("writing HEAD's info/exclude: %w", err)
		}
		if err := os.WriteFile(filepath.Join(tmp, ".git", "info", "exclude"), data, 0o600); err != nil {
			return nil, fmt.Errorf("writing HEAD's info/exclude: %w", err)
		}
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("reading info/exclude: %w", err)
	}
	args, err := ignoreSettings(repo)
	if err != nil {
		return nil, err
	}
	args = append(args, "check-ignore", "--no-index", "-z", "--stdin")
	out, err := gitx.InOther(tmp, strings.NewReader(strings.Join(files, "\x00")+"\x00"), args...)
	if err != nil && gitx.ExitCode(err) != 1 {
		return nil, fmt.Errorf("checking HEAD's ignore rules: %w", err)
	}
	hidden = make(map[string]bool)
	for name := range strings.SplitSeq(out, "\x00") {
		if name != "" {
			hidden[name] = true
		}
	}
	return hidden, nil
}

// ignoreSettings are -c options giving the empty repository the settings by
// which the repository itself matches ignore rules: its own config may set
// core.excludesFile (or core.ignoreCase) over the global one that the empty
// repository would read. Unset everywhere, both use the same default.
func ignoreSettings(repo *gitx.Repo) ([]string, error) {
	var args []string
	for _, key := range []string{"core.excludesFile", "core.ignoreCase"} {
		get := []string{"config", "--get", key}
		if key == "core.excludesFile" {
			get = []string{"config", "--path", "--get", key}
		}
		value, err := repo.Git(get...)
		switch {
		case err == nil:
		case gitx.ExitCode(err) == 1:
			continue
		default:
			return nil, fmt.Errorf("reading %s: %w", key, err)
		}
		value = strings.TrimSpace(value)
		if key == "core.excludesFile" && value != "" && !filepath.IsAbs(value) {
			value = filepath.Join(repo.Root, value)
		}
		args = append(args, "-c", key+"="+value)
	}
	return args, nil
}

// writeHeadIgnores writes every .gitignore of HEAD under dir at its path.
func writeHeadIgnores(repo *gitx.Repo, dir string) error {
	// rev-parse -q exits 1, saying nothing, only when HEAD is not there yet
	// (the first commit); any other failure is an error, not "no rules".
	if _, err := repo.Git("rev-parse", "--verify", "-q", "HEAD^{commit}"); err != nil {
		if gitx.ExitCode(err) == 1 {
			return nil
		}
		return fmt.Errorf("resolving HEAD: %w", err)
	}
	listed, err := repo.Git("ls-tree", "-r", "-z", "HEAD")
	if err != nil {
		return fmt.Errorf("listing HEAD's files: %w", err)
	}
	for entry := range strings.SplitSeq(listed, "\x00") {
		meta, name, ok := strings.Cut(entry, "\t")
		// git reads no rules through a symlinked .gitignore: only a
		// regular file (100644, 100755) counts.
		if !ok || path.Base(name) != ".gitignore" || !strings.HasPrefix(meta, "100") {
			continue
		}
		data, err := repo.ShowFile("HEAD", name)
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("writing HEAD's %s: %w", name, err)
		}
		if err := os.WriteFile(target, data, 0o600); err != nil {
			return fmt.Errorf("writing HEAD's %s: %w", name, err)
		}
	}
	return nil
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
