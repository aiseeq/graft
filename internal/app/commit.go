package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
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
	err = a.withLock(ctx, commitLock(repo, cfg), cfg, "graft commit", func() error {
		plan, err := prepareCommit(repo, cfg, msg, opts.Level)
		if err != nil {
			return err
		}
		if err := a.raiseOwnModules(ctx, repo); err != nil {
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
	branch  string
	remotes []string
	message string
	bump    bumpPlan
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
	if cfg.ShipsNothing(changed) {
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
	index, err := saveIndex(repo)
	if err != nil {
		return err
	}
	if _, err := repo.Git("add", "-A"); err != nil {
		return errors.Join(err, restoreIndex(repo, index))
	}
	if err := a.gateForMessage(ctx, repo, cfg, plan.message); err != nil {
		return errors.Join(err, restoreIndex(repo, index))
	}
	if _, err := repo.Git("add", "-A"); err != nil {
		return errors.Join(err, restoreIndex(repo, index))
	}
	if cfg.Ticket != nil && !config.BranchMatches(cfg.Ticket.Skip, plan.branch) {
		if err := requireTicket(repo, cfg, plan.message, ""); err != nil {
			return errors.Join(err, restoreIndex(repo, index))
		}
	}
	backup, err := plan.bump.apply(repo)
	if err != nil {
		return errors.Join(err, restoreIndex(repo, index))
	}
	undo := func(cause error) error { return errors.Join(cause, restoreVersion(repo, backup)) }

	// The rest of the tree is staged already: only the bump is new.
	if paths := backup.Paths(); len(paths) > 0 {
		if _, err := repo.Git(append([]string{"add", "--"}, paths...)...); err != nil {
			return undo(err)
		}
	}
	if err := runChecks(repo, cfg, plan.message, "HEAD", a.Stderr); err != nil {
		return undo(err)
	}
	msg, err := a.withWorkers(repo, plan.message)
	if err != nil {
		return undo(err)
	}
	if _, err := repo.GitInput(strings.NewReader(msg), markerEnv, "commit", "--cleanup=verbatim", "-F", "-"); err != nil {
		return undo(err)
	}
	refs := []string{"HEAD"}
	if plan.bump.tag != "" {
		if _, err := repo.Git("tag", "-a", plan.bump.tag, "-m", plan.bump.tag); err != nil {
			return fmt.Errorf("commit made, but tagging it failed: %w", err)
		}
		refs = append(refs, "refs/tags/"+plan.bump.tag)
	}
	head, err := repo.Git("rev-parse", "--short", "HEAD")
	if err != nil {
		return errors.Join(fmt.Errorf("commit made, but reading it back failed: %w", err), a.push(repo, plan.remotes, refs, config.BranchMatches(cfg.Push.Lease, plan.branch)))
	}
	a.printf("committed %s on %s%s", strings.TrimSpace(head), plan.branch, plan.bump.describe())
	return a.push(repo, plan.remotes, refs, config.BranchMatches(cfg.Push.Lease, plan.branch))
}

type savedIndex struct {
	path   string
	data   []byte
	exists bool
}

func saveIndex(repo *gitx.Repo) (savedIndex, error) {
	s := savedIndex{path: filepath.Join(repo.GitDir, "index")}
	data, err := os.ReadFile(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return s, fmt.Errorf("saving the git index: %w", err)
	}
	if err == nil {
		s.data, s.exists = data, true
	}
	return s, nil
}

func restoreIndex(repo *gitx.Repo, s savedIndex) error {
	if s.path == "" {
		return nil
	}
	if !s.exists {
		if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("restoring the git index: %w", err)
		}
		return nil
	}
	if err := os.WriteFile(s.path, s.data, 0o644); err != nil {
		return fmt.Errorf("restoring the git index: %w", err)
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
			return plan, fmt.Errorf("--%s on %s: version.skip_branches leaves the version alone there; bump on the branch the work is merged into", level, branch)
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
// on.
func restoreVersion(repo *gitx.Repo, backup *version.Backup) error {
	paths := backup.Paths()
	if len(paths) == 0 {
		return nil
	}
	if err := backup.Restore(); err != nil {
		return fmt.Errorf("restoring version files: %w", err)
	}
	if _, err := repo.Git(append([]string{"add", "--"}, paths...)...); err != nil {
		return fmt.Errorf("re-staging restored version files: %w", err)
	}
	return nil
}

func tagExists(repo *gitx.Repo, tag string) (bool, error) {
	return repo.RefExists("refs/tags/" + tag)
}
