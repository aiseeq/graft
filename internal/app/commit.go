package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/fsx"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/hooks"
	"github.com/aiseeq/graft/v2/internal/message"
	"github.com/aiseeq/graft/v2/internal/userconfig"
	"github.com/aiseeq/graft/v2/internal/version"
)

// CommitOptions are the arguments of graft commit.
type CommitOptions struct {
	Message message.Source
	Level   version.Level
}

// Commit is the one way a change enters the repository: gate, version bump,
// stage everything, content checks, commit with the work item key, push to
// every remote. It finishes a merge git left open the same way, once its
// conflicts are resolved. The whole run holds the repository lock.
func (a *App) Commit(ctx context.Context, opts CommitOptions) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	msg, err := message.Read(opts.Message)
	if err != nil {
		return err
	}
	uc, err := userconfig.CommitSettings()
	if err != nil {
		return err
	}
	if uc.Conventional {
		if err := message.CheckConventional(msg); err != nil {
			return err
		}
	}
	if opts.Level != version.Patch && cfg.Version.Mode != config.ModeFile {
		return fmt.Errorf("--%s needs version.mode %s; with %s, versions come from graft release", opts.Level, config.ModeFile, cfg.Version.Mode)
	}
	stale, err := hooks.Outdated(repo, cfg.Hooks.Dir)
	if err != nil {
		return err
	}
	if len(stale) > 0 {
		a.warn("the hooks are out of date (%s); run graft init", strings.Join(stale, "; "))
	}
	a.warnUnversioned(repo, cfg)
	err = a.withLock(ctx, commitLock(repo, cfg), cfg, "graft commit", func() error {
		plan, err := prepareCommit(repo, cfg, msg, opts.Level)
		if err != nil {
			return err
		}
		if err := a.raiseOwnModules(ctx, repo, cfg, plan.branch); err != nil {
			return err
		}
		return a.commit(ctx, repo, cfg, plan)
	})
	if err != nil {
		return err
	}
	a.dailyFlags(ctx, repo, cfg, "")
	a.dailySweep(ctx, repo, cfg)
	return nil
}

// commitPlan is everything a commit needs, worked out before the gate runs so
// a bad version file or an unknown remote fails fast.
type commitPlan struct {
	branch         string
	remotes        []string
	message        string
	bump           bumpPlan
	bumpBeforeGate bool
	// head is HEAD when the commit starts: what the gate is about to check the
	// work on.
	head headState
	// release marks the commit graft release makes: it changes only the
	// version files and needs no work item under ticket.required.
	release bool
}

func prepareCommit(repo *gitx.Repo, cfg *config.Config, msg string, level version.Level) (commitPlan, error) {
	var plan commitPlan
	merge, err := mergeOrNormalContext(repo)
	if err != nil {
		return plan, err
	}
	branch, err := repo.Branch()
	if err != nil {
		return plan, err
	}
	remotes, err := pushRemotes(repo, cfg)
	if err != nil {
		return plan, err
	}
	clean, err := repo.IsClean()
	if err != nil {
		return plan, err
	}
	if clean && !merge {
		return plan, errNothingToCommit
	}
	bump, err := planBump(repo, cfg, level, branch)
	if err != nil {
		return plan, err
	}
	if cfg.Ticket != nil && !config.BranchMatches(cfg.Ticket.Skip, branch) {
		msg = message.WithKey(msg, message.BranchKey(branch, cfg.Ticket.Regexp))
	}
	return commitPlan{branch: branch, remotes: remotes, message: msg, bump: bump}, nil
}

// requireTicket refuses, under ticket.required, a commit that names no work
// item: a revert and a commit that changes only files that ship nothing pass.
// The commit's files are the index against base, HEAD when it is "".
func requireTicket(repo *gitx.Repo, cfg *config.Config, msg, base string) error {
	return requireTicketWithVersion(repo, cfg, msg, base, false)
}

