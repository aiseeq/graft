package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/deploy"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/jira"
)

type activeFreeze struct {
	target, name, version, branch, tag string
	time                               time.Time
	config                             *config.DeployTarget
}

type freezeLookup struct {
	freeze *activeFreeze
	found  bool
}

var freezeTagPart = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func freezeSlug(s string) string { return strings.Trim(freezeTagPart.ReplaceAllString(s, "_"), "_.-") }

func activeTargetFreeze(repo *gitx.Repo, cfg *config.Config, name string, target *config.DeployTarget) (freezeLookup, error) {
	if target.Freeze == nil {
		// This target has not opted into freezes.
		return freezeLookup{}, nil
	}
	remotes, err := pushRemotes(repo, cfg)
	if err != nil {
		return freezeLookup{}, err
	}
	for _, remote := range remotes {
		out, err := repo.Git("ls-remote", "--tags", remote, "refs/tags/freeze/"+name+"/*")
		if err != nil {
			return freezeLookup{}, err
		}
		for line := range strings.SplitSeq(out, "\n") {
			_, ref, ok := strings.Cut(line, "\t")
			if !ok || strings.HasSuffix(ref, "^{}") {
				continue
			}
			tag := strings.TrimPrefix(ref, "refs/tags/")
			freeze, err := readFreezeTag(repo, remote, name, tag, target)
			return freezeLookup{freeze: freeze, found: err == nil}, err
		}
	}
	// No remote carries an active freeze tag for this target.
	return freezeLookup{}, nil
}

func readFreezeTag(repo *gitx.Repo, remote, name, tag string, target *config.DeployTarget) (*activeFreeze, error) {
	if _, err := repo.Git("fetch", remote, "refs/tags/"+tag+":refs/tags/"+tag); err != nil {
		return nil, err
	}
	contents, err := repo.Git("for-each-ref", "--format=%(contents)", "refs/tags/"+tag)
	if err != nil {
		return nil, err
	}
	f := &activeFreeze{target: name, tag: tag, config: target}
	for line := range strings.SplitSeq(contents, "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		if err := setFreezeField(f, key, value); err != nil {
			return nil, fmt.Errorf("invalid freeze tag %s: %w", tag, err)
		}
	}
	if target.ApprovedRelease != nil {
		f.version = strings.TrimPrefix(f.name, target.ApprovedRelease.Prefix)
	}
	return f, nil
}

func setFreezeField(f *activeFreeze, key, value string) error {
	switch key {
	case "version":
		f.name = value
	case "branch":
		f.branch = value
	case "time":
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return fmt.Errorf("time: %w", err)
		}
		f.time = parsed
	}
	return nil
}

// DeployFreeze starts a release freeze, or prints the active freeze when no
// version name is supplied.
func (a *App) DeployFreeze(ctx context.Context, target, versionName string) error {
	r, err := a.deployTarget(target)
	if err != nil {
		return err
	}
	if r.target.Freeze == nil {
		return fmt.Errorf("deploy.targets.%s has no freeze configured", target)
	}
	current, err := activeTargetFreeze(r.repo, r.cfg, target, r.target)
	if err != nil {
		return err
	}
	if versionName == "" {
		if !current.found {
			return fmt.Errorf("no active freeze for %s", target)
		}
		a.printf("%s is frozen at %s on %s since %s", target, current.freeze.name, current.freeze.branch, current.freeze.time.Format(time.RFC3339))
		return nil
	}
	if current.found {
		return fmt.Errorf("%s is already frozen at %s on %s", target, current.freeze.name, current.freeze.branch)
	}
	if err := deployFreezeTrunk(r.repo, r.cfg); err != nil {
		return err
	}
	return a.cutFreeze(ctx, r, target, versionName)
}

