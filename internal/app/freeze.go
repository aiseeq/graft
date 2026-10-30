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
	"github.com/aiseeq/graft/v2/internal/envs"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/jira"
	"github.com/aiseeq/graft/v2/internal/tasks"
)

type activeFreeze struct {
	target, name, version, branch, tag string
	time                               time.Time
	config                             *config.DeployTarget
}

type remoteTag struct{ sha, remote string }

type freezeRefusalError struct{ error }

var freezeTagPart = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
var errNoActiveFreeze = errors.New("no active freeze")

func freezeSlug(s string) string { return strings.Trim(freezeTagPart.ReplaceAllString(s, "_"), "_.-") }

func parseRemoteTags(out, remote string) map[string]remoteTag {
	tags := map[string]remoteTag{}
	for line := range strings.SplitSeq(out, "\n") {
		sha, ref, ok := strings.Cut(line, "\t")
		if ok && !strings.HasSuffix(ref, "^{}") {
			tags[strings.TrimPrefix(ref, "refs/tags/")] = remoteTag{sha: sha, remote: remote}
		}
	}
	return tags
}

func activeTargetFreeze(repo *gitx.Repo, cfg *config.Config, name string, target *config.DeployTarget) (*activeFreeze, error) {
	if target.Freeze == nil {
		// This target has not opted into freezes.
		return nil, errNoActiveFreeze
	}
	remotes, err := pushRemotes(repo, cfg)
	if err != nil {
		return nil, err
	}
	for _, remote := range remotes {
		out, err := repo.Git("ls-remote", "--tags", remote, "refs/tags/freeze/"+name+"/*")
		if err != nil {
			return nil, err
		}
		tags := parseRemoteTags(out, remote)
		for _, tag := range slices.Sorted(maps.Keys(tags)) {
			return readFreezeTag(repo, tags[tag], name, tag, target)
		}
	}
	// No remote carries an active freeze tag for this target.
	return nil, errNoActiveFreeze
}

func readRemoteTag(repo *gitx.Repo, ref remoteTag) (string, error) {
	if _, err := repo.Git("fetch", "--no-write-fetch-head", "--quiet", ref.remote, ref.sha); err != nil {
		return "", err
	}
	return repo.Git("cat-file", "tag", ref.sha)
}

