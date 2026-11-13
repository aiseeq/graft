package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/checks"
	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/gomods"
	"github.com/aiseeq/graft/v2/internal/hooks"
	commitmessage "github.com/aiseeq/graft/v2/internal/message"
	"github.com/aiseeq/graft/v2/internal/npmmods"
	"github.com/aiseeq/graft/v2/internal/userconfig"
	"github.com/aiseeq/graft/v2/internal/version"
)

// Amend folds the work tree into the last commit through the same gate and
// checks as a new commit, keeping its message. It does not push: the commit it
// rewrites is unpublished, or published only on branches that are rewritten by
// design (push.force_with_lease).
func (a *App) Amend(ctx context.Context) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	return a.withLock(ctx, commitLock(repo, cfg), cfg, "graft amend", func() error {
		if err := checkAmendable(repo, cfg); err != nil {
			return err
		}
		message, ticketed, branch, err := amendMessage(repo, cfg)
		if err != nil {
			return err
		}
		base, err := repo.ParentTree("HEAD")
		if err != nil {
			return err
		}
		index, start, err := stageCommitStart(repo, base)
		if err != nil {
			return err
		}
		frozen, advisoryFailed, err := a.gateForMessage(ctx, repo, cfg, branch, message)
		if err != nil {
			return errors.Join(err, restoreIndex(index))
		}
		if err := a.stageGateChanges(repo, start, "amend", index.head, amendAgain); err != nil {
			return errors.Join(err, restoreIndex(index))
		}
		if empty, err := stagedIs(repo, base); err != nil || empty {
			if err == nil {
				err = errGateEmptiedAmend
			}
			return errors.Join(err, restoreIndex(index))
		}
		if ticketed {
			// The amended commit is checked whole, against its parent.
			if err := requireTicket(repo, cfg, message, base); err != nil {
				return errors.Join(err, restoreIndex(index))
			}
		}
		// The amended commit follows HEAD's parent: undoing part of HEAD is
		// no revert.
		parent, err := repo.Parent("HEAD")
		if err != nil {
			return errors.Join(err, restoreIndex(index))
		}
		// HEAD first: a move while they ran makes the checks say the merged
		// files are taken back.
		checksErr := runChecks(ctx, repo, cfg, message, parent, a.Stderr)
		if err := checkHeadStayed(repo, index.head, amendAgain); err != nil {
			return errors.Join(err, restoreIndex(index))
		}
		if checksErr != nil {
			return errors.Join(checksErr, restoreIndex(index))
		}
		if message, err = a.withWorkers(repo, cfg, message); err != nil {
			return errors.Join(err, restoreIndex(index))
		}
		if err := checkHeadStayed(repo, index.head, amendAgain); err != nil {
			return errors.Join(err, restoreIndex(index))
		}
		previousHead, err := repo.Git("rev-parse", "HEAD")
		if err != nil {
			return errors.Join(err, restoreIndex(index))
		}
		return a.finishAmend(repo, cfg, message, frozen, strings.TrimSpace(previousHead), advisoryFailed, index)
	})
}

func (a *App) finishAmend(repo *gitx.Repo, cfg *config.Config, message string, frozen *activeFreeze, previousHead string, advisoryFailed []string, index savedIndex) error {
	if _, err := repo.GitInput(strings.NewReader(message), markerEnv, "commit", "--amend", "--cleanup=verbatim", "-F", "-"); err != nil {
		return errors.Join(err, restoreIndex(index))
	}
	if frozen != nil {
		if err := a.replaceAmendAdvisoryRecord(repo, cfg, frozen, previousHead, advisoryFailed); err != nil {
			return fmt.Errorf("amend is made, but %w", err)
		}
	}
	head, err := repo.Git("rev-parse", "--short", "HEAD")
	if err != nil {
		return err
	}
	a.printf("amended the last commit, now %s (not pushed)", strings.TrimSpace(head))
	return nil
}

// replaceAmendAdvisoryRecord records the amended commit's advisory entry
// here and takes the replaced commit's entries away. The new entry is made
// whatever the removal meets: a remote that does not answer leaves the old
// entries there, not the amend without one.
func (a *App) replaceAmendAdvisoryRecord(repo *gitx.Repo, cfg *config.Config, frozen *activeFreeze, previousHead string, failed []string) error {
	checks := strings.Join(failed, ", ")
	removeErr := removeCommitAdvisoryLogs(repo, cfg, frozen, previousHead)
	if err := a.recordCommitAdvisoryLocal(repo, cfg, frozen, failed); err != nil {
		if removeErr != nil {
			err = errors.Join(err, fmt.Errorf("removing the replaced commit's: %w", removeErr))
		}
		return fmt.Errorf("the freeze advisory record for %s failed: %w", checks, err)
	}
	if removeErr != nil {
		return fmt.Errorf("the freeze advisory record for %s is made, while the replaced commit's could not be removed: %w", checks, removeErr)
	}
	return nil
}

