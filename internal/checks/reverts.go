package checks

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

// Reverts finds the staged files that take back a recent commit's change: the
// commit's diff of the file applies to the staged tree and did not apply to
// HEAD's. A tree from an old base committed on a fresh parent silently undoes
// merged work that way, a deleted file as much as changed lines. A revert that
// is meant names the commit (7 characters of its hash or more, as git prints
// it short) or the file in the message. Files moved by the commit, the
// version files graft bumps, the exceptions and files taken out of the index
// that stay on disk, ignored, do not count. A partial
// take-back, part of the commit's diff of a file, is not seen: the check is
// per file. base is the commit the staged tree is to follow: HEAD for a new
// commit, HEAD's parent for an amend, whose own commit is the one being
// rewritten; "" for an amend of a root commit, with nothing before it.
func Reverts(repo *gitx.Repo, cfg config.Reverts, versionFiles []string, message, base string) ([]Finding, error) {
	if base == "" {
		return nil, nil
	}
	if ok, err := repo.HasHead(); err != nil || !ok {
		return nil, err
	}
	staged, err := revertCandidates(repo, cfg, versionFiles, base)
	if err != nil || len(staged) == 0 {
		return nil, err
	}
	recent, err := recentCommits(repo, cfg.Depth, base)
	if err != nil {
		return nil, err
	}
	tree, err := repo.Git("write-tree")
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "graft-reverts-")
	if err != nil {
		return nil, fmt.Errorf("a directory for the revert check: %w", err)
	}
	defer os.RemoveAll(tmp)
	now, before := filepath.Join(tmp, "staged"), filepath.Join(tmp, "base")
	for index, rev := range map[string]string{now: strings.TrimSpace(tree), before: base} {
		if _, err := repo.GitEnv([]string{"GIT_INDEX_FILE=" + index}, "read-tree", rev); err != nil {
			return nil, err
		}
	}
	var findings []Finding
	for _, c := range recent {
		if c.parent == "" || names(message, c.sha) {
			continue
		}
		for _, f := range c.files {
			if !slices.Contains(staged, f) || strings.Contains(message, f) {
				continue
			}
			back, err := takesBack(repo, c, f, now, before, base)
			if err != nil {
				return nil, err
			}
			if back {
				findings = append(findings, Finding{Path: f, Reason: fmt.Sprintf("takes back %s %s; if that is meant, name the commit (7 characters of its hash or more) or the file in the message", gitx.Short(c.sha), c.subject)})
			}
		}
	}
	return findings, nil
}

// hashRe is a word that can be a commit hash, short as git prints it or full.
var hashRe = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)

// names tells whether message names the commit sha by a prefix of its hash.
func names(message, sha string) bool {
	for _, h := range hashRe.FindAllString(message, -1) {
		if strings.HasPrefix(sha, h) {
			return true
		}
	}
	return false
}

// revertCandidates are the staged files that can take something back: not
// the paths of moves, the version files or the exceptions.
func revertCandidates(repo *gitx.Repo, cfg config.Reverts, versionFiles []string, base string) ([]string, error) {
	out, err := repo.Git("diff", "--cached", "-M", "--name-status", "-z", "--no-ext-diff", base)
	if err != nil {
		return nil, err
	}
	var files, deleted []string
	fields := strings.Split(out, "\x00")
	for i := 0; i+1 < len(fields); i++ {
		status := fields[i]
		if strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C") {
			// A move is no revert, back to where the file was or not.
			i += 2
			continue
		}
		i++
		files = append(files, fields[i])
		if status == "D" {
			deleted = append(deleted, fields[i])
		}
	}
	untracked, err := ignoredOnDisk(repo, deleted)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(files, func(f string) bool {
		return f == "" || slices.Contains(versionFiles, f) || config.Matches(cfg.Exceptions, f) || slices.Contains(untracked, f)
	}), nil
}

// ignoredOnDisk are the files of deleted that stay in the work tree and that
// .gitignore now ignores: taken out of the index (git rm --cached and a rule,
// as graft's own hint for a committed file says), not out of the work. A file
// reached through a symbolic link that replaced one of its folders counts when
// .gitignore ignores the link; a file whose folder a plain file replaced is
// gone.
func ignoredOnDisk(repo *gitx.Repo, deleted []string) ([]string, error) {
	byPath := map[string][]string{} // the path check-ignore asks about: the file, or the link on its way
	var paths []string
	for _, f := range deleted {
		p, err := pathOnDisk(repo.Root, f)
		if err != nil {
			return nil, fmt.Errorf("a file the commit removes: %w", err)
		}
		if p == "" {
			continue
		}
		if _, seen := byPath[p]; !seen {
			paths = append(paths, p)
		}
		byPath[p] = append(byPath[p], f)
	}
	if len(paths) == 0 {
		return nil, nil
	}
	out, err := repo.GitInput(strings.NewReader(strings.Join(paths, "\x00")+"\x00"), nil, "check-ignore", "--stdin", "-z")
	if gitx.ExitCode(err) == 1 {
		return nil, nil // none of them ignored
	}
	if err != nil {
		return nil, fmt.Errorf("checking which removed files .gitignore ignores: %w", err)
	}
	var ignored []string
	for p := range strings.SplitSeq(out, "\x00") {
		ignored = append(ignored, byPath[p]...)
	}
	return ignored, nil
}

