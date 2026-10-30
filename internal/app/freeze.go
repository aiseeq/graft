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
	found := map[string]remoteTag{}
	for _, remote := range remotes {
		tags, err := listRemoteTags(repo, remote, "refs/tags/freeze/"+name+"/*")
		if err != nil {
			return nil, err
		}
		for tag, ref := range tags {
			if previous, ok := found[tag]; ok && previous.sha != ref.sha {
				return nil, fmt.Errorf("freeze tag %s differs between remotes %s and %s", tag, previous.remote, ref.remote)
			}
			found[tag] = ref
		}
	}
	if len(found) > 1 {
		return nil, fmt.Errorf("multiple active freezes for %s: %s", name, strings.Join(slices.Sorted(maps.Keys(found)), ", "))
	}
	for tag, ref := range found {
		return readFreezeTag(repo, ref, name, tag, target)
	}
	// No remote carries an active freeze tag for this target.
	return nil, errNoActiveFreeze
}

func readRemoteTag(repo *gitx.Repo, ref remoteTag) (string, error) {
	contents, err := readRemoteTags(repo, map[string][]remoteTag{"": {ref}})
	return contents[""], err
}

func readRemoteTags(repo *gitx.Repo, tags map[string][]remoteTag) (map[string]string, error) {
	byRemote := map[string]map[string]bool{}
	selected := map[string]remoteTag{}
	for tag, refs := range tags {
		for _, ref := range refs {
			if previous, ok := selected[tag]; ok && previous.sha != ref.sha {
				return nil, fmt.Errorf("tag %s differs between remotes %s and %s", tag, previous.remote, ref.remote)
			}
			selected[tag] = ref
			if byRemote[ref.remote] == nil {
				byRemote[ref.remote] = map[string]bool{}
			}
			byRemote[ref.remote][ref.sha] = true
		}
	}
	for remote, shas := range byRemote {
		args := []string{"fetch", "--no-write-fetch-head", "--quiet", remote}
		args = append(args, slices.Sorted(maps.Keys(shas))...)
		if _, err := repo.Git(args...); err != nil {
			return nil, fmt.Errorf("fetching tags from %s: %w", remote, err)
		}
	}
	contents := make(map[string]string, len(selected))
	for tag, ref := range selected {
		body, err := repo.Git("cat-file", "tag", ref.sha)
		if err != nil {
			return nil, fmt.Errorf("reading tag %s: %w", tag, err)
		}
		contents[tag] = body
	}
	return contents, nil
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
	for otherName, other := range r.cfg.Deploy.Targets {
		if otherName == target || other.Freeze == nil || other.Requires != r.target.Requires {
			continue
		}
		_, err := activeTargetFreeze(r.repo, r.cfg, otherName, other)
		if errors.Is(err, errNoActiveFreeze) {
			continue
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("cannot freeze %s while %s is frozen over %s", target, otherName, r.target.Requires)
	}
	if err := deployFreezeTrunk(r.repo, r.cfg); err != nil {
		return err
	}
	return a.cutFreeze(ctx, r, target, versionName)
}

func (a *App) cutFreeze(ctx context.Context, r *deployRun, target, versionName string) (retErr error) {
	version, err := findFreezeVersion(ctx, r.cfg, versionName)
	if err != nil {
		return err
	}
	if version.ID == "" {
		return fmt.Errorf("Jira has no version %q", versionName)
	}
	short := strings.TrimPrefix(versionName, r.target.ApprovedRelease.Prefix)
	branch := strings.ReplaceAll(r.target.Freeze.Branch, "{version}", short)
	remotes, err := pushRemotes(r.repo, r.cfg)
	if err != nil {
		return err
	}
	previous, branchErr := r.repo.Git("symbolic-ref", "--short", "-q", "HEAD")
	if branchErr != nil && gitx.ExitCode(branchErr) != 1 {
		return branchErr
	}
	previous = strings.TrimSpace(previous)
	created := false
	defer func() {
		if retErr != nil {
			if previous != "" {
				_, restoreErr := r.repo.Git("switch", previous)
				retErr = errors.Join(retErr, restoreErr)
			}
			if created {
				_, deleteErr := r.repo.Git("branch", "-D", branch)
				retErr = errors.Join(retErr, deleteErr)
			}
		}
	}()
	created, err = checkoutFreezeBranch(r.repo, r.cfg.Deploy.Remote, branch)
	if err != nil {
		return err
	}
	if err := a.push(r.repo, remotes, []string{"refs/heads/" + branch}, false, true); err != nil {
		return fmt.Errorf("freeze is not set: pushing release branch %s failed: %w", branch, err)
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
	msg := fmt.Sprintf("version: %s\nbranch: %s\ntime: %s\nwho: %s\n", versionName, branch, now.Format(time.RFC3339Nano), strings.TrimSpace(who))
	if _, err := r.repo.Git("tag", "-d", tag); err != nil && gitx.ExitCode(err) != 1 {
		return err
	}
	if _, err := r.repo.Git("tag", "-a", tag, "-m", msg); err != nil {
		return err
	}
	if err := a.push(r.repo, remotes, []string{"refs/tags/" + tag}, false, true); err != nil {
		_, cleanupErr := cleanupRemoteFreezeTags(r.repo, remotes, tag)
		return errors.Join(fmt.Errorf("freeze is not set: pushing %s failed", tag), cleanupErr)
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

func checkoutFreezeBranch(repo *gitx.Repo, remote, branch string) (bool, error) {
	remoteRef := "refs/remotes/" + remote + "/" + branch
	remoteHeads, err := repo.Git("ls-remote", "--heads", remote, "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	remoteSHA := ""
	if line := strings.TrimSpace(remoteHeads); line != "" {
		remoteSHA, _, _ = strings.Cut(line, "\t")
	}
	if remoteSHA == "" {
		_, err := repo.Git("switch", "-c", branch, "HEAD")
		return err == nil, err
	}
	if _, err := repo.Git("fetch", "--quiet", remote, "refs/heads/"+branch+":"+remoteRef); err != nil {
		return false, err
	}
	localSHA, localErr := repo.Git("rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if localErr == nil && strings.TrimSpace(localSHA) != remoteSHA {
		return false, fmt.Errorf("local branch %s differs from remote %s; resolve it before freezing", branch, remote)
	}
	created := false
	if localErr != nil {
		if _, err := repo.Git("branch", branch, remoteRef); err != nil {
			return false, err
		}
		created = true
	}
	_, err = repo.Git("switch", branch)
	return created, err
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
		var journalRefs []string
		var freezeRefs []string
		remoteTags, err := listRemoteTags(repo, remote, "refs/tags/"+f.tag)
		if err != nil {
			return err
		}
		for tag := range remoteTags {
			freezeRefs = append(freezeRefs, ":refs/tags/"+tag)
		}
		for tag, remoteRefs := range journal {
			for _, ref := range remoteRefs {
				if ref.remote == remote {
					journalRefs = append(journalRefs, ":refs/tags/"+tag)
					break
				}
			}
		}
		if len(journalRefs) > 0 {
			args := append([]string{"push", remote}, journalRefs...)
			if _, err := repo.Git(args...); err != nil {
				return fmt.Errorf("could not remove freeze journal tags from %s; freeze %s remains active: %w", remote, f.tag, err)
			}
		}
		if len(freezeRefs) > 0 {
			args := append([]string{"push", remote}, freezeRefs...)
			if _, err := repo.Git(args...); err != nil {
				return fmt.Errorf("could not remove freeze tag %s from %s: %w", f.tag, remote, err)
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
	return a.writeFreezeLog(repo, cfg, f, commit, where, kind, reason, true, false)
}

func (a *App) pushPendingFreezeLogs(repo *gitx.Repo, cfg *config.Config, f *activeFreeze) error {
	pattern := "freeze-log/" + f.target + "/" + freezeSlug(f.version) + "/*"
	listed, err := repo.Git("tag", "--list", pattern)
	if err != nil {
		return err
	}
	remotes, err := pushRemotes(repo, cfg)
	if err != nil {
		return err
	}
	var errs []error
	for tag := range strings.SplitSeq(strings.TrimSpace(listed), "\n") {
		if tag == "" {
			continue
		}
		local, err := repo.Git("rev-parse", "refs/tags/"+tag)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		local = strings.TrimSpace(local)
		for _, remote := range remotes {
			found, err := listRemoteTags(repo, remote, "refs/tags/"+tag)
			if err != nil {
				errs = append(errs, fmt.Errorf("checking freeze journal tag %s on %s: %w", tag, remote, err))
				continue
			}
			if ref, ok := found[tag]; ok {
				if ref.sha != local {
					errs = append(errs, fmt.Errorf("freeze journal tag %s differs on %s", tag, remote))
				}
				continue
			}
			if err := a.push(repo, []string{remote}, []string{"refs/tags/" + tag}, false, true); err != nil {
				errs = append(errs, fmt.Errorf("publishing pending freeze journal tag %s to %s: %w", tag, remote, err))
			}
		}
	}
	return errors.Join(errs...)
}

func (a *App) commitAdvisoryLog(repo *gitx.Repo, cfg *config.Config, f *activeFreeze, commit, task string) (string, error) {
	return a.writeFreezeLog(repo, cfg, f, commit, "commit", "advisory", task, true, false)
}

func (a *App) commitAdvisoryLogLocal(repo *gitx.Repo, cfg *config.Config, f *activeFreeze, commit, task string) (string, error) {
	return a.writeFreezeLog(repo, cfg, f, commit, "commit", "advisory", task, true, true)
}

func (a *App) writeFreezeLog(repo *gitx.Repo, cfg *config.Config, f *activeFreeze, commit, where, kind, reason string, keepOnPushFailure, localOnly bool) (string, error) {
	now, err := deployNow()
	if err != nil {
		return "", err
	}
	base := fmt.Sprintf("freeze-log/%s/%s/%s-%s", f.target, freezeSlug(f.version), now.Format("20060102T150405"), gitx.Short(commit))
	remotes, err := pushRemotes(repo, cfg)
	if err != nil {
		return "", err
	}
	reason = strings.ReplaceAll(reason, "\r\n", "; ")
	reason = strings.ReplaceAll(reason, "\n", "; ")
	reason = strings.ReplaceAll(reason, "\r", "; ")
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
		if localOnly {
			return tag, nil
		}
		if err := a.push(repo, remotes, []string{"refs/tags/" + tag}, false, true); err != nil {
			if keepOnPushFailure {
				collision, _, collisionErr := remoteFreezeTagState(repo, remotes, tag)
				if collisionErr != nil {
					return tag, errors.Join(err, collisionErr)
				}
				if !collision {
					return tag, err
				}
				if _, deleteErr := repo.Git("tag", "-d", tag); deleteErr != nil {
					return tag, errors.Join(err, deleteErr)
				}
				continue
			}
			collision, cleanupErr := cleanupRemoteFreezeTags(repo, remotes, tag)
			if cleanupErr != nil {
				return tag, errors.Join(fmt.Errorf("freeze-log tag %s was not written", tag), cleanupErr)
			}
			if collision {
				continue
			}
			return tag, fmt.Errorf("freeze-log tag %s was not written", tag)
		}
		_, err = repo.Git("tag", "-d", tag)
		return tag, err
	}
}

func cleanupRemoteFreezeTags(repo *gitx.Repo, remotes []string, tag string) (bool, error) {
	collision, pushed, err := remoteFreezeTagState(repo, remotes, tag)
	if err != nil {
		return false, err
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

func remoteFreezeTagState(repo *gitx.Repo, remotes []string, tag string) (bool, []string, error) {
	localSHA, err := repo.Git("rev-parse", "refs/tags/"+tag)
	if err != nil {
		return false, nil, err
	}
	localSHA = strings.TrimSpace(localSHA)
	var collision bool
	var pushed []string
	for _, remote := range remotes {
		remoteTags, err := listRemoteTags(repo, remote, "refs/tags/"+tag)
		if err != nil {
			return false, nil, err
		}
		for _, ref := range remoteTags {
			if ref.sha == localSHA {
				pushed = append(pushed, remote)
			} else {
				collision = true
			}
		}
	}
	slices.Sort(pushed)
	return collision, pushed, nil
}

func remoteTagExists(repo *gitx.Repo, remotes []string, tag string) (bool, error) {
	for _, remote := range remotes {
		remoteTags, err := listRemoteTags(repo, remote, "refs/tags/"+tag)
		if err != nil {
			return false, err
		}
		if len(remoteTags) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func listRemoteTags(repo *gitx.Repo, remote, pattern string) (map[string]remoteTag, error) {
	out, err := repo.Git("ls-remote", "--tags", remote, pattern)
	if err != nil {
		return nil, err
	}
	return parseRemoteTags(out, remote), nil
}

func activeFreezeForRun(r *deployRun) (*activeFreeze, error) {
	var active *activeFreeze
	for _, name := range slices.Sorted(maps.Keys(r.cfg.Deploy.Targets)) {
		target := r.cfg.Deploy.Targets[name]
		if target.Freeze == nil || (name != r.name && target.Requires != r.name) {
			continue
		}
		freeze, err := activeTargetFreeze(r.repo, r.cfg, name, target)
		if errors.Is(err, errNoActiveFreeze) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if active != nil {
			return nil, fmt.Errorf("multiple active freezes apply to %s: %s and %s", r.name, active.target, freeze.target)
		}
		active = freeze
	}
	if active == nil {
		return nil, errNoActiveFreeze
	}
	return active, nil
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

func (a *App) warnFreezeAdvisory(f *activeFreeze, task string, err error) {
	fmt.Fprintf(a.Stderr, "graft: %s failed, advisory while %s is frozen: %v\n", task, f.name, err)
}

func (a *App) recordCommitAdvisory(repo *gitx.Repo, cfg *config.Config, f *activeFreeze, failed []string) error {
	head, err := repo.Git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	var errs []error
	for _, task := range failed {
		if tag, err := a.commitAdvisoryLog(repo, cfg, f, strings.TrimSpace(head), task); err != nil {
			var pushErr *PushError
			if errors.As(err, &pushErr) {
				var commands []string
				for _, remote := range pushErr.Failed {
					commands = append(commands, "git push "+remote+" refs/tags/"+tag)
				}
				errs = append(errs, fmt.Errorf("commit %s task %s: freeze journal lacks the advisory record; tag %s remains locally; recover with %s: %w", gitx.Short(strings.TrimSpace(head)), task, tag, strings.Join(commands, "; "), err))
			} else {
				errs = append(errs, fmt.Errorf("commit %s task %s: freeze journal lacks the advisory record in tag %s: %w", gitx.Short(strings.TrimSpace(head)), task, tag, err))
			}
		} else {
			a.printf("recorded freeze advisory in %s", tag)
		}
	}
	return errors.Join(errs...)
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
	commits, err := freezeCommits(r, f, prev)
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
	var refusals error
	for _, c := range commits {
		if err := checkFreezeCommit(ctx, r, f, g.gate, keyRe, c, reverted[c.sha]); err != nil {
			var refusal freezeRefusalError
			if !errors.As(err, &refusal) {
				return err
			}
			refusals = errors.Join(refusals, freezeRefusalError{fmt.Errorf("%s: %w", gitx.Short(c.sha), refusal)})
		}
	}
	if refusals != nil {
		return freezeRefusalError{refusals}
	}
	return nil
}

func freezeCommits(r *deployRun, f *activeFreeze, prev string) ([]deliveredCommit, error) {
	commits, err := deliveredCommits(r.repo, prev, r.head)
	if err != nil {
		return nil, err
	}
	bypassed, err := bypassedCommits(r.repo, r.cfg, f)
	if err != nil || len(bypassed) == 0 {
		return commits, err
	}
	args := []string{"log", "--format=%H", prev + ".." + r.head, "--not"}
	for sha := range bypassed {
		args = append(args, sha)
	}
	listed, err := r.repo.Git(args...)
	if err != nil {
		return nil, fmt.Errorf("listing commits not already bypassed: %w", err)
	}
	allowed := map[string]bool{}
	for line := range strings.SplitSeq(listed, "\n") {
		if sha := strings.TrimSpace(line); sha != "" {
			allowed[sha] = true
		}
	}
	return slices.DeleteFunc(commits, func(c deliveredCommit) bool { return !allowed[c.sha] }), nil
}

func bypassedCommits(repo *gitx.Repo, cfg *config.Config, f *activeFreeze) (map[string]bool, error) {
	tags, err := freezeLogTags(repo, cfg, f)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	bypassTags := map[string][]remoteTag{}
	for tag, refs := range tags {
		if !strings.HasSuffix(tag, "-bypass") {
			continue
		}
		bypassTags[tag] = refs
	}
	contents, err := readRemoteTags(repo, bypassTags)
	if err != nil {
		return nil, err
	}
	for _, body := range contents {
		for line := range strings.SplitSeq(body, "\n") {
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
		remoteTags, err := listRemoteTags(repo, remote, pattern)
		if err != nil {
			return nil, err
		}
		for name, ref := range remoteTags {
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
	var refusal error
	for _, key := range keys {
		if err := checkFreezeIssue(ctx, gate, f, key, &matched); err != nil {
			var rejected freezeRefusalError
			if !errors.As(err, &rejected) {
				return err
			}
			refusal = errors.Join(refusal, err)
		}
	}
	if refusal == nil && len(matched) > 0 {
		return nil
	}
	ignored, err := shipsNothing(r.repo, r.cfg, f.config.ApprovedRelease, commit.sha)
	if err != nil {
		return err
	}
	if ignored {
		return nil
	}
	if refusal != nil {
		return freezeRefusalError{refusal}
	}
	if len(matched) == 0 {
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
	versionID := issue.FixVersionID(f.name)
	if versionID == "" {
		return fmt.Errorf("%s: Jira did not return the id of fix version %q", key, f.name)
	}
	added, hasAddition, err := gate.fixVersionHistoryAfter(ctx, key, versionID, f.time)
	if err != nil {
		return err
	}
	if !hasAddition && issue.Created.After(f.time) {
		added = true
	}
	if !hasAddition && issue.Created.IsZero() {
		return fmt.Errorf("%s: created time is missing; cannot check when it joined %s", key, f.name)
	}
	if added && !slices.Contains(f.config.Freeze.Types, issue.Type) {
		return freezeRefusalError{fmt.Errorf("%s joined %s after the freeze", key, f.name)}
	}
	return nil
}
