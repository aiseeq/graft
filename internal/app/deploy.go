package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/modfile"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/deploy"
	"github.com/aiseeq/graft/v2/internal/deploystate"
	"github.com/aiseeq/graft/v2/internal/dotenv"
	"github.com/aiseeq/graft/v2/internal/envs"
	"github.com/aiseeq/graft/v2/internal/fsx"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/jira"
	"github.com/aiseeq/graft/v2/internal/loadgate"
	"github.com/aiseeq/graft/v2/internal/lock"
	"github.com/aiseeq/graft/v2/internal/message"
	"github.com/aiseeq/graft/v2/internal/proc"
	"github.com/aiseeq/graft/v2/internal/tasks"
	"github.com/aiseeq/graft/v2/internal/userconfig"
	"github.com/aiseeq/graft/v2/internal/version"
)

// Environment variables graft passes to the deploy script.
const (
	EnvDeployTarget   = "GRAFT_DEPLOY_TARGET"
	EnvDeployEnv      = "GRAFT_DEPLOY_ENV"
	EnvDeploySHA      = "GRAFT_DEPLOY_SHA"
	EnvDeployVersion  = "GRAFT_DEPLOY_VERSION"
	EnvDeployPrevious = "GRAFT_DEPLOY_PREVIOUS_SHA"
)

// ExitCodeError carries the exit status of a child process graft stands in
// for, so graft exits with the same status.
type ExitCodeError struct {
	Code int
	Err  error
}

func (e *ExitCodeError) Error() string { return e.Err.Error() }

func (e *ExitCodeError) Unwrap() error { return e.Err }

// DeployOptions are the arguments of graft deploy.
type DeployOptions struct {
	Target string
	// Args are passed to the deploy script after its configured arguments.
	Args []string
	// Redeploy deploys the commit the target already runs (deployed_sha).
	Redeploy       bool
	TakeBack       bool
	TakeBackReason string
	// GlintTicket is the item that takes the glint findings of the deployed
	// commit, at a release acceptance: the items move with them
	// (release_notes.glint).
	GlintTicket string
	// Unapproved collects approval results without refusing the deploy.
	Unapproved bool
	// PastFreeze permits a freeze refusal when it gives the bypass reason.
	PastFreeze string
	// ReviewerAgreed lists KEY@accountId pairs whose reviewers answered yes.
	ReviewerAgreed string
}

type deployRun struct {
	a       *App
	repo    *gitx.Repo
	cfg     *config.Config
	name    string
	target  *config.DeployTarget
	env     *envs.Env
	head    string
	version string
	prev    string
	// tasks runs the target's deps.
	tasks *tasks.Runner
	// branch is the branch deployed; hotfix tells whether it is a hotfix
	// branch.
	branch string
	hotfix bool
	// now is when the window and the hold are judged.
	now time.Time
	// gated is set once approved_release let the commits out; withheld are
	// then the commits to record as withheld.
	gated    bool
	withheld []string
	// released are the commits withheld from an earlier release that the
	// approved release lets out now.
	released []string
	// passed are the items the approved release lets out; hotfixItems the
	// labelled items of a hotfix.
	passed, hotfixItems []string
	dormantItems        []config.DormantReleaseItem
	// approvedVersions are the approved versions the release took items
	// from, with approved_release.prefix.
	approvedVersions       []string
	approvalExceptions     []gateFailure
	approvalReasons        []string
	approvalItemExceptions map[string][]string
	pageWithheld           []string
	approvalCollectionErr  error
	// moveOut are the items whose commits a deploy to a held target
	// reverts: they leave the window's version for the next one.
	moveOut *versionMove
	// dryRun is graft deploy check: the checks record nothing and never
	// wait for a sudo password.
	dryRun bool
	// reviewGuardActive lets deploy check describe a known guard stop while
	// remaining a successful read-only report.
	reviewGuardActive bool
	// freeze is the active freeze related to this target, read once per run.
	freeze                         *activeFreeze
	freezeLogKind, freezeLogReason string
	freezeAdvisoryFailed           []string
	takeBackReason                 string
	takeBackActive                 bool
	takeBackJournal                *deploy.TakeBackJournal
	freezePreflightDeps            []string
	// glint holds the moves for the glint findings of the deployed commit.
	glint *glintCheck
}

// readSHA reads a commit record of an environment, without a sudo prompt
// in a dry run; a record that needs sudo where sudo cannot work is refused
// before sudo runs, in a dry run too.
func (r *deployRun) readSHA(ctx context.Context, env *envs.Env, rec *config.DeployedSHA) (string, error) {
	if recordUsesSudo(rec, env) {
		if err := proc.RefusedSudo(); err != nil {
			return "", err
		}
	}
	if r.dryRun {
		return deploy.PeekDeployedSHA(ctx, env, rec)
	}
	return deploy.ReadDeployedSHA(ctx, env, rec)
}

// Deploy runs the project's deploy script for a target between graft's
// checks: published HEAD, confirmation, the version already on the required
// target; afterwards the deployed commit and the release notes.
func (a *App) Deploy(ctx context.Context, o DeployOptions) error {
	r, err := a.deployTarget(o.Target)
	if err != nil {
		return err
	}
	if err := refusedSudo(r.target, r.env); err != nil {
		return err
	}
	if err := deployPreflight(r); err != nil {
		return err
	}
	if r.glint, err = newGlintCheck(r, o.GlintTicket, func(context.Context) (string, error) { return r.head, nil }); err != nil {
		return err
	}
	if err := r.glint.checkTicket(ctx); err != nil {
		return err
	}
	// Its heavy tasks, and those of every graft its script starts, use the
	// queue of heavy tasks within one budget; a deploy inside a deploy keeps
	// the outer's.
	lw := r.cfg.Deploy.LoadWait
	warnLoadWaitThenStart(a.Stderr, lw)
	if os.Getenv(loadgate.EnvPriority) == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return fmt.Errorf("the budget for waiting: %w", err)
		}
		budget, err := loadgate.NewBudget(filepath.Join(cache, "graft"), lw.Budget)
		if err != nil {
			return err
		}
		defer func() {
			for _, f := range []string{budget, budget + ".total", budget + ".intervals", budget + ".lock", budget + ".until"} {
				if err := fsx.Remove(f); err != nil {
					fmt.Fprintf(a.Stderr, "graft: warning: removing the budget for waiting: %v\n", err)
				}
			}
		}()
		p := loadgate.Priority{For: "graft deploy " + o.Target, Budget: budget, Fail: true}
		for k, v := range p.Env() {
			if err := os.Setenv(k, v); err != nil {
				return fmt.Errorf("setting %s: %w", k, err)
			}
		}
	}
	path := filepath.Join(r.repo.CommonDir, "graft-locks", "deploy-"+o.Target+".lock")
	lk, err := lock.AcquireMode(ctx, path, lock.Exclusive, r.cfg.Lock.Timeout, "graft deploy "+o.Target, a.Stderr)
	if err != nil {
		return err
	}
	releaseDeploy, restoreDeploy, err := beginMachineDeployment(r.repo.Root, o.Target, lk)
	if err != nil {
		return err
	}
	err = a.deploy(ctx, r, o)
	if releaseErr := releaseDeploy(); releaseErr != nil {
		err = errors.Join(err, fmt.Errorf("releasing the machine deployment marker: %w", releaseErr))
	}
	if restoreErr := restoreDeploy(); restoreErr != nil {
		err = errors.Join(err, restoreErr)
	}
	if releaseErr := lk.Release(); releaseErr != nil {
		err = errors.Join(err, fmt.Errorf("releasing the deploy lock: %w", releaseErr))
	}
	a.noteDeployJournal(ctx, r, err)
	if err != nil {
		return err
	}
	a.dailyFlags(ctx, r.repo, r.cfg, r.env.Name)
	return nil
}

func (a *App) noteDeployJournal(ctx context.Context, r *deployRun, deployErr error) {
	if r.head == "" {
		return
	}
	message, err := r.repo.Git("show", "-s", "--format=%B", r.head)
	if err != nil {
		a.warn("ticket journal was not written: reading deployed commit message: %v", err)
		return
	}
	identity := r.version
	if identity == "" {
		identity = gitx.Short(r.head)
	}
	result := map[bool]string{true: "ok", false: "failed"}[deployErr == nil]
	a.ticketJournalForMessage(ctx, r.repo, message, fmt.Sprintf("deploy %s %s: %s", r.name, identity, result))
}

func warnLoadWaitThenStart(w io.Writer, lw config.LoadWait) {
	if lw.ThenStartExplicit {
		fmt.Fprintln(w, "graft: warning: deploy.load_wait.then: start no longer starts over the limit (user rule); treated as fail")
	}
}

func beginMachineDeployment(project, target string, lk *lock.Lock) (func() error, func() error, error) {
	release, err := deploystate.Register(project, target)
	if err != nil {
		if releaseErr := lk.Release(); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("releasing the deploy lock: %w", releaseErr))
		}
		return nil, nil, err
	}
	previous := os.Getenv(deploystate.Env)
	if err := os.Setenv(deploystate.Env, deploystate.ChildIdentity(previous, project, target)); err != nil {
		var releaseErrs []error
		if releaseErr := release(); releaseErr != nil {
			releaseErrs = append(releaseErrs, fmt.Errorf("releasing the machine deployment marker: %w", releaseErr))
		}
		if releaseErr := lk.Release(); releaseErr != nil {
			releaseErrs = append(releaseErrs, fmt.Errorf("releasing the deploy lock: %w", releaseErr))
		}
		return nil, nil, errors.Join(append([]error{fmt.Errorf("setting %s: %w", deploystate.Env, err)}, releaseErrs...)...)
	}
	restore := func() error {
		var err error
		if previous == "" {
			err = os.Unsetenv(deploystate.Env)
		} else {
			err = os.Setenv(deploystate.Env, previous)
		}
		if err != nil {
			return fmt.Errorf("restoring %s: %w", deploystate.Env, err)
		}
		return nil
	}
	return release, restore, nil
}

func targetUsesSudo(target *config.DeployTarget, env *envs.Env) bool {
	return target.Confirm == "sudo" || recordUsesSudo(target.DeployedSHA, env)
}

// recordUsesSudo says whether reading or writing rec runs sudo here: a local
// record kept under sudo.
func recordUsesSudo(rec *config.DeployedSHA, env *envs.Env) bool {
	return rec != nil && rec.Sudo && !env.IsRemote()
}

func refusedSudo(target *config.DeployTarget, env *envs.Env) error {
	if targetUsesSudo(target, env) {
		return proc.RefusedSudo()
	}
	return nil
}

func (a *App) deployTarget(name string) (*deployRun, error) {
	repo, cfg, err := a.open()
	if err != nil {
		return nil, err
	}
	if cfg.Deploy == nil {
		return nil, errors.New("deploy is not configured: add a deploy section to .graft.yaml")
	}
	t, ok := cfg.Deploy.Targets[name]
	if !ok {
		names := slices.Sorted(func(yield func(string) bool) {
			for n := range cfg.Deploy.Targets {
				if !yield(n) {
					return
				}
			}
		})
		return nil, fmt.Errorf("unknown deploy target %q (configured: %s)", name, strings.Join(names, ", "))
	}
	problems, err := attributionProblems(repo, cfg)
	if err != nil {
		return nil, err
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%s:\n  %s", config.FileName, strings.Join(problems, "\n  "))
	}
	env, err := envs.Get(cfg, repo.Root, t.Env)
	if err != nil {
		return nil, err
	}
	return &deployRun{a: a, repo: repo, cfg: cfg, name: name, target: t, env: env, tasks: a.runner(repo, cfg)}, nil
}

func (a *App) deploy(ctx context.Context, r *deployRun, o DeployOptions) (retErr error) {
	r.takeBackReason = o.TakeBackReason
	if err := deploy.CheckTree(r.repo, r.cfg.Deploy); err != nil {
		return err
	}
	head, err := deploy.PublishedHead(r.repo, r.cfg.Deploy)
	if err != nil {
		return err
	}
	r.head = head
	if r.cfg.Version.Mode != config.ModeNone {
		if r.version, err = projectVersion(r.repo, r.cfg); err != nil {
			return err
		}
	}
	a.printf("deploying %s (version %s, commit %s) to %s", r.name, r.version, gitx.Short(head), r.env.Destination())
	if err := a.startRelease(r); err != nil {
		return err
	}
	if r.freeze, err = activeFreezeForRun(r); err != nil {
		if !errors.Is(err, errNoActiveFreeze) {
			return err
		}
	}
	// requires is a quick read-only check: it fails before a long deps run.
	if err := a.requireDeployed(ctx, r); err != nil {
		return err
	}
	if err := a.checkTargetWithReview(ctx, r, o); err != nil {
		return err
	}
	if err := a.checkFreeze(ctx, r, o); err != nil {
		return err
	}
	if err := a.checkFreezeAdvisoryPreflight(r); err != nil {
		return err
	}
	closeTakeBackJournal, err := a.prepareDeployTarget(ctx, r)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, closeTakeBackJournal()) }()
	// One unit: the deps' lines name it, and its line holds them, the
	// confirmation and the script. Its name has a space no task name can
	// have.
	err = r.tasks.Unit("deploy "+r.name, func() error {
		if err := a.runDeployDeps(ctx, r); err != nil {
			return err
		}
		if r.target.Confirm == "sudo" {
			if err := r.tasks.Confirm(ctx, r.name, true); err != nil {
				return err
			}
		}
		if err := a.enforceReviewGuard(ctx, r, o.ReviewerAgreed, false); err != nil {
			return err
		}
		return a.runDeployScript(ctx, r, o.Args)
	})
	if err != nil {
		return err
	}
	if err := deploy.CheckHead(r.repo, r.cfg.Deploy, r.head); err != nil {
		return errors.Join(fmt.Errorf("the deploy script finished, but %w; the deployed commit is not recorded", err), a.recordFreezeJournal(r))
	}
	if err := requireIntegrationsChecked(r); err != nil {
		return errors.Join(err, a.recordFreezeJournal(r))
	}
	a.printf("deployed %s to %s", gitx.Short(r.head), r.name)
	return a.recordDeployment(ctx, r)
}

func (a *App) prepareDeployTarget(ctx context.Context, r *deployRun) (func() error, error) {
	if err := a.checkIntegrations(ctx, r); err != nil {
		return nil, err
	}
	if !r.takeBackActive {
		return func() error { return nil }, nil
	}
	journal, err := deploy.OpenTakeBackJournal(ctx, r.env, r.target.DeployedSHA)
	if err != nil {
		return nil, fmt.Errorf("deploy %s: nothing was deployed: opening take-back journal %s: %w", r.name, r.target.DeployedSHA.Path+".journal", err)
	}
	r.takeBackJournal = journal
	return journal.Close, nil
}

func (a *App) checkTargetWithReview(ctx context.Context, r *deployRun, o DeployOptions) error {
	if err := a.checkTargetRuns(ctx, r, o); err != nil {
		return err
	}
	return a.enforceReviewGuard(ctx, r, o.ReviewerAgreed, false)
}

func (a *App) runDeployDeps(ctx context.Context, r *deployRun) error {
	if len(r.target.Deps) == 0 {
		return nil
	}
	a.printf("deploy %s: running %s first", r.name, strings.Join(r.target.Deps, ", "))
	if err := r.tasks.RunDepsWithAdvisory(ctx, r.target.Deps, func(task string, err error) bool {
		frozen := r.freezeForBranch()
		if !freezeAdvisory(frozen, task) {
			return false
		}
		a.warnFreezeAdvisory(frozen, task, err)
		r.freezeAdvisoryFailed = append(r.freezeAdvisoryFailed, task)
		return true
	}); err != nil {
		return fmt.Errorf("deploy %s: nothing was deployed: %w", r.name, err)
	}
	return nil
}

// startRelease reads the time and the branch, and refuses a target with a
// window outside it: outside the window nothing else matters.
func (a *App) startRelease(r *deployRun) error {
	var err error
	if r.now, err = deployNow(); err != nil {
		return err
	}
	if r.branch, err = r.repo.Branch(); err != nil {
		return err
	}
	if r.hotfix = r.cfg.Deploy.IsHotfix(r.branch); r.hotfix {
		return nil
	}
	return a.checkWindow(r)
}

// checkReleaseFlow runs the checks between the deployed commit and the new
// one: a hotfix branch, the hold on a release candidate, and for an approved
// release that nothing deployed is taken back and every commit is checked.
// A deploy stops at the first refusal; a dry run goes on and returns them
// all.
func (a *App) checkReleaseFlow(ctx context.Context, r *deployRun, unapproved bool) error {
	var errs []error
	stop := func(err error) bool {
		if err != nil {
			errs = append(errs, err)
		}
		return err != nil && !r.dryRun
	}
	if r.hotfix && stop(a.checkHotfix(ctx, r)) {
		return errors.Join(errs...)
	}
	if stop(a.checkHold(ctx, r)) || r.hotfix || r.target.ApprovedRelease == nil {
		return errors.Join(errs...)
	}
	if stop(a.checkContainsDeployed(r)) || stop(a.checkApproved(ctx, r, unapproved)) || !r.gated {
		return errors.Join(errs...)
	}
	stop(a.checkVersionContents(ctx, r))
	return errors.Join(errs...)
}