func readFreezeTag(repo *gitx.Repo, ref remoteTag, name, tag string, target *config.DeployTarget) (*activeFreeze, error) {
	contents, err := readRemoteTag(repo, ref)
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
	if f.name == "" || f.branch == "" || f.time.IsZero() {
		return nil, fmt.Errorf("invalid freeze tag %s: version, branch and time are required", tag)
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
	if errors.Is(err, errNoActiveFreeze) {
		err = nil
	}
	if err != nil {
		return err
	}
	if versionName == "" {
		if current == nil {
			return fmt.Errorf("no active freeze for %s", target)
		}
		a.printf("%s is frozen at %s on %s since %s", target, current.name, current.branch, current.time.Format(time.RFC3339))
		return nil
	}
	if current != nil {
		return fmt.Errorf("%s is already frozen at %s on %s", target, current.name, current.branch)
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
		_, cleanupErr := r.repo.Git("tag", "-d", tag)
		return errors.Join(err, cleanupErr)
	}
	if _, err := r.repo.Git("tag", "-d", tag); err != nil {
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
	_, version, _, err := findProjectVersion(ctx, client, cfg.ReleaseNotes.Jira.ProjectKeys, name)
	return version, err
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
	freeze, err := activeTargetFreeze(r.repo, r.cfg, target, r.target)
	if errors.Is(err, errNoActiveFreeze) {
		err = nil
	}
	if err != nil {
		return err
	}
	if freeze == nil {
		return fmt.Errorf("no active freeze for %s", target)
	}
	if err := a.removeFreezeTags(r.repo, r.cfg, freeze); err != nil {
		return err
	}
	a.printf("thawed %s at %s: %s", target, freeze.name, reason)
	return nil
}

func (a *App) removeFreezeTags(repo *gitx.Repo, cfg *config.Config, f *activeFreeze) error {
	remotes, err := pushRemotes(repo, cfg)
	if err != nil {
		return err
	}
	journal, err := freezeLogTags(repo, cfg, f)
	if err != nil {
		return err
	}
	for _, remote := range remotes {
		var refs []string
		out, err := repo.Git("ls-remote", "--tags", remote, "refs/tags/"+f.tag)
		if err != nil {
			return err
		}
		for tag := range parseRemoteTags(out, remote) {
			refs = append(refs, ":refs/tags/"+tag)
		}
		for tag, remoteRefs := range journal {
			for _, ref := range remoteRefs {
				if ref.remote == remote {
					refs = append(refs, ":refs/tags/"+tag)
					break
				}
			}
		}
		if len(refs) > 0 {
			args := append([]string{"push", remote}, refs...)
			if _, err := repo.Git(args...); err != nil {
				return err
			}
		}
	}
	local, err := repo.Git("tag", "--list", "freeze-log/"+f.target+"/"+freezeSlug(f.version)+"/*", f.tag)
	if err != nil {
		return err
	}
	for tag := range strings.SplitSeq(strings.TrimSpace(local), "\n") {
		if tag != "" {
			if _, err := repo.Git("tag", "-d", tag); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *App) freezeLog(repo *gitx.Repo, cfg *config.Config, f *activeFreeze, commit, where, kind, reason string) (string, error) {
	now, err := deployNow()
	if err != nil {
		return "", err
	}
	base := fmt.Sprintf("freeze-log/%s/%s/%s-%s", f.target, freezeSlug(f.version), now.Format("20060102T150405"), gitx.Short(commit))
	remotes, err := pushRemotes(repo, cfg)
	if err != nil {
		return "", err
	}
	msg := fmt.Sprintf("target: %s\ncommit: %s\nreason: %s\n", where, commit, reason)
	for n := 0; ; n++ {
		suffix := ""
		if n > 0 {
			suffix = fmt.Sprintf("-%d", n)
		}
		tag := base + suffix + "-" + kind
		exists, err := remoteTagExists(repo, remotes, tag)
		if err != nil {
			return tag, err
		}
		if exists {
			continue
		}
		if _, err := repo.Git("tag", "-a", tag, commit, "-m", msg); err != nil {
			return tag, err
		}
		if err := a.push(repo, remotes, []string{"refs/tags/" + tag}, false); err != nil {
			collision, cleanupErr := cleanupFreezeLogPush(repo, remotes, tag)
			if cleanupErr != nil {
				return tag, errors.Join(err, cleanupErr)
			}
			if collision {
				continue
			}
			return tag, err
		}
		_, err = repo.Git("tag", "-d", tag)
		return tag, err
	}
}

func cleanupFreezeLogPush(repo *gitx.Repo, remotes []string, tag string) (bool, error) {
	localSHA, err := repo.Git("rev-parse", "refs/tags/"+tag)
	if err != nil {
		return false, err
	}
	localSHA = strings.TrimSpace(localSHA)
	var collision bool
	var pushed []string
	for _, remote := range remotes {
		out, err := repo.Git("ls-remote", "--tags", remote, "refs/tags/"+tag)
		if err != nil {
			return false, err
		}
		for _, ref := range parseRemoteTags(out, remote) {
			if ref.sha == localSHA {
				pushed = append(pushed, remote)
			} else {
				collision = true
			}
		}
	}
	var cleanupErrs []error
	for _, remote := range pushed {
		if _, err := repo.Git("push", remote, ":refs/tags/"+tag); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("remove partial freeze-log tag from %s: %w", remote, err))
		}
	}
	if _, err := repo.Git("tag", "-d", tag); err != nil {
		cleanupErrs = append(cleanupErrs, err)
	}
	return collision, errors.Join(cleanupErrs...)
}

func remoteTagExists(repo *gitx.Repo, remotes []string, tag string) (bool, error) {
	for _, remote := range remotes {
		out, err := repo.Git("ls-remote", "--tags", remote, "refs/tags/"+tag)
		if err != nil {
			return false, err
		}
		if len(parseRemoteTags(out, remote)) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func activeFreezeForRun(r *deployRun) (*activeFreeze, error) {
	for _, name := range slices.Sorted(maps.Keys(r.cfg.Deploy.Targets)) {
		target := r.cfg.Deploy.Targets[name]
		if target.Freeze == nil || (name != r.name && target.Requires != r.name) {
			continue
		}
		return activeTargetFreeze(r.repo, r.cfg, name, target)
	}
	// This deploy target has no related freeze configured.
	return nil, errNoActiveFreeze
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

func filterFreezeAdvisoryNames(tasks []string, f *activeFreeze) []string {
	var selected []string
	for _, task := range tasks {
		if freezeAdvisory(f, task) {
			selected = append(selected, task)
		}
	}
	return selected
}

func configuredFreezeAdvisory(cfg *config.Config) bool {
	if cfg == nil || cfg.Deploy == nil {
		return false
	}
	for _, target := range cfg.Deploy.Targets {
		if target.Freeze != nil && len(target.Freeze.Advisory) > 0 {
			return true
		}
	}
	return false
}

func configWithoutFreezeAdvisory(cfg *config.Config, f *activeFreeze) *config.Config {
	filtered := *cfg
	filtered.Gate = filterFreezeAdvisory(cfg.Gate, f, func(step config.Step) string { return step.Task })
	return &filtered
}

func (a *App) frozenForBranch(repo *gitx.Repo, cfg *config.Config, branch string) (*activeFreeze, error) {
	if cfg == nil || cfg.Deploy == nil {
		// A commit gate without deploy configuration cannot have a freeze.
		return nil, errNoActiveFreeze
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Deploy.Targets)) {
		target := cfg.Deploy.Targets[name]
		if target.Freeze == nil || target.Freeze.Advisory == nil {
			continue
		}
		pattern := strings.ReplaceAll(regexp.QuoteMeta(target.Freeze.Branch), regexp.QuoteMeta("{version}"), ".+")
		matches, err := regexp.MatchString("^"+pattern+"$", branch)
		if err != nil {
			return nil, fmt.Errorf("matching freeze branch pattern for %s: %w", name, err)
		}
		if !matches {
			continue
		}
		freeze, err := activeTargetFreeze(repo, cfg, name, target)
		if errors.Is(err, errNoActiveFreeze) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if freeze != nil && freeze.branch == branch {
			return freeze, nil
		}
	}
	// No active freeze uses this branch.
	return nil, errNoActiveFreeze
}

func (a *App) runFreezeAdvisory(ctx context.Context, runner *tasks.Runner, f *activeFreeze, branch string, selected []string) []string {
	if f == nil || f.branch != branch {
		return nil
	}
	var failed []string
	for _, task := range selected {
		if err := runner.Run(ctx, task, nil); err != nil {
			a.warn("%s failed, advisory while %s is frozen: %v", task, f.name, err)
			failed = append(failed, task)
		}
	}
	return failed
}

func (a *App) recordCommitAdvisory(repo *gitx.Repo, cfg *config.Config, f *activeFreeze, failed []string) error {
	head, err := repo.Git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	for _, task := range failed {
		if tag, err := a.freezeLog(repo, cfg, f, strings.TrimSpace(head), "commit", "advisory", task); err != nil {
			a.warn("could not record advisory failure: %v", err)
		} else {
			a.printf("recorded freeze advisory in %s", tag)
		}
	}
	return nil
}

func (a *App) freezeIssueCheck(ctx context.Context, r *deployRun, f *activeFreeze) error {
	if f.config.ApprovedRelease == nil {
		return errors.New("freeze requires approved_release.prefix to identify Jira versions")
	}
	if f.config.DeployedSHA == nil {
		return fmt.Errorf("freeze target %s has no deployed_sha", f.target)
	}
	acceptance := r.cfg.Deploy.Targets[f.config.Requires]
	if acceptance == nil || acceptance.DeployedSHA == nil {
		return fmt.Errorf("freeze target %s requires deployed_sha on both targets", f.target)
	}
	env, err := envs.Get(r.cfg, r.repo.Root, f.config.Env)
	if err != nil {
		return err
	}
	prev, err := r.readSHA(ctx, env, f.config.DeployedSHA)
	if err != nil {
		return err
	}
	if prev == "" {
		return fmt.Errorf("no deployed commit recorded on %s, what goes out is unknown", f.target)
	}
	commits, err := deliveredCommits(r.repo, prev, r.head)
	if err != nil {
		return err
	}
	bypassed, err := bypassedCommits(r.repo, r.cfg, f)
	if err != nil {
		return err
	}
	if len(bypassed) > 0 {
		args := []string{"log", "--format=%H", prev + ".." + r.head, "--not"}
		for sha := range bypassed {
			args = append(args, sha)
		}
		listed, err := r.repo.Git(args...)
		if err != nil {
			return fmt.Errorf("listing commits not already bypassed: %w", err)
		}
		allowed := map[string]bool{}
		for line := range strings.SplitSeq(listed, "\n") {
			if sha := strings.TrimSpace(line); sha != "" {
				allowed[sha] = true
			}
		}
		commits = slices.DeleteFunc(commits, func(c deliveredCommit) bool { return !allowed[c.sha] })
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
		if bypassed[c.sha] {
			continue
		}
		if err := checkFreezeCommit(ctx, r, f, g.gate, keyRe, c, reverted[c.sha]); err != nil {
			return err
		}
	}
	return nil
}

func bypassedCommits(repo *gitx.Repo, cfg *config.Config, f *activeFreeze) (map[string]bool, error) {
	tags, err := freezeLogTags(repo, cfg, f)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for tag, refs := range tags {
		if !strings.HasSuffix(tag, "-bypass") {
			continue
		}
		contents, err := readRemoteTag(repo, refs[0])
		if err != nil {
			return nil, err
		}
		for line := range strings.SplitSeq(contents, "\n") {
			if after, ok := strings.CutPrefix(line, "commit: "); ok {
				out[after] = true
			}
		}
	}
	return out, nil
}

func freezeLogTags(repo *gitx.Repo, cfg *config.Config, f *activeFreeze) (map[string][]remoteTag, error) {
	remotes, err := pushRemotes(repo, cfg)
	if err != nil {
		return nil, err
	}
	tags := map[string][]remoteTag{}
	pattern := "refs/tags/freeze-log/" + f.target + "/" + freezeSlug(f.version) + "/*"
	for _, remote := range remotes {
		out, err := repo.Git("ls-remote", "--tags", remote, pattern)
		if err != nil {
			return nil, err
		}
		for name, ref := range parseRemoteTags(out, remote) {
			tags[name] = append(tags[name], ref)
		}
	}
	return tags, nil
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
		return freezeRefusalError{fmt.Errorf("%s names no item in %s", gitx.Short(commit.sha), f.name)}
	}
	var matched []string
	for _, key := range keys {
		if err := checkFreezeIssue(ctx, gate, f, key, &matched); err != nil {
			return err
		}
	}
	if len(matched) == 0 {
		ignored, err := shipsNothing(r.repo, r.cfg, f.config.ApprovedRelease, commit.sha)
		if err != nil {
			return err
		}
		if ignored {
			return nil
		}
		return freezeRefusalError{fmt.Errorf("%s not in %s", strings.Join(keys, ", "), f.name)}
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
	added, hasAddition, err := gate.client.FixVersionHistoryAfter(ctx, key, f.name, f.time)
	if err != nil {
		return err
	}
	if !hasAddition && issue.Created.After(f.time) {
		added = true
	}
	if added && !slices.Contains(f.config.Freeze.Types, issue.Type) {
		return freezeRefusalError{fmt.Errorf("%s joined %s after the freeze", key, f.name)}
	}
	return nil
}
