package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/deploy"
	"github.com/aiseeq/graft/internal/dotenv"
	"github.com/aiseeq/graft/internal/envs"
	"github.com/aiseeq/graft/internal/gitx"
	"github.com/aiseeq/graft/internal/jira"
	"github.com/aiseeq/graft/internal/lock"
	"github.com/aiseeq/graft/internal/tasks"
	"github.com/aiseeq/graft/internal/userconfig"
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
	path := filepath.Join(r.repo.CommonDir, "graft-locks", "deploy-"+o.Target+".lock")
	lk, err := lock.AcquireMode(ctx, path, lock.Exclusive, r.cfg.Lock.Timeout, "graft deploy "+o.Target, a.Stderr)
	if err != nil {
		return err
	}
	err = a.deploy(ctx, r, o)
	if releaseErr := lk.Release(); releaseErr != nil {
		err = errors.Join(err, fmt.Errorf("releasing the deploy lock: %w", releaseErr))
	}
	return err
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
	// requires is a quick read-only check: it fails before a long deps run.
	if err := a.requireDeployed(ctx, r); err != nil {
		return err
	}
	// The deployed commit is read before deps, sudo and the script: a target
	// that already runs HEAD has nothing to gain from them.
	if r.target.DeployedSHA != nil {
		if r.prev, err = deploy.ReadDeployedSHA(ctx, r.env, r.target.DeployedSHA); err != nil {
			return err
		}
		if err := a.refuseSameCommit(r, o.Redeploy); err != nil {
			return err
		}
	}
	if len(r.target.Deps) > 0 {
		a.printf("deploy %s: running %s first", r.name, strings.Join(r.target.Deps, ", "))
		if err := r.tasks.RunDeps(ctx, r.target.Deps); err != nil {
			return fmt.Errorf("deploy %s: nothing was deployed: %w", r.name, err)
		}
	}
	if r.target.Confirm == "sudo" {
		if err := deploy.ConfirmSudo(ctx, r.name, a.Stderr); err != nil {
			return err
		}
	}
	if err := a.runDeployScript(ctx, r, o.Args); err != nil {
		return err
	}
	if err := deploy.CheckHead(r.repo, r.cfg.Deploy, r.head); err != nil {
		return fmt.Errorf("the deploy script finished, but %w; the deployed commit is not recorded", err)
	}
	a.printf("deployed %s to %s", gitx.Short(r.head), r.name)
	a.recordDeployment(ctx, r)
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
	var problems []string
	seen := map[string]bool{}
	check := func(key string) {
		if seen[key] {
			return
		}
		seen[key] = true
		if _, err := lookup.Value(key); err != nil {
			problems = append(problems, err.Error())
		}
	}
	for _, k := range r.target.Keys {
		switch {
		case k.NonEmpty:
			seen[k.Name] = true
			if v, err := lookup.Value(k.Name); err != nil {
				problems = append(problems, err.Error())
			} else if v == "" {
				problems = append(problems, k.Name+" is empty")
			}
		case !k.Optional:
			check(k.Name)
		}
	}
	for _, w := range r.target.Run.Argv {
		refs, err := dotenv.Refs(w)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		for _, key := range refs {
			check(key)
		}
	}
	problems = append(problems, r.tasks.DepsMissing(r.target.Deps)...)
	if len(problems) > 0 {
		return fmt.Errorf("deploy %s: nothing was done, missing:\n  %s", r.name, strings.Join(problems, "\n  "))
	}
	return nil
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
	err = cmd.Run()
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
// release is live by now, so a failure here is reported, not returned.
func (a *App) recordDeployment(ctx context.Context, r *deployRun) {
	if r.target.DeployedSHA == nil {
		return
	}
	if err := deploy.WriteDeployedSHA(ctx, r.env, r.target.DeployedSHA, r.head); err != nil {
		a.warn("the deployed commit is not recorded: %v", err)
	}
	if r.target.ReleaseNotes != nil {
		a.releaseNotes(ctx, r)
	}
}

func (a *App) warn(format string, args ...any) {
	fmt.Fprintf(a.Stderr, "graft: warning: "+format+"\n", args...)
}

func (a *App) releaseNotes(ctx context.Context, r *deployRun) {
	notes := r.cfg.ReleaseNotes.Jira
	if r.prev == "" {
		a.warn("release notes skipped: no previously deployed commit recorded (the next deploy will have one)")
		return
	}
	if ok, err := r.repo.RevExists(r.prev); err != nil || !ok {
		a.warn("release notes skipped: the previously deployed commit %s is unknown here (git fetch?)", r.prev)
		return
	}
	keys, err := deliveredKeys(r.repo, r.prev, r.head, notes.ProjectKeys)
	if err != nil {
		a.warn("release notes skipped: %v", err)
		return
	}
	if len(keys) == 0 {
		a.printf("release notes: no work item keys in %s..%s", gitx.Short(r.prev), gitx.Short(r.head))
		return
	}
	client, err := jiraClient()
	if err != nil {
		a.warn("release notes skipped: %v", err)
		return
	}
	text, err := envs.Fill(notes.Comment, map[string]string{"target": r.name, "version": r.version, "short": gitx.Short(r.head), "sha": r.head}, func(s string) string { return s })
	if err != nil {
		a.warn("release notes skipped: %v", err)
		return
	}
	me, err := client.Myself(ctx)
	if err != nil {
		a.warn("release notes skipped: whose items are whose is unknown: %v", err)
		return
	}
	for _, key := range keys {
		issue, err := client.Issue(ctx, key)
		if err != nil {
			a.warn("release note for %s not posted: %v", key, err)
			continue
		}
		// A key can reach a commit by hand, as a reference to an item that is
		// closed or somebody else's. An item handed over for review stays
		// yours: its history shows it assigned to you.
		if issue.Done {
			a.warn("release note for %s not posted, %s left alone: it is already %s", key, key, issue.Status)
			continue
		}
		if issue.AssigneeID != me {
			yours, err := client.WasAssignedTo(ctx, key, me)
			if err != nil {
				a.warn("release note for %s not posted: %v", key, err)
				continue
			}
			if !yours {
				whose := "unassigned"
				if issue.AssigneeID != "" {
					whose = "assigned to " + issue.AssigneeName
				}
				a.warn("release note for %s not posted, %s left alone: %s and never assigned to the owner of the Jira token", key, key, whose)
				continue
			}
		}
		if err := client.Comment(ctx, key, text); err != nil {
			a.warn("release note for %s not posted: %v", key, err)
			continue
		}
		a.printf("release note posted to %s", key)
		if to := r.target.ReleaseNotes.Transition; to != "" {
			moved, err := client.Transition(ctx, issue, to, notes.SkipStatuses)
			switch {
			case err != nil:
				a.warn("%s not moved to %s: %v", key, to, err)
			case moved:
				a.printf("%s moved to %s", key, to)
			}
		}
	}
}

// deliveredKeys lists the project's work item keys mentioned in the subjects
// and bodies of the delivered commits, once each, sorted.
func deliveredKeys(repo *gitx.Repo, prev, head string, projectKeys []string) ([]string, error) {
	out, err := repo.Git("log", "--format=%s%n%b", prev+".."+head)
	if err != nil {
		return nil, err
	}
	re := regexp.MustCompile(`\b(` + strings.Join(projectKeys, "|") + `)-[0-9]+\b`)
	var keys []string
	for _, k := range re.FindAllString(out, -1) {
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys, nil
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