func (a *App) checkFreeze(ctx context.Context, r *deployRun, o DeployOptions) error {
	frozen := r.freezeForRequiredTarget()
	if frozen == nil {
		if o.PastFreeze != "" {
			a.warn("deploy %s: no active freeze applies; nothing was bypassed", r.name)
		}
		return nil
	}
	if r.hotfix {
		r.freezeLogKind, r.freezeLogReason = "bypass", r.branch
		if o.PastFreeze != "" {
			r.freezeLogReason += ": " + o.PastFreeze
		}
		return nil
	}
	var refusals []error
	if r.branch != frozen.branch {
		refusals = append(refusals, freezeRefusalError{fmt.Errorf("%s is frozen at %s; deploy from %s (or pass --past-freeze with a reason)", frozen.target, frozen.name, frozen.branch)})
	}
	if err := a.freezeIssueCheck(ctx, r, frozen); err != nil {
		var refusal freezeRefusalError
		if !errors.As(err, &refusal) {
			return err
		}
		refusals = append(refusals, refusal)
	}
	if len(refusals) == 0 {
		return nil
	}
	refusal := freezeRefusalError{errors.Join(refusals...)}
	if o.PastFreeze == "" {
		return refusal
	}
	a.warn("deploy %s bypasses freeze %s: %s: %s", r.name, frozen.name, o.PastFreeze, refusal.Error())
	r.freezeLogKind, r.freezeLogReason = "bypass", o.PastFreeze+": "+refusal.Error()
	return nil
}

// checkTargetRuns reads and shows what the target runs, then applies the
// same-commit and release checks. The deployed commit is read before deps,
// sudo and the script.
func (a *App) checkTargetRuns(ctx context.Context, r *deployRun, o DeployOptions) error {
	if r.target.DeployedSHA != nil {
		var err error
		if r.prev, err = r.readSHA(ctx, r.env, r.target.DeployedSHA); err != nil {
			return err
		}
	}
	if err := a.showDeployed(ctx, r); err != nil {
		return err
	}
	if r.target.DeployedSHA == nil {
		return nil
	}
	if err := a.refuseSameCommit(r, o.Redeploy); err != nil {
		return err
	}
	if r.target.ApprovedRelease == nil {
		if err := a.checkTakeBack(r, o.TakeBack); err != nil {
			return err
		}
	} else if o.TakeBack {
		a.printf("nothing is taken back; --take-back has no effect")
	}
	return a.checkReleaseFlow(ctx, r, o.Unapproved)
}

func (a *App) checkTakeBack(r *deployRun, allow bool) error {
	if r.prev == "" || r.prev == r.head {
		if allow {
			a.printf("nothing is taken back; --take-back has no effect")
		}
		return nil
	}
	shallow, err := r.repo.Git("rev-parse", "--is-shallow-repository")
	if err != nil {
		return err
	}
	known, err := r.repo.RevExists(r.prev)
	if err != nil {
		return err
	}
	if strings.TrimSpace(shallow) == "true" {
		a.allowUncomparedTakeBack(r, allow, "shallow clone")
		return nil
	}
	if !known {
		a.allowUncomparedTakeBack(r, allow, gitx.Short(r.prev)+" not in the local repository")
		return nil
	}
	out, err := r.repo.Git("rev-list", "--count", r.head+".."+r.prev)
	if err != nil {
		return err
	}
	takes, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || takes == 0 {
		if err == nil && allow {
			a.printf("nothing is taken back; --take-back has no effect")
		}
		return err
	}
	if allow {
		r.takeBackActive = true
		a.printf("taking back %s: %s", plural(takes, "commit"), r.takeBackReason)
		return nil
	}
	list, err := r.repo.Git("log", "--oneline", r.head+".."+r.prev)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimSpace(list), "\n")
	if len(lines) > 20 {
		lines = append(lines[:20], fmt.Sprintf("… and %d more", takes-20))
	}
	return fmt.Errorf("deploy %s: nothing was deployed: takes back %d deployed commits:\n  %s\nmerge the deployed commits into this one (a rebase or cherry-pick leaves their SHAs out and is refused again), or pass --take-back --reason TEXT", r.name, takes, strings.Join(lines, "\n  "))
}

func (a *App) allowUncomparedTakeBack(r *deployRun, allow bool, comparison string) {
	if !allow {
		return
	}
	r.takeBackActive = true
	r.takeBackReason += " (comparison unavailable: " + comparison + ")"
	a.printf("take-back reason recorded: %s", r.takeBackReason)
}

// showDeployed prints what the target runs and what the deploy changes
// there, before a check can refuse: the version, the commit and its branch
// the target runs, then the commits the deploy brings and those it takes
// back.
func (a *App) showDeployed(ctx context.Context, r *deployRun) error {
	s, err := readDeployed(ctx, r.repo, r.cfg, r.env, r.target, r.prev)
	if err != nil {
		return err
	}
	a.printf("%s runs version %s; commit %s", r.name, s.version, s.commit)
	if r.prev == "" || r.prev == r.head {
		return nil
	}
	shallow, err := r.repo.Git("rev-parse", "--is-shallow-repository")
	if err != nil {
		return err
	}
	if strings.TrimSpace(shallow) == "true" {
		a.printf("shallow clone: cannot tell whether this deploy takes commits back; git fetch --unshallow")
		return nil
	}
	known, err := r.repo.RevExists(r.prev)
	if err != nil || !known {
		return err
	}
	count := func(rng string) (int, error) {
		out, err := r.repo.Git("rev-list", "--count", rng)
		if err != nil {
			return 0, err
		}
		return strconv.Atoi(strings.TrimSpace(out))
	}
	brings, err := count(r.prev + ".." + r.head)
	if err != nil {
		return err
	}
	takes, err := count(r.head + ".." + r.prev)
	if err != nil {
		return err
	}
	if takes == 0 {
		a.printf("deploying %s over it brings %s, takes none back", gitx.Short(r.head), plural(brings, "commit"))
		return nil
	}
	a.printf("deploying %s over it brings %s and takes back %d it runs: git log %s..%s", gitx.Short(r.head), plural(brings, "commit"), takes, gitx.Short(r.head), gitx.Short(r.prev))
	return nil
}

// refusesSameCommit reports whether confirmation protects a repeated deploy
// of the commit the target already runs.
func refusesSameCommit(r *deployRun) bool {
	return r.prev == r.head && r.target.Confirm != ""
}

// refuseSameCommit refuses a repeated deploy only for targets that require
// confirmation, unless redeploy explicitly asks for it.
func (a *App) refuseSameCommit(r *deployRun, redeploy bool) error {
	if !refusesSameCommit(r) {
		if r.prev == r.head {
			a.printf("%s already runs %s, deploying it again", r.name, gitx.Short(r.head))
		}
		return nil
	}
	if redeploy {
		a.printf("%s already runs %s, deploying it again (--redeploy)", r.name, gitx.Short(r.head))
		return nil
	}
	subject, err := r.repo.Git("log", "-1", "--format=%s", r.head)
	if err != nil {
		return err
	}
	return fmt.Errorf("%s already runs %s %s; pass --redeploy to deploy it again", r.name, gitx.Short(r.head), strings.TrimSpace(subject))
}

// requireDeployed enforces requires: the required target must already run
// the version being deployed.
func (a *App) requireDeployed(ctx context.Context, r *deployRun) error {
	if r.target.Requires == "" {
		return nil
	}
	req := r.cfg.Deploy.Targets[r.target.Requires]
	env, err := envs.Get(r.cfg, r.repo.Root, req.Env)
	if err != nil {
		return err
	}
	got, err := deploy.DeployedVersion(ctx, env, req.Version)
	if err != nil {
		return err
	}
	if got != r.version {
		return fmt.Errorf("%s runs version %s, not %s: deploy there first (graft deploy %s)", r.target.Requires, got, r.version, r.target.Requires)
	}
	a.printf("%s already runs version %s", r.target.Requires, got)
	return nil
}

// deployPreflight checks, before the fetch, the sudo prompt and anything
// else, that every required .env key and ${KEY} of the deploy script and of
// the target's deps is set, and lists all that are not.
func deployPreflight(r *deployRun) error {
	lookup := dotenv.NewLookup(filepath.Join(r.repo.Root, r.cfg.DotEnv))
	c := tasks.NewKeyCheck(lookup, nil)
	c.Keys(r.target.Keys)
	c.Refs(r.target.Run.Argv)
	advisory := configuredFreezeAdvisoryForTarget(r)
	var deps []string
	r.freezePreflightDeps = nil
	for _, task := range r.target.Deps {
		if advisory[task] {
			r.freezePreflightDeps = append(r.freezePreflightDeps, task)
		} else {
			deps = append(deps, task)
		}
	}
	problems := append(c.Problems(), r.tasks.DepsMissing(deps)...)
	if len(problems) > 0 {
		return fmt.Errorf("deploy %s: nothing was done, missing:\n  %s", r.name, strings.Join(problems, "\n  "))
	}
	return nil
}

func configuredFreezeAdvisoryForTarget(r *deployRun) map[string]bool {
	advisory := map[string]bool{}
	for name, target := range r.cfg.Deploy.Targets {
		if target.Freeze == nil || (name != r.name && target.Requires != r.name) {
			continue
		}
		for _, task := range target.Freeze.Advisory {
			advisory[task] = true
		}
	}
	return advisory
}

func (a *App) checkFreezeAdvisoryPreflight(r *deployRun) error {
	if len(r.freezePreflightDeps) == 0 {
		return nil
	}
	frozen := r.freezeForBranch()
	if frozen != nil {
		var required []string
		var missing []string
		for _, task := range r.freezePreflightDeps {
			if !freezeAdvisory(frozen, task) {
				required = append(required, task)
				continue
			}
			problems := r.tasks.DepsMissing([]string{task})
			if len(problems) == 0 {
				continue
			}
			err := fmt.Errorf("nothing was run, missing:\n  %s", strings.Join(problems, "\n  "))
			a.warnFreezeAdvisory(frozen, task, err)
			r.freezeAdvisoryFailed = append(r.freezeAdvisoryFailed, task)
			r.tasks.Skip(task)
		}
		missing = r.tasks.DepsMissing(required)
		if len(missing) > 0 {
			return fmt.Errorf("deploy %s: nothing was done, missing:\n  %s", r.name, strings.Join(missing, "\n  "))
		}
		return nil
	}
	problems := r.tasks.DepsMissing(r.target.Deps)
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("deploy %s: nothing was done, missing:\n  %s", r.name, strings.Join(problems, "\n  "))
}

func (a *App) runDeployScript(ctx context.Context, r *deployRun, args []string) error {
	lookup := dotenv.NewLookup(filepath.Join(r.repo.Root, r.cfg.DotEnv))
	argv, err := dotenv.ExpandAll(r.target.Run.Argv, lookup)
	if err != nil {
		return err
	}
	argv = append(argv, args...)
	keys, err := tasks.KeyValues(lookup, r.target.Keys)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = r.repo.Root
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, a.Stdout, a.Stderr
	cmd.Env = append(append(os.Environ(), keys...),
		EnvDeployTarget+"="+r.name,
		EnvDeployEnv+"="+r.env.Name,
		EnvDeploySHA+"="+r.head,
		EnvDeployVersion+"="+r.version,
		EnvDeployPrevious+"="+r.prev,
	)
	err = r.tasks.Measure(ctx, r.target.Run.Source, cmd)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &ExitCodeError{Code: exitErr.ExitCode(), Err: fmt.Errorf("the deploy script failed with exit status %d; nothing recorded", exitErr.ExitCode())}
	}
	if err != nil {
		return fmt.Errorf("running the deploy script: %w", err)
	}
	return nil
}

// recordDeployment writes the deployed commit and posts release notes. The
// release is live by now, so a failure here is reported and the rest goes on;
// only a fix version left unset makes the deploy exit non-zero.
func (a *App) recordDeployment(ctx context.Context, r *deployRun) error {
	journalErr := a.recordFreezeJournal(r)
	if r.target.DeployedSHA == nil {
		return journalErr
	}
	shaRecorded := true
	if err := deploy.WriteDeployedSHA(ctx, r.env, r.target.DeployedSHA, r.head); err != nil {
		shaRecorded = false
		a.warn("the deployed commit is not recorded: %v", err)
	}
	if r.takeBackActive && r.takeBackJournal != nil {
		if err := r.takeBackJournal.Write(r.head, r.takeBackReason); err != nil {
			a.warn("the take-back reason is not recorded: %v", err)
			journalErr = errors.Join(journalErr, err)
		}
	}
	withheldErr := errors.Join(a.recordWithheld(ctx, r), a.mergeIntoTrunk(r))
	a.hotfixDone(r)
	versionsErr := a.recordVersions(ctx, r)
	pageErr := a.publishReleasePage(ctx, r)
	withheldErr = errors.Join(withheldErr, versionsErr, a.finishFreeze(r, shaRecorded, pageErr, journalErr))
	if r.target.ReleaseNotes == nil {
		return withheldErr
	}
	unset := a.releaseNotes(ctx, r)
	a.deploySweep(ctx, r)
	var missing []string
	if len(unset.fixVersion) > 0 {
		missing = append(missing, "the fix version is not set on "+strings.Join(unset.fixVersion, ", "))
	}
	if len(unset.assignee) > 0 {
		missing = append(missing, "the assignee is not set on "+strings.Join(unset.assignee, ", "))
	}
	if len(missing) > 0 {
		return errors.Join(withheldErr, fmt.Errorf("deployed %s to %s, but %s (see above)", gitx.Short(r.head), r.name, strings.Join(missing, "; ")))
	}
	return withheldErr
}

func (a *App) finishFreeze(r *deployRun, shaRecorded bool, pageErr, journalErr error) error {
	frozen := r.freezeForOwnedTarget()
	if frozen == nil || frozen.branch != r.branch {
		return errors.Join(pageErr, journalErr)
	}
	pendingErr := a.pushPendingFreezeLogs(r.repo, r.cfg, frozen)
	if r.target.ReleasePage == nil {
		a.printf("freeze journal for %s has no release page configured", frozen.name)
	}
	if !shaRecorded || pageErr != nil || journalErr != nil || pendingErr != nil {
		a.warn("freeze stays active at %s until deployed_sha, the release page, and the freeze journal are complete", frozen.name)
		return errors.Join(pageErr, journalErr, pendingErr)
	}
	return a.removeFreezeTags(r.repo, r.cfg, frozen)
}