func (a *App) cutFreeze(ctx context.Context, r *deployRun, target, versionName string) error {
	version, err := findFreezeVersion(ctx, r.cfg, versionName)
	if err != nil {
		return err
	}
	if version.ID == "" {
		return fmt.Errorf("Jira has no version %q", versionName)
	}
	short := strings.TrimPrefix(versionName, r.target.ApprovedRelease.Prefix)
	branch := strings.ReplaceAll(r.target.Freeze.Branch, "{version}", short)
	if strings.Contains(branch, "{version}") {
		return fmt.Errorf("deploy.targets.%s.freeze.branch has an unsupported placeholder", target)
	}
	remotes, err := pushRemotes(r.repo, r.cfg)
	if err != nil {
		return err
	}
	if err := checkoutFreezeBranch(r.repo, r.cfg.Deploy.Remote, branch); err != nil {
		return err
	}
	if err := a.push(r.repo, remotes, []string{"refs/heads/" + branch}, false); err != nil {
		return err
	}
	now, err := deployNow()
	if err != nil {
		return err
	}
	who, err := r.repo.Git("config", "user.name")
	if err != nil {
		return err
	}
	tag := "freeze/" + target + "/" + freezeSlug(short)
	msg := fmt.Sprintf("version: %s\nbranch: %s\ntime: %s\nwho: %s\n", versionName, branch, now.Format(time.RFC3339), strings.TrimSpace(who))
	if _, err := r.repo.Git("tag", "-a", tag, "-m", msg); err != nil {
		return err
	}
	if err := a.push(r.repo, remotes, []string{"refs/tags/" + tag}, false); err != nil {
		return err
	}
	a.printf("froze %s at %s on %s", target, versionName, branch)
	return nil
}

func findFreezeVersion(ctx context.Context, cfg *config.Config, name string) (jira.Version, error) {
	client, err := jiraClient()
	if err != nil {
		return jira.Version{}, err
	}
	for _, project := range cfg.ReleaseNotes.Jira.ProjectKeys {
		versions, err := client.ProjectVersions(ctx, project)
		if err != nil {
			return jira.Version{}, err
		}
		for _, version := range versions {
			if version.Name == name {
				return version, nil
			}
		}
	}
	// The caller reports that the requested version is not present.
	return jira.Version{}, nil
}

func checkoutFreezeBranch(repo *gitx.Repo, remote, branch string) error {
	remoteRef := "refs/remotes/" + remote + "/" + branch
	if _, err := repo.Git("show-ref", "--verify", "--quiet", remoteRef); err != nil {
		_, err := repo.Git("switch", "-c", branch, "HEAD")
		return err
	}
	remoteSHA, err := repo.Git("rev-parse", remoteRef)
	if err != nil {
		return err
	}
	localSHA, localErr := repo.Git("rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if localErr == nil && strings.TrimSpace(localSHA) != strings.TrimSpace(remoteSHA) {
		return fmt.Errorf("local branch %s differs from remote %s; resolve it before freezing", branch, remote)
	}
	if localErr != nil {
		if _, err := repo.Git("branch", branch, remoteRef); err != nil {
			return err
		}
	}
	_, err = repo.Git("switch", branch)
	return err
}

func deployFreezeTrunk(repo *gitx.Repo, cfg *config.Config) error {
	remotes, err := pushRemotes(repo, cfg)
	if err != nil {
		return err
	}
	for _, remote := range remotes {
		if _, err := repo.Git("fetch", "--quiet", remote); err != nil {
			return fmt.Errorf("fetch trunk from %s: %w", remote, err)
		}
	}
	branch, err := repo.Branch()
	if err != nil {
		return err
	}
	trunk, err := trunkRef(repo, cfg, "", remotes)
	if err != nil {
		return err
	}
	trunkBranch := strings.TrimPrefix(trunk, cfg.Deploy.Remote+"/")
	if branch != trunkBranch {
		return fmt.Errorf("freeze must be cut from the trunk branch %s", trunkBranch)
	}
	head, err := repo.Git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	remoteHead, err := repo.Git("rev-parse", trunk)
	if err != nil {
		return err
	}
	if strings.TrimSpace(head) != strings.TrimSpace(remoteHead) {
		return fmt.Errorf("HEAD differs from remote trunk %s; update the trunk before freezing", trunk)
	}
	if err := deploy.CheckTree(repo, cfg.Deploy); err != nil {
		return err
	}
	return nil
}

