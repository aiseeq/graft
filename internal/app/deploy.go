package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/deploy"
	"github.com/aiseeq/graft/v2/internal/dotenv"
	"github.com/aiseeq/graft/v2/internal/envs"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/jira"
	"github.com/aiseeq/graft/v2/internal/loadgate"
	"github.com/aiseeq/graft/v2/internal/lock"
	"github.com/aiseeq/graft/v2/internal/message"
	"github.com/aiseeq/graft/v2/internal/tasks"
	"github.com/aiseeq/graft/v2/internal/userconfig"
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
	Redeploy bool
	// GlintTicket is the item that takes the glint findings of the deployed
	// commit, at a release acceptance: the items move with them
	// (release_notes.glint).
	GlintTicket string
	// Unapproved skips the approved_release check.
	Unapproved bool
	// PastFreeze permits a freeze refusal when it gives the bypass reason.
	PastFreeze string
}

type deployRun struct {
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
	// approvedVersions are the approved versions the release took items
	// from, with approved_release.prefix.
	approvedVersions []string
	// moveOut are the items whose commits a deploy to a held target
	// reverts: they leave the window's version for the next one.
	moveOut *versionMove
	// dryRun is graft deploy check: the checks record nothing and never
	// wait for a sudo password.
	dryRun bool
	// freeze is the active freeze related to this target, read once per run.
	freeze                         *activeFreeze
	freezeLogKind, freezeLogReason string
	freezeAdvisoryFailed           []string
	freezePreflightDeps            []string
	// glint holds the moves for the glint findings of the deployed commit.
	glint *glintCheck
}

// readSHA reads a commit record of an environment, without a sudo prompt
// in a dry run.
func (r *deployRun) readSHA(ctx context.Context, env *envs.Env, rec *config.DeployedSHA) (string, error) {
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
	if err := deployPreflight(r); err != nil {
		return err
	}
	if r.glint, err = newGlintCheck(r, o.GlintTicket, func(context.Context) (string, error) { return r.head, nil }); err != nil {
		return err
	}
	if err := r.glint.checkTicket(ctx); err != nil {
		return err
	}
	// Its heavy tasks, and those of every graft its script starts, skip
	// the queue of heavy tasks within one budget; a deploy inside a deploy
	// keeps the outer's.
	if os.Getenv(loadgate.EnvPriority) == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return fmt.Errorf("the budget for waiting: %w", err)
		}
		lw := r.cfg.Deploy.LoadWait
		budget, err := loadgate.NewBudget(filepath.Join(cache, "graft"), lw.Budget)
		if err != nil {
			return err
		}
		defer func() {
			for _, f := range []string{budget, budget + ".lock"} {
				if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
					fmt.Fprintf(a.Stderr, "graft: warning: removing the budget for waiting: %v\n", err)
				}
			}
		}()
		p := loadgate.Priority{For: "graft deploy " + o.Target, Budget: budget, Fail: lw.Then == loadgate.ThenFail}
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
	err = a.deploy(ctx, r, o)
	if releaseErr := lk.Release(); releaseErr != nil {
		err = errors.Join(err, fmt.Errorf("releasing the deploy lock: %w", releaseErr))
	}
	if err != nil {
		return err
	}
	a.dailyFlags(ctx, r.repo, r.cfg, r.env.Name)
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
	return &deployRun{repo: repo, cfg: cfg, name: name, target: t, env: env, tasks: a.runner(repo, cfg)}, nil
}