func (a *App) recordCommitAdvisoryLocal(repo *gitx.Repo, cfg *config.Config, frozen *activeFreeze, failed []string) error {
	if frozen == nil || len(failed) == 0 {
		return nil
	}
	head, err := repo.Git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	remotes, err := pushRemotes(repo, cfg)
	if err != nil {
		return err
	}
	for _, task := range failed {
		tag, err := a.commitAdvisoryLogLocal(repo, cfg, frozen, strings.TrimSpace(head), task)
		if err != nil {
			var localErr *localFreezeLogError
			if !errors.As(err, &localErr) {
				return fmt.Errorf("amended commit %s task %s: freeze journal record could not be made: %w", gitx.Short(strings.TrimSpace(head)), task, err)
			}
			tag = localErr.tag
			a.printf("warning: advisory record %s is local because checking its remotes failed: %v", tag, localErr.err)
		}
		var commands []string
		for _, remote := range remotes {
			commands = append(commands, "git push "+remote+" refs/tags/"+tag)
		}
		a.printf("advisory record %s is local; push the amended commit, then run: %s", tag, strings.Join(commands, "; "))
	}
	return nil
}

// amendMessage is the message of the amended commit: HEAD's, with the key of
// the branch's ticket when the branch is ticketed.
func amendMessage(repo *gitx.Repo, cfg *config.Config) (message string, ticketed bool, branch string, err error) {
	message, err = repo.Git("log", "-1", "--format=%B", "HEAD")
	if err != nil {
		return "", false, "", err
	}
	// log appends a newline of its own after the message's.
	message = strings.TrimRight(message, "\n") + "\n"
	branch, err = repo.Branch()
	if err != nil {
		return "", false, "", err
	}
	ticketed = cfg.Ticket != nil && !config.BranchMatches(cfg.Ticket.Skip, branch)
	if ticketed {
		message = commitmessage.WithKey(message, commitmessage.BranchKey(branch, cfg.Ticket.Regexp))
	}
	return message, ticketed, branch, nil
}

// checkAmendable refuses to amend a missing commit, one with nothing to fold
// in, or one published on a branch outside push.force_with_lease.
func checkAmendable(repo *gitx.Repo, cfg *config.Config) error {
	if err := requireNormalContext(repo); err != nil {
		return err
	}
	hasHead, err := repo.HasHead()
	if err != nil {
		return err
	}
	if !hasHead {
		return errors.New("there is no commit to amend yet: use graft commit")
	}
	published, err := repo.RemoteBranchesContaining("HEAD")
	if err != nil {
		return err
	}
	// Branches in push.force_with_lease are rewritten by design (rebased
	// work branches); history anywhere else stays as published.
	var kept []string
	for _, b := range published {
		if !config.BranchMatches(cfg.Push.Lease, b.Branch) {
			kept = append(kept, b.Remote+"/"+b.Branch)
		}
	}
	if len(kept) > 0 {
		return fmt.Errorf("the last commit is already on %s: amending it would rewrite published history; make a new commit instead: graft commit -m \"fix: ...\"", strings.Join(kept, ", "))
	}
	clean, err := repo.IsClean()
	if err != nil {
		return err
	}
	if clean {
		return errors.New("nothing to amend: the work tree is clean")
	}
	return nil
}

// ReleaseOptions are the arguments of graft release.
type ReleaseOptions struct {
	Level version.Level
	Patch bool
	// Version, when set, is the exact version to release (git-tag mode).
	Version string
}

// InitialRelease is the version graft release tags in git-tag mode when the
// history has no version tag yet.
var InitialRelease = version.Semver{Major: 0, Minor: 1, Patch: 0}

// Release tags a published HEAD with the project version and pushes the tag.
// HEAD must already be on every remote (graft commit pushed it, maybe on
// another branch); on a branch, that branch is pushed first, so a branch
// fast-forwarded to pushed work goes out before its tag.
// In file mode the version is the committed version file; in git-tag mode it
// is the next version after the latest tag.
func (a *App) Release(ctx context.Context, opts ReleaseOptions) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	return a.withLock(ctx, repoLock(repo), cfg, "graft release", func() error {
		err := releaseAuthorsThen(repo, cfg, func() error {
			return a.releaseLocked(ctx, repo, cfg, opts)
		})
		if err == nil {
			a.warnInstallFreshness(ctx, repo, cfg)
		}
		return err
	})
}

func releaseAuthorsThen(repo *gitx.Repo, cfg *config.Config, run func() error) error {
	if err := checkReleaseAuthors(repo, cfg); err != nil {
		return err
	}
	return run()
}