// DeployThaw ends the active freeze and removes its freeze and journal tags.
func (a *App) DeployThaw(ctx context.Context, target, reason string) error {
	r, err := a.deployTarget(target)
	if err != nil {
		return err
	}
	if reason == "" {
		return errors.New("--reason is required")
	}
	lookup, err := activeTargetFreeze(r.repo, r.cfg, target, r.target)
	if err != nil {
		return err
	}
	if !lookup.found {
		return fmt.Errorf("no active freeze for %s", target)
	}
	return removeFreezeTags(a, r.repo, r.cfg, lookup.freeze)
}

func removeFreezeTags(a *App, repo *gitx.Repo, cfg *config.Config, f *activeFreeze) error {
	remotes, err := pushRemotes(repo, cfg)
	if err != nil {
		return err
	}
	var refs []string
	for _, remote := range remotes {
		out, e := repo.Git("ls-remote", "--tags", remote, "refs/tags/freeze/"+f.target+"/*", "refs/tags/freeze-log/"+f.target+"/*")
		if e != nil {
			return e
		}
		for line := range strings.SplitSeq(out, "\n") {
			_, ref, ok := strings.Cut(line, "\t")
			if ok && !strings.HasSuffix(ref, "^{}") {
				refs = append(refs, ":"+ref)
			}
		}
	}
	refs = slices.Compact(refs)
	for _, remote := range remotes {
		args := append([]string{"push", remote}, refs...)
		if len(refs) > 0 {
			if _, err := repo.Git(args...); err != nil {
				return err
			}
		}
	}
	local, err := repo.Git("tag", "--list", f.tag)
	if err != nil {
		return err
	}
	if strings.TrimSpace(local) != "" {
		if _, err := repo.Git("tag", "-d", f.tag); err != nil {
			return err
		}
	}
	return nil
}

func freezeLog(a *App, repo *gitx.Repo, cfg *config.Config, f *activeFreeze, commit, where, kind, reason string) error {
	now, err := deployNow()
	if err != nil {
		return err
	}
	base := fmt.Sprintf("freeze-log/%s/%s/%s", f.target, freezeSlug(f.version), now.Format("20060102T150405"))
	tag := base + "-" + kind
	for n := 1; ; n++ {
		tags, err := repo.Git("tag", "--list", tag)
		if err != nil {
			return err
		}
		if strings.TrimSpace(tags) == "" {
			break
		}
		tag = fmt.Sprintf("%s-%d-%s", base, n, kind)
	}
	msg := fmt.Sprintf("target: %s\ncommit: %s\nreason: %s\n", where, commit, reason)
	if _, err := repo.Git("tag", "-a", tag, commit, "-m", msg); err != nil {
		return err
	}
	remotes, err := pushRemotes(repo, cfg)
	if err != nil {
		return err
	}
	return a.push(repo, remotes, []string{"refs/tags/" + tag}, false)
}

func activeFreezeForRun(r *deployRun) (freezeLookup, error) {
	for _, name := range slices.Sorted(maps.Keys(r.cfg.Deploy.Targets)) {
		target := r.cfg.Deploy.Targets[name]
		if target.Freeze == nil || (name != r.name && target.Requires != r.name) {
			continue
		}
		return activeTargetFreeze(r.repo, r.cfg, name, target)
	}
	// This deploy target has no related freeze configured.
	return freezeLookup{}, nil
}

func (r *deployRun) freezeForRequiredTarget() *activeFreeze {
	if r.freeze != nil && r.freeze.target != r.name && r.freeze.config.Requires == r.name {
		return r.freeze
	}
	return nil
}

func (r *deployRun) freezeForOwnedTarget() *activeFreeze {
	if r.freeze != nil && r.freeze.target == r.name {
		return r.freeze
	}
	return nil
}

func (r *deployRun) freezeForBranch() *activeFreeze {
	if r.freeze != nil && r.freeze.branch == r.branch {
		return r.freeze
	}
	return nil
}

func freezeAdvisory(f *activeFreeze, task string) bool {
	return f != nil && slices.Contains(f.config.Freeze.Advisory, task)
}

func filterFreezeAdvisory[T any](tasks []T, f *activeFreeze, name func(T) string) []T {
	return slices.DeleteFunc(slices.Clone(tasks), func(task T) bool { return freezeAdvisory(f, name(task)) })
}

func configWithoutFreezeAdvisory(cfg *config.Config, f *activeFreeze) *config.Config {
	filtered := *cfg
	filtered.Gate = filterFreezeAdvisory(cfg.Gate, f, func(step config.Step) string { return step.Task })
	return &filtered
}