func requireTicketWithVersion(repo *gitx.Repo, cfg *config.Config, msg, base string, releaseVersion bool) error {
	if !cfg.Ticket.Required || hasTicketKey(cfg, msg) {
		return nil
	}
	if subject, _, _ := strings.Cut(msg, "\n"); isRevert(subject) {
		return nil
	}
	changed, err := repo.StagedFiles(base)
	if err != nil {
		return err
	}
	versionFiles := make(map[string]bool)
	if releaseVersion {
		for _, target := range version.Targets(cfg.Version) {
			versionFiles[target.Path] = true
		}
	}
	versionOnly := releaseVersion && len(changed) > 0
	for _, path := range changed {
		if !versionFiles[path] {
			versionOnly = false
			break
		}
	}
	if cfg.ShipsNothing(changed) || versionOnly {
		return nil
	}
	return fmt.Errorf("the commit names no work item (ticket.required): put a key matching %s in the message or commit from a branch named for the item; only a revert and changes ignored by every approved_release target need none", cfg.Ticket.Pattern)
}

// hasTicketKey tells whether some whole word of msg is a work item key; with
// release_notes.jira.project_keys, a KEY-123 key counts only for those projects.
func hasTicketKey(cfg *config.Config, msg string) bool {
	if cfg.Ticket == nil || cfg.Ticket.Regexp == nil {
		return false
	}
	return slices.ContainsFunc(message.WordKeys(msg, cfg.Ticket.Regexp), func(key string) bool {
		if cfg.ReleaseNotes == nil || cfg.ReleaseNotes.Jira == nil || len(cfg.ReleaseNotes.Jira.ProjectKeys) == 0 {
			return true
		}
		prefix, _, ok := strings.Cut(key, "-")
		return !ok || !uppercaseProjectPrefix(prefix) || slices.Contains(cfg.ReleaseNotes.Jira.ProjectKeys, prefix)
	})
}