func (a *App) releaseLocked(ctx context.Context, repo *gitx.Repo, cfg *config.Config, opts ReleaseOptions) error {
	if cfg.Version.Mode == config.ModeFile && opts.Version != "" {
		return errors.New("in version.mode file release takes no --version")
	}
	if cfg.Version.Mode == config.ModeFile && (opts.Level != version.Patch || opts.Patch) {
		return a.releaseFileCommit(ctx, repo, cfg, opts)
	}
	next, err := releaseVersion(repo, cfg, opts)
	if err != nil {
		return err
	}
	tag := cfg.Version.TagPrefix + next.String()
	exists, err := tagExists(repo, tag)
	if err != nil {
		return err
	}
	if exists {
		if cfg.Version.Mode == config.ModeFile {
			released, err := releasedAs(repo, cfg.Version.TagPrefix)
			if err != nil {
				return err
			}
			if released != "" {
				return fmt.Errorf("HEAD is already released as %s: commit the change first", released)
			}
			return fmt.Errorf("tag %s already exists on an earlier commit: use graft release --patch, --minor or --major for the next version", tag)
		}
		return fmt.Errorf("tag %s already exists", tag)
	}
	remotes, err := pushRemotes(repo, cfg)
	if err != nil {
		return err
	}
	if err := requirePublished(repo, remotes); err != nil {
		return err
	}
	branch, err := repo.Branch()
	if err != nil && !errors.Is(err, gitx.ErrDetachedHead) {
		return err
	}
	if err == nil {
		if err := a.pushBehindBranch(repo, remotes, branch, hasConfiguredFreeze(cfg)); err != nil {
			return err
		}
	}
	if _, err := repo.Git("tag", "-a", tag, "-m", tag); err != nil {
		return err
	}
	a.printf("tagged %s", tag)
	return a.push(repo, remotes, []string{"refs/tags/" + tag}, false, hasConfiguredFreeze(cfg))
}

type releaseCommit struct {
	sha       string
	author    string
	committer string
}

type releaseAuthorViolation struct {
	sha     string
	role    string
	address string
}

func releaseAuthorViolations(commits []releaseCommit, allowed []string) []releaseAuthorViolation {
	var violations []releaseAuthorViolation
	for _, commit := range commits {
		for _, candidate := range []struct{ role, address string }{
			{"author", commit.author}, {"committer", commit.committer},
		} {
			permitted := false
			for _, address := range allowed {
				if strings.EqualFold(candidate.address, address) {
					permitted = true
					break
				}
			}
			if !permitted {
				violations = append(violations, releaseAuthorViolation{commit.sha, candidate.role, candidate.address})
			}
		}
	}
	return violations
}

// releaseAuthorLog is the git log of the commits no reachable version tag
// holds: the tags go as ^refs/tags/<tag> lines on stdin, since a project
// with thousands of them would pass the Windows command line limit as
// arguments.
func releaseAuthorLog(tags []string) (args []string, input string) {
	var excluded strings.Builder
	for _, tag := range tags {
		excluded.WriteString("^refs/tags/" + tag + "\n")
	}
	return []string{"log", "--format=%H%x00%ae%x00%ce", "--stdin", "HEAD"}, excluded.String()
}

