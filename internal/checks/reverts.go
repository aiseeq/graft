package checks

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

// Reverts finds the staged files that take back a recent commit's change: the
// commit's diff of the file applies to the staged tree and did not apply to
// HEAD's. A tree from an old base committed on a fresh parent silently undoes
// merged work that way, a deleted file as much as changed lines. A revert that
// is meant names the commit (7 characters of its hash or more, as git prints
// it short) or the file in the message. Files moved by the commit, the version files graft bumps and
// the exceptions do not count. A partial take-back, part of the commit's diff
// of a file, is not seen: the check is per file.
func Reverts(repo *gitx.Repo, cfg config.Reverts, versionFiles []string, message string) ([]Finding, error) {
	if ok, err := repo.HasHead(); err != nil || !ok {
		return nil, err
	}
	staged, err := revertCandidates(repo, cfg, versionFiles)
	if err != nil || len(staged) == 0 {
		return nil, err
	}
	recent, err := recentCommits(repo, cfg.Depth)
	if err != nil {
		return nil, err
	}
	tree, err := repo.Git("write-tree")
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "graft-reverts-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	now, before := filepath.Join(tmp, "staged"), filepath.Join(tmp, "head")
	for index, rev := range map[string]string{now: strings.TrimSpace(tree), before: "HEAD"} {
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
			back, err := takesBack(repo, c, f, now, before)
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
func revertCandidates(repo *gitx.Repo, cfg config.Reverts, versionFiles []string) ([]string, error) {
	out, err := repo.Git("diff", "--cached", "-M", "--name-status", "-z", "--no-ext-diff", "HEAD")
	if err != nil {
		return nil, err
	}
	var files []string
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
	}
	return slices.DeleteFunc(files, func(f string) bool {
		return f == "" || slices.Contains(versionFiles, f) || config.Matches(cfg.Exceptions, f)
	}), nil
}

// recentCommit is a commit of HEAD's first-parent line with the files it
// changed.
type recentCommit struct {
	sha, parent, subject string
	files                []string
}

func recentCommits(repo *gitx.Repo, depth int) ([]recentCommit, error) {
	out, err := repo.Git("log", "--first-parent", "--no-merges", "--no-renames", fmt.Sprintf("--max-count=%d", depth),
		"--format=%x01%H%x00%P%x00%s", "--name-only", "-z", "HEAD")
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
// tree and did not apply to HEAD's.
func takesBack(repo *gitx.Repo, c recentCommit, file, now, before string) (bool, error) {
	diff, err := repo.Git("diff", "--no-renames", "--no-ext-diff", "--binary", c.parent, c.sha, "--", file)
	if err != nil || diff == "" {
		return false, err
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