func (a *App) recordFreezeJournal(r *deployRun) error {
	var errs []error
	if frozen := r.freezeForBranch(); frozen != nil {
		if err := a.pushPendingFreezeLogs(r.repo, r.cfg, frozen); err != nil {
			errs = append(errs, err)
		}
		for _, task := range r.freezeAdvisoryFailed {
			if tag, err := a.freezeLog(r.repo, r.cfg, frozen, r.head, r.name, "advisory", task); err != nil {
				a.warn("freeze-log tag %s was not fully published: %v", tag, err)
				errs = append(errs, err)
			}
		}
	}
	if frozen := r.freezeForRequiredTarget(); frozen != nil && r.freezeLogKind != "" {
		if err := a.pushPendingFreezeLogs(r.repo, r.cfg, frozen); err != nil {
			errs = append(errs, err)
		}
		if tag, err := a.freezeLog(r.repo, r.cfg, frozen, r.head, r.name, r.freezeLogKind, r.freezeLogReason); err != nil {
			a.warn("freeze-log tag %s was not fully published: %v", tag, err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// noteKeys lists the keys the deploy delivers, saying why when there are
// none to note.
func (a *App) noteKeys(r *deployRun) []string {
	if r.prev == "" {
		a.warn("release notes skipped: no previously deployed commit recorded (the next deploy will have one)")
		return nil
	}
	known, err := r.repo.RevExists(r.prev)
	if err != nil {
		a.warn("release notes skipped: %v", err)
		return nil
	}
	if !known {
		a.warn("release notes skipped: the previously deployed commit %s is unknown here (git fetch?)", r.prev)
		return nil
	}
	keys, reverted, quiet, err := deliveredKeys(r)
	if err != nil {
		a.warn("release notes skipped: %v", err)
		return nil
	}
	for _, key := range reverted {
		a.printf("release note for %s not posted: this deploy only takes its work out", key)
	}
	for _, key := range quiet {
		a.warn("release note for %s not posted, %s left alone: its commits in this deploy are %s only", key, key, strings.Join(quietTypes[:len(quietTypes)-1], ", ")+" or "+quietTypes[len(quietTypes)-1])
	}
	if len(keys) == 0 && len(reverted) == 0 && len(quiet) == 0 {
		a.printf("release notes: no work item keys in %s..%s", gitx.Short(r.prev), gitx.Short(r.head))
	}
	return keys
}

// noteText is the release note of the target filled in.
func noteText(r *deployRun) (string, error) {
	values := map[string]string{"target": r.name, "version": r.version, "short": gitx.Short(r.head), "sha": r.head}
	if project := r.cfg.ReleaseNotes.Jira.Project; project != "" {
		values["project"] = project
	}
	comment := r.cfg.NoteComment(r.target)
	if strings.Contains(comment, "{release}") {
		names, _, err := pageRelease(r)
		if err != nil {
			return "", err
		}
		values["release"] = strings.Join(names, ", ")
	}
	comment, err := envs.Fill(comment, values, func(s string) string { return s })
	if err != nil {
		return "", err
	}
	if r.takeBackActive {
		if comment != "" {
			comment += "\n\n"
		}
		comment += "Take-back reason: " + r.takeBackReason
	}
	return comment, nil
}

// notesUnset lists the keys of the items a deploy could not finish.
type notesUnset struct {
	fixVersion, assignee []string
}

// assignFailed reports, one line per item, an item moved but not handed over.
func (a *App) assignFailed(unset *notesUnset, key string, err error) {
	fmt.Fprintf(a.Stderr, "graft: %s: assignee NOT SET: %v\n", key, err)
	unset.assignee = append(unset.assignee, key)
}

func (a *App) warn(format string, args ...any) {
	fmt.Fprintf(a.Stderr, "graft: warning: "+format+"\n", args...)
}

// releaseNotes posts the notes and returns the keys of the items whose fix
// version or assignee, when the target asks for them, is not set.
func (a *App) releaseNotes(ctx context.Context, r *deployRun) notesUnset {
	notes := r.cfg.ReleaseNotes.Jira
	keys := a.noteKeys(r)
	held, err := readHeld(r)
	if err != nil {
		a.warn("%v", err)
	}
	if len(keys) == 0 && len(held) == 0 {
		return notesUnset{}
	}
	fix := &fixVersionRun{app: a, conf: r.target.ReleaseNotes.FixVersion, outsideKeys: outsideDormantKeys(r.cfg)}
	// skipAll reports notes that cannot be posted at all; the fix version of
	// every delivered item is then unknown.
	skipAll := func(format string, err error) notesUnset {
		a.warn(format, err)
		for _, key := range keys {
			fix.failed(key, fmt.Errorf("release notes skipped: %w", err))
		}
		return notesUnset{fixVersion: fix.unset}
	}
	client, err := jiraClient()
	if err != nil {
		return skipAll("release notes skipped: %v", err)
	}
	fix.client = client
	text := ""
	if len(keys) > 0 {
		if text, err = noteText(r); err != nil {
			return skipAll("release notes skipped: %v", err)
		}
	}
	me, err := client.Myself(ctx)
	if err != nil {
		return skipAll("release notes skipped: whose items are whose is unknown: %v", err)
	}
	mover := &noteMover{app: a, client: client, conf: r.target.ReleaseNotes, approved: r.target.ApprovedRelease, me: me, target: r.name, glint: r.glint, standing: notes.SkipKeys}
	sub := &subtaskRun{mover: mover, notes: notes, me: me, reached: map[string]string{}, run: r,
		targetHead: func(context.Context) (string, error) { return r.head, nil }}
	items := &noteItems{app: a, run: r, client: client, me: me, text: text, fix: fix, mover: mover, sub: sub}
	for _, key := range keys {
		items.post(ctx, key)
	}
	judged := slices.DeleteFunc(slices.Clone(keys), func(k string) bool { return slices.Contains(items.unsure, k) })
	unread := a.retryHeld(ctx, sub, held, judged)
	if r.target.ReleaseNotes.Subtasks {
		sub.follow(ctx)
	}
	if err := writeHeld(r, append(unread, mover.held...)); err != nil {
		a.warn("%v", err)
	}
	return notesUnset{fixVersion: fix.unset, assignee: mover.unset.assignee}
}

// noteItems posts the release note to the delivered items and moves them on.
type noteItems struct {
	app    *App
	run    *deployRun
	client *jira.Client
	// me is the owner of the Jira token; text, the note.
	me, text string
	fix      *fixVersionRun
	mover    *noteMover
	sub      *subtaskRun
	// unsure are the keys this deploy could not read or post to: one held
	// before stays held, moved when it may be.
	unsure           []string
	fallbackReported map[string]bool
}

// notifyFallback reports once per task that Jira refused notifyUsers=false.
func (n *noteItems) notifyFallback(key string) {
	if n.fallbackReported == nil {
		n.fallbackReported = map[string]bool{}
	}
	if n.fallbackReported[key] {
		return
	}
	n.fallbackReported[key] = true
	n.app.printf("release note for %s: Jira rejected notifyUsers=false; retried with notifications enabled", key)
}

// post gives one delivered item its note, its fix version and its move.
func (n *noteItems) post(ctx context.Context, key string) {
	a, notes := n.app, n.run.cfg.ReleaseNotes.Jira
	if out := n.run.moveOut; out != nil && slices.Contains(out.keys, key) {
		a.printf("release note for %s not posted: this deploy reverts its work from the candidate", key)
		return
	}
	if slices.Contains(notes.SkipKeys, key) {
		a.printf("release note for %s not posted, %s left alone: release_notes.jira.skip_keys", key, key)
		return
	}
	issue, err := n.client.Issue(ctx, key)
	if err != nil {
		a.warn("release note for %s not posted: %v", key, err)
		n.fix.failed(key, err)
		n.unsure = append(n.unsure, key)
		return
	}
	if n.mover.leaveDormant(issue) {
		return
	}
	// A done item only learns what shipped: its status and versions stay,
	// and its subtasks go by their own commits.
	learns := issue.Done
	if !learns {
		n.sub.delivered = append(n.sub.delivered, issue)
		n.sub.reached[issue.Key] = issue.Status
	}
	yours, err := a.noteable(ctx, n.client, notes, n.me, issue)
	if err != nil {
		a.warn("release note for %s not posted: %v", key, err)
		if !learns {
			n.fix.failed(key, err)
		}
		n.unsure = append(n.unsure, key)
		return
	}
	if !yours {
		return
	}
	quietReason := quietNoteReason(notes.Quiet, issue)
	posted := quietReason == ""
	if posted {
		if err := n.client.Comment(ctx, key, n.text); err != nil {
			a.warn("release note for %s not posted: %v", key, err)
			posted = false
			n.unsure = append(n.unsure, key)
		} else {
			a.printf("release note posted to %s", key)
		}
	} else {
		a.printf("release note for %s not posted: release_notes.jira.quiet (%s)", key, quietReason)
	}
	if learns {
		return
	}
	n.fix.add(ctx, issue, quietReason != "", func() { n.notifyFallback(key) })
	if posted || quietReason != "" {
		n.sub.reached[issue.Key] = n.mover.delivered(ctx, issue)
	}
}

// noteMover moves items by the target's transitions and hands them over to
// the transition's assignee.
type noteMover struct {
	app    *App
	client *jira.Client
	conf   *config.TargetNotes
	// me is the owner of the Jira token, whose comment require_comment
	// asks for; target names the deploy target.
	me, target string
	unset      notesUnset
	// held are the items left where they were for want of the comment or
	// for the glint findings.
	held []heldItem
	// glint is the check of release_notes.glint.
	glint *glintCheck
	// subtasksStay keeps a subtask with its assignee on a handover: the
	// reviewer checks the item, not each subtask. standing are the items
	// release_notes.jira.skip_keys names: no reviewer gets those, so their
	// subtasks are handed over.
	standing []string
	// namedHandover identifies explicit keys whose assignment failures must
	// keep the handover held and fail the command.
	namedHandover map[string]bool
	approved      *config.ApprovedRelease
	readOnly      bool
}

// errHeld is a move withheld for want of the comment require_comment asks
// for, or for the glint findings, reported already.
var errHeld = errors.New("held")

var errTypeNotAllowed = errors.New("transition does not allow this issue type")
var errDormant = errors.New("dormant item stays in place")

func (m *noteMover) dormantEntry(issue jira.Issue) (config.DormantReleaseItem, bool) {
	if m == nil {
		return config.DormantReleaseItem{}, false
	}
	return m.approved.DormantFor(issue.Key, issue.Parent)
}

func (m *noteMover) leaveDormant(issue jira.Issue) bool {
	item, ok := m.dormantEntry(issue)
	if !ok {
		return false
	}
	m.app.printf("%s", dormantLeftMessage(item, issue))
	return true
}

func dormantLeftMessage(item config.DormantReleaseItem, issue jira.Issue) string {
	why := item.Reason
	if item.DisabledBy != "" {
		why += "; disabled by " + item.DisabledBy
	}
	if item.Key != issue.Key && issue.Parent == item.Key {
		return fmt.Sprintf("%s ships dormant with its parent %s (%s): left in %s, not moved", issue.Key, item.Key, why, issue.Status)
	}
	return fmt.Sprintf("%s ships dormant (%s): left in %s, not moved", issue.Key, why, issue.Status)
}

func outsideDormantKeys(cfg *config.Config) map[string]bool {
	keys := map[string]bool{}
	if cfg == nil || cfg.Deploy == nil {
		return keys
	}
	for _, target := range cfg.Deploy.Targets {
		if target != nil && target.ApprovedRelease != nil {
			for _, item := range target.ApprovedRelease.Dormant {
				if item.OutsideVersion != "" {
					keys[item.Key] = true
				}
			}
		}
	}
	return keys
}

func quietNoteReason(q *config.QuietNotes, issue jira.Issue) string {
	if q == nil {
		return ""
	}
	if q.Subtasks && issue.Subtask {
		return "subtask"
	}
	for _, label := range q.Labels {
		if slices.Contains(issue.Labels, label) {
			return "label " + label
		}
	}
	return ""
}

// delivered moves a delivered item along only from the statuses a transition
// names: a paused item stays paused whatever its commits say. It returns the
// status the item is left in.
func (m *noteMover) delivered(ctx context.Context, issue jira.Issue) string {
	if m.leaveDormant(issue) {
		return issue.Status
	}
	to, t := m.conf.DeliveredStatus(issue.Status, issue.Type)
	switch {
	case len(m.conf.Transitions) == 0 || m.conf.Leading(issue.Status) != nil:
	case t == nil:
		m.app.printf("%s stays %s: release_notes.transitions move only from %s", issue.Key, issue.Status, strings.Join(m.conf.FromStatuses(), ", "))
	default:
		if err := m.move(ctx, issue, t, ""); err != nil {
			if !errors.Is(err, errHeld) && !errors.Is(err, errTypeNotAllowed) && !errors.Is(err, errDormant) {
				m.app.warn("%v", err)
			}
			return issue.Status
		}
		return to
	}
	return issue.Status
}

func transitionTypeStayMessage(key, status, after string, t *config.NoteTransition) string {
	how := ""
	if after != "" {
		how = " after its parent " + after
	}
	return fmt.Sprintf("%s stays %s%s: the transition to %s moves only %s", key, status, how, t.To, t.TypeNames())
}

// move takes the item by the transition and hands it over; after names the
// parent a subtask follows. An item moved but not handed over is reported
// and recorded, not returned: the move itself happened. An item to be handed
// over without the comment require_comment asks for, or any with the glint
// findings release_notes.glint asks about, is not moved: errHeld.
func (m *noteMover) move(ctx context.Context, issue jira.Issue, t *config.NoteTransition, after string) error {
	if m.leaveDormant(issue) {
		return errDormant
	}
	how := ""
	if after != "" {
		how = " after its parent " + after
	}
	if !t.AllowsType(issue.Type) {
		fmt.Fprintln(m.app.Stderr, transitionTypeStayMessage(issue.Key, issue.Status, after, t))
		return errTypeNotAllowed
	}
	if why := m.glint.held(ctx, m.client); why != "" {
		fmt.Fprintf(m.app.Stderr, "graft: %s: NOT MOVED to %s%s: %s; once glint finds nothing in the deployed commit, the next deploy to %s or graft deploy handover %s moves it, at a release acceptance --glint-ticket KEY names an item for all the findings\n", issue.Key, t.To, how, why, m.target, m.target)
		m.held = append(m.held, heldItem{key: issue.Key, after: after})
		return errHeld
	}
	handover := t.Assign != "" && issue.AssigneeID != t.Assign
	label := ""
	if handover {
		var err error
		if label, err = m.keptBy(ctx, issue, t); err != nil {
			// Unknown labels hand the item over, as without the setting.
			m.app.warn("%s: the labels of its parent %s are unknown: %v", issue.Key, issue.Parent, err)
		}
	}
	if err := m.requireInstruction(ctx, issue, t, after, handover, label, how); err != nil {
		return err
	}
	if m.readOnly {
		if err := jira.TransitionAvailable(issue, t.To); err != nil {
			return err
		}
		hand := ""
		if handover && label == "" {
			hand = "; assign " + t.Assign
		} else if label != "" {
			hand = "; keep assignee: " + label
		}
		m.app.printf("- %s: %s → %s%s%s", issue.Key, issue.Status, t.To, how, hand)
		return nil
	}
	if err := m.client.Transition(ctx, issue, t.To); err != nil {
		return fmt.Errorf("%s not moved to %s%s: %w", issue.Key, t.To, how, err)
	}
	m.app.printf("%s moved to %s%s%s", issue.Key, t.To, how, m.glint.takenBy())
	if !handover {
		return nil
	}
	if label != "" {
		m.app.printf("%s stays with its assignee: %s", issue.Key, label)
		return nil
	}
	if err := m.client.Assign(ctx, issue.Key, t.Assign); err != nil {
		if m.namedHandover[issue.Key] {
			fmt.Fprintf(m.app.Stderr, "graft: %s: assignee NOT SET (%v)\n", issue.Key, err)
			m.unset.assignee = append(m.unset.assignee, issue.Key)
		} else {
			m.app.assignFailed(&m.unset, issue.Key, err)
		}
		return nil
	}
	m.app.printf("%s assigned to %s", issue.Key, t.Assign)
	return nil
}

func (m *noteMover) requireInstruction(ctx context.Context, issue jira.Issue, t *config.NoteTransition, after string, handover bool, label, how string) error {
	if !handover || label != "" || t.RequireComment == "" {
		return nil
	}
	if why := m.uninstructed(ctx, issue, t, after); why != "" {
		fmt.Fprintf(m.app.Stderr, "graft: %s: NOT MOVED to %s%s: %s; once it is written, graft deploy handover %s moves it\n", issue.Key, t.To, how, why, m.target)
		m.held = append(m.held, heldItem{key: issue.Key, after: after})
		return errHeld
	}
	return nil
}

// keptBy says what keeps the item with its assignee: being a subtask, with
// subtasksStay, or a label of the transition's keep_assignee_labels, its own
// or its parent's: the subtasks of an internal parent are internal. It is ""
// for nothing.
func (m *noteMover) keptBy(ctx context.Context, issue jira.Issue, t *config.NoteTransition) (string, error) {
	if issue.Parent != "" && !slices.Contains(m.standing, issue.Parent) {
		return "a subtask of " + issue.Parent, nil
	}
	kept := func(labels []string) string {
		if i := slices.IndexFunc(labels, func(l string) bool { return slices.Contains(t.KeepAssigneeLabels, l) }); i >= 0 {
			return labels[i]
		}
		return ""
	}
	if label := kept(issue.Labels); label != "" {
		return "label " + label + " (keep_assignee_labels)", nil
	}
	if len(t.KeepAssigneeLabels) == 0 || issue.Parent == "" {
		return "", nil
	}
	parent, err := m.client.Issue(ctx, issue.Parent)
	if err != nil {
		return "", err
	}
	if label := kept(parent.Labels); label != "" {
		return "label " + label + " of its parent " + issue.Parent + " (keep_assignee_labels)", nil
	}
	return "", nil
}

// subtaskRun moves subtasks after their parents once the notes are posted.
type subtaskRun struct {
	mover     *noteMover
	notes     *config.JiraNotes
	me        string
	delivered []jira.Issue
	// reached maps the keys of the delivered items to the status each is
	// left in.
	reached    map[string]string
	run        *deployRun
	targetHead func(context.Context) (string, error)
	commits    map[string][]loggedCommit
	landed     map[string]bool
	loaded     bool
}

// follow moves the subtasks of the delivered items that are in a status a
// transition leads to.
func (s *subtaskRun) follow(ctx context.Context) {
	seen := map[string]bool{}
	for i := range s.delivered {
		issue := &s.delivered[i]
		if t := s.mover.conf.Leading(s.reached[issue.Key]); t != nil {
			for _, key := range issue.Subtasks {
				s.move(ctx, key, issue.Key, t, seen)
			}
		}
	}
}

// move takes one subtask by the transition when it is in the transition's
// from, as a delivered item, and is the token owner's: a subtask further on
// stays, Ready for release when its parent comes to Testing, say.
func (s *subtaskRun) move(ctx context.Context, key, parent string, t *config.NoteTransition, seen map[string]bool) {
	if seen[key] {
		return
	}
	seen[key] = true
	app := s.mover.app
	issue, err := s.mover.client.Issue(ctx, key)
	if err != nil {
		app.warn("%s not moved after its parent %s: %v", key, parent, err)
		return
	}
	if s.mover.leaveDormant(issue) {
		return
	}
	if issue.Done || issue.Status == t.To {
		return
	}
	if slices.Contains(s.notes.SkipKeys, key) {
		app.printf("%s stays %s after its parent %s: release_notes.jira.skip_keys", key, issue.Status, parent)
		return
	}
	if !slices.Contains(t.From, issue.Status) {
		app.printf("%s stays %s after its parent %s: the transition to %s moves only from %s", key, issue.Status, parent, t.To, strings.Join(t.From, ", "))
		return
	}
	yours, err := app.noteable(ctx, s.mover.client, s.notes, s.me, issue)
	if err != nil {
		app.warn("%s not moved after its parent %s: %v", key, parent, err)
		return
	}
	if !yours {
		return
	}
	if s.run != nil {
		missing, err := s.commitsOffTarget(ctx, key)
		if err != nil {
			app.warn("%s not moved after its parent %s: checking its commits: %v", key, parent, err)
			return
		}
		if len(missing) > 0 {
			app.printf("%s stays %s after its parent %s: its commits not on %s: %s", key, issue.Status, parent, s.mover.target, strings.Join(missing, ", "))
			return
		}
	}
	if err := s.mover.move(ctx, issue, t, parent); err != nil && !errors.Is(err, errHeld) && !errors.Is(err, errTypeNotAllowed) {
		app.warn("%v", err)
	}
}

// commitsOffTarget lazily loads the remote commits the deployed head lacks,
// then names, by short hash, those of this subtask whose change is not on the
// target by content either: a commit on the target is, whatever its hash.
func (s *subtaskRun) commitsOffTarget(ctx context.Context, key string) ([]string, error) {
	if err := s.loadCommits(ctx); err != nil {
		return nil, err
	}
	head, err := s.targetHead(ctx)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, c := range s.commits[key] {
		content, ok := s.landed[c.sha]
		if !ok && c.parent != "" {
			content, err = landed(s.run, head, c.sha, c.parent, s.run.target.ApprovedRelease)
			if err != nil {
				return nil, fmt.Errorf("checking %s: %w", c.sha, err)
			}
		}
		s.landed[c.sha] = content
		if !content {
			missing = append(missing, c.sha[:12])
		}
	}
	return missing, nil
}

// loadCommits reads, once, on the first eligible subtask, the non-merge
// commits on remotes that the deployed head lacks: only they can keep a
// subtask back, so a long history on the target costs nothing.
func (s *subtaskRun) loadCommits(ctx context.Context) error {
	if s.loaded {
		return nil
	}
	head, err := s.targetHead(ctx)
	if err != nil {
		return err
	}
	commits, err := sweepLog(s.run, time.Time{}, "--remotes", "--not", head)
	if err != nil {
		return fmt.Errorf("reading remote commit history: %w", err)
	}
	keyRe := projectKeyRe(s.run.cfg)
	s.commits = map[string][]loggedCommit{}
	s.landed = map[string]bool{}
	for _, c := range commits {
		for _, key := range commitKeys(keyRe, s.run.target.ApprovedRelease, c.deliveredCommit) {
			s.commits[key] = append(s.commits[key], c)
		}
	}
	s.loaded = true
	return nil
}

// noteable tells whether an item gets a note, warning when it does not. A key
// can reach a commit by hand, as a reference to an item that is closed or
// somebody else's; only a subject that opens with such a reference counts,
// so a done item here has its work in this deploy,
// closed by another product's deploy say, and gets the note. An item handed over for review stays yours: its
// history shows it assigned to you.
func (a *App) noteable(ctx context.Context, client *jira.Client, notes *config.JiraNotes, me string, issue jira.Issue) (bool, error) {
	key := issue.Key
	if issue.NotStarted || slices.Contains(notes.SkipStatuses, issue.Status) {
		a.warn("release note for %s not posted, %s left alone: it is %s", key, key, issue.Status)
		return false, nil
	}
	if issue.AssigneeID == me {
		return true, nil
	}
	yours, err := client.WasAssignedTo(ctx, key, me)
	if err != nil || yours {
		return yours, err
	}
	whose := "unassigned"
	if issue.AssigneeID != "" {
		whose = "assigned to " + issue.AssigneeName
	}
	a.warn("release note for %s not posted, %s left alone: %s and never assigned to the owner of the Jira token", key, key, whose)
	return false, nil
}

// fixVersionRun gives the noted items of one deploy the next version of their
// project, reading each project's versions once.
type fixVersionRun struct {
	app         *App
	conf        *config.FixVersion
	client      *jira.Client
	next        map[string]fixChoice
	unset       []string
	outsideKeys map[string]bool
}

type fixChoice struct {
	version jira.Version
	err     error
}

// add sets the fix version of a noted item, unless it has it already.
func (f *fixVersionRun) add(ctx context.Context, issue jira.Issue, quiet bool, fallback func()) {
	if f.conf == nil {
		return
	}
	if f.outsideKeys[issue.Key] {
		f.app.printf("fix version left unchanged for %s: declared dormant outside a version", issue.Key)
		return
	}
	project, _, _ := strings.Cut(issue.Key, "-")
	choice, ok := f.next[project]
	if !ok {
		versions, err := f.client.ProjectVersions(ctx, project)
		if err == nil {
			err = f.freezeApproved(ctx, versions)
		}
		switch {
		case err != nil:
		case f.conf.Template != nil:
			choice.version, err = jira.NextDatedVersion(project, versions, f.conf.Template)
		default:
			choice.version, err = jira.NextVersion(project, versions, f.conf.Prefix)
		}
		choice.err = err
		if f.next == nil {
			f.next = map[string]fixChoice{}
		}
		f.next[project] = choice
	}
	if choice.err != nil {
		f.failed(issue.Key, choice.err)
		return
	}
	v := choice.version
	if slices.ContainsFunc(issue.FixVersions, func(have jira.Version) bool { return have.ID == v.ID }) {
		f.app.printf("%s already has fix version %s", issue.Key, v.Name)
		return
	}
	retried, err := f.client.AddFixVersionNotify(ctx, issue.Key, v.ID, quiet)
	if retried && fallback != nil {
		fallback()
	}
	if err != nil {
		f.failed(issue.Key, err)
		return
	}
	f.app.printf("fix version %s added to %s", v.Name, issue.Key)
}

// freezeApproved marks the candidate versions whose approval has begun: the
// approved set of items is theirs, and a later deploy adds nothing to it.
func (f *fixVersionRun) freezeApproved(ctx context.Context, versions []jira.Version) error {
	for i, v := range versions {
		if v.Released || v.Archived || !f.conf.Matches(v.Name) {
			continue
		}
		approvers, err := f.client.VersionApprovers(ctx, v.ID)
		if err != nil {
			return err
		}
		if jira.ApprovalBegun(approvers) {
			versions[i].Frozen = true
			f.app.printf("%s is being approved: no item joins it", v.Name)
		}
	}
	return nil
}

// failed reports, one line per item, a fix version that is not set.
func (f *fixVersionRun) failed(key string, err error) {
	if f.conf == nil {
		return
	}
	fmt.Fprintf(f.app.Stderr, "graft: %s: fix version NOT SET: %v\n", key, err)
	f.unset = append(f.unset, key)
}

// checkApproved lets out only the commits of checked items: every commit going
// out names an item in one of the statuses, with an approved version of the
// prefix, or both, as the target asks. A revert takes nothing new out and
// passes; a merge brings in commits that are checked themselves. Commits a
// release branch withheld from an earlier release go out now and are
// checked with the rest.
func (a *App) checkApproved(ctx context.Context, r *deployRun, skip bool) error {
	conf := r.target.ApprovedRelease
	if conf == nil {
		return nil
	}
	if skip {
		a.collectApproved(ctx, r)
		return nil
	}
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("deploy %s: nothing was deployed: approved release: "+format, append([]any{r.name}, args...)...)
	}
	out, g, err := a.approvalRun(ctx, r)
	if err != nil {
		if conf.Version != nil {
			id := "commit-items"
			if ref := conf.Version.Refs[id]; ref != "" {
				id += " (rule " + ref + ")"
			}
			return refuse("%s: %v; fix the issue or retry", id, err)
		}
		return refuse("%v", err)
	}
	r.dormantItems = config.UniqueDormant(out.dormant)
	var failures []error
	if len(out.failures) > 0 {
		lines := make([]string, len(out.failures))
		for i, f := range out.failures {
			lines[i] = f.line()
		}
		id := ""
		if conf.Version != nil {
			id = "commit-items"
			if ref := conf.Version.Refs[id]; ref != "" {
				id += " (rule " + ref + ")"
			}
			id += ": "
		}
		items := g.gate.describe(out.failures)
		if conf.Prefix != "" {
			items = append(items, g.outsideHints(ctx, out.failures)...)
		}
		failures = append(failures, refuse("%scommits outside an approved release:\n  %s\nTheir items:\n  %s\n%s, revert them, or pass --unapproved", id,
			strings.Join(lines, "\n  "), strings.Join(items, "\n  "), g.gate.remedy()))
		if !r.dryRun {
			return failures[0]
		}
	}
	if conf.Version != nil {
		failures = append(failures, g.checkReleaseVersion(ctx, r.dryRun)...)
	}
	r.dormantItems = config.UniqueDormant(r.dormantItems, g.dormant)
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	out.dormant = r.dormantItems
	a.passApproved(r, out)
	return nil
}

// collectApproved preserves the results for the page without blocking a deploy.
func (a *App) collectApproved(ctx context.Context, r *deployRun) {
	a.warn("collecting approved release (--unapproved): refusals do not stop the deploy")
	out, g, err := a.approvalRun(ctx, r)
	if err != nil {
		r.approvalCollectionErr = err
		a.warn("approved release collection failed (--unapproved): %v", err)
		return
	}
	r.passed = uniqueSorted(out.passed)
	r.pageWithheld = out.withheld
	if r.target.ApprovedRelease.Prefix != "" {
		r.approvedVersions = slices.Sorted(maps.Keys(out.approved))
	}
	r.approvalExceptions = out.failures
	r.dormantItems = config.UniqueDormant(out.dormant)
	for _, f := range out.failures {
		a.warn("shipped by exception (--unapproved): %s", f.line())
	}
	if r.target.ApprovedRelease.Version != nil {
		for _, failure := range g.checkReleaseVersion(ctx, true) {
			r.approvalReasons = append(r.approvalReasons, failure.Error())
			a.warn("shipped by exception (--unapproved): %v", failure)
		}
		r.approvalItemExceptions = g.versionItemFailures
		r.dormantItems = config.UniqueDormant(r.dormantItems, g.dormant)
		r.approvalCollectionErr = g.collectionErr
		if g.collectionErr != nil {
			a.warn("approved release collection failed (--unapproved): %v", g.collectionErr)
		}
	}
}

// approvalRun checks the commits from the deployed one to HEAD and those
// withheld before against approved_release.
func (a *App) approvalRun(ctx context.Context, r *deployRun) (gateOutcome, *gateRun, error) {
	if r.prev == "" {
		return gateOutcome{}, nil, errors.New("no deployed commit recorded, what goes out is unknown; check it by hand and pass --unapproved")
	}
	known, err := r.repo.RevExists(r.prev)
	if err != nil {
		return gateOutcome{}, nil, err
	}
	if !known {
		return gateOutcome{}, nil, fmt.Errorf("the deployed commit %s is unknown here (git fetch?)", r.prev)
	}
	withheld, err := r.readWithheld(ctx)
	if err != nil {
		return gateOutcome{}, nil, err
	}
	g, err := newGateRun(a, r.repo, r.cfg, r.target)
	if err != nil {
		return gateOutcome{}, nil, err
	}
	var deployedPaths map[string]bool
	if len(withheld) > 0 {
		if deployedPaths, err = repoPaths(r.repo, r.prev, r.head); err != nil {
			return gateOutcome{}, nil, fmt.Errorf("checking files changed since the deployed commit: %w", err)
		}
		if deployedPaths, err = excludeVersionOnlyBumps(r.repo, r.prev, r.head, deployedPaths, r.cfg.Version); err != nil {
			return gateOutcome{}, nil, fmt.Errorf("checking version files changed since the deployed commit: %w", err)
		}
	}
	active, reverted, err := partitionWithheld(r.repo, r.prev, r.head, withheld, r.cfg.VersionFiles(), deployedPaths)
	if err != nil {
		return gateOutcome{}, nil, err
	}
	buildBranch := config.BranchMatches(r.cfg.Version.Build, r.branch)
	out, err := g.run(ctx, r.prev, r.head, active, buildBranch)
	out.withheld = append(reverted, out.withheld...)
	return out, g, err
}

// partitionWithheld applies the path filter only to withheld commits already
// in the deployed history. Other commits retain the original check.
func partitionWithheld(repo *gitx.Repo, prev, head string, withheld, versionFiles []string, deployedPaths map[string]bool) (active, reverted []string, retErr error) {
	if len(withheld) == 0 {
		return nil, nil, nil
	}
	isReverted, err := withheldRevertState(repo, head, withheld)
	if err != nil {
		return nil, nil, err
	}
	active = make([]string, 0, len(withheld))
	reverted = make([]string, 0, len(withheld))
	versionPathSet := make(map[string]bool, len(versionFiles))
	for _, p := range versionFiles {
		versionPathSet[path.Clean(p)] = true
	}
	for _, sha := range withheld {
		inPrev, err := repo.IsAncestor(sha, prev)
		if err != nil {
			return nil, nil, fmt.Errorf("checking whether withheld commit %s is in deployed history: %w", gitx.Short(sha), err)
		}
		if !inPrev {
			check, err := withheldCheckAsHead(repo, head, sha, isReverted[sha], versionFiles)
			if err != nil {
				return nil, nil, fmt.Errorf("checking withheld commit %s in deployed history: %w", gitx.Short(sha), err)
			}
			if check {
				active = append(active, sha)
			} else {
				reverted = append(reverted, sha)
			}
			continue
		}
		paths, err := withheldPaths(repo, sha)
		if err != nil {
			return nil, nil, fmt.Errorf("checking files changed by withheld commit %s: %w", gitx.Short(sha), err)
		}
		if len(paths) > 0 && onlyVersionPaths(paths, versionPathSet) {
			// Version-only commits use the original check without excluding
			// every path, which would make an empty diff vacuously present.
			if isReverted[sha] {
				present, err := deploy.ChangePresent(repo, head, sha)
				if err != nil {
					return nil, nil, fmt.Errorf("checking withheld commit %s in deployed history: %w", gitx.Short(sha), err)
				}
				if !present {
					reverted = append(reverted, sha)
					continue
				}
			}
			active = append(active, sha)
			continue
		}
		if len(paths) == 0 || !intersectsPaths(paths, deployedPaths) {
			// No path this deploy can ship was touched; keep the marker intact.
			reverted = append(reverted, sha)
		} else {
			// A touched path may carry any part of W, restored or not: W is
			// checked.
			active = append(active, sha)
		}
	}
	return active, reverted, nil
}

func withheldRevertState(repo *gitx.Repo, head string, withheld []string) (map[string]bool, error) {
	out, err := repo.Git("log", "--reverse", "--format=%H%x00%s%x00%b%x00%x1e", head)
	if err != nil {
		return nil, err
	}
	isReverted := map[string]bool{}
	revertTargets := map[string]string{}
	for record := range strings.SplitSeq(out, "\x1e") {
		parts := strings.SplitN(strings.TrimSpace(record), "\x00", 3)
		if len(parts) != 3 {
			continue
		}
		sha, subject, body := strings.TrimSpace(parts[0]), parts[1], parts[2]
		if !isRevert(subject) {
			continue
		}
		for _, match := range revertsRe.FindAllStringSubmatch(body, -1) {
			target, ambiguous := withheldRevertTarget(match[1], withheld, revertTargets)
			if ambiguous {
				return nil, fmt.Errorf("%s reverts ambiguous commit prefix %s", gitx.Short(sha), match[1])
			}
			if target == "" {
				continue
			}
			isReverted[target] = !isReverted[target]
			revertTargets[sha] = target
		}
	}
	return isReverted, nil
}

func onlyVersionPaths(paths []string, versionPathSet map[string]bool) bool {
	for _, p := range paths {
		if !versionPathSet[path.Clean(p)] {
			return false
		}
	}
	return true
}

// withheldCheckAsHead preserves the original partition rule for commits that
// were not ancestors of the deployed commit.
func withheldCheckAsHead(repo *gitx.Repo, head, sha string, isReverted bool, versionFiles []string) (bool, error) {
	if !isReverted {
		return true, nil
	}
	return deploy.ChangePresentExcept(repo, head, sha, versionFiles...)
}

func repoPaths(repo *gitx.Repo, from, to string) (map[string]bool, error) {
	out, err := repo.Git("diff", "--no-renames", "--name-only", "-z", from, to)
	if err != nil {
		return nil, err
	}
	paths := map[string]bool{}
	for p := range strings.SplitSeq(out, "\x00") {
		if p == "" {
			continue
		}
		paths[p] = true
	}
	return paths, nil
}

func withheldPaths(repo *gitx.Repo, sha string) ([]string, error) {
	out, err := repo.Git("diff-tree", "-r", "--root", "-m", "--first-parent", "--diff-merges=first-parent", "--no-commit-id", "--no-renames", "--name-only", "-z", sha)
	if err != nil {
		return nil, err
	}
	return strings.FieldsFunc(out, func(r rune) bool { return r == '\x00' }), nil
}

func intersectsPaths(paths []string, changed map[string]bool) bool {
	for _, p := range paths {
		if changed[p] {
			return true
		}
	}
	return false
}

func excludeVersionOnlyBumps(repo *gitx.Repo, prev, head string, changed map[string]bool, v config.Version) (map[string]bool, error) {
	targets := versionTargetsByPath(v)
	changedVersionFiles := make([]string, 0, len(targets))
	for file := range targets {
		if changed[file] {
			changedVersionFiles = append(changedVersionFiles, file)
		}
	}
	if len(changedVersionFiles) == 0 {
		return changed, nil
	}
	prevTree, err := versionBlobTree(repo, prev)
	if err != nil {
		return nil, fmt.Errorf("reading deployed version tree: %w", err)
	}
	headTree, err := versionBlobTree(repo, head)
	if err != nil {
		return nil, fmt.Errorf("reading current version tree: %w", err)
	}
	for _, file := range changedVersionFiles {
		bump, err := versionFileIsVersionBump(repo, file, prevTree, headTree, targets[file])
		if err != nil { //nolint:silent-error-handling // A version file that does not parse is not a proven bump: its path stays changed and the withheld commits on it are checked.
			continue
		}
		if bump {
			delete(changed, file)
		}
	}
	return changed, nil
}

func withheldRevertTarget(prefix string, withheld []string, revertTargets map[string]string) (string, bool) {
	var target string
	consider := func(candidate string) bool {
		if target != "" && target != candidate {
			return false
		}
		target = candidate
		return true
	}
	for _, sha := range withheld {
		if strings.HasPrefix(sha, prefix) && !consider(sha) {
			return "", true
		}
	}
	for _, revert := range slices.Sorted(maps.Keys(revertTargets)) {
		if strings.HasPrefix(revert, prefix) && !consider(revertTargets[revert]) {
			return "", true
		}
	}
	return target, false
}

// passApproved records what an approved release with no failures lets out.
func (a *App) passApproved(r *deployRun, out gateOutcome) {
	r.withheld, r.released, r.gated, r.passed = out.withheld, out.released, true, uniqueSorted(out.passed)
	if r.target.ApprovedRelease.Prefix != "" {
		r.approvedVersions = slices.Sorted(maps.Keys(out.approved))
	}
	a.printf("approved release: %d commits since %s, all in %s", out.commits, gitx.Short(r.prev), strings.Join(slices.Sorted(maps.Keys(out.approved)), ", "))
	for _, item := range out.dormant {
		a.printf("%s", item.Line())
	}
}

// gateRun is one pass of approved_release over the commits going out.
type gateRun struct {
	app                 *App
	repo                *gitx.Repo
	cfg                 *config.Config
	conf                *config.ApprovedRelease
	gate                *approvalGate
	keyRe               *regexp.Regexp
	activeKeys          map[string]bool
	activeCommits       int
	dormant             []config.DormantReleaseItem
	collectionErr       error
	versionItemFailures map[string][]string
	// liveVersions are the approved versions of the items without a dormant
	// entry that go out (prefix without version); outsidePending, the items
	// let out by an outside_version entry until the run tells whether live
	// work of that version goes out too.
	liveVersions   map[string]jira.Version
	outsidePending []outsidePending
	// advised are the items already told their outside_version is stale.
	advised map[string]bool
	// liveKeys are the keys of the commits that are live work (with version:
	// shipsLiveWork), which alone anchor outside_version entries.
	liveKeys map[string]bool
}

// outsidePending is an item that passed by its outside_version entry alone.
type outsidePending struct {
	commit deliveredCommit
	item   config.DormantReleaseItem
}

func newGateRun(a *App, repo *gitx.Repo, cfg *config.Config, target *config.DeployTarget) (*gateRun, error) {
	client, err := jiraClient()
	if err != nil {
		return nil, err
	}
	return &gateRun{
		app: a, repo: repo, cfg: cfg, conf: target.ApprovedRelease,
		gate:         newApprovalGate(client, cfg, target, map[string]jira.Issue{}),
		keyRe:        projectKeyRe(cfg),
		activeKeys:   map[string]bool{},
		liveVersions: map[string]jira.Version{},
	}, nil
}

// projectKeyRe matches the work item keys of the project.
func projectKeyRe(cfg *config.Config) *regexp.Regexp {
	return regexp.MustCompile(`\b(` + strings.Join(cfg.ReleaseNotes.Jira.ProjectKeys, "|") + `)-[0-9]+\b`)
}

// gateFailure is a commit outside the release.
type gateFailure struct {
	commit deliveredCommit
	keys   []string
	why    string
}

func (f gateFailure) line() string {
	return gitx.Short(f.commit.sha) + " " + f.commit.subject + ": " + f.why
}

type gateOutcome struct {
	commits  int
	approved map[string]bool
	failures []gateFailure
	// withheld are the commits reverted on a build branch: the trunk still
	// has them, and the next release checks them.
	withheld []string
	// passed are the items named by the commits let out.
	passed  []string
	dormant []config.DormantReleaseItem
	// released are the commits withheld from an earlier release that go out
	// now: outside prev..head, they are delivered all the same.
	released []string
}

var revertsRe = regexp.MustCompile(`(?m)^This reverts commit ([0-9a-f]{7,40})\b`)

// run checks the commits of prev..head and the commits withheld before.
// From a build branch (a release branch cut at the cutoff) a commit reverted
// in the range is withheld rather than gone.
func (g *gateRun) run(ctx context.Context, prev, head string, withheld []string, buildBranch bool) (gateOutcome, error) {
	out := gateOutcome{approved: map[string]bool{}}
	commits, err := deliveredCommits(g.repo, prev, head)
	if err != nil {
		return out, err
	}
	out.commits = len(commits)
	reverted, err := g.reverted(commits)
	if err != nil {
		return out, err
	}
	all, earlier, err := g.withEarlier(commits, withheld)
	if err != nil {
		return out, err
	}
	for _, c := range all {
		line := gitx.Short(c.sha) + " " + c.subject
		if isRevert(c.subject) {
			g.app.printf("approved release: %s: a revert, passes", line)
			continue
		}
		if by, ok := reverted[c.sha]; ok {
			if buildBranch {
				out.withheld = append(out.withheld, c.sha)
				g.app.printf("approved release: %s: reverted by %s on this release branch, withheld: the trunk keeps it and the next release checks it", line, gitx.Short(by))
			} else {
				g.app.printf("approved release: %s: reverted by %s, passes", line, gitx.Short(by))
			}
			continue
		}
		fail, err := g.check(ctx, c, line, &out)
		if err != nil {
			return out, err
		}
		switch {
		case fail != nil:
			out.failures = append(out.failures, *fail)
		case earlier[c.sha]:
			out.released = append(out.released, c.sha)
		}
	}
	linkFailures, err := g.linkOutside(ctx)
	if err != nil {
		return out, err
	}
	out.failures = append(out.failures, linkFailures...)
	if err := g.adviseApproved(ctx, out.approved); err != nil {
		return out, err
	}
	out.dormant = config.UniqueDormant(out.dormant, g.dormant)
	return out, nil
}

// linkOutside settles the items let out by an outside_version entry alone,
// without version (with it, the release version check does): the entry is
// linked when live work of its version goes out in this run; otherwise it is
// the old dormant form, which needs an approved version of the item's own.
func (g *gateRun) linkOutside(ctx context.Context) ([]gateFailure, error) {
	var failures []gateFailure
	var whys [][]string
	byCommit := map[string]int{}
	told := map[string]bool{}
	for _, p := range g.outsidePending {
		key, version := p.item.Key, p.item.OutsideVersion
		anchor, live := g.liveVersions[version]
		linked, err := g.gate.outsideLinked(ctx, p.item, anchor, live)
		if err != nil {
			return nil, err
		}
		if linked {
			g.dormant = config.UniqueDormant(g.dormant, []config.DormantReleaseItem{p.item})
			if !told[key] {
				told[key] = true
				g.app.printf("approved release: %s ships dormant outside %s, not checked against the version", key, version)
			}
			continue
		}
		why := g.gate.notInApprovedVersion(g.gate.issues[key]) + " (dormant: still needs an approved version; its outside_version entry is for " + version + ", and no live work of " + version + ", unreleased and approved, goes out in this release)"
		if i, ok := byCommit[p.commit.sha]; ok {
			failures[i].keys = append(failures[i].keys, key)
			whys[i] = append(whys[i], why)
			continue
		}
		byCommit[p.commit.sha] = len(failures)
		failures = append(failures, gateFailure{commit: p.commit, keys: []string{key}})
		whys = append(whys, []string{why})
	}
	for i := range failures {
		failures[i].why = strings.Join(whys[i], "; ")
	}
	return failures, nil
}

// reverted maps each commit the reverts among commits take back to the
// revert.
func (g *gateRun) reverted(commits []deliveredCommit) (map[string]string, error) {
	reverted := map[string]string{}
	for _, c := range commits {
		if !isRevert(c.subject) {
			continue
		}
		for _, m := range revertsRe.FindAllStringSubmatch(c.body, -1) {
			full, err := g.repo.Git("rev-parse", "--verify", "--quiet", m[1]+"^{commit}")
			if err != nil {
				return nil, fmt.Errorf("%s reverts %s, which is unknown here (git fetch?)", gitx.Short(c.sha), m[1])
			}
			reverted[strings.TrimSpace(full)] = c.sha
		}
	}
	return reverted, nil
}

// withEarlier adds to commits those withheld from an earlier release, and
// tells which they are.
func (g *gateRun) withEarlier(commits []deliveredCommit, withheld []string) ([]deliveredCommit, map[string]bool, error) {
	all := slices.Clone(commits)
	earlier := map[string]bool{}
	for _, sha := range withheld {
		if slices.ContainsFunc(all, func(c deliveredCommit) bool { return c.sha == sha }) {
			continue
		}
		c, err := oneCommit(g.repo, sha)
		if err != nil {
			return nil, nil, fmt.Errorf("withheld commit %s: %w", sha, err)
		}
		g.app.printf("approved release: %s %s: withheld from an earlier release, goes out now", gitx.Short(sha), c.subject)
		all = append(all, c)
		earlier[c.sha] = true
	}
	return all, earlier, nil
}

// check lets a commit out by the items it names or is attributed to, or by
// ignore_paths, recording the items in out; a commit kept outside comes back
// as the failure.
func (g *gateRun) check(ctx context.Context, c deliveredCommit, line string, out *gateOutcome) (*gateFailure, error) {
	fail := gateFailure{commit: c, why: "names no item"}
	if g.conf.Version != nil {
		ignored, why, err := shipsNothing(g.repo, g.cfg, g.conf, c.sha)
		if err != nil {
			return nil, err
		}
		if why != "" && !ignored {
			g.app.printf("approved release: %s: %s", line, why)
		}
		if ignored {
			if why == "" {
				why = "changes only ignore_paths, passes"
			}
			g.app.printf("approved release: %s: %s", line, why)
			return nil, nil
		}
		g.activeCommits++
		for _, key := range itemKeys(g.keyRe, g.conf, c) {
			g.activeKeys[key] = true
		}
		if err := g.noteLiveKeys(c); err != nil {
			return nil, err
		}
	}
	if at, ok := g.conf.Attributed[c.sha]; ok {
		instead := ""
		if named := slices.DeleteFunc(itemKeys(g.keyRe, nil, c), func(k string) bool { return k == at.Item }); len(named) > 0 {
			instead = " instead of " + strings.Join(named, ", ")
		}
		g.app.printf("approved release: %s: attributed to %s%s: %s", line, at.Item, instead, at.Reason)
	}
	if fail.keys = itemKeys(g.keyRe, g.conf, c); len(fail.keys) > 0 {
		pass, err := g.gate.commit(ctx, fail.keys)
		if err != nil {
			return nil, err
		}
		g.rememberDormant(fail.keys, out)
		if pass.ok {
			if err := g.notePass(c, pass); err != nil {
				return nil, err
			}
			out.passed = append(out.passed, fail.keys...)
			g.dormantAdvice(ctx, fail.keys)
			for _, label := range pass.labels {
				out.approved[label] = true
			}
			for _, done := range pass.done {
				g.app.printf("approved release: %s: %s is %s, passes", line, done, g.gate.issues[done].Status)
			}
			return nil, nil
		}
		fail.why, fail.keys = pass.why, pass.failed
	}
	ignored, why, err := shipsNothing(g.repo, g.cfg, g.conf, c.sha)
	if err != nil {
		return nil, err
	}
	if ignored {
		if why == "" {
			why = "changes only ignore_paths, passes"
		}
		g.app.printf("approved release: %s: %s", line, why)
		return nil, nil
	}
	return &fail, nil
}

// noteLiveKeys keeps, with version and outside_version entries, the keys of a
// commit that is live work.
func (g *gateRun) noteLiveKeys(c deliveredCommit) error {
	if !g.conf.HasOutside() {
		return nil
	}
	live, err := shipsLiveWork(g.repo, g.cfg, g.conf, c.sha)
	if err != nil || !live {
		return err
	}
	if g.liveKeys == nil {
		g.liveKeys = map[string]bool{}
	}
	for _, key := range itemKeys(g.keyRe, g.conf, c) {
		g.liveKeys[key] = true
	}
	return nil
}

// notePass keeps what a passing commit tells about outside_version entries:
// the versions of its live items, the items that wait for one, and the
// entries of items already in an approved version, which ship as the old form.
// A commit that ships nothing is no live work (shipsLiveWork).
func (g *gateRun) notePass(c deliveredCommit, pass commitPass) error {
	for key, version := range pass.ownVersion {
		if item, ok := g.conf.OutsideFor(key); ok {
			g.dormant = config.UniqueDormant(g.dormant, []config.DormantReleaseItem{item.Plain()})
		}
		if g.conf.Version == nil {
			g.adviseOutside(key, version)
		}
	}
	if g.conf.Version != nil {
		return nil // the release version check links the outside entries.
	}
	for _, key := range pass.outside {
		item, _ := g.conf.OutsideFor(key)
		g.outsidePending = append(g.outsidePending, outsidePending{commit: c, item: item})
	}
	if len(pass.live) == 0 || !g.conf.HasOutside() {
		return nil
	}
	live, err := shipsLiveWork(g.repo, g.cfg, g.conf, c.sha)
	if err != nil || !live {
		return err
	}
	for _, label := range pass.live {
		g.liveVersions[label] = g.gate.approved[label]
	}
	return nil
}

func (g *gateRun) rememberDormant(keys []string, out *gateOutcome) {
	for _, key := range keys {
		issue, ok := g.gate.issues[key]
		if !ok {
			continue
		}
		g.recordDormantIssue(issue)
	}
	out.dormant = config.UniqueDormant(out.dormant, g.dormant)
}

func (g *gateRun) recordDormantIssue(issue jira.Issue) {
	// An outside_version entry is recorded where its link to the release is
	// settled.
	if item, dormant := g.conf.DormantFor(issue.Key, issue.Parent); dormant && item.OutsideVersion == "" {
		g.dormant = config.UniqueDormant(g.dormant, []config.DormantReleaseItem{item})
	}
}

func (g *gateRun) dormantAdvice(ctx context.Context, keys []string) {
	for _, key := range keys {
		issue, ok := g.gate.issues[key]
		if !ok {
			continue
		}
		item, dormant := g.conf.DormantFor(key, issue.Parent)
		if !dormant || item.OutsideVersion != "" {
			continue
		}
		advice := issue
		if item.Key != issue.Key {
			parent, err := g.gate.issue(ctx, item.Key)
			if err != nil {
				g.app.warn("%s: cannot check dormant parent %s for status advice: %v", key, item.Key, err)
				continue
			}
			advice = parent
		}
		if advice.Done {
			g.app.printf("%s is %s: drop it from approved_release.dormant", item.Key, advice.Status)
		} else if slices.Contains(g.gate.readyFor(advice), advice.Status) {
			g.app.printf("%s is %s: drop it from approved_release.dormant to let the deploy close it", item.Key, advice.Status)
		}
	}
}

// oneCommit reads a commit's subject and body.
func oneCommit(repo *gitx.Repo, sha string) (deliveredCommit, error) {
	commits, err := logCommits(repo, "-1", sha)
	if err != nil {
		return deliveredCommit{}, fmt.Errorf("unknown here (git fetch?): %w", err)
	}
	if len(commits) != 1 {
		return deliveredCommit{}, fmt.Errorf("git log -1 %s: %d commits", sha, len(commits))
	}
	return commits[0], nil
}

// itemKeys are the sorted items a commit is the work of.
func itemKeys(keyRe *regexp.Regexp, conf *config.ApprovedRelease, c deliveredCommit) []string {
	return uniqueSorted(workKeys(keyRe, conf, c))
}

// commitKeys are the project's item keys a commit names, for non-release gates.
func commitKeys(keyRe *regexp.Regexp, conf *config.ApprovedRelease, c deliveredCommit) []string {
	if items := attributedItems(conf, c); len(items) > 0 {
		return items
	}
	return uniqueSorted(keyRe.FindAllString(c.subject+"\n"+c.body, -1))
}

// attributedItems is the item approved_release.attribute ties a commit to, a
// revert of such a commit included: its message quotes the one it reverts;
// conf may be nil.
func attributedItems(conf *config.ApprovedRelease, c deliveredCommit) []string {
	if conf == nil {
		return nil
	}
	if at, ok := conf.Attributed[c.sha]; ok {
		return []string{at.Item}
	}
	var items []string
	for _, m := range revertsRe.FindAllStringSubmatch(c.body, -1) {
		for _, sha := range slices.Sorted(maps.Keys(conf.Attributed)) {
			if strings.HasPrefix(sha, m[1]) {
				items = append(items, conf.Attributed[sha].Item)
			}
		}
	}
	return uniqueSorted(items)
}

// attributionProblems lists the attributions the repository contradicts: a
// commit it does not have, or one whose message names the attributed item
// already, which the attribution does not change.
func attributionProblems(repo *gitx.Repo, cfg *config.Config) ([]string, error) {
	if cfg.Deploy == nil {
		return nil, nil
	}
	var problems []string
	format, err := repo.Git("rev-parse", "--show-object-format")
	if err != nil {
		return nil, fmt.Errorf("reading repository object format: %w", err)
	}
	var wantHashLength int
	switch strings.TrimSpace(format) {
	case "sha1":
		wantHashLength = sha1CommitHashLength
	case "sha256":
		wantHashLength = sha256CommitHashLength
	default:
		return nil, fmt.Errorf("unsupported repository object format %q", strings.TrimSpace(format))
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Deploy.Targets)) {
		conf := cfg.Deploy.Targets[name].ApprovedRelease
		if conf == nil {
			continue
		}
		keyRe := projectKeyRe(cfg)
		for i, at := range conf.Attribute {
			where := fmt.Sprintf("deploy.targets.%s.approved_release.attribute[%d]", name, i)
			if len(at.Commit) != wantHashLength {
				problems = append(problems, fmt.Sprintf("%s: commit %s must be the full %d-digit hash for this repository", where, at.Commit, wantHashLength))
				continue
			}
			known, err := repo.RevExists(at.Commit)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", where, err)
			}
			if !known {
				problems = append(problems, fmt.Sprintf("%s: commit %s is not in this repository (git fetch?)", where, at.Commit))
				continue
			}
			c, err := oneCommit(repo, at.Commit)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", where, err)
			}
			if keys := itemKeys(keyRe, nil, c); slices.Equal(keys, []string{at.Item}) {
				problems = append(problems, fmt.Sprintf("%s: commit %s %q names %s already: the attribution changes nothing", where, gitx.Short(c.sha), c.subject, at.Item))
			}
		}
	}
	return problems, nil
}