func checkReleaseAuthors(repo *gitx.Repo, cfg *config.Config) error {
	if cfg.Release == nil || !cfg.Release.Authors.Set {
		return nil
	}
	tags, _, _, err := versionTags(repo, cfg.Version.TagPrefix)
	if err != nil {
		return err
	}
	args, input := releaseAuthorLog(tags)
	out, err := repo.GitInput(strings.NewReader(input), nil, args...)
	if err != nil {
		return fmt.Errorf("read commits for release.authors: %w", err)
	}
	var commits []releaseCommit
	for line := range strings.SplitSeq(strings.TrimSuffix(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\x00")
		if len(fields) != 3 {
			return fmt.Errorf("read commits for release.authors: unexpected git log output")
		}
		commits = append(commits, releaseCommit{sha: fields[0], author: fields[1], committer: fields[2]})
	}
	violations := releaseAuthorViolations(commits, cfg.Release.Authors.Addresses)
	if len(violations) == 0 {
		return nil
	}
	var message strings.Builder
	message.WriteString("release refused: unreleased commits have e-mail addresses outside release.authors:")
	for _, violation := range violations {
		fmt.Fprintf(&message, "\n  %s %s %s", gitx.Short(violation.sha), violation.role, violation.address)
	}
	message.WriteString("\nrewrite the author or add the address to release.authors")
	return errors.New(message.String())
}

// pushBehindBranch pushes branch to the remotes whose copy of it lags behind
// HEAD, so the tag does not point past the branch there. A failed push leaves
// no tag; git's output above it tells a rejection (the remote branch moved on
// since this clone last fetched it) from a network or access failure.
func (a *App) pushBehindBranch(repo *gitx.Repo, remotes []string, branch string, noFollowTags bool) error {
	behind, err := remotesBehind(repo, remotes, branch)
	if err != nil || len(behind) == 0 {
		return err
	}
	err = a.push(repo, behind, []string{"refs/heads/" + branch}, false, noFollowTags)
	var pushErr *PushError
	if errors.As(err, &pushErr) {
		return fmt.Errorf("branch %s was not pushed to %s, no tag is made; if the remote rejected it, git fetch, bring the branch up to date, and run graft release again",
			branch, strings.Join(pushErr.Failed, ", "))
	}
	return err
}

// remotesBehind are the remotes whose branch lacks HEAD. A remote without the
// branch is left alone: release does not create branches there, and the tagged
// commit is on it already.
func remotesBehind(repo *gitx.Repo, remotes []string, branch string) ([]string, error) {
	var behind []string
	for _, r := range remotes {
		tip, err := remoteBranchTip(repo, r, branch)
		if err != nil {
			return nil, err
		}
		if tip == "" {
			continue
		}
		known, err := repo.RefExists(tip + "^{commit}")
		if err != nil {
			return nil, err
		}
		if known {
			has, err := repo.IsAncestor("HEAD", tip)
			if err != nil {
				return nil, err
			}
			if has {
				continue
			}
		}
		// A tip this clone has not fetched is not known to hold HEAD: the push
		// either fast-forwards it or is rejected, and no tag is made then.
		behind = append(behind, r)
	}
	return behind, nil
}

// remoteBranchTip is the commit of branch on remote: this clone's copy of it,
// or, when the clone has none (the branch was never fetched from there), the
// remote's own answer; "" when the remote has no such branch.
func remoteBranchTip(repo *gitx.Repo, remote, branch string) (string, error) {
	ref := "refs/remotes/" + remote + "/" + branch
	exists, err := repo.RefExists(ref)
	if err != nil {
		return "", err
	}
	if exists {
		return ref, nil
	}
	out, err := repo.Git("ls-remote", remote, "refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("look up branch %s on %s: %w", branch, remote, err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], nil
}

// requirePublished refuses to tag a commit the remotes do not have yet.
func requirePublished(repo *gitx.Repo, remotes []string) error {
	published, err := repo.RemotesContaining("HEAD")
	if err != nil {
		return err
	}
	var missing []string
	for _, r := range remotes {
		if !slices.Contains(published, r) {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the commit to tag is not on %s yet: push it first (graft commit pushes it), then release", strings.Join(missing, ", "))
	}
	return nil
}

// fileVersionAt is the version the version file holds in rev.
func fileVersionAt(repo *gitx.Repo, cfg *config.Config, rev string) (version.Semver, error) {
	content, err := repo.ShowFile(rev, cfg.Version.File)
	if err != nil {
		return version.Semver{}, err
	}
	raw, err := version.Targets(cfg.Version)[0].Extract(content)
	if err != nil {
		return version.Semver{}, err
	}
	return version.ParseFile(raw)
}

// versionAt is the project version of commit rev: its version file, or the
// version tags before it.
func versionAt(repo *gitx.Repo, cfg *config.Config, rev string) (string, error) {
	switch cfg.Version.Mode {
	case config.ModeFile:
		v, err := fileVersionAt(repo, cfg, rev)
		if err != nil {
			return "", err
		}
		return v.String(), nil
	case config.ModeGitTag:
		out, err := repo.Git("describe", "--tags", "--match", cfg.Version.TagPrefix+"*", rev)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(out), nil
	default:
		return "", fmt.Errorf("version.mode is %s: the project has no version", cfg.Version.Mode)
	}
}

func releaseVersion(repo *gitx.Repo, cfg *config.Config, opts ReleaseOptions) (version.Semver, error) {
	switch cfg.Version.Mode {
	case config.ModeFile:
		if opts.Version != "" {
			return version.Semver{}, errors.New("in version.mode file release takes no --version")
		}
		if opts.Level != version.Patch || opts.Patch {
			return version.Semver{}, errors.New("in version.mode file a release step needs a commit")
		}
		next, err := fileVersionAt(repo, cfg, "HEAD")
		if err != nil {
			return version.Semver{}, err
		}
		return next, nil
	case config.ModeGitTag:
		latest, found, err := latestTag(repo, cfg.Version.TagPrefix)
		if err != nil {
			return version.Semver{}, err
		}
		if opts.Version != "" {
			if opts.Level != version.Patch {
				return version.Semver{}, errors.New("--version and --minor/--major exclude each other")
			}
			want, err := version.Parse(opts.Version)
			if err != nil {
				return version.Semver{}, err
			}
			if found && !latest.Less(want) {
				return version.Semver{}, fmt.Errorf("--version %s is not above the latest tag %s%s", want, cfg.Version.TagPrefix, latest)
			}
			return want, nil
		}
		if !found {
			return InitialRelease, nil
		}
		if err := requireUnreleased(repo, cfg.Version.TagPrefix); err != nil {
			return version.Semver{}, err
		}
		return latest.Bump(opts.Level), nil
	default:
		return version.Semver{}, fmt.Errorf("version.mode %s has no releases", cfg.Version.Mode)
	}
}

// releaseFileCommit makes the version bump and release tag part of one commit
// transaction. The repository lock stays held through the commit and push.
func (a *App) releaseFileCommit(ctx context.Context, repo *gitx.Repo, cfg *config.Config, opts ReleaseOptions) error {
	if commitLock(repo, cfg) == repoLock(repo) {
		return a.releaseFileCommitLocked(ctx, repo, cfg, opts)
	}
	return a.withLock(ctx, commitLock(repo, cfg), cfg, "graft release commit", func() error {
		return a.releaseFileCommitLocked(ctx, repo, cfg, opts)
	})
}

func (a *App) releaseFileCommitLocked(ctx context.Context, repo *gitx.Repo, cfg *config.Config, opts ReleaseOptions) error {
	// A merge, cherry-pick or revert git left open would make the release
	// commit finish it: its parents, author and changes are not a release's.
	if err := requireNormalContext(repo); err != nil {
		return err
	}
	branch, err := repo.Branch()
	if err != nil {
		return err
	}
	if config.BranchMatches(cfg.Version.Skip, branch) {
		return fmt.Errorf("cannot release on %s: version.skip_branches leaves the version alone there; release on the branch the work is merged into", branch)
	}
	if config.BranchMatches(cfg.Version.Build, branch) {
		return fmt.Errorf("cannot release on %s: version.build_branches count builds in the fourth part there; release on the trunk", branch)
	}
	clean, err := repo.IsClean()
	if err != nil {
		return err
	}
	if !clean {
		return errors.New("the work tree is dirty; run graft commit first")
	}
	released, err := releasedAs(repo, cfg.Version.TagPrefix)
	if err != nil {
		return err
	}
	if released != "" {
		return fmt.Errorf("HEAD is already released as %s: commit the change first; a new version of the same code ships nothing new", released)
	}
	plan, err := planBump(repo, cfg, opts.Level, branch)
	if err != nil {
		return err
	}
	if plan.next == nil {
		return errors.New("release needs a version bump")
	}
	tag := cfg.Version.TagPrefix + plan.next.String()
	exists, err := tagExists(repo, tag)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("tag %s already exists", tag)
	}
	remotes, err := pushRemotes(repo, cfg)
	if err != nil {
		return err
	}
	if err := requireBranchCurrent(repo, remotes, branch); err != nil {
		return err
	}
	plan.tag = tag
	msg := "chore(release): " + tag
	if cfg.Ticket != nil && !config.BranchMatches(cfg.Ticket.Skip, branch) {
		msg = commitmessage.WithKey(msg, commitmessage.BranchKey(branch, cfg.Ticket.Regexp))
	}
	// The release commit carries the version files only: it ships the code
	// that was committed and gated, so its own modules are not raised here
	// (the next graft commit raises them), and a refusal leaves nothing behind.
	commit := commitPlan{branch: branch, remotes: remotes, message: msg, bump: plan, bumpBeforeGate: true, release: true}
	return a.commit(ctx, repo, cfg, commit)
}

// requireBranchCurrent refuses a release over a branch some remote has moved
// past: the release commit would not reach it, and its tag would name a
// commit no remote branch holds. The remotes are asked now, not this clone's
// copies of their branches.
func requireBranchCurrent(repo *gitx.Repo, remotes []string, branch string) error {
	for _, r := range remotes {
		out, err := repo.Git("ls-remote", r, "refs/heads/"+branch)
		if err != nil {
			return fmt.Errorf("look up branch %s on %s: %w", branch, r, err)
		}
		fields := strings.Fields(out)
		if len(fields) == 0 {
			continue
		}
		tip := fields[0]
		known, err := repo.RefExists(tip + "^{commit}")
		if err != nil {
			return err
		}
		if known {
			has, err := repo.IsAncestor(tip, "HEAD")
			if err != nil {
				return err
			}
			if has {
				continue
			}
		}
		return fmt.Errorf("%s on %s is at %.7s, which HEAD does not contain: bring the branch up to date (git pull), then release", branch, r, tip)
	}
	return nil
}

// requireUnreleased refuses a release of a commit that already carries a
// version tag: release run again after a commit that failed would publish
// the same code under a new version.
func requireUnreleased(repo *gitx.Repo, prefix string) error {
	released, err := releasedAs(repo, prefix)
	if err != nil {
		return err
	}
	if released != "" {
		return fmt.Errorf("HEAD is already released as %s: commit the change first, or pass --version to tag the same commit again", released)
	}
	return nil
}

// releasedAs is the version tag HEAD carries already, "" for none.
func releasedAs(repo *gitx.Repo, prefix string) (string, error) {
	out, err := repo.Git("tag", "--points-at", "HEAD", "--list", prefix+"*")
	if err != nil {
		return "", err
	}
	for tag := range strings.FieldsSeq(out) {
		if version.IsPlain(strings.TrimPrefix(tag, prefix)) {
			return tag, nil
		}
	}
	return "", nil
}

// warnInstallFreshness checks installed task binaries after a successful
// commit, release or merge. A failed advisory check must not change the command result.
func (a *App) warnInstallFreshness(ctx context.Context, repo *gitx.Repo, cfg *config.Config) {
	reference, err := installFreshnessReference(repo, cfg)
	if err != nil {
		a.warn("cannot check installed binary freshness: %v", err)
		return
	}
	for _, warning := range a.runner(repo, cfg).InstallFreshnessWarnings(ctx, reference) {
		a.warn("%s", warning)
	}
}

// installFreshnessReference is the latest reachable version tag in git-tag
// mode, otherwise HEAD. Before the first version tag, HEAD is the only useful
// reference available.
func installFreshnessReference(repo *gitx.Repo, cfg *config.Config) (string, error) {
	ref := "HEAD"
	if cfg.Version.Mode == config.ModeGitTag {
		latest, found, err := latestTag(repo, cfg.Version.TagPrefix)
		if err != nil {
			return "", err
		}
		if found {
			ref = cfg.Version.TagPrefix + latest.String()
		}
	}
	out, err := repo.Git("rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolving reference %s: %w", ref, err)
	}
	return strings.TrimSpace(out), nil
}

// latestTag finds the highest MAJOR.MINOR.PATCH tag reachable from HEAD.
// Tags that are not plain versions (v1.0.0-rc1, other prefixes) are ignored:
// graft never creates them.
func latestTag(repo *gitx.Repo, prefix string) (version.Semver, bool, error) {
	_, best, found, err := versionTags(repo, prefix)
	return best, found, err
}

func versionTags(repo *gitx.Repo, prefix string) ([]string, version.Semver, bool, error) {
	hasHead, err := repo.HasHead()
	if err != nil || !hasHead {
		return nil, version.Semver{}, false, err
	}
	out, err := repo.Git("tag", "--list", "--merged", "HEAD", prefix+"*")
	if err != nil {
		return nil, version.Semver{}, false, err
	}
	var (
		tags  []string
		best  version.Semver
		found bool
	)
	for tag := range strings.FieldsSeq(out) {
		raw := strings.TrimPrefix(tag, prefix)
		if !version.IsPlain(raw) {
			continue
		}
		v, err := version.Parse(raw)
		if err != nil {
			return nil, version.Semver{}, false, fmt.Errorf("tag %s: %w", tag, err)
		}
		tags = append(tags, tag)
		if !found || best.Less(v) {
			best, found = v, true
		}
	}
	return tags, best, found, nil
}

// BumpVersion raises the version in the version file and its synced files
// without committing: for scripts that fold the bump into a commit they make
// themselves, such as merging a work branch whose commits skipped it.
func (a *App) BumpVersion(ctx context.Context, level version.Level) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	if cfg.Version.Mode != config.ModeFile {
		return fmt.Errorf("version bump needs version.mode %s; with %s there is no version file", config.ModeFile, cfg.Version.Mode)
	}
	branch, err := repo.Branch()
	if err != nil {
		return err
	}
	return a.withLock(ctx, commitLock(repo, cfg), cfg, "graft version bump", func() error {
		plan, err := planBump(repo, cfg, level, branch)
		if err != nil {
			return err
		}
		if plan.skipped != "" {
			return fmt.Errorf("%s is in version.skip_branches: its commits leave the version alone; bump on the branch the work is merged into with graft release --minor", branch)
		}
		current, err := version.Current(repo.Root, cfg.Version)
		if err != nil {
			return err
		}
		backup, err := plan.apply(repo)
		if err != nil {
			return err
		}
		a.printf("version %s -> %s (%s)", current, plan.next, strings.Join(backup.Paths(), ", "))
		return nil
	})
}