func (a *App) frozenForBranch(repo *gitx.Repo, cfg *config.Config, branch string) (freezeLookup, error) {
	if cfg == nil || cfg.Deploy == nil {
		// A commit gate without deploy configuration cannot have a freeze.
		return freezeLookup{}, nil
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Deploy.Targets)) {
		target := cfg.Deploy.Targets[name]
		if target.Freeze == nil || target.Freeze.Advisory == nil {
			continue
		}
		lookup, err := activeTargetFreeze(repo, cfg, name, target)
		if err != nil {
			return freezeLookup{}, err
		}
		if lookup.found && lookup.freeze.branch == branch {
			return lookup, nil
		}
	}
	// No active freeze uses this branch.
	return freezeLookup{}, nil
}

func (a *App) runFreezeAdvisory(ctx context.Context, repo *gitx.Repo, cfg *config.Config, f *activeFreeze, branch, target, head string) error {
	if f == nil || f.branch != branch {
		return nil
	}
	for _, task := range f.config.Freeze.Advisory {
		if err := a.runner(repo, cfg).Run(ctx, task, nil); err != nil {
			a.warn("%s failed, advisory while %s is frozen", task, f.name)
			if err := freezeLog(a, repo, cfg, f, head, target, "advisory", task); err != nil {
				a.warn("could not record advisory failure: %v", err)
			}
		}
	}
	return nil
}

func (a *App) freezeIssueCheck(ctx context.Context, r *deployRun, f *activeFreeze) error {
	if f.config.ApprovedRelease == nil {
		return errors.New("freeze requires approved_release.prefix to identify Jira versions")
	}
	prev := r.prev
	if prev == "" {
		return errors.New("no deployed commit recorded, what goes out is unknown")
	}
	commits, err := deliveredCommits(r.repo, prev, r.head)
	if err != nil {
		return err
	}
	g, err := newGateRun(a, r.repo, r.cfg, f.config.ApprovedRelease)
	if err != nil {
		return err
	}
	keyRe := projectKeyRe(r.cfg)
	reverted, err := g.reverted(commits)
	if err != nil {
		return err
	}
	for _, c := range commits {
		if err := checkFreezeCommit(ctx, r, f, g.gate, keyRe, c, reverted[c.sha]); err != nil {
			return err
		}
	}
	return nil
}

func checkFreezeCommit(ctx context.Context, r *deployRun, f *activeFreeze, gate *approvalGate, keyRe *regexp.Regexp, commit deliveredCommit, reverted string) error {
	if isRevert(commit.subject) || reverted != "" {
		return nil
	}
	keys := commitKeys(keyRe, f.config.ApprovedRelease, commit)
	if len(keys) == 0 {
		ignored, err := shipsNothing(r.repo, r.cfg, f.config.ApprovedRelease, commit.sha)
		if err != nil || ignored {
			return err
		}
		return fmt.Errorf("%s names no item in %s", gitx.Short(commit.sha), f.name)
	}
	var matched []string
	for _, key := range keys {
		if err := checkFreezeIssue(ctx, gate, f, key, &matched); err != nil {
			return err
		}
	}
	if len(matched) == 0 {
		return fmt.Errorf("%s not in %s", strings.Join(keys, ", "), f.name)
	}
	return nil
}

func checkFreezeIssue(ctx context.Context, gate *approvalGate, f *activeFreeze, key string, matched *[]string) error {
	issue, err := gate.issue(ctx, key)
	if err != nil {
		return err
	}
	if !issue.HasFixVersion(f.name) && issue.Parent != "" {
		issue, err = gate.issue(ctx, issue.Parent)
		if err != nil {
			return err
		}
		key = issue.Key
	}
	if !issue.HasFixVersion(f.name) {
		return nil
	}
	*matched = append(*matched, key)
	added, err := gate.client.FixVersionAddedAfter(ctx, key, f.name, f.time)
	if err != nil {
		return err
	}
	if added && !slices.Contains(f.config.Freeze.Types, issue.Type) {
		return fmt.Errorf("%s joined %s after the freeze", key, f.name)
	}
	return nil
}
