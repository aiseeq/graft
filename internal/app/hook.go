package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/aiseeq/graft/internal/hooks"
	"github.com/aiseeq/graft/internal/tasks"
)

// HookPreCommit decides whether git may create a commit.
//
// graft's own commits pass: graft has already run the gate and the checks.
// Commits git's machinery drives (merge, rebase, cherry-pick, revert) pass
// through the checks and the gate: refusing them would strand a conflict
// resolution. A hand-written git commit is refused: it skips the version bump,
// the work item key and the push.
func (a *App) HookPreCommit(ctx context.Context) error {
	if v := os.Getenv(tasks.MarkerEnv); v != "" {
		marked, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%s=%q: %w", tasks.MarkerEnv, v, err)
		}
		if marked {
			return nil
		}
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