// Version prints the project version.
func (a *App) Version(describe bool) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	if describe {
		v, err := describeVersion(repo, cfg)
		if err != nil {
			return err
		}
		fmt.Fprintln(a.Stdout, v)
		return nil
	}
	v, err := projectVersion(repo, cfg)
	if errors.Is(err, errNoVersionTags) {
		head, headErr := repo.Git("rev-parse", "--short", "HEAD")
		if headErr != nil {
			return fmt.Errorf("no version tags and no commits yet: %w", headErr)
		}
		fmt.Fprintf(a.Stdout, "no version tags yet (HEAD %s)\n", strings.TrimSpace(head))
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(a.Stdout, v)
	return nil
}

var (
	errNoVersionTags = errors.New("no version tags yet")
	errNoCommits     = errors.New("the repository has no commits yet")
)

// describeVersion identifies the build of the work tree for stamping into
// binaries: the version file, or git describe from the highest version tag,
// with -dirty for uncommitted changes to tracked files. Without a version
// (mode none, or no tag yet) it reads like a describe of a v0.0.0 tag at the
// root: v0.0.0-<commits>-g<hash>.
func describeVersion(repo *gitx.Repo, cfg *config.Config) (string, error) {
	hasHead, err := repo.HasHead()
	if err != nil {
		return "", err
	}
	if !hasHead {
		return "", errNoCommits
	}
	dirty, err := trackedChanges(repo)
	if err != nil {
		return "", err
	}
	suffix := ""
	if dirty {
		suffix = "-dirty"
	}
	v, err := projectVersion(repo, cfg)
	switch {
	case err == nil && cfg.Version.Mode == config.ModeFile:
		return v + suffix, nil
	case err == nil:
		return v, nil // git describe --dirty has added the suffix
	case !errors.Is(err, errNoVersionTags) && cfg.Version.Mode != config.ModeNone:
		return "", err
	}
	count, err := repo.Git("rev-list", "--count", "HEAD")
	if err != nil {
		return "", err
	}
	short, err := repo.Git("rev-parse", "--short=7", "HEAD")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s0.0.0-%s-g%s%s", cfg.Version.TagPrefix, strings.TrimSpace(count), strings.TrimSpace(short), suffix), nil
}