const (
	sha1CommitHashLength   = 40
	sha256CommitHashLength = 64
)

// shipsNothing tells whether a commit ships nothing: besides the version
// files graft commit bumps, it changes only approved_release's ignore_paths,
// or go.mod/go.sum too while the module graph of every main package outside
// ignore_paths stays as it was. why explains how the commit was judged.
func shipsNothing(repo *gitx.Repo, cfg *config.Config, conf *config.ApprovedRelease, sha string) (bool, string, error) {
	out, err := repo.Git("diff-tree", "--no-commit-id", "--name-only", "-r", "--root", "-z", sha)
	if err != nil {
		return false, "", err
	}
	bumped := cfg.VersionFiles()
	versionTargets := versionTargetsByPath(cfg.Version)
	changedVersions := map[string]bool{}
	for file := range strings.SplitSeq(out, "\x00") {
		if file == "" {
			continue
		}
		if key := path.Clean(file); versionTargets[key] != nil {
			changedVersions[key] = true
		}
	}
	if len(changedVersions) > 0 {
		unchangedExceptVersion, err := versionFilesUnchangedExceptVersion(repo, sha, changedVersions, versionTargets)
		if err != nil {
			return false, "", err
		}
		if !unchangedExceptVersion {
			return false, "", nil
		}
	}
	files, ignored := classifyShipChanges(out, bumped, conf)
	if len(files) == 0 {
		for f := range strings.SplitSeq(out, "\x00") {
			if f != "" && slices.ContainsFunc(bumped, func(b string) bool { return path.Clean(b) == path.Clean(f) }) {
				if len(ignored) == 0 {
					return true, "changes only the version files, passes", nil
				}
				break
			}
		}
		return len(ignored) > 0, "", nil
	}
	if !onlyModuleFiles(files) {
		return false, "", nil
	}
	return moduleBumpIsUnshipped(repo, sha, files, conf)
}