func (a *App) deploy(ctx context.Context, r *deployRun, o DeployOptions) error {
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
	if err := a.checkTargetRuns(ctx, r, o); err != nil {
		return err
	}
	if err := a.checkFreeze(ctx, r, o); err != nil {
		return err
	}
	if err := a.checkFreezeAdvisoryPreflight(r); err != nil {
		return err
	}
	if err := a.checkIntegrations(ctx, r); err != nil {
		return err
	}
	// One unit: the deps' lines name it, and its line holds them, the
	// confirmation and the script. Its name has a space no task name can
	// have.
	err = r.tasks.Unit("deploy "+r.name, func() error {
		if err := a.runDeployDeps(ctx, r); err != nil {
			return err
		}
		if r.target.Confirm == "sudo" {
			start := time.Now()
			err := deploy.ConfirmSudo(ctx, r.name, a.Stderr)
			r.tasks.Waited(tasks.WhySudo, time.Since(start))
			if err != nil {
				return err
			}
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

// checkTargetRuns reads what the target runs, shows it and refuses a deploy
// it does not take. The deployed commit is read before deps, sudo and the
// script: a target that already runs HEAD has nothing to gain from them.
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
	return a.checkReleaseFlow(ctx, r, o.Unapproved)
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

// refuseSameCommit refuses to deploy the commit the target already runs,
// unless redeploy asks for exactly that.
func (a *App) refuseSameCommit(r *deployRun, redeploy bool) error {
	if r.prev != r.head {
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
	err = r.tasks.Measure(r.target.Run.Source, cmd)
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
	comment := r.cfg.NoteComment(r.target)
	if strings.Contains(comment, "{release}") {
		names, _, err := pageRelease(r)
		if err != nil {
			return "", err
		}
		values["release"] = strings.Join(names, ", ")
	}
	return envs.Fill(comment, values, func(s string) string { return s })
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
	fix := &fixVersionRun{app: a, conf: r.target.ReleaseNotes.FixVersion}
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
	mover := &noteMover{app: a, client: client, conf: r.target.ReleaseNotes, me: me, target: r.name, glint: r.glint}
	sub := &subtaskRun{mover: mover, notes: notes, me: me, reached: map[string]string{}}
	// unsure are the keys this deploy could not read or post to: one held
	// before stays held, moved when it may be.
	var unsure []string
	for _, key := range keys {
		if r.moveOut != nil && slices.Contains(r.moveOut.keys, key) {
			a.printf("release note for %s not posted: this deploy reverts its work from the candidate", key)
			continue
		}
		if slices.Contains(notes.SkipKeys, key) {
			a.printf("release note for %s not posted, %s left alone: release_notes.jira.skip_keys", key, key)
			continue
		}
		issue, err := client.Issue(ctx, key)
		if err != nil {
			a.warn("release note for %s not posted: %v", key, err)
			fix.failed(key, err)
			unsure = append(unsure, key)
			continue
		}

		sub.delivered = append(sub.delivered, issue)
		sub.reached[issue.Key] = issue.Status
		yours, err := a.noteable(ctx, client, notes, me, issue)
		if err != nil {
			a.warn("release note for %s not posted: %v", key, err)
			fix.failed(key, err)
			unsure = append(unsure, key)
			continue
		}
		if !yours {
			continue
		}
		posted := true
		if err := client.Comment(ctx, key, text); err != nil {
			a.warn("release note for %s not posted: %v", key, err)
			posted = false
			unsure = append(unsure, key)
		} else {
			a.printf("release note posted to %s", key)
		}
		fix.add(ctx, issue)
		if posted {
			sub.reached[issue.Key] = mover.delivered(ctx, issue)
		}
	}
	judged := slices.DeleteFunc(slices.Clone(keys), func(k string) bool { return slices.Contains(unsure, k) })
	unread := a.retryHeld(ctx, sub, held, judged)
	if r.target.ReleaseNotes.Subtasks {
		sub.follow(ctx)
	}
	if err := writeHeld(r, append(unread, mover.held...)); err != nil {
		a.warn("%v", err)
	}
	return notesUnset{fixVersion: fix.unset, assignee: mover.unset.assignee}
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
}

// errHeld is a move withheld for want of the comment require_comment asks
// for, or for the glint findings, reported already.
var errHeld = errors.New("held")

// delivered moves a delivered item along only from the statuses a transition
// names: a paused item stays paused whatever its commits say. It returns the
// status the item is left in.
func (m *noteMover) delivered(ctx context.Context, issue jira.Issue) string {
	switch t := m.conf.Moving(issue.Status); {
	case len(m.conf.Transitions) == 0 || m.conf.Leading(issue.Status) != nil:
	case t == nil:
		m.app.printf("%s stays %s: release_notes.transitions move only from %s", issue.Key, issue.Status, strings.Join(m.conf.FromStatuses(), ", "))
	default:
		if err := m.move(ctx, issue, t, ""); err != nil {
			if !errors.Is(err, errHeld) {
				m.app.warn("%v", err)
			}
			return issue.Status
		}
		return t.To
	}
	return issue.Status
}

// move takes the item by the transition and hands it over; after names the
// parent a subtask follows. An item moved but not handed over is reported
// and recorded, not returned: the move itself happened. An item to be handed
// over without the comment require_comment asks for, or any with the glint
// findings release_notes.glint asks about, is not moved: errHeld.
func (m *noteMover) move(ctx context.Context, issue jira.Issue, t *config.NoteTransition, after string) error {
	how := ""
	if after != "" {
		how = " after its parent " + after
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
	if handover && label == "" && t.RequireComment != "" {
		if why := m.uninstructed(ctx, issue, t, after); why != "" {
			fmt.Fprintf(m.app.Stderr, "graft: %s: NOT MOVED to %s%s: %s; once it is written, graft deploy handover %s moves it\n", issue.Key, t.To, how, why, m.target)
			m.held = append(m.held, heldItem{key: issue.Key, after: after})
			return errHeld
		}
	}
	if err := m.client.Transition(ctx, issue, t.To); err != nil {
		return fmt.Errorf("%s not moved to %s%s: %w", issue.Key, t.To, how, err)
	}
	m.app.printf("%s moved to %s%s%s", issue.Key, t.To, how, m.glint.takenBy())
	if !handover {
		return nil
	}
	if label != "" {
		m.app.printf("%s stays with its assignee: %s (keep_assignee_labels)", issue.Key, label)
		return nil
	}
	if err := m.client.Assign(ctx, issue.Key, t.Assign); err != nil {
		m.app.assignFailed(&m.unset, issue.Key, err)
		return nil
	}
	m.app.printf("%s assigned to %s", issue.Key, t.Assign)
	return nil
}

// keptBy names the label of the transition's keep_assignee_labels that keeps
// the item with its assignee, its own or its parent's: the subtasks of an
// internal parent are internal. It is "" for none.
func (m *noteMover) keptBy(ctx context.Context, issue jira.Issue, t *config.NoteTransition) (string, error) {
	kept := func(labels []string) string {
		if i := slices.IndexFunc(labels, func(l string) bool { return slices.Contains(t.KeepAssigneeLabels, l) }); i >= 0 {
			return labels[i]
		}
		return ""
	}
	if label := kept(issue.Labels); label != "" {
		return "label " + label, nil
	}
	if len(t.KeepAssigneeLabels) == 0 || issue.Parent == "" {
		return "", nil
	}
	parent, err := m.client.Issue(ctx, issue.Parent)
	if err != nil {
		return "", err
	}
	if label := kept(parent.Labels); label != "" {
		return "label " + label + " of its parent " + issue.Parent, nil
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
	reached map[string]string
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
	if yours {
		if err := s.mover.move(ctx, issue, t, parent); err != nil && !errors.Is(err, errHeld) {
			app.warn("%v", err)
		}
	}
}

// noteable tells whether an item gets a note, warning when it does not. A key
// can reach a commit by hand, as a reference to an item that is closed or
// somebody else's. An item handed over for review stays yours: its history
// shows it assigned to you.
func (a *App) noteable(ctx context.Context, client *jira.Client, notes *config.JiraNotes, me string, issue jira.Issue) (bool, error) {
	key := issue.Key
	if issue.Done || issue.NotStarted || slices.Contains(notes.SkipStatuses, issue.Status) {
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
	app    *App
	conf   *config.FixVersion
	client *jira.Client
	next   map[string]fixChoice
	unset  []string
}

type fixChoice struct {
	version jira.Version
	err     error
}

// add sets the fix version of a noted item, unless it has it already.
func (f *fixVersionRun) add(ctx context.Context, issue jira.Issue) {
	if f.conf == nil {
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
	if err := f.client.AddFixVersion(ctx, issue.Key, v.ID); err != nil {
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
		a.warn("approved release not checked (--unapproved): everything since the deployed commit goes out")
		return nil
	}
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("deploy %s: nothing was deployed: approved release: "+format, append([]any{r.name}, args...)...)
	}
	out, g, err := a.approvalRun(ctx, r)
	if err != nil {
		return refuse("%v", err)
	}
	if len(out.failures) > 0 {
		lines := make([]string, len(out.failures))
		for i, f := range out.failures {
			lines[i] = f.line()
		}
		return refuse("commits outside an approved release:\n  %s\nTheir items:\n  %s\n%s, revert them, or pass --unapproved",
			strings.Join(lines, "\n  "), strings.Join(g.gate.describe(out.failures), "\n  "), g.gate.remedy())
	}
	a.passApproved(r, out)
	return nil
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
	g, err := newGateRun(a, r.repo, r.cfg, r.target.ApprovedRelease)
	if err != nil {
		return gateOutcome{}, nil, err
	}
	active, reverted, err := partitionWithheld(r.repo, r.head, withheld, r.cfg.VersionFiles())
	if err != nil {
		return gateOutcome{}, nil, err
	}
	buildBranch := config.BranchMatches(r.cfg.Version.Build, r.branch)
	out, err := g.run(ctx, r.prev, r.head, active, buildBranch)
	out.withheld = append(reverted, out.withheld...)
	return out, g, err
}

// partitionWithheld separates commits whose changes remain reverted in the
// deployed history. It also follows reverts of reverts, which make the
// original change active again.
func partitionWithheld(repo *gitx.Repo, head string, withheld, versionFiles []string) (active, reverted []string, retErr error) {
	if len(withheld) == 0 {
		return nil, nil, nil
	}
	out, err := repo.Git("log", "--reverse", "--format=%H%x00%s%x00%b%x00%x1e", head)
	if err != nil {
		return nil, nil, err
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
				return nil, nil, fmt.Errorf("%s reverts ambiguous commit prefix %s", gitx.Short(sha), match[1])
			}
			if target == "" {
				continue
			}
			isReverted[target] = !isReverted[target]
			revertTargets[sha] = target
		}
	}
	active = make([]string, 0, len(withheld))
	reverted = make([]string, 0, len(withheld))
	for _, sha := range withheld {
		if isReverted[sha] {
			present, err := deploy.ChangePresentExcept(repo, head, sha, versionFiles...)
			if err != nil {
				return nil, nil, fmt.Errorf("checking withheld commit %s in deployed history: %w", gitx.Short(sha), err)
			}
			if !present {
				reverted = append(reverted, sha)
				continue
			}
		}
		active = append(active, sha)
	}
	return active, reverted, nil
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
}

// gateRun is one pass of approved_release over the commits going out.
type gateRun struct {
	app   *App
	repo  *gitx.Repo
	cfg   *config.Config
	conf  *config.ApprovedRelease
	gate  *approvalGate
	keyRe *regexp.Regexp
}

func newGateRun(a *App, repo *gitx.Repo, cfg *config.Config, conf *config.ApprovedRelease) (*gateRun, error) {
	client, err := jiraClient()
	if err != nil {
		return nil, err
	}
	return &gateRun{
		app: a, repo: repo, cfg: cfg, conf: conf,
		gate:  &approvalGate{client: client, conf: conf, issues: map[string]jira.Issue{}, versions: map[string]bool{}},
		keyRe: projectKeyRe(cfg),
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
	passed []string
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
	return out, nil
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
	if at, ok := g.conf.Attributed[c.sha]; ok {
		instead := ""
		if named := slices.DeleteFunc(commitKeys(g.keyRe, nil, c), func(k string) bool { return k == at.Item }); len(named) > 0 {
			instead = " instead of " + strings.Join(named, ", ")
		}
		g.app.printf("approved release: %s: attributed to %s%s: %s", line, at.Item, instead, at.Reason)
	}
	if fail.keys = commitKeys(g.keyRe, g.conf, c); len(fail.keys) > 0 {
		pass, err := g.gate.commit(ctx, fail.keys)
		if err != nil {
			return nil, err
		}
		if pass.ok {
			out.passed = append(out.passed, fail.keys...)
			out.approved[pass.label] = true
			if pass.done != "" {
				g.app.printf("approved release: %s: %s is %s, passes", line, pass.done, g.gate.issues[pass.done].Status)
			}
			return nil, nil
		}
		fail.why = pass.why
	}
	ignored, err := shipsNothing(g.repo, g.cfg, g.conf, c.sha)
	if err != nil {
		return nil, err
	}
	if ignored {
		g.app.printf("approved release: %s: changes only ignore_paths, passes", line)
		return nil, nil
	}
	return &fail, nil
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

// commitKeys are the project's item keys a commit names in its subject and
// body, or the item approved_release.attribute ties it to, a revert of such
// a commit included: its message quotes the one it reverts; conf may be nil.
func commitKeys(keyRe *regexp.Regexp, conf *config.ApprovedRelease, c deliveredCommit) []string {
	if conf != nil {
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
		if len(items) > 0 {
			return uniqueSorted(items)
		}
	}
	return uniqueSorted(keyRe.FindAllString(c.subject+"\n"+c.body, -1))
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
			if keys := commitKeys(keyRe, nil, c); slices.Equal(keys, []string{at.Item}) {
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

// shipsNothing tells whether a commit changes only approved_release's
// ignore_paths, besides the version bump graft commit puts into every commit.
func shipsNothing(repo *gitx.Repo, cfg *config.Config, conf *config.ApprovedRelease, sha string) (bool, error) {
	if len(conf.Ignore) == 0 {
		return false, nil
	}
	out, err := repo.Git("diff-tree", "--no-commit-id", "--name-only", "-r", "--root", "-z", sha)
	if err != nil {
		return false, err
	}
	bumped := cfg.VersionFiles()
	var files []string
	for f := range strings.SplitSeq(out, "\x00") {
		if f = strings.TrimSpace(f); f != "" && !slices.Contains(bumped, f) {
			files = append(files, f)
		}
	}
	return conf.Ignored(files), nil
}

// approvalGate reads each item and each version once per deploy.
type approvalGate struct {
	client  *jira.Client
	conf    *config.ApprovedRelease
	issues  map[string]jira.Issue
	history map[string]fixVersionHistoryResult
	// versions maps a version id to whether every approver approved it.
	versions map[string]bool
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

// gatePass is the verdict on one commit.
type gatePass struct {
	ok bool
	// label names what let the commit out: a version or a status.
	label string
	// done is the key of a done item that let the commit out, worth a line
	// of its own: it may be an old item a commit only refers to.
	done string
	// why the commit stays outside.
	why string
}

// commit lets a commit out when one of the items it names passes.
func (g *approvalGate) commit(ctx context.Context, keys []string) (gatePass, error) {
	var whys []string
	for _, key := range keys {
		pass, err := g.item(ctx, key)
		if err != nil || pass.ok {
			return pass, err
		}
		whys = append(whys, pass.why)
	}
	return gatePass{why: strings.Join(whys, "; ")}, nil
}

// item checks one item against the statuses and the approved version.
func (g *approvalGate) item(ctx context.Context, key string) (gatePass, error) {
	issue, err := g.issue(ctx, key)
	if err != nil {
		return gatePass{}, err
	}
	var pass gatePass
	if len(g.conf.Statuses) > 0 {
		checked, err := g.checkedIssue(ctx, issue)
		if err != nil {
			return pass, err
		}
		if checked == nil {
			return gatePass{why: fmt.Sprintf("%s is %s, not %s", key, issue.Status, orList(g.conf.Statuses))}, nil
		}
		pass = gatePass{ok: true, label: checked.Status}
		if checked.Done {
			pass.done = checked.Key
		}
	}
	if g.conf.Prefix != "" {
		version, err := g.approvedVersion(ctx, issue)
		if err != nil {
			return gatePass{}, err
		}
		if version == "" {
			why := fmt.Sprintf("%s not in an approved %q version", key, g.conf.Prefix)
			if issue.Parent != "" {
				why += ", nor is its parent " + issue.Parent
			}
			return gatePass{why: why}, nil
		}
		pass.ok, pass.label = true, version
	}
	return pass, nil
}

// checkedIssue returns the item when it is in one of the statuses, or its
// parent when that is, and nil when neither is.
func (g *approvalGate) checkedIssue(ctx context.Context, issue jira.Issue) (*jira.Issue, error) {
	if slices.Contains(g.conf.Statuses, issue.Status) {
		return &issue, nil
	}
	if issue.Parent == "" {
		return nil, nil
	}
	parent, err := g.issue(ctx, issue.Parent)
	if err != nil {
		return nil, err
	}
	if slices.Contains(g.conf.Statuses, parent.Status) {
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
// approver approved, or else its parent's: a subtask goes out with the
// item it is part of. It is "" when neither has one.
func (g *approvalGate) approvedVersion(ctx context.Context, issue jira.Issue) (string, error) {
	name, err := g.ownApprovedVersion(ctx, issue)
	if err != nil || name != "" || issue.Parent == "" {
		return name, err
	}
	parent, err := g.issue(ctx, issue.Parent)
	if err != nil {
		return "", err
	}
	return g.ownApprovedVersion(ctx, parent)
}

func (g *approvalGate) ownApprovedVersion(ctx context.Context, issue jira.Issue) (string, error) {
	for _, v := range issue.FixVersions {
		if !strings.HasPrefix(v.Name, g.conf.Prefix) {
			continue
		}
		full, seen := g.versions[v.ID]
		if !seen {
			approvers, err := g.client.VersionApprovers(ctx, v.ID)
			if err != nil {
				return "", err
			}
			full = jira.FullyApproved(approvers)
			g.versions[v.ID] = full
		}
		if full {
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
				line += "; " + g.fixVersions(issue)
			}
			lines = append(lines, line)
		}
	}
	return lines
}

// fixVersions lists the item's fix versions of the prefix and whether every
// approver approved each.
func (g *approvalGate) fixVersions(issue jira.Issue) string {
	var vs []string
	for _, v := range issue.FixVersions {
		if !strings.HasPrefix(v.Name, g.conf.Prefix) {
			continue
		}
		state := "not approved by every approver"
		if g.versions[v.ID] {
			state = "approved"
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
	if len(g.conf.Statuses) > 0 {
		ways = append(ways, "move their items to "+orList(g.conf.Statuses))
	}
	if g.conf.Prefix != "" {
		ways = append(ways, "put their items into the approved version")
	}
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
// name (subjects and bodies, merges too) or are attributed to, once each,
// sorted: the commits of prev..head and those withheld from an earlier
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
		named := commitKeys(keyRe, r.target.ApprovedRelease, c)
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