// trackedChanges reports uncommitted changes to tracked files, the way git
// describe --dirty sees them.
func trackedChanges(repo *gitx.Repo) (bool, error) {
	out, err := repo.Git("status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// projectVersion is the version of the work tree: the version file, or git
// describe from the highest version tag.
func projectVersion(repo *gitx.Repo, cfg *config.Config) (string, error) {
	switch cfg.Version.Mode {
	case config.ModeFile:
		v, err := version.Current(repo.Root, cfg.Version)
		if err != nil {
			return "", err
		}
		return v.String(), nil
	case config.ModeGitTag:
		latest, found, err := latestTag(repo, cfg.Version.TagPrefix)
		if err != nil {
			return "", err
		}
		if !found {
			return "", errNoVersionTags
		}
		// Describe from the highest version tag by name: with several tags on
		// one commit, git describe alone may pick a lower one.
		out, err := repo.Git("describe", "--tags", "--match", cfg.Version.TagPrefix+latest.String(), "--dirty")
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(out), nil
	default:
		return "", fmt.Errorf("version.mode is %s: the project has no version", cfg.Version.Mode)
	}
}

// Init installs the hooks and the pinned tools that fail their check.
func (a *App) Init(ctx context.Context) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	changes, err := hooks.Install(repo, cfg.Hooks.Dir)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		a.printf("hooks already installed in %s, nothing changed", cfg.Hooks.Dir)
	}
	for _, c := range changes {
		a.printf("%s", c.What)
	}
	if err := a.initDotEnv(repo, cfg); err != nil {
		return err
	}
	if len(cfg.Tools) == 0 {
		return nil
	}
	return a.checkTools(ctx, cfg, true)
}

