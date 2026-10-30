// Package app implements graft's commands on top of the building blocks in the
// sibling packages.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/checks"
	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/hooks"
	"github.com/aiseeq/graft/v2/internal/lock"
	"github.com/aiseeq/graft/v2/internal/services"
	"github.com/aiseeq/graft/v2/internal/stats"
	"github.com/aiseeq/graft/v2/internal/tasks"
	"github.com/aiseeq/graft/v2/internal/version"
)

// App carries the process context the commands run in.
type App struct {
	Dir string // directory graft was started in
	// ToolVersion is this graft build's version, checked against the
	// project's graft: key.
	ToolVersion string
	// NoCache runs the tasks with cache as if they had none (--no-cache).
	NoCache bool
	// NoCacheEnv is GRAFT_NO_CACHE as main reads it: the same for every run.
	NoCacheEnv bool
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	// Command is the graft command being run (commit, gate, deploy, ...):
	// the stats record tasks as run under it. Empty for graft <task>.
	Command string

	stats *stats.Recorder
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
	if err := a.requireGraft(cfg); err != nil {
		return nil, nil, err
	}
	return repo, cfg, nil
}

// requireGraft refuses to work on a project that needs a newer graft: an
// older one would reject or, worse, ignore what the config asks for.
func (a *App) requireGraft(cfg *config.Config) error {
	if cfg.MinGraft == "" {
		return nil
	}
	need, err := version.Parse(cfg.MinGraft)
	if err != nil {
		return fmt.Errorf("graft: %w", err)
	}
	have, err := version.ParseBuild(a.ToolVersion)
	if err != nil {
		return fmt.Errorf("this project needs graft >= %s and this build cannot tell its version (%w); install it, %s",
			need, err, hooks.InstallHint)
	}
	if have.Less(need) {
		return fmt.Errorf("this project needs graft >= %s, this is graft %s; install it, %s", need, a.ToolVersion, hooks.InstallHint)
	}
	return nil
}

func (a *App) printf(format string, args ...any) {
	fmt.Fprintf(a.Stdout, "graft: "+format+"\n", args...)
}

// withLock runs fn holding the repository lock.
func (a *App) withLock(ctx context.Context, path string, cfg *config.Config, holder string, fn func() error) error {
	lk, err := lock.Acquire(ctx, path, cfg.Lock.Timeout, holder, a.Stderr)
	if err != nil {
		return err
	}
	err = fn()
	if releaseErr := lk.Release(); releaseErr != nil {
		err = errors.Join(err, fmt.Errorf("releasing the lock %s: %w", filepath.Base(path), releaseErr))
	}
	return err
}

// repoLock is the lock shared by every worktree of the repository.
func repoLock(repo *gitx.Repo) string { return filepath.Join(repo.CommonDir, lockFile) }

// commitLock is the lock commits and amends take: the repository's, or with
// the config's lock: {scope: worktree} the worktree's own. The main worktree's git directory is
// the common one, so its commits still exclude a release.
func commitLock(repo *gitx.Repo, cfg *config.Config) string {
	if cfg.Lock.Scope == config.LockWorktree {
		return filepath.Join(repo.GitDir, lockFile)
	}
	return repoLock(repo)
}

// newRunner is a runner in root that records its tasks under the command.
func (a *App) newRunner(root string, cfg *config.Config) *tasks.Runner {
	r := tasks.NewRunner(root, cfg, a.Stdout, a.Stderr)
	r.Stdin = a.Stdin
	// One recorder per run: a failing stats file warns once, however many
	// runners the command makes.
	if a.stats == nil {
		a.stats = stats.NewRecorder(a.Stderr)
	}
	r.Stats, r.Top = a.stats, a.Command
	return r
}

// runner prepares the task runner for the repository.
func (a *App) runner(repo *gitx.Repo, cfg *config.Config) *tasks.Runner {
	r := a.newRunner(repo.Root, cfg)
	r.GraftVersion = a.ToolVersion
	r.NoCache = a.NoCache || a.NoCacheEnv
	r.Repo, r.LockDir = repo, lockDir(repo)
	if cfg.TestDB != nil {
		r.TestDB = func(ctx context.Context) (tasks.TestDB, error) { return a.testDB(ctx, repo, cfg) }
	}
	r.Service = func(ctx context.Context, action, name string) error {
		return a.serviceAction(ctx, services.New(repo.Root, cfg, r, a.Stdout), ServiceAction(action), name)
	}
	r.Version = func() (string, bool, error) {
		v, err := describeVersion(repo, cfg)
		if errors.Is(err, errNoCommits) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		return v, true, nil
	}
	return r
}

func (a *App) gate(ctx context.Context, repo *gitx.Repo, cfg *config.Config) error {
	a.warnStaleWorktreeLinks(repo, cfg)
	return a.runner(repo, cfg).Gate(ctx)
}