func uppercaseProjectPrefix(prefix string) bool {
	if prefix == "" {
		return false
	}
	for _, r := range prefix {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func (a *App) commit(ctx context.Context, repo *gitx.Repo, cfg *config.Config, plan commitPlan) error {
	var err error
	if plan.head, err = currentHead(repo); err != nil {
		return err
	}
	backup, frozen, advisoryFailed, err := a.prepareCommitGate(ctx, repo, cfg, plan)
	if err != nil {
		return err
	}
	undo := func(cause error) error { return errors.Join(cause, restoreVersion(repo, backup, plan.head)) }
	headMoved := func() error {
		err := checkHeadStayed(repo, plan.head, commitAgain)
		if err == nil {
			return nil
		}
		return errors.Join(undo(err), restageOverHead(repo))
	}

	// The rest of the tree is staged already: only the bump is new.
	if paths := backup.Paths(); len(paths) > 0 {
		if _, err := repo.Git(append([]string{"add", "--"}, paths...)...); err != nil {
			return undo(err)
		}
	}
	// HEAD first: a move while they ran makes the checks say the merged files
	// are taken back.
	checksErr := runChecks(ctx, repo, cfg, plan.message, "HEAD", a.Stderr)
	if err := headMoved(); err != nil {
		return err
	}
	if checksErr != nil {
		return undo(checksErr)
	}
	msg, err := a.withWorkers(repo, cfg, plan.message)
	if err != nil {
		return undo(err)
	}
	if err := headMoved(); err != nil {
		return err
	}
	if _, err := repo.GitInput(strings.NewReader(msg), markerEnv, "commit", "--cleanup=verbatim", "-F", "-"); err != nil {
		return undo(err)
	}
	return a.finishCommit(repo, cfg, plan, frozen, advisoryFailed)
}

func (a *App) prepareCommitGate(ctx context.Context, repo *gitx.Repo, cfg *config.Config, plan commitPlan) (*version.Backup, *activeFreeze, []string, error) {
	index, start, err := stageCommitStart(repo, "")
	if err != nil {
		return nil, nil, nil, err
	}
	var backup *version.Backup
	if plan.bumpBeforeGate {
		backup, err = plan.bump.apply(repo)
		if err != nil {
			return nil, nil, nil, errors.Join(err, restoreIndex(index))
		}
		if paths := backup.Paths(); len(paths) > 0 {
			if _, err := repo.Git(append([]string{"add", "--"}, paths...)...); err != nil {
				return nil, nil, nil, errors.Join(err, restoreVersion(repo, backup, plan.head), restoreIndex(index))
			}
			start, err = saveCommitStart(repo, "")
			if err != nil {
				return nil, nil, nil, errors.Join(err, restoreVersion(repo, backup, plan.head), restoreIndex(index))
			}
		}
	}
	restoreBeforeGate := func(cause error) error {
		return errors.Join(cause, restoreVersion(repo, backup, plan.head), restoreIndex(index))
	}
	frozen, advisoryFailed, err := a.gateForMessage(ctx, repo, cfg, plan.branch, plan.message)
	if err != nil {
		return nil, nil, nil, restoreBeforeGate(err)
	}
	if err := a.stageGateChanges(repo, start, "commit", plan.head, commitAgain); err != nil {
		return nil, nil, nil, restoreBeforeGate(err)
	}
	empty, err := gateLeftHead(repo)
	if err != nil {
		return nil, nil, nil, restoreBeforeGate(err)
	}
	if empty {
		return nil, nil, nil, restoreBeforeGate(errGateLeftHead)
	}
	if cfg.Ticket != nil && !config.BranchMatches(cfg.Ticket.Skip, plan.branch) {
		if err := requireTicketWithVersion(repo, cfg, plan.message, "", plan.release); err != nil {
			return nil, nil, nil, restoreBeforeGate(err)
		}
	}
	if backup == nil {
		backup, err = plan.bump.apply(repo)
		if err != nil {
			return nil, nil, nil, errors.Join(err, restoreIndex(index))
		}
	}
	return backup, frozen, advisoryFailed, nil
}

func (a *App) finishCommit(repo *gitx.Repo, cfg *config.Config, plan commitPlan, frozen *activeFreeze, advisoryFailed []string) error {
	refs := []string{"HEAD"}
	var postCommitErrs []error
	if plan.bump.tag != "" {
		if _, err := repo.Git("tag", "-a", plan.bump.tag, "-m", plan.bump.tag); err != nil {
			postCommitErrs = append(postCommitErrs, fmt.Errorf("commit made, but tagging it failed: %w", err))
		} else {
			refs = append(refs, "refs/tags/"+plan.bump.tag)
		}
	}
	head, err := repo.Git("rev-parse", "--short", "HEAD")
	if err != nil {
		postCommitErrs = append(postCommitErrs, fmt.Errorf("commit made, but reading it back failed: %w", err))
	} else {
		a.printf("committed %s on %s%s", strings.TrimSpace(head), plan.branch, plan.bump.describe())
	}
	if err := a.pushCommit(repo, cfg, plan, refs); err != nil {
		postCommitErrs = append(postCommitErrs, err)
	}
	if err := a.recordCommitAdvisoryAfterPush(repo, cfg, frozen, advisoryFailed); err != nil {
		postCommitErrs = append(postCommitErrs, err)
	}
	return errors.Join(postCommitErrs...)
}

// pushCommit pushes the commit and its tag. A release pushes the branch
// first and its tag only once every remote took the branch: a tag must not
// name a commit no remote branch has. When the branch push fails, the local
// tag goes too, and the version file of the release commit stays for a
// graft release without a step once the branch is up to date.
func (a *App) pushCommit(repo *gitx.Repo, cfg *config.Config, plan commitPlan, refs []string) error {
	lease, noFollow := config.BranchMatches(cfg.Push.Lease, plan.branch), hasConfiguredFreeze(cfg)
	if !plan.release || len(refs) < 2 {
		return a.push(repo, plan.remotes, refs, lease, noFollow)
	}
	if err := a.push(repo, plan.remotes, refs[:1], lease, noFollow); err != nil {
		if _, delErr := repo.Git("tag", "-d", plan.bump.tag); delErr != nil {
			return errors.Join(err, fmt.Errorf("removing the local tag %s: %w", plan.bump.tag, delErr))
		}
		return fmt.Errorf("%w\nthe release commit is made here, tag %s is not: bring %s up to date with its remote, push it, then run graft release to tag the committed version", err, plan.bump.tag, plan.branch)
	}
	return a.push(repo, plan.remotes, refs[1:], false, noFollow)
}

func (a *App) recordCommitAdvisoryAfterPush(repo *gitx.Repo, cfg *config.Config, frozen *activeFreeze, failed []string) error {
	if frozen == nil {
		return nil
	}
	if err := a.recordCommitAdvisory(repo, cfg, frozen, failed); err != nil {
		return fmt.Errorf("commit is made, but the freeze journal lacks the advisory record: %w", err)
	}
	return nil
}

// gateLeftHead says whether the staged tree is HEAD's: the gate put every
// path the commit started with back as HEAD has it. Without HEAD there is
// nothing to compare with.
func gateLeftHead(repo *gitx.Repo) (bool, error) {
	headTree, err := repo.Git("rev-parse", "--verify", "--quiet", "HEAD^{tree}")
	if gitx.ExitCode(err) == 1 {
		// No HEAD, or a HEAD without its tree, which is an error.
		hasHead, hasErr := repo.HasHead()
		if hasErr != nil || !hasHead {
			return false, hasErr
		}
	}
	if err != nil {
		return false, fmt.Errorf("reading HEAD tree after the gate: %w", err)
	}
	return stagedIs(repo, strings.TrimSpace(headTree))
}

// stagedIs says whether the staged tree is tree.
func stagedIs(repo *gitx.Repo, tree string) (bool, error) {
	staged, err := repo.Git("write-tree")
	if err != nil {
		return false, fmt.Errorf("writing the staged tree after the gate: %w", err)
	}
	return strings.TrimSpace(staged) == tree, nil
}

type savedIndex struct {
	repo   *gitx.Repo
	path   string
	data   []byte
	exists bool
	head   headState
}

func saveIndex(repo *gitx.Repo) (savedIndex, error) {
	s := savedIndex{repo: repo, path: filepath.Join(repo.GitDir, "index")}
	var err error
	if s.head, err = currentHead(repo); err != nil {
		return s, err
	}
	data, err := os.ReadFile(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return s, fmt.Errorf("saving the git index: %w", err)
	}
	if err == nil {
		s.data, s.exists = data, true
	}
	return s, nil
}

// restoreIndex puts the index back as it was saved. Over a HEAD that moved
// since, as a merge beside graft moves it, the saved index would stage the
// undoing of what came in: the work is staged on the new HEAD instead.
func restoreIndex(s savedIndex) error {
	if s.path == "" {
		return nil
	}
	head, err := currentHead(s.repo)
	if err != nil {
		return fmt.Errorf("restoring the git index: %w", err)
	}
	if head != s.head {
		return restageOverHead(s.repo)
	}
	if !s.exists {
		if err := fsx.Remove(s.path); err != nil {
			return fmt.Errorf("restoring the git index: %w", err)
		}
		return nil
	}
	if err := os.WriteFile(s.path, s.data, 0o644); err != nil {
		return fmt.Errorf("restoring the git index: %w", err)
	}
	return nil
}

// headState is where HEAD is: its branch, "" when detached, and its commit,
// "" on an unborn branch.
type headState struct {
	branch string
	sha    string
}

func (h headState) String() string {
	sha := "no commit"
	if h.sha != "" {
		sha = gitx.Short(h.sha)
	}
	if h.branch == "" {
		return sha
	}
	return sha + " on " + h.branch
}

// currentHead reads HEAD, a few times in every commit: one git for its commit
// and its branch, which fails only on an unborn branch or a broken repository,
// the cases headOfUnusual tells apart.
func currentHead(repo *gitx.Repo) (headState, error) {
	out, err := repo.Git("rev-parse", "HEAD^{commit}", "--symbolic-full-name", "HEAD")
	if gitx.ExitCode(err) == 128 {
		return headOfUnusual(repo)
	}
	if err != nil {
		return headState{}, fmt.Errorf("reading HEAD: %w", err)
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return headState{}, fmt.Errorf("reading HEAD: git rev-parse gave %q, want a commit and a ref", out)
	}
	h := headState{sha: fields[0]}
	if fields[1] != "HEAD" {
		h.branch = strings.TrimPrefix(fields[1], "refs/heads/")
	}
	return h, nil
}

// headOfUnusual is currentHead where git could not read both at once.
func headOfUnusual(repo *gitx.Repo) (headState, error) {
	var h headState
	branch, err := repo.Branch()
	if err != nil && !errors.Is(err, gitx.ErrDetachedHead) {
		return h, fmt.Errorf("reading the branch of HEAD: %w", err)
	}
	h.branch = branch
	sha, err := repo.Git("rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if gitx.ExitCode(err) == 1 {
		return h, nil
	}
	if err != nil {
		return h, fmt.Errorf("reading HEAD: %w", err)
	}
	h.sha = strings.TrimSpace(sha)
	return h, nil
}

// What to do after a HEAD that moved. An amend run again would take the commit
// that came in for its own, so it does not say to run it again.
const (
	commitAgain = "this command committed nothing, and what the work tree still holds is staged on HEAD; look at HEAD and the tree, and run the command again if the work belongs there"
	amendAgain  = "this command amended nothing, and the last commit is no longer the one it was for; what the work tree still holds is staged on HEAD; look at HEAD and the tree, and make that a commit of its own with graft commit unless the last commit is yours to amend"
)

// checkHeadStayed refuses a commit whose HEAD is no longer the one its gate
// ran on: the commit would be built from the old HEAD's tree, and the merged
// or committed work would go back with it.
func checkHeadStayed(repo *gitx.Repo, start headState, next string) error {
	now, err := currentHead(repo)
	if err != nil {
		return err
	}
	if now == start {
		return nil
	}
	return fmt.Errorf("HEAD moved from %s to %s while the gate ran, by something beside graft: %s", start, now, next)
}

// restageOverHead makes the index the new HEAD with the work tree staged on
// it, which holds the work the moved HEAD did not take.
func restageOverHead(repo *gitx.Repo) error {
	head, err := currentHead(repo)
	if err != nil {
		return fmt.Errorf("restaging over the moved HEAD: %w", err)
	}
	args := []string{"read-tree", "HEAD"}
	if head.sha == "" {
		args = []string{"read-tree", "--empty"}
	}
	if _, err := repo.Git(args...); err != nil {
		return fmt.Errorf("restaging over the moved HEAD: %w", err)
	}
	if _, err := repo.Git("add", "-A"); err != nil {
		return fmt.Errorf("restaging over the moved HEAD: %w", err)
	}
	return nil
}

type commitStart struct {
	tree  string
	paths map[string]bool
}

// stageCommitStart stages the initial work tree and records what the gate may update.
func stageCommitStart(repo *gitx.Repo, base string) (savedIndex, commitStart, error) {
	index, err := saveIndex(repo)
	if err != nil {
		return index, commitStart{}, err
	}
	if _, err := repo.Git("add", "-A"); err != nil {
		return index, commitStart{}, errors.Join(fmt.Errorf("staging the work tree before the gate: %w", err), restoreIndex(index))
	}
	start, err := saveCommitStart(repo, base)
	if err != nil {
		return index, commitStart{}, errors.Join(err, restoreIndex(index))
	}
	return index, start, nil
}

// saveCommitStart records the staged tree and paths changed from base before the gate.
func saveCommitStart(repo *gitx.Repo, base string) (commitStart, error) {
	start := commitStart{paths: make(map[string]bool)}
	tree, err := repo.Git("write-tree")
	if err != nil {
		return start, fmt.Errorf("writing the staged tree before the gate: %w", err)
	}
	start.tree = strings.TrimSpace(tree)

	var out string
	if base != "" {
		out, err = repo.Git("diff-tree", "-r", "--name-only", "--no-renames", "-z", base, start.tree)
	} else {
		hasHead, headErr := repo.HasHead()
		if headErr != nil {
			return start, fmt.Errorf("checking HEAD before the gate: %w", headErr)
		}
		if hasHead {
			out, err = repo.Git("diff-tree", "-r", "--name-only", "--no-renames", "-z", "HEAD", start.tree)
		} else {
			out, err = repo.Git("ls-files", "-z")
		}
	}
	if err != nil {
		return start, fmt.Errorf("listing paths staged before the gate: %w", err)
	}
	for path := range strings.SplitSeq(out, "\x00") {
		if path != "" {
			start.paths[path] = true
		}
	}
	return start, nil
}

// stageGateChanges stages only paths already changed when the gate began.
// A HEAD that moved by then stops it, again checked after the listing: from
// there on a merge adds nothing to what the listing calls left out.
func (a *App) stageGateChanges(repo *gitx.Repo, start commitStart, command string, head headState, again string) error {
	after, err := repo.WorkTreeTree()
	if err != nil {
		return fmt.Errorf("writing the work tree tree after the gate: %w", err)
	}
	out, err := repo.Git("diff-tree", "-r", "--name-only", "--no-renames", "-z", start.tree, after)
	if err != nil {
		return fmt.Errorf("listing paths changed by the gate: %w", err)
	}
	if err := checkHeadStayed(repo, head, again); err != nil {
		return err
	}
	var kept []string
	for path := range strings.SplitSeq(out, "\x00") {
		if path == "" {
			continue
		}
		if start.paths[path] {
			a.printf("%s: %s changed while the gate ran (a gate step or another writer); committed as the gate left it", command, path)
			kept = append(kept, path)
		} else {
			a.printf("%s left out %s: changed only while the gate ran, by another writer or a gate step; commit it once its writer is done", command, path)
		}
	}
	// The index goes back to what the commit started with: a git add, mv or
	// rm another writer ran while the gate ran is left out as its paths are.
	if _, err := repo.Git("read-tree", start.tree); err != nil {
		return fmt.Errorf("restoring the index the gate started with: %w", err)
	}
	if len(kept) == 0 {
		return nil
	}
	keptSet := make(map[string]bool, len(kept))
	for _, path := range kept {
		keptSet[path] = true
	}
	entries, err := repo.Git("ls-tree", "-r", "-z", "--full-tree", after)
	if err != nil {
		return fmt.Errorf("listing work tree entries changed by the gate: %w", err)
	}
	var input strings.Builder
	seen := make(map[string]bool, len(kept))
	for record := range strings.SplitSeq(entries, "\x00") {
		if record == "" {
			continue
		}
		header, path, ok := strings.Cut(record, "\t")
		if !ok {
			return fmt.Errorf("parsing work tree entry changed by the gate: missing path separator")
		}
		if !keptSet[path] {
			continue
		}
		fields := strings.Fields(header)
		if len(fields) != 3 {
			return fmt.Errorf("parsing work tree entry changed by the gate: malformed entry")
		}
		fmt.Fprintf(&input, "%s %s 0\t%s\x00", fields[0], fields[2], path)
		seen[path] = true
	}
	for _, path := range kept {
		if !seen[path] {
			fmt.Fprintf(&input, "0 0000000000000000000000000000000000000000 0\t%s\x00", path)
		}
	}
	if _, err := repo.GitInput(strings.NewReader(input.String()), nil, "update-index", "-z", "--index-info"); err != nil {
		return fmt.Errorf("staging paths changed by the gate: %w", err)
	}
	return nil
}

// bumpPlan is the version change a commit will make.
type bumpPlan struct {
	cfg  config.Version
	next *version.Semver
	tag  string
	// skipped is the branch whose commits leave the version alone.
	skipped string
}

func planBump(repo *gitx.Repo, cfg *config.Config, level version.Level, branch string) (bumpPlan, error) {
	plan := bumpPlan{cfg: cfg.Version}
	if cfg.Version.Mode != config.ModeFile {
		return plan, nil
	}
	if config.BranchMatches(cfg.Version.Skip, branch) {
		if level != version.Patch {
			return plan, fmt.Errorf("--%s on %s: version.skip_branches leaves the version alone there; use graft release --%s on the branch the work is merged into", level, branch, level)
		}
		plan.skipped = branch
		return plan, nil
	}
	current, err := version.Current(repo.Root, cfg.Version)
	if err != nil {
		return plan, fmt.Errorf("reading the version: %w", err)
	}
	var next version.Semver
	switch {
	case config.BranchMatches(cfg.Version.Build, branch):
		if level != version.Patch {
			return plan, fmt.Errorf("--%s on %s: version.build_branches count builds in the fourth part there; bump on the trunk", level, branch)
		}
		next = current.BumpBuild()
	case current.Build > 0:
		return plan, fmt.Errorf("%s is %s, a build_branches version, on %s: a merge took it from a hotfix or release branch; put back the version %s had before the merge", cfg.Version.File, current, branch, branch)
	default:
		next = current.Bump(level)
	}
	plan.next = &next
	if cfg.Version.TagOnCommit {
		plan.tag = cfg.Version.TagPrefix + next.String()
		exists, err := tagExists(repo, plan.tag)
		if err != nil {
			return plan, err
		}
		if exists {
			return plan, fmt.Errorf("tag %s already exists: the version file is behind the tags", plan.tag)
		}
	}
	return plan, nil
}

// apply writes the new version; without a bump the backup is empty.
func (p bumpPlan) apply(repo *gitx.Repo) (*version.Backup, error) {
	if p.next == nil {
		return &version.Backup{}, nil
	}
	return version.Write(repo.Root, p.cfg, *p.next)
}

func (p bumpPlan) describe() string {
	if p.skipped != "" {
		return ", version untouched on " + p.skipped + " (version.skip_branches)"
	}
	if p.next == nil {
		return ""
	}
	return ", version " + p.next.String()
}

// restoreVersion puts the version files back as they were and stages them
// that way, so a failed commit leaves no bump behind for the next one to stack
// on. Over a HEAD that moved since start, a file that was as start had it
// comes back as the new HEAD has it: the old content would take the move back.
func restoreVersion(repo *gitx.Repo, backup *version.Backup, start headState) error {
	if backup == nil {
		return nil
	}
	if len(backup.Paths()) == 0 {
		return nil
	}
	now, err := currentHead(repo)
	if err != nil {
		return fmt.Errorf("restoring version files: %w", err)
	}
	// A failed rebase leaves the backup as saved: the bump goes back all the same.
	var rebaseErr error
	if now.sha != start.sha && now.sha != "" && start.sha != "" {
		if err := rebaseVersionFiles(repo, backup, start.sha, now.sha); err != nil {
			rebaseErr = fmt.Errorf("following the moved HEAD with the version files: %w", err)
		}
	}
	paths := backup.Paths()
	if len(paths) == 0 {
		return rebaseErr
	}
	if err := backup.Restore(); err != nil {
		return errors.Join(rebaseErr, fmt.Errorf("restoring version files: %w", err))
	}
	if _, err := repo.Git(append([]string{"add", "--"}, paths...)...); err != nil {
		return errors.Join(rebaseErr, fmt.Errorf("re-staging restored version files: %w", err))
	}
	return rebaseErr
}

// rebaseVersionFiles carries the backup from one commit to another. Commits
// and work tree are compared through git's own filters: under core.autocrlf a
// file the commit has unchanged differs from it in its bytes.
func rebaseVersionFiles(repo *gitx.Repo, backup *version.Backup, from, to string) error {
	blob := func(rev, path string) (string, error) {
		id, err := repo.Git("rev-parse", "--verify", "-q", rev+":"+filepath.ToSlash(path))
		if gitx.ExitCode(err) == 1 {
			return "", nil
		}
		return strings.TrimSpace(id), err
	}
	return backup.Rebase(func(path string, saved []byte) (bool, error) {
		old, err := blob(from, path)
		if err != nil || old == "" {
			return false, err
		}
		id, err := repo.GitInput(bytes.NewReader(saved), nil, "hash-object", "--stdin", "--path="+filepath.ToSlash(path))
		if err != nil {
			return false, err
		}
		return strings.TrimSpace(id) == old, nil
	}, func(path string) ([]byte, bool, error) {
		next, err := blob(to, path)
		if err != nil || next == "" {
			return nil, false, err
		}
		content, err := repo.Git("cat-file", "--filters", to+":"+filepath.ToSlash(path))
		if err != nil {
			return nil, false, err
		}
		return []byte(content), true, nil
	})
}

func tagExists(repo *gitx.Repo, tag string) (bool, error) {
	return repo.RefExists("refs/tags/" + tag)
}

// warnUnversioned warns on a project with no version: mode none without a
// reason for it, or git-tag mode before its first version tag.
func (a *App) warnUnversioned(repo *gitx.Repo, cfg *config.Config) {
	switch cfg.Version.Mode {
	case config.ModeNone:
		if strings.TrimSpace(cfg.Version.Reason) == "" {
			a.warn("the project has no version (version.mode %s): pick %s or %s, or say why in version.reason", config.ModeNone, config.ModeGitTag, config.ModeFile)
		}
	case config.ModeGitTag:
		_, found, err := latestTag(repo, cfg.Version.TagPrefix)
		switch {
		case err != nil:
			// A warning only: the tags it cannot read are graft release's
			// to refuse, not the commit's.
			a.warn("cannot tell the project's version from its %s* tags: %v", cfg.Version.TagPrefix, err)
		case !found:
			a.warn("the project has no version yet: no %s* version tag reachable from HEAD; graft release tags the first, %s%s", cfg.Version.TagPrefix, cfg.Version.TagPrefix, InitialRelease)
		}
	}
}