func versionTargetsByPath(v config.Version) map[string][]version.Target {
	targets := map[string][]version.Target{}
	for _, target := range version.Targets(v) {
		key := path.Clean(target.Path)
		targets[key] = append(targets[key], target)
	}
	return targets
}

// versionFilesUnchangedExceptVersion compares changed version targets at both
// sides of the commit after replacing every configured target in each file
// with the same marker. Missing files and invalid targets fail closed.
func versionFilesUnchangedExceptVersion(repo *gitx.Repo, sha string, changed map[string]bool, targets map[string][]version.Target) (bool, error) {
	parents, err := repo.Git("rev-list", "--parents", "-n", "1", sha)
	if err != nil {
		return false, fmt.Errorf("listing parents of version-only candidate %s: %w", sha, err)
	}
	parentFields := strings.Fields(parents)
	if len(parentFields) < 2 {
		return false, nil
	}
	parentTree, err := versionBlobTree(repo, parentFields[1])
	if err != nil {
		return false, fmt.Errorf("reading parent tree of version-only candidate %s: %w", sha, err)
	}
	commitTree, err := versionBlobTree(repo, sha)
	if err != nil {
		return false, fmt.Errorf("reading tree of version-only candidate %s: %w", sha, err)
	}
	for file := range changed {
		isVersionBump, err := versionFileIsVersionBump(repo, file, parentTree, commitTree, targets[file])
		if err != nil {
			return false, err
		}
		if !isVersionBump {
			return false, nil
		}
	}
	return true, nil
}