// gateForMessage runs the gate of a commit whose message is known: steps find
// it in the file named by GRAFT_COMMIT_MESSAGE_FILE, removed afterwards.
func (a *App) gateForMessage(ctx context.Context, repo *gitx.Repo, cfg *config.Config, message string) (err error) {
	f, err := os.CreateTemp(repo.GitDir, "GRAFT_COMMIT_MESSAGE.*")
	if err != nil {
		return fmt.Errorf("writing the commit message for the gate: %w", err)
	}
	defer func() {
		if rmErr := os.Remove(f.Name()); rmErr != nil {
			err = errors.Join(err, fmt.Errorf("removing %s: %w", f.Name(), rmErr))
		}
	}()
	if _, err := f.WriteString(message); err != nil {
		return errors.Join(fmt.Errorf("writing %s: %w", f.Name(), err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", f.Name(), err)
	}
	r := a.runner(repo, cfg)
	r.Extra = map[string]string{tasks.MessageFileEnv: f.Name()}
	return r.Gate(ctx)
}

// markerEnv lets graft's own git commit through the pre-commit hook.
var markerEnv = []string{tasks.MarkerEnv + "=1"}

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
	// Leaks pass only by graft:leak-ok and leaks.allow; .graft.yaml
	// exceptions belong to the other checks.
	if slices.ContainsFunc(e.Findings, func(f checks.Finding) bool { return f.Leak }) {
		b.WriteString("\nprivate names and terms come from the user config (leaks); rewrite them in neutral words, or mark")
		b.WriteString("\nthe line with graft:leak-ok <reason>, or allow a term everywhere in leaks.allow")
	}
	if slices.ContainsFunc(e.Findings, func(f checks.Finding) bool { return !f.Leak }) {
		b.WriteString("\nfix the files, or add an exception with a reason to .graft.yaml (checks.*.exceptions);")
		b.WriteString("\nto drop a file from the commit: git rm --cached <file> and add it to .gitignore")
	}
	return b.String()
}

// runChecks inspects the index: secrets, binaries and sizes in the changed
// files, the version files agreeing with each other, and in a public
// repository, the user's private names in the added lines and in message.
// revertBase is what the revert check holds the index to (checks.Reverts).
func runChecks(repo *gitx.Repo, cfg *config.Config, message, revertBase string, log io.Writer) error {
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
	if cfg.Checks.Reverts.IsEnabled() {
		if message == "" {
			if message, err = machineryMessage(repo); err != nil {
				return err
			}
		}
		back, err := checks.Reverts(repo, cfg.Checks.Reverts, cfg.VersionFiles(), message, revertBase)
		if err != nil {
			return err
		}
		findings = append(findings, back...)
	}
	drift, err := checks.VersionSync(repo, cfg.Version)
	if err != nil {
		return err
	}
	findings = append(findings, drift...)
	problems, err := attributionProblems(repo, cfg)
	if err != nil {
		return err
	}
	for _, p := range problems {
		findings = append(findings, checks.Finding{Path: config.FileName, Reason: p})
	}
	ruleFindings, err := flagRuleFindings(repo, cfg)
	if err != nil {
		return err
	}
	findings = append(findings, ruleFindings...)
	leakFound, err := leakFindings(repo, message, log)
	if err != nil {
		return err
	}
	findings = append(findings, leakFound...)
	if len(findings) > 0 {
		return &ChecksError{Findings: findings}
	}
	return nil
}

// machineryMessage is the message of the commit git's machinery is making:
// a rebase's, or a merge's, cherry-pick's or revert's; "" when none is in
// progress.
func machineryMessage(repo *gitx.Repo) (string, error) {
	for _, f := range []string{filepath.Join("rebase-merge", "message"), "MERGE_MSG"} {
		data, err := os.ReadFile(filepath.Join(repo.GitDir, f))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		return string(data), err
	}
	return "", nil
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
	Lease  bool
}

func (e *PushError) Error() string {
	var retry []string
	for _, r := range e.Failed {
		retry = append(retry, "git "+strings.Join(append(pushArgs(r, e.Lease), e.Refs...), " "))
	}
	return fmt.Sprintf("push failed for %d of %d remotes (%s); the commit is made locally, retry with: %s",
		len(e.Failed), e.Total, strings.Join(e.Failed, ", "), strings.Join(retry, "; "))
}

// push publishes refs to every remote, one at a time, and keeps going past a
// failed remote: the others still get the commit, and the summary names each
// one that did not.
func (a *App) push(repo *gitx.Repo, remotes, refs []string, lease bool) error {
	if len(remotes) == 0 {
		a.printf("no remotes configured, nothing pushed")
		return nil
	}
	var failed []string
	for _, remote := range remotes {
		args := append(pushArgs(remote, lease), refs...)
		if _, err := repo.Git(args...); err != nil {
			failed = append(failed, remote)
			fmt.Fprintf(a.Stderr, "graft: push to %s FAILED: %v\n", remote, err)
			continue
		}
		a.printf("pushed to %s", remote)
	}
	if len(failed) > 0 {
		return &PushError{Failed: failed, Total: len(remotes), Refs: refs, Lease: lease}
	}
	return nil
}

// pushArgs is git push to remote, with --force-with-lease for a branch that is
// rebased after it was pushed: the push replaces only what this clone last saw.
func pushArgs(remote string, lease bool) []string {
	if lease {
		return []string{"push", "--force-with-lease", remote}
	}
	return []string{"push", remote}
}

// mergeOrNormalContext tells whether a merge is in progress, which a graft
// commit finishes once its conflicts are resolved, and refuses the other
// operations git can be in the middle of.
func mergeOrNormalContext(repo *gitx.Repo) (bool, error) {
	ctx, err := hooks.Detect(repo.GitDir)
	if err != nil || ctx == hooks.Normal {
		return false, err
	}
	if ctx != hooks.Merge {
		return false, fmt.Errorf("a %s is in progress: finish it with git (the pre-commit hook gates it) or abort it first", ctx)
	}
	out, err := repo.Git("diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil {
		return false, err
	}
	if unmerged := strings.Split(strings.TrimRight(out, "\x00"), "\x00"); unmerged[0] != "" {
		return false, fmt.Errorf("the merge has unresolved conflicts in %s: resolve them and git add the files, then graft commit", strings.Join(unmerged, ", "))
	}
	return true, nil
}

// requireNormalContext refuses to start on top of an operation git is in the
// middle of.
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
var errGateLeftHead = errors.New("nothing to commit: the gate left the work tree as HEAD has it")
var errGateEmptiedAmend = errors.New("nothing to amend: the gate left the amended commit as its parent has it")