// pathOnDisk is the path of the work tree that keeps file f there: f itself,
// or the first symbolic link among its folders when f is reached through it;
// "" when f is gone, a plain file standing where one of its folders was too.
func pathOnDisk(root, f string) (string, error) {
	parts := strings.Split(f, "/")
	dir := root
	for i, part := range parts {
		dir = filepath.Join(dir, part)
		info, err := os.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("%s: %w", f, err)
		}
		if i == len(parts)-1 {
			return f, nil
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Stat(filepath.Join(root, filepath.FromSlash(f)))
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || err == nil && target.IsDir() {
				return "", nil
			}
			if err != nil {
				return "", fmt.Errorf("%s through the symbolic link %s: %w", f, strings.Join(parts[:i+1], "/"), err)
			}
			return strings.Join(parts[:i+1], "/"), nil
		}
		if !info.IsDir() {
			return "", nil
		}
	}
	return "", nil
}

// recentCommit is a commit of the base's first-parent line with the files
// it changed.
type recentCommit struct {
	sha, parent, subject string
	files                []string
}

func recentCommits(repo *gitx.Repo, depth int, base string) ([]recentCommit, error) {
	out, err := repo.Git("log", "--first-parent", "--no-merges", "--no-renames", fmt.Sprintf("--max-count=%d", depth),
		"--format=%x01%H%x00%P%x00%s", "--name-only", "-z", base)
	if err != nil {
		return nil, err
	}
	var commits []recentCommit
	for rec := range strings.SplitSeq(out, "\x01") {
		f := strings.Split(strings.TrimLeft(rec, "\n"), "\x00")
		if len(f) < 3 {
			continue
		}
		c := recentCommit{sha: f[0]}
		if parents := strings.Fields(f[1]); len(parents) > 0 {
			c.parent = parents[0]
		}
		c.subject = strings.TrimRight(f[2], "\n")
		for _, name := range f[3:] {
			if name = strings.TrimSpace(name); name != "" {
				c.files = append(c.files, name)
			}
		}
		commits = append(commits, c)
	}
	return commits, nil
}

// takesBack tells whether the commit's diff of the file applies to the staged
// tree and did not apply to the base's, and the staged change undoes some of it:
// removes a line the commit added or brings back one it removed. A patch
// with no context after it is anchored at the end of the file, so appending
// to a file whose end repeats what the commit appended after lets the
// commit's patch apply again, with nothing taken back.
func takesBack(repo *gitx.Repo, c recentCommit, file, now, before, base string) (bool, error) {
	diff, err := repo.Git("diff", "--no-renames", "--no-ext-diff", "--binary", c.parent, c.sha, "--", file)
	if err != nil || diff == "" {
		return false, err
	}
	staged, err := repo.Git("diff", "--cached", "--no-renames", "--no-ext-diff", "--binary", base, "--", file)
	if err != nil {
		return false, err
	}
	if !undoes(diff, staged) {
		return false, nil
	}
	fits := func(index string) (bool, error) {
		_, err := repo.GitInput(bytes.NewReader([]byte(diff)), []string{"GIT_INDEX_FILE=" + index}, "apply", "--cached", "--check")
		if err == nil {
			return true, nil
		}
		if gitx.ExitCode(err) == 1 {
			return false, nil
		}
		return false, err
	}
	if ok, err := fits(now); err != nil || !ok {
		return false, err
	}
	ok, err := fits(before)
	return !ok, err
}

// undoes tells whether the staged diff removes a line the commit's diff added
// or adds one it removed; blank lines do not count. A commit's diff with no
// such lines, binary, an empty file or a mode, counts as undone: the patch
// applying is all there is to go by.
func undoes(commit, staged string) bool {
	added, removed := changedLines(commit)
	if len(added)+len(removed) == 0 {
		return true
	}
	stagedAdded, stagedRemoved := changedLines(staged)
	for l := range stagedRemoved {
		if added[l] {
			return true
		}
	}
	for l := range stagedAdded {
		if removed[l] {
			return true
		}
	}
	return false
}

// changedLines are the non-blank lines a unified diff adds and removes; nil
// for a binary diff.
func changedLines(diff string) (added, removed map[string]bool) {
	if strings.Contains(diff, "\nGIT binary patch\n") || strings.Contains(diff, "\nBinary files ") {
		return nil, nil
	}
	added, removed = map[string]bool{}, map[string]bool{}
	inHunk := false
	for l := range strings.SplitSeq(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			inHunk = true
		case strings.HasPrefix(l, "diff --git "):
			inHunk = false
		case !inHunk || strings.TrimSpace(l[min(1, len(l)):]) == "":
		case l[0] == '+':
			added[l[1:]] = true
		case l[0] == '-':
			removed[l[1:]] = true
		}
	}
	return added, removed
}
