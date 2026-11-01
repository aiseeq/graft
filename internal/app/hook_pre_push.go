package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/leaks"
)

// HookPrePush scans commits that the push target does not already contain.
// The hook is global, so it runs for any repository: one without leak
// settings or pushing elsewhere than a public target is let through before
// anything is opened, and a bare repository is read as one.
func (a *App) HookPrePush(ctx context.Context, _ string, targetURL string) error {
	settings, ok, err := leakSettings()
	if err != nil || !ok {
		return err
	}
	public, err := remoteIsPublic(ctx, targetURL, settings, a.Stderr)
	if err != nil || !public {
		return err
	}
	repo, err := gitx.OpenObjects(a.Dir)
	if err != nil {
		return err
	}
	refs, err := io.ReadAll(a.Stdin)
	if err != nil {
		return fmt.Errorf("reading pre-push refs: %w", err)
	}
	local, remoteSHAs := pushedSHAs(string(refs))
	if len(local) == 0 {
		return nil
	}
	exclusions, err := targetObjects(repo, targetURL, remoteSHAs)
	if err != nil {
		return err
	}
	tags, err := pushedTags(repo, local, exclusions)
	if err != nil {
		return err
	}
	commits, err := pushedCommits(repo, local, exclusions)
	if err != nil {
		return err
	}
	if len(commits) == 0 && len(tags) == 0 {
		return nil
	}
	rules, err := loadLeakRules(settings, a.Stderr)
	if err != nil {
		return err
	}
	found, err := a.scanTagMessages(repo, rules, tags)
	if err != nil {
		return err
	}
	if err := a.scanLeakCommits(repo, rules, commits); err != nil || found > 0 {
		return fmt.Errorf("%w; knowingly bypass with git push --no-verify", errors.Join(err, tagFindings(found)))
	}
	return nil
}

// pushedTag is an annotated tag a push brings, with its message.
type pushedTag struct {
	sha, name, message string
}

// pushedTags reads the annotated tags among the pushed objects not at the
// target, tags of tags included, and refuses a push of a tree or a blob:
// graft scans commits and tag messages only, and says so rather than letting
// one through unread.
func pushedTags(repo *gitx.Repo, local, exclusions []string) ([]pushedTag, error) {
	at := map[string]bool{}
	for _, sha := range exclusions {
		at[sha] = true
	}
	var tags []pushedTag
	for _, sha := range local {
		for sha != "" && !at[sha] {
			kind, err := repo.Git("cat-file", "-t", sha)
			if err != nil {
				return nil, err
			}
			switch kind = strings.TrimSpace(kind); kind {
			case "commit":
				sha = ""
			case "tag":
				t, object, err := readTag(repo, sha)
				if err != nil {
					return nil, err
				}
				tags = append(tags, t)
				at[sha] = true
				sha = object
			default:
				return nil, fmt.Errorf("the push sends %s, a %s: graft scans commits and tag messages only; knowingly bypass with git push --no-verify", gitx.Short(sha), kind)
			}
		}
	}
	return tags, nil
}

// readTag reads an annotated tag: its name and message, and the object it
// tags.
func readTag(repo *gitx.Repo, sha string) (pushedTag, string, error) {
	body, err := repo.Git("cat-file", "tag", sha)
	if err != nil {
		return pushedTag{}, "", err
	}
	header, message, _ := strings.Cut(body, "\n\n")
	t := pushedTag{sha: sha, message: strings.TrimRight(message, "\n")}
	var object string
	for line := range strings.SplitSeq(header, "\n") {
		if v, ok := strings.CutPrefix(line, "object "); ok {
			object = v
		} else if v, ok := strings.CutPrefix(line, "tag "); ok {
			t.name = v
		}
	}
	return t, object, nil
}

// scanTagMessages scans the messages of the pushed annotated tags and prints
// their findings; it returns how many there were.
func (a *App) scanTagMessages(repo *gitx.Repo, rules *leaks.Rules, tags []pushedTag) (int, error) {
	total := 0
	for _, t := range tags {
		found, err := rules.Scan(repo, "", nil, t.message)
		if err != nil {
			return 0, err
		}
		if len(found) == 0 {
			continue
		}
		a.printf("tag %s (%s)", t.name, gitx.Short(t.sha))
		for _, f := range found {
			path := f.Path
			if path == leaks.MessagePath {
				path = "tag message"
			}
			fmt.Fprintf(a.Stdout, "  %s:%d: %s\n", path, f.Line, f.Reason)
		}
		total += len(found)
	}
	return total, nil
}

func tagFindings(n int) error {
	if n == 0 {
		return nil
	}
	return fmt.Errorf("%d private names or terms found in tag messages", n)
}

func pushedSHAs(input string) (local, remote []string) {
	localSet, remoteSet := map[string]bool{}, map[string]bool{}
	for line := range strings.SplitSeq(input, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 4 || allZero(fields[1]) {
			continue
		}
		localSet[fields[1]] = true
		if !allZero(fields[3]) {
			remoteSet[fields[3]] = true
		}
	}
	for sha := range localSet {
		local = append(local, sha)
	}
	for sha := range remoteSet {
		remote = append(remote, sha)
	}
	slices.Sort(local)
	slices.Sort(remote)
	return local, remote
}

func targetObjects(repo *gitx.Repo, targetURL string, fromInput []string) ([]string, error) {
	out, err := gitx.InOther(repo.Root, nil, "ls-remote", targetURL)
	if err != nil {
		return nil, fmt.Errorf("git ls-remote %q failed: %w", targetURL, err)
	}
	objects := map[string]bool{}
	for _, sha := range fromInput {
		objects[sha] = true
	}
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			objects[fields[0]] = true
		}
	}
	shas := make([]string, 0, len(objects))
	for sha := range objects {
		shas = append(shas, sha)
	}
	slices.Sort(shas)
	if len(shas) == 0 {
		return nil, nil
	}
	input := strings.Join(shas, "\n") + "\n"
	checked, err := repo.GitInput(strings.NewReader(input), nil, "cat-file", "--batch-check")
	if err != nil {
		return nil, fmt.Errorf("checking target objects locally: %w", err)
	}
	var localObjects []string
	for line := range strings.SplitSeq(strings.TrimSpace(checked), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] != "missing" {
			localObjects = append(localObjects, fields[0])
		}
	}
	return localObjects, nil
}

func pushedCommits(repo *gitx.Repo, local, exclusions []string) ([]string, error) {
	// The revisions go on stdin: a target with thousands of refs is past
	// the command line length Windows allows.
	input := strings.Join(local, "\n") + "\n"
	if len(exclusions) > 0 {
		input += "--not\n" + strings.Join(exclusions, "\n") + "\n"
	}
	out, err := repo.GitInput(strings.NewReader(input), nil, "rev-list", "--reverse", "--parents", "--stdin")
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

func allZero(s string) bool {
	return s != "" && strings.Trim(s, "0") == ""
}
