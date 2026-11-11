package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/hooks"
	"github.com/aiseeq/graft/v2/internal/stats"
	"github.com/aiseeq/graft/v2/internal/tasks"
	"github.com/aiseeq/graft/v2/internal/userconfig"
	"github.com/aiseeq/graft/v2/internal/workers"
)

// HookPreCommit decides whether git may create a commit.
//
// graft's own commits pass: graft has already run the gate and the checks.
// Commits git's machinery drives (merge, rebase, cherry-pick, revert) pass
// through the checks and the gate: refusing them would strand a conflict
// resolution. A hand-written git commit is refused: it skips the version bump,
// the work item key and the push. On a branch of the user's
// commit.local_branches it passes through the checks alone: such a branch is
// no work to ship but a batch for its project to look at and merge, which
// brings the gate.
// agentSessionEnv names the Claude Code session a command runs in.
const agentSessionEnv = "CLAUDE_CODE_SESSION_ID"

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
		local, err := localBranch(repo)
		if err != nil {
			return err
		}
		if local == "" {
			a.recordRefusedCommit(repo, "")
			return errors.New(commitHowTo(cfg))
		}
		a.printf("commit on %s, a local branch (commit.local_branches of the user config): the checks only, no gate, version or push", local)
		return runChecks(ctx, repo, cfg, "", "HEAD", a.Stderr)
	}
	a.printf("%s commit: running the checks and the gate", kind)
	if err := runChecks(ctx, repo, cfg, "", "HEAD", a.Stderr); err != nil {
		return err
	}
	if err := a.gate(ctx, repo, cfg); err != nil {
		return fmt.Errorf("%w\nresolve the %s so that the gate passes, then continue", err, kind)
	}
	return nil
}

// HookGlobalPreCommit applies the project pre-commit hook to projects that
// have not installed the local graft shim.
func (a *App) HookGlobalPreCommit(ctx context.Context) error {
	repo, err := gitx.Open(".")
	if errors.Is(err, gitx.ErrNotRepo) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(repo.Root, ".graft.yaml")); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	// graft init's shim already ran: git runs the config hooks and the
	// hooks directory's both.
	if local, err := hooks.LocalShimActive(repo, hooks.PreCommit); err != nil || local {
		return err
	}
	return a.HookPreCommit(ctx)
}

// recordRefusedCommit logs a raw git commit for graft stats --refused: one the
// hook refused, or with kind stats.KindBypass one that went past it. A failure
// is said and changes nothing: the commit goes as it would have.
func (a *App) recordRefusedCommit(repo *gitx.Repo, kind string) {
	rec := stats.RefusedCommit{Time: time.Now(), Session: os.Getenv(agentSessionEnv), Project: repo.Root, Kind: kind}
	branch, err := headBranch(repo)
	if err != nil {
		a.warn("the refused commit's branch is not recorded: %v", err)
	}
	rec.Branch = branch
	if err := stats.AppendRefusedCommit(rec); err != nil {
		a.warn("the refused commit is not recorded: %v", err)
	}
}

// headBranch is the branch HEAD is on, "" when detached.
func headBranch(repo *gitx.Repo) (string, error) {
	out, err := repo.Git("symbolic-ref", "--short", "-q", "HEAD")
	if gitx.ExitCode(err) == 1 {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// localBranch is the branch HEAD is on when it is one of the user's
// commit.local_branches, else "".
func localBranch(repo *gitx.Repo) (string, error) {
	uc, err := userconfig.CommitSettings()
	if err != nil || len(uc.LocalBranches) == 0 {
		return "", err
	}
	branch, err := headBranch(repo)
	if err != nil || branch == "" {
		return "", err
	}
	for i, g := range uc.LocalBranches {
		re, err := config.CompileGlob(g)
		if err != nil {
			return "", fmt.Errorf("the user config's commit.local_branches[%d]: %w", i, err)
		}
		if re.MatchString(branch) {
			return branch, nil
		}
	}
	return "", nil
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
	a.noteBypass(repo)
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

// noteBypass records a plain commit that reached prepare-commit-msg without
// graft: pre-commit refuses every such commit, so it was skipped, which git
// commit --no-verify does and which leaves this hook running. A commit given
// up after this hook, by an empty message, stays on the list: the line says it
// went past the hook, not that a commit came of it. A failure is said and
// changes nothing.
func (a *App) noteBypass(repo *gitx.Repo) {
	kind, err := hooks.Detect(repo.GitDir)
	if err != nil {
		a.warn("the commit past the hook is not recorded: %v", err)
		return
	}
	if kind != hooks.Normal {
		return
	}
	local, err := localBranch(repo)
	if err != nil {
		a.warn("the commit past the hook is not recorded: %v", err)
		return
	}
	if local == "" {
		a.recordRefusedCommit(repo, stats.KindBypass)
	}
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
	models, err := workers.Models(repo, a.Stderr, cfg.VersionFiles())
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