func versionFileIsVersionBump(repo *gitx.Repo, file string, parentTree, commitTree map[string]versionTreeEntry, targets []version.Target) (bool, error) {
	beforeEntry, beforeExists := parentTree[file]
	afterEntry, afterExists := commitTree[file]
	if !beforeExists || !afterExists || beforeEntry.kind != "blob" || afterEntry.kind != "blob" ||
		beforeEntry.oid == "" || afterEntry.oid == "" || beforeEntry.mode != afterEntry.mode ||
		(beforeEntry.mode != "100644" && beforeEntry.mode != "100755") {
		return false, nil
	}
	before, err := repo.Git("cat-file", "blob", beforeEntry.oid)
	if err != nil {
		return false, fmt.Errorf("reading parent version file %q: %w", file, err)
	}
	after, err := repo.Git("cat-file", "blob", afterEntry.oid)
	if err != nil {
		return false, fmt.Errorf("reading version file %q: %w", file, err)
	}
	beforeContent, afterContent := []byte(before), []byte(after)
	for _, target := range targets {
		beforeVersion, err := target.ParseVersionOnly(beforeContent)
		if err != nil {
			return false, fmt.Errorf("parsing version in parent file %q: %w", file, err)
		}
		afterVersion, err := target.ParseVersionOnly(afterContent)
		if err != nil {
			return false, fmt.Errorf("parsing version in file %q: %w", file, err)
		}
		if !beforeVersion.Less(afterVersion) {
			return false, nil
		}
		beforeContent, err = target.Replace(beforeContent, "__GRAFT_VERSION_VALUE__")
		if err != nil {
			return false, fmt.Errorf("replacing version in parent file %q: %w", file, err)
		}
		afterContent, err = target.Replace(afterContent, "__GRAFT_VERSION_VALUE__")
		if err != nil {
			return false, fmt.Errorf("replacing version in file %q: %w", file, err)
		}
	}
	return bytes.Equal(beforeContent, afterContent), nil
}

// versionTreeEntry describes a path stored in a Git tree.
type versionTreeEntry struct {
	mode, kind, oid string
}

// versionBlobTree lists tree entries by their exact repository path.
func versionBlobTree(repo *gitx.Repo, rev string) (map[string]versionTreeEntry, error) {
	out, err := repo.Git("ls-tree", "-rz", "--full-tree", rev)
	if err != nil {
		return nil, err
	}
	blobs := map[string]versionTreeEntry{}
	for entry := range strings.SplitSeq(out, "\x00") {
		if entry == "" {
			continue
		}
		meta, file, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("malformed tree entry %q", entry)
		}
		blobs[file] = versionTreeEntry{mode: fields[0], kind: fields[1], oid: fields[2]}
	}
	return blobs, nil
}

// classifyShipChanges splits a commit's files, the version files left out,
// into those ignore_paths names and the rest.
func classifyShipChanges(out string, bumped []string, conf *config.ApprovedRelease) (files, ignored []string) {
	for f := range strings.SplitSeq(out, "\x00") {
		if f == "" || slices.ContainsFunc(bumped, func(b string) bool { return path.Clean(b) == path.Clean(f) }) {
			continue
		}
		if conf.Ignored([]string{f}) {
			ignored = append(ignored, f)
			continue
		}
		files = append(files, f)
	}
	return files, ignored
}

func isModuleFile(f string) bool {
	return f == "go.mod" || f == "go.sum" || strings.HasSuffix(f, "/go.mod") || strings.HasSuffix(f, "/go.sum")
}

func onlyModuleFiles(files []string) bool {
	return !slices.ContainsFunc(files, func(f string) bool { return !isModuleFile(f) })
}

// moduleDirs are the directories, "" or ending in "/", of the modules whose
// go.mod or go.sum the files change.
func moduleDirs(files []string) []string {
	var dirs []string
	for _, f := range files {
		dirs = append(dirs, strings.TrimSuffix(strings.TrimSuffix(f, "go.mod"), "go.sum"))
	}
	slices.Sort(dirs)
	return slices.Compact(dirs)
}

type shipsNothingResult struct {
	ok  bool
	why string
}

// shipsNothingMemo keeps the verdicts, not the failed checks, of commits
// already judged in this process: a deploy asks about a commit more than once.
var shipsNothingMemo = struct {
	sync.Mutex
	entries map[string]shipsNothingResult
}{entries: map[string]shipsNothingResult{}}

func moduleBumpIsUnshipped(repo *gitx.Repo, sha string, files []string, conf *config.ApprovedRelease) (bool, string, error) {
	key := []string{repo.CommonDir, sha}
	for _, re := range conf.Ignore {
		key = append(key, re.String())
	}
	k := strings.Join(key, "\x00")
	shipsNothingMemo.Lock()
	cached, ok := shipsNothingMemo.entries[k]
	shipsNothingMemo.Unlock()
	if ok {
		return cached.ok, cached.why, nil
	}
	passed, why, err := inspectModuleBump(repo, sha, moduleDirs(files), conf)
	if err != nil {
		return false, "", fmt.Errorf("approved release: cannot judge the go.mod/go.sum change of %s: %w", sha, err)
	}
	shipsNothingMemo.Lock()
	if len(shipsNothingMemo.entries) >= 128 {
		clear(shipsNothingMemo.entries)
	}
	shipsNothingMemo.entries[k] = shipsNothingResult{passed, why}
	shipsNothingMemo.Unlock()
	return passed, why, nil
}

// inspectModuleBump compares the module graph of the main packages outside
// ignore_paths in the commit's tree and in its parent's, written into a
// temporary directory by writeTree. err tells why the check could not be made.
func inspectModuleBump(repo *gitx.Repo, sha string, dirs []string, conf *config.ApprovedRelease) (ok bool, why string, err error) {
	line, err := repo.Git("rev-list", "--parents", "-n", "1", sha)
	if err != nil {
		return false, "", err
	}
	parents := strings.Fields(line)
	if len(parents) < 2 {
		return false, "go.mod/go.sum change of a commit without a parent", nil
	}
	parent := parents[1]
	changed, why := changedRequirements(repo, sha, parent, dirs)
	if why != "" {
		return false, why, nil
	}
	tmp, err := os.MkdirTemp("", "graft-approved-")
	if err != nil {
		return false, "", fmt.Errorf("create a temporary tree: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tmp); err != nil {
			slog.Error("approved release check: remove temporary tree", "error", err)
		}
	}()
	after, before := filepath.Join(tmp, "commit"), filepath.Join(tmp, "parent")
	for _, t := range []struct{ rev, dir string }{{sha, after}, {parent, before}} {
		if err := writeTree(repo, t.rev, t.dir); err != nil {
			return false, "", fmt.Errorf("write the tree of %s: %w", t.rev, err)
		}
	}
	modules, err := moduleRoots(repo, sha)
	if err != nil {
		return false, "", err
	}
	var moved []string
	for _, dir := range modules {
		rel := filepath.FromSlash(dir)
		mains, err := shippedMainPackages(after, filepath.Join(after, rel), conf)
		if err != nil {
			return false, "", err
		}
		if len(mains) == 0 {
			continue
		}
		now, err := moduleGraph(filepath.Join(after, rel), mains)
		if err != nil {
			return false, "", fmt.Errorf("go list failed: %w", err)
		}
		was, err := moduleGraph(filepath.Join(before, rel), mains)
		if err != nil {
			return false, "", fmt.Errorf("go list failed in the parent: %w", err)
		}
		moved = append(moved, graphChanges(was, now)...)
	}
	if len(moved) > 0 {
		slices.Sort(moved)
		return false, "changes modules of a shipped binary: " + strings.Join(slices.Compact(moved), ", "), nil
	}
	return true, "changes only ignore_paths and go.mod/go.sum, the shipped binaries' modules stay as they were (" + strings.Join(changed, ", ") + "), passes", nil
}

// moduleRoots lists the directories, "." for the root, of every module of
// the commit's tree a build can reach: a module's go.mod change reaches the
// binaries of the modules that import it. Modules under testdata, vendor and
// directories go ignores (".", "_") are left out; a module under ignore_paths
// is not, its main packages are.
func moduleRoots(repo *gitx.Repo, sha string) ([]string, error) {
	out, err := repo.Git("ls-tree", "-r", "--name-only", "-z", "--full-tree", sha)
	if err != nil {
		return nil, err
	}
	var roots []string
	for f := range strings.SplitSeq(out, "\x00") {
		if f != "go.mod" && !strings.HasSuffix(f, "/go.mod") {
			continue
		}
		dir := path.Dir(f)
		if !slices.ContainsFunc(strings.Split(dir, "/"), func(e string) bool {
			return e == "testdata" || e == "vendor" || (e != "." && (strings.HasPrefix(e, ".") || strings.HasPrefix(e, "_")))
		}) {
			roots = append(roots, dir)
		}
	}
	return roots, nil
}

// maxTreeBlob is the largest file writeTree copies besides those go list
// reads (Go files, go.mod, go.sum, vendor/modules.txt); a larger one is
// written empty.
const maxTreeBlob = 16 << 20

func goListReads(p string) bool {
	return strings.HasSuffix(p, ".go") || isModuleFile(p) || p == "vendor/modules.txt" || strings.HasSuffix(p, "/vendor/modules.txt")
}

// writeTree writes the files of rev into dir as git stores them: no index,
// no filter and no hook of the repository runs, and nothing is left in it.
func writeTree(repo *gitx.Repo, rev, dir string) error {
	out, err := repo.Git("ls-tree", "-r", "-z", "--full-tree", rev)
	if err != nil {
		return err
	}
	type file struct{ path, oid string }
	var read, rest []file
	links := map[string]bool{}
	for entry := range strings.SplitSeq(out, "\x00") {
		meta, p, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[1] != "blob" {
			continue
		}
		links[p] = fields[0] == "120000"
		if goListReads(p) {
			read = append(read, file{p, fields[2]})
		} else {
			rest = append(rest, file{p, fields[2]})
		}
	}
	for _, set := range []struct {
		files []file
		limit int64
	}{{read, math.MaxInt64}, {rest, maxTreeBlob}} {
		oids := make([]string, len(set.files))
		for i, f := range set.files {
			oids[i] = f.oid
		}
		blobs, err := repo.ReadBlobs(oids, set.limit)
		if err != nil {
			return err
		}
		for i, f := range set.files {
			full := filepath.Join(dir, filepath.FromSlash(f.path))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return fmt.Errorf("create the directory of %s: %w", f.path, err)
			}
			if links[f.path] {
				err = os.Symlink(string(blobs[i].Content), full)
			} else {
				err = os.WriteFile(full, blobs[i].Content, 0o644)
			}
			if err != nil {
				return fmt.Errorf("write %s: %w", f.path, err)
			}
		}
	}
	return nil
}

