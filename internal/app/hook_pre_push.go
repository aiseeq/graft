package app

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/gitx"
)

// HookPrePush scans commits that the push target does not already contain.
func (a *App) HookPrePush(ctx context.Context, _ string, targetURL string) error {
	repo, err := gitx.Open(a.Dir)
	if err != nil {
		return err
	}
	settings, ok, err := leakSettings()
	if err != nil || !ok {
		return err
	}
	public, err := remoteIsPublic(ctx, targetURL, settings, a.Stderr)
	if err != nil || !public {
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
	commits, err := pushedCommits(repo, local, exclusions)
	if err != nil {
		return err
	}
	if len(commits) == 0 {
		return nil
	}
	rules, err := loadLeakRules(settings, a.Stderr)
	if err != nil {
		return err
	}
	if err := a.scanLeakCommits(repo, rules, commits); err != nil {
		return fmt.Errorf("%w; knowingly bypass with git push --no-verify", err)
	}
	return nil
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
	args := []string{"rev-list", "--reverse", "--parents"}
	args = append(args, local...)
	if len(exclusions) > 0 {
		args = append(args, "--not")
		args = append(args, exclusions...)
	}
	args = append(args, "--")
	out, err := repo.Git(args...)
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
