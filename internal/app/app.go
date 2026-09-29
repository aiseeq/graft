// Package app implements graft's commands on top of the building blocks in the
// sibling packages.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiseeq/graft/internal/checks"
	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/gate"
	"github.com/aiseeq/graft/internal/gitx"
	"github.com/aiseeq/graft/internal/hooks"
	"github.com/aiseeq/graft/internal/lock"
)

// App carries the process context the commands run in.
type App struct {
	Dir    string // directory graft was started in
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// lockFile is the lock's name inside the common git directory, shared by all
// worktrees of a repository.
const lockFile = "graft.lock"

func (a *App) open() (*gitx.Repo, *config.Config, error) {
	repo, err := gitx.Open(a.Dir)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := config.Load(repo.Root)
	if err != nil {
		return nil, nil, err
	}
	return repo, cfg, nil
}

func (a *App) printf(format string, args ...any) {
	fmt.Fprintf(a.Stdout, "graft: "+format+"\n", args...)
}

// withLock runs fn holding the repository lock.
func (a *App) withLock(ctx context.Context, repo *gitx.Repo, cfg *config.Config, holder string, fn func() error) error {
	lk, err := lock.Acquire(ctx, filepath.Join(repo.CommonDir, lockFile), cfg.Lock.Timeout, holder, a.Stderr)
	if err != nil {
		return err
	}
	err = fn()
	if releaseErr := lk.Release(); releaseErr != nil {
		err = errors.Join(err, fmt.Errorf("releasing the repository lock: %w", releaseErr))
	}
	return err
}

func (a *App) gate(ctx context.Context, repo *gitx.Repo, cfg *config.Config) error {
	return gate.Run(ctx, repo.Root, cfg.Gate, a.Stdout)
}

// markerEnv lets graft's own git commit through the pre-commit hook.
var markerEnv = []string{gate.MarkerEnv + "=1"}

// ChecksError lists what the staged content checks found.
type ChecksError struct {
	Findings []checks.Finding
}

func (e *ChecksError) Error() string {
	var b strings.Builder
	b.WriteString("staged changes failed the checks:")
	for _, f := range e.Findings {
		b.WriteString("\n  " + f.String())
	}
	b.WriteString("\nfix the files, or add an exception with a reason to .graft.yaml (checks.*.exceptions);")
	b.WriteString("\nto drop a file from the commit: git rm --cached <file> and add it to .gitignore")
	return b.String()
}

// runChecks inspects the index: secrets, binaries and sizes in the changed
// files, and the version files agreeing with each other.
func runChecks(repo *gitx.Repo, cfg *config.Config) error {
	staged, err := checks.Staged(repo)
	if err != nil {
		return err
	}
	var findings []checks.Finding
	if cfg.Checks.Secrets.IsEnabled() {
		findings = append(findings, checks.Secrets(staged, cfg.Checks.Secrets)...)
	}
	if cfg.Checks.LargeFiles.IsEnabled() {
		findings = append(findings, checks.LargeFiles(staged, cfg.Checks.LargeFiles)...)
	}
	drift, err := checks.VersionSync(repo, cfg.Version)
	if err != nil {
		return err
	}
	findings = append(findings, drift...)
	if len(findings) > 0 {
		return &ChecksError{Findings: findings}
	}
	return nil
}

// pushRemotes resolves the remotes to publish to and verifies the configured
// ones exist before anything is changed.
func pushRemotes(repo *gitx.Repo, cfg *config.Config) ([]string, error) {
	existing, err := repo.Remotes()
	if err != nil {
		return nil, err
	}
	if len(cfg.Push.Remotes) == 0 {
		return existing, nil
	}
	for _, r := range cfg.Push.Remotes {
		if !slices.Contains(existing, r) {
			return nil, fmt.Errorf("push.remotes names %q, but the repository has no such remote (have: %s)", r, strings.Join(existing, ", "))
		}
	}
	return cfg.Push.Remotes, nil
}

// PushError reports the remotes a push did not reach.
type PushError struct {
	Failed []string
	Total  int
	Refs   []string
}

func (e *PushError) Error() string {
	var retry []string
	for _, r := range e.Failed {
		retry = append(retry, "git push "+r+" "+strings.Join(e.Refs, " "))
	}
	return fmt.Sprintf("push failed for %d of %d remotes (%s); the commit is made locally, retry with: %s",
		len(e.Failed), e.Total, strings.Join(e.Failed, ", "), strings.Join(retry, "; "))
}

// push publishes refs to every remote, one at a time, and keeps going past a
// failed remote: the others still get the commit, and the summary names each
// one that did not.
func (a *App) push(repo *gitx.Repo, remotes, refs []string) error {
	if len(remotes) == 0 {
		a.printf("no remotes configured, nothing pushed")
		return nil
	}
	var failed []string
	for _, remote := range remotes {
		args := append([]string{"push", remote}, refs...)
		if _, err := repo.Git(args...); err != nil {
			failed = append(failed, remote)
			fmt.Fprintf(a.Stderr, "graft: push to %s FAILED: %v\n", remote, err)
			continue
		}
		a.printf("pushed to %s", remote)
	}
	if len(failed) > 0 {
		return &PushError{Failed: failed, Total: len(remotes), Refs: refs}
	}
	return nil
}

// requireNormalContext refuses to start a graft commit on top of an operation
// git is in the middle of.
func requireNormalContext(repo *gitx.Repo) error {
	ctx, err := hooks.Detect(repo.GitDir)
	if err != nil {
		return err
	}
	if ctx != hooks.Normal {
		return fmt.Errorf("a %s is in progress: finish it with git (the pre-commit hook gates it) or abort it first", ctx)
	}
	return nil
}

var errNothingToCommit = errors.New("nothing to commit: the work tree is clean")
