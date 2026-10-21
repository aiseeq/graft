package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/hooks"
	"github.com/aiseeq/graft/v2/internal/tasks"
	"github.com/aiseeq/graft/v2/internal/workers"
)

// HookPreCommit decides whether git may create a commit.
//
// graft's own commits pass: graft has already run the gate and the checks.
// Commits git's machinery drives (merge, rebase, cherry-pick, revert) pass
// through the checks and the gate: refusing them would strand a conflict
// resolution. A hand-written git commit is refused: it skips the version bump,
// the work item key and the push.
func (a *App) HookPreCommit(ctx context.Context) error {
	if own, err := graftsOwn(); err != nil || own {
		return err
	}
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	kind, err := hooks.Detect(repo.GitDir)
	if err != nil {
		return err
	}
	if kind == hooks.Normal {
		return errors.New(commitHowTo(cfg))
	}
	a.printf("%s commit: running the checks and the gate", kind)
	if err := runChecks(repo, cfg, "", a.Stderr); err != nil {
		return err
	}
	if err := a.gate(ctx, repo, cfg); err != nil {
		return fmt.Errorf("%w\nresolve the %s so that the gate passes, then continue", err, kind)
	}
	return nil
}

// HookPrepareCommitMsg adds the Worker trailers to the message git is
// about to commit (a git commit, a merge, a squash): graft's own commits add
// them themselves.
func (a *App) HookPrepareCommitMsg(file string) error {
	if own, err := graftsOwn(); err != nil || own {
		return err
	}
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	msg, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("the commit message: %w", err)
	}
	with, err := a.withWorkers(repo, cfg, string(msg))
	if err != nil || with == string(msg) {
		return err
	}
	if err := os.WriteFile(file, []byte(with), 0o644); err != nil {
		return fmt.Errorf("the commit message: %w", err)
	}
	return nil
}

// graftsOwn tells whether graft itself runs the git command the hook is
// called from.
func graftsOwn() (bool, error) {
	v := os.Getenv(tasks.MarkerEnv)
	if v == "" {
		return false, nil
	}
	own, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s=%q: %w", tasks.MarkerEnv, v, err)
	}
	return own, nil
}

// withWorkers is msg with a Worker trailer per model whose work the commit
// being made carries (workers.Models).
func (a *App) withWorkers(repo *gitx.Repo, cfg *config.Config, msg string) (string, error) {
	models, err := workers.Models(repo, cfg.VersionFiles(), a.Stderr)
	if err != nil {
		return "", err
	}
	return workers.WithTrailers(repo, msg, models)
}

// HookPostMerge runs the gate after a real merge commit. A merge without text
// conflicts never runs pre-commit, and that is exactly how a semantic conflict
// lands: one side changes a signature, the other keeps calling the old one,
// no line overlaps and the tree stops compiling. The merge is already made, so
// this can only report it, loudly, while the cause is still obvious.
func (a *App) HookPostMerge(ctx context.Context) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	// A fast-forward brings commits already gated on their own branch; only
	// a commit with a second parent combines two lines of work here.
	merged, err := repo.RevExists("HEAD^2")
	if err != nil {
		return err
	}
	if !merged {
		return nil
	}
	a.printf("merge commit: checking that the merged code passes the gate")
	if err := a.gate(ctx, repo, cfg); err != nil {
		return fmt.Errorf(`%w

the merge had no text conflicts, but the result fails the gate: a semantic conflict.
The merge commit is already made. Fix the code and fold the fix into it:
  graft amend
Do not push until the gate passes`, err)
	}
	return nil
}