// changedRequirements names the modules whose requirement the commit changes
// in each go.mod of dirs; why is set when it changes another directive.
func changedRequirements(repo *gitx.Repo, sha, parent string, dirs []string) ([]string, string) {
	changed := map[string]bool{}
	for _, dir := range dirs {
		path := dir + "go.mod"
		if !hasFile(repo, sha, path) || !hasFile(repo, parent, path) {
			return nil, path + " is added or removed"
		}
		now, err := readModFile(repo, sha, path)
		if err != nil {
			return nil, err.Error()
		}
		was, err := readModFile(repo, parent, path)
		if err != nil {
			return nil, err.Error()
		}
		a, b := *now, *was
		a.Require, b.Require, a.Syntax, b.Syntax = nil, nil, nil, nil
		if !reflect.DeepEqual(a, b) {
			return nil, path + " changes directives other than require"
		}
		old := map[string]string{}
		for _, r := range was.Require {
			old[r.Mod.Path] = r.Mod.Version
		}
		for _, r := range now.Require {
			if v, ok := old[r.Mod.Path]; !ok || v != r.Mod.Version {
				changed[r.Mod.Path] = true
			}
			delete(old, r.Mod.Path)
		}
		for p := range old {
			changed[p] = true
		}
	}
	return slices.Sorted(maps.Keys(changed)), ""
}

func hasFile(repo *gitx.Repo, rev, path string) bool {
	_, err := repo.Git("cat-file", "-e", rev+":"+path)
	return err == nil
}

// readModFile parses path at rev.
func readModFile(repo *gitx.Repo, rev, path string) (*modfile.File, error) {
	text, err := repo.Git("show", rev+":"+path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s at %s", path, rev)
	}
	f, err := modfile.Parse(path, []byte(text), nil)
	if err != nil {
		return nil, fmt.Errorf("cannot parse %s at %s", path, rev)
	}
	return f, nil
}

// shippedMainPackages lists the import paths of the main packages of the
// module at root whose directory is outside ignore_paths.
func shippedMainPackages(work, root string, conf *config.ApprovedRelease) ([]string, error) {
	data, err := goList(root, "-e", "-f", "{{if eq .Name \"main\"}}{{.Dir}}\t{{.ImportPath}}{{range .GoFiles}}\t{{.}}{{end}}{{range .CgoFiles}}\t{{.}}{{end}}{{end}}", "./...")
	if err != nil {
		return nil, fmt.Errorf("go list failed: %s", oneLine(err.Error()))
	}
	var mains []string
	for line := range strings.SplitSeq(data, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 2 || fields[0] == "" {
			continue
		}
		rel, err := filepath.Rel(work, fields[0])
		if err != nil {
			return nil, fmt.Errorf("cannot locate main package: %s", oneLine(err.Error()))
		}
		var files []string
		for _, f := range fields[2:] {
			files = append(files, path.Join(filepath.ToSlash(rel), f))
		}
		// A main package ships unless ignore_paths names every file of it.
		if !conf.Ignored(files) {
			mains = append(mains, fields[1])
		}
	}
	return mains, nil
}

// moduleGraph maps each module the packages mains build from to the version
// and replacement the build selects in the module at root: the modules from
// go list -deps, their versions from the build list (go list -m all), which a
// go.mod that lists a lower version than a dependency requires does not
// mislead.
func moduleGraph(root string, mains []string) (map[string]string, error) {
	if _, err := os.Stat(filepath.Join(root, "vendor", "modules.txt")); err == nil {
		return vendoredGraph(root, mains)
	}
	deps, err := goList(root, append([]string{"-deps", "-f", "{{with .Module}}{{.Path}}{{end}}"}, mains...)...)
	if err != nil {
		return nil, err
	}
	built, err := goList(root, "-m", "-f", "{{.Path}}\t{{.Version}}{{with .Replace}} => {{.Path}} {{.Version}}{{end}}", "all")
	if err != nil {
		return nil, err
	}
	selected := map[string]string{}
	for line := range strings.SplitSeq(built, "\n") {
		if path, version, ok := strings.Cut(line, "\t"); ok {
			selected[path] = version
		}
	}
	graph := map[string]string{}
	for path := range strings.FieldsSeq(deps) {
		graph[path] = selected[path]
	}
	return graph, nil
}

// vendoredGraph is moduleGraph of a module that builds from its vendor
// directory, where go list -m all does not work and go checks that
// vendor/modules.txt agrees with go.mod: the versions go list -deps gives.
func vendoredGraph(root string, mains []string) (map[string]string, error) {
	data, err := goList(root, append([]string{"-deps", "-f", "{{with .Module}}{{.Path}}\t{{.Version}}{{with .Replace}} => {{.Path}} {{.Version}}{{end}}{{end}}"}, mains...)...)
	if err != nil {
		return nil, err
	}
	graph := map[string]string{}
	for line := range strings.SplitSeq(data, "\n") {
		if p, version, ok := strings.Cut(line, "\t"); ok {
			graph[p] = version
		}
	}
	return graph, nil
}

// graphChanges names the modules added, removed or moved between two graphs.
func graphChanges(was, now map[string]string) []string {
	var moved []string
	for path, v := range now {
		if old, ok := was[path]; !ok || old != v {
			moved = append(moved, path)
		}
	}
	for path := range was {
		if _, ok := now[path]; !ok {
			moved = append(moved, path)
		}
	}
	slices.Sort(moved)
	return moved
}

// goList runs go list in root outside any workspace, with this machine's
// GOOS, GOARCH and GOFLAGS.
func goList(root string, args ...string) (string, error) {
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Dir, cmd.Env = root, append(os.Environ(), "GOWORK=off")
	data, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s", oneLine(string(data)))
	}
	return string(data), nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// approvalGate reads each item and each version once per deploy.
type approvalGate struct {
	client  *jira.Client
	conf    *config.ApprovedRelease
	issues  map[string]jira.Issue
	history map[string]fixVersionHistoryResult
	// versions maps a version id to whether every approver approved it;
	// approved, the approved versions found by name.
	versions map[string]bool
	approved map[string]jira.Version
	// review are the statuses work waits in for a reviewer; skipKeys are the
	// standing items release_notes.jira.skip_keys names.
	review, skipKeys []string
	target           *config.DeployTarget
}

func newApprovalGate(client *jira.Client, cfg *config.Config, target *config.DeployTarget, issues map[string]jira.Issue) *approvalGate {
	return &approvalGate{
		client: client, conf: target.ApprovedRelease, issues: issues, versions: map[string]bool{}, approved: map[string]jira.Version{},
		review: cfg.ReviewStatuses(), skipKeys: cfg.ReleaseNotes.Jira.SkipKeys, target: target,
	}
}

type fixVersionHistoryResult struct {
	added, hasAddition bool
	err                error
}

func (g *approvalGate) fixVersionHistoryAfter(ctx context.Context, key, versionID string, at time.Time) (bool, bool, error) {
	cacheKey := key + "\x00" + versionID + "\x00" + at.Format(time.RFC3339Nano)
	if result, ok := g.history[cacheKey]; ok {
		return result.added, result.hasAddition, result.err
	}
	added, hasAddition, err := g.client.FixVersionHistoryAfter(ctx, key, versionID, at)
	if g.history == nil {
		g.history = map[string]fixVersionHistoryResult{}
	}
	g.history[cacheKey] = fixVersionHistoryResult{added: added, hasAddition: hasAddition, err: err}
	return added, hasAddition, err
}

// gatePass is the verdict on one item.
type gatePass struct {
	ok bool
	// label names what let the commit out: a version or a status.
	label string
	// done is the key of a done item that let the commit out, worth a line
	// of its own: it may be an old item a commit only refers to.
	done string
	// why the commit stays outside.
	why string
	// outside: the item passed by its outside_version entry alone, until the
	// run links the entry; ownVersion: an item with such an entry passed by
	// an approved version of its own.
	outside, ownVersion bool
}

// commitPass is the verdict on one commit.
type commitPass struct {
	ok bool
	// labels name what let the items out; done, the done items among them.
	labels, done []string
	// live are the labels of the items without a dormant entry; outside, the
	// items let out by their outside_version entry alone; ownVersion maps an
	// item with an outside_version entry already in an approved version to it.
	live       []string
	outside    []string
	ownVersion map[string]string
	// failed are the items that keep the commit outside, why says why.
	failed []string
	why    string
}

// commit lets a commit out when all the items that are its work pass, with
// every item the commit is the work of.
func (g *approvalGate) commit(ctx context.Context, keys []string) (commitPass, error) {
	var out commitPass
	var whys []string
	for _, key := range keys {
		pass, err := g.item(ctx, key)
		if err != nil {
			return commitPass{}, err
		}
		if !pass.ok {
			out.failed = append(out.failed, key)
			whys = append(whys, pass.why)
			continue
		}
		if pass.outside {
			out.outside = append(out.outside, key)
			continue
		}
		out.labels = append(out.labels, pass.label)
		if pass.ownVersion {
			if out.ownVersion == nil {
				out.ownVersion = map[string]string{}
			}
			out.ownVersion[key] = pass.label
		}
		if _, dormant := g.conf.DormantFor(key, g.issues[key].Parent); !dormant {
			out.live = append(out.live, pass.label)
		}
		if pass.done != "" {
			out.done = append(out.done, pass.done)
		}
	}
	out.ok, out.why = len(out.failed) == 0, strings.Join(whys, "; ")
	return out, nil
}

func (g *approvalGate) readyFor(issue jira.Issue) []string {
	if g.target != nil {
		return g.target.ReadyStatusesFor(issue.Labels, issue.Type)
	}
	if g.conf != nil {
		return g.conf.StatusesFor(issue.Labels)
	}
	return nil
}

// openParent says why an item or its parent is held by subtasks not in Done.
// With a version prefix, every parent is checked; without one, only parents
// whose approved status would let their work out are checked.
func (g *approvalGate) openParent(ctx context.Context, issue jira.Issue) (string, error) {
	holder, ok, err := g.holder(ctx, issue)
	if err != nil || !ok || slices.Contains(g.skipKeys, holder.Key) {
		return "", err
	}
	// An outside_version entry of the holder covers none of its subtasks.
	if item, dormant := g.conf.DormantFor(holder.Key, holder.Parent); dormant && item.OutsideVersion == "" {
		return "", nil
	}
	return g.openSubtasks(ctx, holder)
}

// holder is the item whose subtasks openParent looks at: the item itself
// when it is Done, ready, or checked by a version prefix; for subtasks, it is
// the parent when ready or when a version prefix applies.
func (g *approvalGate) holder(ctx context.Context, issue jira.Issue) (jira.Issue, bool, error) {
	if issue.Parent == "" {
		ready := slices.Contains(g.readyFor(issue), issue.Status)
		return issue, issue.Done || ready || g.conf.Prefix != "", nil
	}
	parent, err := g.issue(ctx, issue.Parent)
	if err != nil {
		return jira.Issue{}, false, err
	}
	if slices.Contains(g.readyFor(parent), parent.Status) || g.conf.Prefix != "" {
		return parent, true, nil
	}
	return jira.Issue{}, false, nil
}

// openSubtasks says which of holder's subtasks keep it in, "" when none does.
func (g *approvalGate) openSubtasks(ctx context.Context, holder jira.Issue) (string, error) {
	var open []string
	for _, key := range holder.Subtasks {
		if slices.Contains(g.skipKeys, key) {
			continue
		}
		// An outside_version entry stands in for the version check alone:
		// the subtask holds its parent as without the entry.
		if item, dormant := g.conf.DormantFor(key, holder.Key); dormant && item.OutsideVersion == "" {
			continue
		}
		st, err := g.issue(ctx, key)
		if err != nil {
			return "", err
		}
		if !st.Done {
			open = append(open, key+" ("+st.Status+")")
		}
	}
	if len(open) == 0 {
		return "", nil
	}
	return fmt.Sprintf("%s is %s with open subtasks %s: move them to Done or name %s in release_notes.jira.skip_keys", holder.Key, holder.Status, strings.Join(open, ", "), holder.Key), nil
}

// item checks one item against the statuses and the approved version. An
// outside_version entry stands in for the approved version alone: the item
// is checked as without it otherwise, its status aside, which a dormant item
// need not have; the run (or the release version check) links the entry.
func (g *approvalGate) item(ctx context.Context, key string) (gatePass, error) {
	issue, err := g.issue(ctx, key)
	if err != nil {
		return gatePass{}, err
	}
	_, outside := g.conf.OutsideFor(key)
	if _, dormant := g.conf.DormantFor(key, issue.Parent); dormant && !outside {
		return g.dormantItem(ctx, issue)
	}
	if g.conf.Version != nil {
		return g.versionItem(ctx, issue, outside)
	}
	why, err := g.openParent(ctx, issue)
	if err != nil {
		return gatePass{}, err
	}
	if why != "" {
		return gatePass{why: why}, nil
	}
	var pass gatePass
	statuses := g.conf.StatusesFor(issue.Labels)
	if len(statuses) > 0 && !outside {
		checked, err := g.checkedIssue(ctx, issue)
		if err != nil {
			return pass, err
		}
		if checked == nil {
			if issue.Parent != "" {
				return gatePass{why: g.parentNotReady(ctx, issue)}, nil
			}
			return gatePass{why: fmt.Sprintf("%s is %s, not %s", key, issue.Status, orList(statuses))}, nil
		}
		pass = gatePass{ok: true, label: checked.Status}
		if checked.Done {
			pass.done = checked.Key
		}
	}
	if g.conf.Prefix != "" {
		return g.inApprovedVersion(ctx, issue, pass, outside)
	}
	return pass, nil
}

// inApprovedVersion is the check an outside_version entry stands in for: the
// item in an approved version of the prefix. With the entry, an item in one
// ships as the old dormant form; an item in none waits for the run to link
// its entry.
func (g *approvalGate) inApprovedVersion(ctx context.Context, issue jira.Issue, pass gatePass, outside bool) (gatePass, error) {
	version, err := g.approvedVersion(ctx, issue)
	if err != nil {
		return gatePass{}, err
	}
	if version == "" {
		if outside {
			return gatePass{ok: true, outside: true}, nil
		}
		return gatePass{why: g.notInApprovedVersion(issue)}, nil
	}
	pass.ok, pass.label, pass.ownVersion = true, version, outside
	return pass, nil
}

func (g *approvalGate) versionItem(ctx context.Context, issue jira.Issue, outside bool) (gatePass, error) {
	if g.conf.Prefix == "" {
		return gatePass{ok: true, label: "release version"}, nil
	}
	why, err := g.openParent(ctx, issue)
	if err != nil {
		return gatePass{}, err
	}
	if why != "" {
		return gatePass{why: why}, nil
	}
	return g.inApprovedVersion(ctx, issue, gatePass{}, outside)
}

func (g *approvalGate) dormantItem(ctx context.Context, issue jira.Issue) (gatePass, error) {
	version, err := g.approvedVersion(ctx, issue)
	if err != nil {
		return gatePass{}, err
	}
	if version == "" {
		return gatePass{why: g.notInApprovedVersion(issue) + " (dormant: still needs an approved version)"}, nil
	}
	return gatePass{ok: true, label: version}, nil
}

// checkedIssue returns the item when it is in one of the statuses of its
// labels and nil when it is not; a subtask is checked by its parent alone, by
// the parent's statuses and labels.
func (g *approvalGate) checkedIssue(ctx context.Context, issue jira.Issue) (*jira.Issue, error) {
	if issue.Parent == "" {
		if slices.Contains(g.conf.StatusesFor(issue.Labels), issue.Status) {
			return &issue, nil
		}
		return nil, nil
	}
	parent, err := g.issue(ctx, issue.Parent)
	if err != nil {
		return nil, err
	}
	if slices.Contains(g.conf.StatusesFor(parent.Labels), parent.Status) {
		return &parent, nil
	}
	return nil, nil
}

func (g *approvalGate) issue(ctx context.Context, key string) (jira.Issue, error) {
	if issue, ok := g.issues[key]; ok {
		return issue, nil
	}
	issue, err := g.client.Issue(ctx, key)
	if err != nil {
		return jira.Issue{}, err
	}
	g.issues[key] = issue
	return issue, nil
}

// approvedVersion names the item's fix version of the prefix that every
// approver approved, or its parent's for a subtask. A subtask goes out with
// the item it is part of; its own fix version does not count. It is "" when
// neither has an approved version.
func (g *approvalGate) approvedVersion(ctx context.Context, issue jira.Issue) (string, error) {
	if issue.Parent == "" {
		return g.ownApprovedVersion(ctx, issue)
	}
	parent, err := g.issue(ctx, issue.Parent)
	if err != nil {
		return "", err
	}
	return g.ownApprovedVersion(ctx, parent)
}

// fullyApproved tells whether every approver approved the version id.
func (g *approvalGate) fullyApproved(ctx context.Context, id string) (bool, error) {
	if full, seen := g.versions[id]; seen {
		return full, nil
	}
	approvers, err := g.client.VersionApprovers(ctx, id)
	if err != nil {
		return false, err
	}
	full := jira.FullyApproved(approvers)
	if g.versions == nil {
		g.versions = map[string]bool{}
	}
	g.versions[id] = full
	return full, nil
}

func (g *approvalGate) ownApprovedVersion(ctx context.Context, issue jira.Issue) (string, error) {
	match := (*regexp.Regexp)(nil)
	if g.conf.Prefix == "" && g.conf.Version != nil {
		match = regexp.MustCompile(g.conf.Version.Match)
	}
	for _, v := range issue.FixVersions {
		if g.conf.Prefix != "" && !strings.HasPrefix(v.Name, g.conf.Prefix) || match != nil && !match.MatchString(v.Name) {
			continue
		}
		full, err := g.fullyApproved(ctx, v.ID)
		if err != nil {
			return "", err
		}
		if full {
			if g.approved == nil {
				g.approved = map[string]jira.Version{}
			}
			g.approved[v.Name] = v
			return v.Name, nil
		}
	}
	return "", nil
}