// InitGlobal installs the user's Git-wide pre-push leak hook.
func (a *App) InitGlobal() error {
	changes, err := hooks.InstallGlobal()
	if err != nil {
		return err
	}
	runs, version, err := hooks.GitRunsConfigHooks()
	if err != nil {
		return err
	}
	if !runs {
		a.warn("git %s runs no hooks from its config: global hooks need git %d.%d or newer", version, hooks.GlobalHooksSince[0], hooks.GlobalHooksSince[1])
	}
	if len(changes) == 0 {
		a.printf("global hooks already installed, nothing changed")
		return nil
	}
	for _, change := range changes {
		a.printf("%s", change.What)
	}
	return nil
}

// initDotEnv copies dotenv_template to the dotenv file, readable by the owner
// only, when the dotenv file does not exist; an existing one is never touched.
func (a *App) initDotEnv(repo *gitx.Repo, cfg *config.Config) error {
	if cfg.DotEnvTemplate == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(repo.Root, filepath.FromSlash(cfg.DotEnvTemplate)))
	if err != nil {
		return fmt.Errorf("dotenv_template: %w", err)
	}
	path := filepath.Join(repo.Root, filepath.FromSlash(cfg.DotEnv))
	// O_EXCL: a file that appears meanwhile is not overwritten either.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		a.printf("%s exists, %s not copied", cfg.DotEnv, cfg.DotEnvTemplate)
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating %s: %w", cfg.DotEnv, err)
	}
	_, err = f.Write(data)
	if err = errors.Join(err, f.Close()); err != nil {
		// A partial file would keep the next init from copying again.
		return errors.Join(fmt.Errorf("writing %s: %w", cfg.DotEnv, err), os.Remove(path))
	}
	a.printf("created %s from %s: fill in its values", cfg.DotEnv, cfg.DotEnvTemplate)
	return nil
}

