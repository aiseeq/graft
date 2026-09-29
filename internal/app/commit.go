package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/gitx"
	"github.com/aiseeq/graft/internal/message"
	"github.com/aiseeq/graft/internal/version"
)

// CommitOptions are the arguments of graft commit.
type CommitOptions struct {
	Message message.Source
	Level   version.Level
}

// Commit is the one way a change enters the repository: gate, version bump,
// stage everything, content checks, commit with the work item key, push to
// every remote. The whole run holds the repository lock.
func (a *App) Commit(ctx context.Context, opts CommitOptions) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	msg, err := message.Read(opts.Message)
	if err != nil {
		return err
	}
	if opts.Level != version.Patch && cfg.Version.Mode != config.ModeFile {
		return fmt.Errorf("--%s needs version.mode %s; with %s, versions come from graft release", opts.Level, config.ModeFile, cfg.Version.Mode)
	}
	return a.withLock(ctx, repo, cfg, "graft commit", func() error {
		plan, err := prepareCommit(repo, cfg, msg, opts.Level)
		if err != nil {
			return err
		}
		return a.commit(ctx, repo, cfg, plan)
	})
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
	if err := requireNormalContext(repo); err != nil {
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
	if clean {
		return plan, errNothingToCommit
	}
	bump, err := planBump(repo, cfg, level)
	if err != nil {
		return plan, err
	}
	var key string
	if cfg.Ticket != nil {
		key = message.BranchKey(branch, cfg.Ticket.Regexp)
	}
	return commitPlan{branch: branch, remotes: remotes, message: message.WithKey(msg, key), bump: bump}, nil
}

func (a *App) commit(ctx context.Context, repo *gitx.Repo, cfg *config.Config, plan commitPlan) error {
	if err := a.gate(ctx, repo, cfg); err != nil {
		return err
	}
	backup, err := plan.bump.apply(repo)
	if err != nil {
		return err
	}
	undo := func(cause error) error { return errors.Join(cause, restoreVersion(repo, backup)) }

	if _, err := repo.Git("add", "-A"); err != nil {
		return undo(err)
	}
	if err := runChecks(repo, cfg); err != nil {
		return undo(err)
	}
	if _, err := repo.GitInput(strings.NewReader(plan.message), markerEnv, "commit", "--cleanup=verbatim", "-F", "-"); err != nil {
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
		return errors.Join(fmt.Errorf("commit made, but reading it back failed: %w", err), a.push(repo, plan.remotes, refs))
	}
	a.printf("committed %s on %s%s", strings.TrimSpace(head), plan.branch, plan.bump.describe())
	return a.push(repo, plan.remotes, refs)
}

// bumpPlan is the version change a commit will make.
type bumpPlan struct {
	cfg  config.Version
	next *version.Semver
	tag  string
}

func planBump(repo *gitx.Repo, cfg *config.Config, level version.Level) (bumpPlan, error) {
	plan := bumpPlan{cfg: cfg.Version}
	if cfg.Version.Mode != config.ModeFile {
		return plan, nil
	}
	current, err := version.Current(repo.Root, cfg.Version)
	if err != nil {
		return plan, fmt.Errorf("reading the version: %w", err)
	}
	next := current.Bump(level)
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