// describe names each item of the failing commits once, with its summary,
// status and the fix versions of the prefix: what to fix in the tracker.
func (g *approvalGate) describe(failures []gateFailure) []string {
	var lines []string
	seen := map[string]bool{}
	for _, f := range failures {
		if len(f.keys) == 0 && !seen[""] {
			seen[""] = true
			lines = append(lines, "commits naming no item: tie each to its item with approved_release.attribute, or revert it")
		}
		for _, key := range f.keys {
			if seen[key] {
				continue
			}
			seen[key] = true
			issue, ok := g.issues[key]
			if !ok {
				continue
			}
			line := key + " " + issue.Summary + ": " + issue.Status
			if g.conf.Prefix != "" {
				line += "; " + g.versionsOfRelease(issue)
			}
			lines = append(lines, line)
		}
	}
	return lines
}

// notInApprovedVersion says why an item is not in an approved version of the
// prefix. A subtask's own version does not count: only its parent's can let
// it out, so for a subtask the refusal names the parent.
func (g *approvalGate) notInApprovedVersion(issue jira.Issue) string {
	if issue.Parent != "" {
		return fmt.Sprintf("%s goes out with its parent %s, which is not in an approved %q version", issue.Key, issue.Parent, g.conf.Prefix)
	}
	return fmt.Sprintf("%s not in an approved %q version", issue.Key, g.conf.Prefix)
}

// versionsOfRelease lists the fix versions that decide the item's release:
// its own, or its parent's for a subtask.
func (g *approvalGate) versionsOfRelease(issue jira.Issue) string {
	if issue.Parent == "" {
		return g.fixVersions(issue)
	}
	parent, ok := g.issues[issue.Parent]
	if !ok {
		return "goes out with its parent " + issue.Parent
	}
	return "goes out with its parent " + issue.Parent + ": " + g.fixVersions(parent)
}

// fixVersions lists the item's fix versions of the prefix and whether every
// approver approved each, as far as the gate read it.
func (g *approvalGate) fixVersions(issue jira.Issue) string {
	var vs []string
	for _, v := range issue.FixVersions {
		if !strings.HasPrefix(v.Name, g.conf.Prefix) {
			continue
		}
		// Approval is read only where the gate came to versions; a refusal
		// before that leaves it unread, which is not the same as unapproved.
		state := "approval not read"
		if full, seen := g.versions[v.ID]; seen {
			state = "not approved by every approver"
			if full {
				state = "approved"
			}
		}
		vs = append(vs, v.Name+" ("+state+")")
	}
	if len(vs) == 0 {
		return fmt.Sprintf("in no %q version", g.conf.Prefix)
	}
	return "fix versions " + strings.Join(vs, ", ")
}

// remedy says how an item gets a commit out.
func (g *approvalGate) remedy() string {
	var ways []string
	if len(g.conf.Statuses) > 0 || len(g.conf.LabelStatuses) > 0 {
		parts := []string{"move their items (a subtask's parent for a subtask) to " + orList(g.conf.Statuses)}
		for _, label := range slices.Sorted(maps.Keys(g.conf.LabelStatuses)) {
			parts = append(parts, fmt.Sprintf("items labelled %s to %s", label, orList(g.conf.LabelStatuses[label])))
		}
		ways = append(ways, strings.Join(parts, ", "))
	}
	if g.conf.Prefix != "" {
		ways = append(ways, "if the code ships switched off, declare the exact item in approved_release.dormant with its outside_version and disabled_by; live work outside the version is reverted")
	}
	ways = append(ways, "finish any subtasks called open")
	return strings.Join(ways, " and ")
}

// orList joins names as "a, b or c".
func orList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

// isRevert recognizes git's own subject (Revert "…") and the Conventional
// Commits revert type. A revert of a revert (Revert "Revert "…"") brings the
// work back and is not one: git's revert subjects nest, an odd number of them
// is a revert.
func isRevert(subject string) bool {
	nested := 0
	for s := subject; ; nested++ {
		rest, ok := strings.CutPrefix(s, `Revert "`)
		if !ok {
			break
		}
		s = rest
	}
	if nested > 0 {
		return nested%2 == 1
	}
	s := strings.ToLower(subject)
	return strings.HasPrefix(s, "revert ") || strings.HasPrefix(s, "revert:") || strings.HasPrefix(s, "revert(") || strings.HasPrefix(s, "revert!")
}

type deliveredCommit struct {
	sha, subject, body string
	// merge is a commit with more than one parent.
	merge bool
}

// deliveredCommits lists the commits that go out, merges left out.
func deliveredCommits(repo *gitx.Repo, prev, head string) ([]deliveredCommit, error) {
	return logCommits(repo, "--no-merges", prev+".."+head)
}

// logCommits lists the commits git log selects with args.
func logCommits(repo *gitx.Repo, args ...string) ([]deliveredCommit, error) {
	out, err := repo.Git(append([]string{"log", "--format=%H%x1f%P%x1f%s%x1f%b%x1e"}, args...)...)
	if err != nil {
		return nil, err
	}
	var commits []deliveredCommit
	for rec := range strings.SplitSeq(out, "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x1f", 4)
		if len(f) != 4 {
			return nil, fmt.Errorf("unexpected git log record %q", rec)
		}
		commits = append(commits, deliveredCommit{sha: f[0], subject: f[2], body: f[3], merge: strings.Contains(f[1], " ")})
	}
	return commits, nil
}

func uniqueSorted(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

// deliveredKeys lists the project's work item keys the delivered commits
// count for (itemKeys, merges too) or are attributed to, once each, sorted: the commits of prev..head and those withheld from an earlier
// release that go out now. Withheld commits and the reverts that withheld
// them delivered nothing, nor did a revert or a commit a revert among them
// takes out: reverted are the keys only those name.
func deliveredKeys(r *deployRun) (keys, reverted, quiet []string, err error) {
	commits, err := goingOut(r)
	if err != nil {
		return nil, nil, nil, err
	}
	isUndone := undoneBy(commits)
	keyRe := projectKeyRe(r.cfg)
	// merged are the keys merge commits name: a merge brings what its
	// branch's commits do, so the commits naming the key decide, and when
	// none does, whether the merge brings any change.
	type mergedKey struct {
		key     string
		changes bool
	}
	var merged []mergedKey
	for _, c := range commits {
		if slices.Contains(r.withheld, c.sha) || withholds(c.body, r.withheld) {
			continue
		}
		named := itemKeys(keyRe, r.target.ApprovedRelease, c)
		switch {
		case isUndone(c):
			reverted = append(reverted, named...)
		case c.merge:
			changes, err := mergeChanges(r, c, isUndone)
			if err != nil {
				return nil, nil, nil, err
			}
			for _, k := range named {
				merged = append(merged, mergedKey{k, changes})
			}
		case isQuiet(c):
			quiet = append(quiet, named...)
		default:
			keys = append(keys, named...)
		}
	}
	for _, m := range merged {
		switch {
		case slices.Contains(keys, m.key) || slices.Contains(quiet, m.key) || slices.Contains(reverted, m.key):
		case m.changes:
			keys = append(keys, m.key)
		default:
			quiet = append(quiet, m.key)
		}
	}
	keys = uniqueSorted(keys)
	reverted = slices.DeleteFunc(uniqueSorted(reverted), func(k string) bool { return slices.Contains(keys, k) })
	quiet = slices.DeleteFunc(uniqueSorted(quiet), func(k string) bool { return slices.Contains(keys, k) || slices.Contains(reverted, k) })
	return keys, reverted, quiet, nil
}

// goingOut lists the commits of prev..head and those withheld from an
// earlier release that go out now.
func goingOut(r *deployRun) ([]deliveredCommit, error) {
	commits, err := logCommits(r.repo, r.prev+".."+r.head)
	if err != nil {
		return nil, err
	}
	for _, sha := range r.released {
		c, err := oneCommit(r.repo, sha)
		if err != nil {
			return nil, fmt.Errorf("commit %s withheld from an earlier release: %w", gitx.Short(sha), err)
		}
		commits = append(commits, c)
	}
	return commits, nil
}

// undoneBy tells which commits take work out: a revert, and the commits a
// revert among commits reverts.
func undoneBy(commits []deliveredCommit) func(deliveredCommit) bool {
	var undone []string
	for _, c := range commits {
		if isRevert(c.subject) {
			for _, m := range revertsRe.FindAllStringSubmatch(c.body, -1) {
				undone = append(undone, m[1])
			}
		}
	}
	return func(c deliveredCommit) bool {
		return isRevert(c.subject) || slices.ContainsFunc(undone, func(p string) bool { return strings.HasPrefix(c.sha, p) })
	}
}

// mergeChanges tells whether a merge brings a commit the target has not run
// that changes it: neither of the quietTypes nor one taking work out.
func mergeChanges(r *deployRun, merge deliveredCommit, isUndone func(deliveredCommit) bool) (bool, error) {
	brought, err := logCommits(r.repo, "--no-merges", merge.sha+"^1.."+merge.sha, "^"+r.prev)
	if err != nil {
		return false, fmt.Errorf("the commits merge %s brings: %w", gitx.Short(merge.sha), err)
	}
	return slices.ContainsFunc(brought, func(b deliveredCommit) bool { return !isUndone(b) && !isQuiet(b) }), nil
}

// quietTypes are the Conventional Commits types of the commits that change
// nothing the target runs: an item named only by them is not delivered.
var quietTypes = []string{"docs", "test", "style", "ci"}

// isQuiet tells whether the commit is of one of the quietTypes.
func isQuiet(c deliveredCommit) bool {
	return slices.Contains(quietTypes, message.ConventionalType(c.subject))
}

// withholds tells whether a commit message reverts one of the withheld
// commits.
func withholds(text string, withheld []string) bool {
	for _, m := range revertsRe.FindAllStringSubmatch(text, -1) {
		if slices.ContainsFunc(withheld, func(sha string) bool { return strings.HasPrefix(sha, m[1]) }) {
			return true
		}
	}
	return false
}

func jiraClient() (*jira.Client, error) {
	ucfg, path, err := userconfig.Load()
	if err != nil {
		return nil, err
	}
	if ucfg.Jira.EnvFile == "" {
		return nil, fmt.Errorf("jira.env_file is not set in %s", path)
	}
	creds, err := jira.LoadCredentials(ucfg.Jira.EnvFile)
	if err != nil {
		return nil, err
	}
	return jira.New(creds), nil
}

// DeployCheckHead is graft deploy check-head: the deploy script calls it
// between building and shipping.
func (a *App) DeployCheckHead() error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	if cfg.Deploy == nil {
		return errors.New("deploy is not configured")
	}
	want := os.Getenv(EnvDeploySHA)
	if want == "" {
		return fmt.Errorf("%s is not set: check-head runs inside graft deploy", EnvDeploySHA)
	}
	if err := deploy.CheckHead(repo, cfg.Deploy, want); err != nil {
		return err
	}
	a.printf("HEAD is still %s and the tree is clean", gitx.Short(want))
	return nil
}

// DeployStatus runs the target's status command; args go to its {args}.
func (a *App) DeployStatus(ctx context.Context, target string, args []string) error {
	r, err := a.deployTarget(target)
	if err != nil {
		return err
	}
	if !r.target.Status.IsSet() && r.target.DeployedSHA == nil {
		return fmt.Errorf("deploy.targets.%s: neither status nor deployed_sha is configured", target)
	}
	if r.target.DeployedSHA != nil {
		a.printDeployedCommit(ctx, r)
	}
	if !r.target.Status.IsSet() {
		if len(args) > 0 {
			return fmt.Errorf("deploy.targets.%s.status is not configured, it takes no arguments", target)
		}
		return nil
	}
	cmd, err := r.env.CommandArgs(ctx, r.target.Status, args)
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = a.Stdout, a.Stderr
	return runPassthrough(cmd, "status")
}

// printDeployedCommit names the commit the target runs, with its subject from
// the local repository. It only informs: status goes on whatever happens.
func (a *App) printDeployedCommit(ctx context.Context, r *deployRun) {
	sha, err := deploy.PeekDeployedSHA(ctx, r.env, r.target.DeployedSHA)
	switch {
	case err != nil:
		a.warn("deployed commit unknown: %v", err)
		return
	case sha == "":
		a.printf("no deployed commit recorded in %s", r.target.DeployedSHA.Path)
		return
	}
	known, err := r.repo.RevExists(sha)
	if err != nil {
		a.warn("deployed %s: %v", sha, err)
		return
	}
	if !known {
		a.printf("deployed %s, not in the local repository (git fetch?)", gitx.Short(sha))
		return
	}
	line, err := r.repo.Git("log", "-1", "--format=%h %s (%cs)", sha)
	if err != nil {
		a.warn("deployed %s: %v", sha, err)
		return
	}
	a.printf("deployed %s", strings.TrimSpace(line))
}

// DeployLogs runs the target's logs command, keeping the lines matching
// grep; args go to its {args}.
func (a *App) DeployLogs(ctx context.Context, target string, lines int, grep string, args []string) error {
	r, err := a.deployTarget(target)
	if err != nil {
		return err
	}
	if r.target.Logs == "" {
		return fmt.Errorf("deploy.targets.%s.logs is not configured", target)
	}
	var filter *regexp.Regexp
	if grep != "" {
		if filter, err = regexp.Compile(grep); err != nil {
			return fmt.Errorf("--grep: %w", err)
		}
	}
	if len(args) > 0 && !strings.Contains(r.target.Logs, config.ArgsPlaceholder) {
		return fmt.Errorf("deploy.targets.%s.logs takes no arguments (no %s in it)", target, config.ArgsPlaceholder)
	}
	line, err := envs.ShellArgs(r.target.Logs, args, func(s string) (string, error) {
		return envs.Fill(s, map[string]string{"lines": strconv.Itoa(lines)}, envs.Quote)
	})
	if err != nil {
		return err
	}
	cmd := r.env.Shell(ctx, line)
	cmd.Stderr = a.Stderr
	if filter == nil {
		cmd.Stdout = a.Stdout
		return runPassthrough(cmd, "logs")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("logs: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("logs: starting the command: %w", err)
	}
	matched, scanErr := copyMatching(stdout, a.Stdout, filter)
	if err := errors.Join(scanErr, cmd.Wait()); err != nil {
		return fmt.Errorf("logs: %w", err)
	}
	if matched == 0 {
		a.printf("no lines match %q", grep)
	}
	return nil
}

func copyMatching(r io.Reader, w io.Writer, re *regexp.Regexp) (int, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	matched := 0
	for sc.Scan() {
		if re.MatchString(sc.Text()) {
			matched++
			fmt.Fprintln(w, sc.Text())
		}
	}
	return matched, sc.Err()
}

func runPassthrough(cmd *exec.Cmd, what string) error {
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &ExitCodeError{Code: exitErr.ExitCode(), Err: fmt.Errorf("%s command failed with exit status %d", what, exitErr.ExitCode())}
	}
	return err
}

// parentNotReady says why a subtask's parent does not let it out: its own
// status does not count.
func (g *approvalGate) parentNotReady(ctx context.Context, issue jira.Issue) string {
	parent, err := g.issue(ctx, issue.Parent)
	if err != nil {
		return fmt.Sprintf("%s goes out with its parent %s, which could not be read: %v", issue.Key, issue.Parent, err)
	}
	return fmt.Sprintf("%s goes out with its parent %s, which is %s, not %s", issue.Key, parent.Key, parent.Status, orList(g.conf.StatusesFor(parent.Labels)))
}

// adviseApproved tells, for every outside_version entry whose item, or its
// parent, has among its fix versions an approved version this release ships
// (prefix without version), that the entry outlived its release, whether or
// not the item has commits in the range.
func (g *gateRun) adviseApproved(ctx context.Context, approved map[string]bool) error {
	if g.conf.Prefix == "" || g.conf.Version != nil || len(approved) == 0 {
		return nil // with version, the release version check tells it.
	}
	for _, item := range g.conf.Dormant {
		if _, ok := g.conf.OutsideFor(item.Key); !ok || g.advised[item.Key] {
			continue
		}
		issue, err := g.gate.issue(ctx, item.Key)
		if err != nil {
			return err
		}
		if where := shippedFixVersion(issue, approved); where != "" {
			g.adviseOutside(item.Key, where)
			continue
		}
		if issue.Parent == "" {
			continue
		}
		parent, err := g.gate.issue(ctx, issue.Parent)
		if err != nil {
			return err
		}
		if where := shippedFixVersion(parent, approved); where != "" {
			g.adviseOutside(item.Key, where+" with its parent "+parent.Key)
		}
	}
	return nil
}

// shippedFixVersion names the first of issue's fix versions among approved.
func shippedFixVersion(issue jira.Issue, approved map[string]bool) string {
	for _, v := range issue.FixVersions {
		if approved[v.Name] {
			return v.Name
		}
	}
	return ""
}