// Check runs the staged content checks on demand.
func (a *App) Check(ctx context.Context) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	if err := runChecks(ctx, repo, cfg, "", "HEAD", a.Stderr); err != nil {
		return err
	}
	a.printf("checks passed")
	return nil
}

// CheckSecrets scans the staged text for secrets alone. It needs no
// .graft.yaml, so any repository's pre-commit hook can call it; a config, when
// there is one, gives its exceptions and extra patterns.
func (a *App) CheckSecrets() error {
	repo, err := gitx.Open(a.Dir)
	if err != nil {
		return err
	}
	var secrets config.Secrets
	cfg, err := config.Load(repo.Root)
	switch {
	case err == nil:
		secrets = cfg.Checks.Secrets
	case !errors.Is(err, config.ErrNotFound):
		return err
	}
	staged, err := checks.Staged(repo)
	if err != nil {
		return err
	}
	if findings := checks.Secrets(staged, secrets); len(findings) > 0 {
		return &ChecksError{Findings: findings}
	}
	a.printf("no secrets in the staged files")
	return nil
}

// Gate runs the gate on demand.
func (a *App) Gate(ctx context.Context) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	// A detached HEAD names no branch to hold the modules on: they are raised
	// as on any work branch.
	branch, err := repo.Branch()
	if err != nil && !errors.Is(err, gitx.ErrDetachedHead) {
		return err
	}
	if err := a.raiseOwnModules(ctx, repo, cfg, branch); err != nil {
		return err
	}
	if err := a.gate(ctx, repo, cfg); err != nil {
		return err
	}
	a.printf("gate passed")
	return nil
}

// raiseOwnModules raises the modules of the user's modules.latest to their
// latest versions in the work tree, before a commit or a gate checks it.
// A hotfix or release branch keeps the modules it has: it carries a candidate
// someone approved, and a raise there would ship a change outside the
// approval; the trunk takes the raise.
func (a *App) raiseOwnModules(ctx context.Context, repo *gitx.Repo, cfg *config.Config, branch string) error {
	settings, err := userconfig.ModulesSettings()
	if err != nil || len(settings.Latest) == 0 {
		return err
	}
	warnPinnedTools(cfg.Tools, settings, a.warn)
	if held := modulesHeldBy(cfg, branch); held != "" {
		a.printf("modules.latest: %s keeps its module versions (%s); the trunk raises them", branch, held)
		return nil
	}
	if err := gomods.Raise(ctx, repo, settings.IsLatest, a.Stderr, a.warn); err != nil {
		return err
	}
	return npmmods.Raise(ctx, repo, settings.IsLatest, a.Stderr, a.warn)
}

// modulesHeldBy names the setting that makes branch a hotfix or release
// branch, or is empty.
func modulesHeldBy(cfg *config.Config, branch string) string {
	switch {
	case branch == "":
		return ""
	case cfg.Deploy != nil && cfg.Deploy.IsHotfix(branch):
		return "deploy.hotfix.branches"
	case config.BranchMatches(cfg.Version.Build, branch):
		return "version.build_branches"
	}
	return ""
}

// warnPinnedTools warns once for each go_install tool pinned to a module in
// modules.latest. Pin parses the version using the config's shared logic.
func warnPinnedTools(toolsByName map[string]*config.Tool, modules userconfig.Modules, warn func(string, ...any)) {
	for _, name := range slices.Sorted(maps.Keys(toolsByName)) {
		if tool := toolsByName[name]; tool != nil {
			if module := modules.PinnedOwnTool(*tool); module != "" {
				warn("tool %s pins our module %s; remove @vX… from go_install in .graft.yaml; modules.latest keeps our tools at the latest version", name, module)
			}
		}
	}
}
