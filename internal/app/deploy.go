package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
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
	// Unapproved skips the approved_release check.
	Unapproved bool
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
	if err := a.checkApproved(ctx, r, o.Unapproved); err != nil {
		return err
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
	return a.recordDeployment(ctx, r)
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
	// The task name has a space no task name can have.
	err = r.tasks.Measure("deploy "+r.name, r.target.Run.Source, cmd)
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
	if r.target.DeployedSHA == nil {
		return nil
	}
	if err := deploy.WriteDeployedSHA(ctx, r.env, r.target.DeployedSHA, r.head); err != nil {
		a.warn("the deployed commit is not recorded: %v", err)
	}
	if r.target.ReleaseNotes == nil {
		return nil
	}
	if unset := a.releaseNotes(ctx, r); len(unset) > 0 {
		return fmt.Errorf("deployed %s to %s, but the fix version is not set on %s (see above)", gitx.Short(r.head), r.name, strings.Join(unset, ", "))
	}
	return nil
}

func (a *App) warn(format string, args ...any) {
	fmt.Fprintf(a.Stderr, "graft: warning: "+format+"\n", args...)
}

// releaseNotes posts the notes and returns the keys of the items whose fix
// version, when the target asks for one, is not set.
func (a *App) releaseNotes(ctx context.Context, r *deployRun) []string {
	notes := r.cfg.ReleaseNotes.Jira
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
	keys, err := deliveredKeys(r.repo, r.prev, r.head, notes.ProjectKeys)
	if err != nil {
		a.warn("release notes skipped: %v", err)
		return nil
	}
	if len(keys) == 0 {
		a.printf("release notes: no work item keys in %s..%s", gitx.Short(r.prev), gitx.Short(r.head))
		return nil
	}
	fix := &fixVersionRun{app: a, conf: r.target.ReleaseNotes.FixVersion}
	// skipAll reports notes that cannot be posted at all; the fix version of
	// every delivered item is then unknown.
	skipAll := func(format string, err error) []string {
		a.warn(format, err)
		for _, key := range keys {
			fix.failed(key, fmt.Errorf("release notes skipped: %w", err))
		}
		return fix.unset
	}
	client, err := jiraClient()
	if err != nil {
		return skipAll("release notes skipped: %v", err)
	}
	fix.client = client
	text, err := envs.Fill(notes.Comment, map[string]string{"target": r.name, "version": r.version, "short": gitx.Short(r.head), "sha": r.head}, func(s string) string { return s })
	if err != nil {
		return skipAll("release notes skipped: %v", err)
	}
	me, err := client.Myself(ctx)
	if err != nil {
		return skipAll("release notes skipped: whose items are whose is unknown: %v", err)
	}
	for _, key := range keys {
		issue, err := client.Issue(ctx, key)
		if err != nil {
			a.warn("release note for %s not posted: %v", key, err)
			fix.failed(key, err)
			continue
		}
		yours, err := a.noteable(ctx, client, notes, me, issue)
		if err != nil {
			a.warn("release note for %s not posted: %v", key, err)
			fix.failed(key, err)
			continue
		}
		if !yours {
			continue
		}
		posted := true
		if err := client.Comment(ctx, key, text); err != nil {
			a.warn("release note for %s not posted: %v", key, err)
			posted = false
		} else {
			a.printf("release note posted to %s", key)
		}
		fix.add(ctx, issue)
		if posted {
			a.moveDelivered(ctx, client, issue, r.target.ReleaseNotes)
		}
	}
	return fix.unset
}

// noteable tells whether an item gets a note, warning when it does not. A key
// can reach a commit by hand, as a reference to an item that is closed or
// somebody else's. An item handed over for review stays yours: its history
// shows it assigned to you.
func (a *App) noteable(ctx context.Context, client *jira.Client, notes *config.JiraNotes, me string, issue jira.Issue) (bool, error) {
	key := issue.Key
	if issue.Done || slices.Contains(notes.SkipStatuses, issue.Status) {
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
		if err == nil {
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
		if v.Released || v.Archived || !strings.HasPrefix(v.Name, f.conf.Prefix) {
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

// moveDelivered moves a delivered item along only from the statuses the
// target names: a paused item stays paused whatever its commits say.
func (a *App) moveDelivered(ctx context.Context, client *jira.Client, issue jira.Issue, notes *config.TargetNotes) {
	to := notes.Transition
	switch {
	case to == "" || issue.Status == to:
	case !slices.Contains(notes.From, issue.Status):
		a.printf("%s stays %s: release_notes.from moves only from %s", issue.Key, issue.Status, strings.Join(notes.From, ", "))
	default:
		if err := client.Transition(ctx, issue, to); err != nil {
			a.warn("%s not moved to %s: %v", issue.Key, to, err)
			return
		}
		a.printf("%s moved to %s", issue.Key, to)
	}
}

// checkApproved lets out only the commits of an approved release: every
// commit going out names an item with a fix version that begins with the
// prefix and that every approver has approved. A revert takes nothing new out
// and passes; a merge brings in commits that are checked themselves.
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
	if r.prev == "" {
		return refuse("no deployed commit recorded, what goes out is unknown; check it by hand and pass --unapproved")
	}
	known, err := r.repo.RevExists(r.prev)
	if err != nil {
		return refuse("%v", err)
	}
	if !known {
		return refuse("the deployed commit %s is unknown here (git fetch?)", r.prev)
	}
	commits, err := deliveredCommits(r.repo, r.prev, r.head)
	if err != nil {
		return refuse("%v", err)
	}
	client, err := jiraClient()
	if err != nil {
		return refuse("%v", err)
	}
	gate := &approvalGate{client: client, prefix: conf.Prefix, items: map[string]string{}, versions: map[string]bool{}}
	keyRe := regexp.MustCompile(`\b(` + strings.Join(r.cfg.ReleaseNotes.Jira.ProjectKeys, "|") + `)-[0-9]+\b`)
	var outside []string
	approved := map[string]bool{}
	for _, c := range commits {
		line := gitx.Short(c.sha) + " " + c.subject
		if isRevert(c.subject) {
			a.printf("approved release: %s: a revert, passes", line)
			continue
		}
		why := "names no item"
		if keys := uniqueSorted(keyRe.FindAllString(c.subject+"\n"+c.body, -1)); len(keys) > 0 {
			version, err := gate.approvedVersion(ctx, keys)
			if err != nil {
				return refuse("%v", err)
			}
			if version != "" {
				approved[version] = true
				continue
			}
			why = fmt.Sprintf("%s not in an approved %q version", strings.Join(keys, ", "), conf.Prefix)
		}
		ignored, err := a.shipsNothing(r, conf, c.sha)
		if err != nil {
			return refuse("%v", err)
		}
		if ignored {
			a.printf("approved release: %s: changes only ignore_paths, passes", line)
			continue
		}
		outside = append(outside, line+": "+why)
	}
	if len(outside) > 0 {
		return refuse("commits outside an approved release:\n  %s\nput their items into the approved version, revert them, or pass --unapproved", strings.Join(outside, "\n  "))
	}
	a.printf("approved release: %d commits since %s, all in %s", len(commits), gitx.Short(r.prev), strings.Join(slices.Sorted(maps.Keys(approved)), ", "))
	return nil
}

// shipsNothing tells whether a commit changes only approved_release's
// ignore_paths, besides the version bump graft commit puts into every commit.
func (a *App) shipsNothing(r *deployRun, conf *config.ApprovedRelease, sha string) (bool, error) {
	if len(conf.Ignore) == 0 {
		return false, nil
	}
	out, err := r.repo.Git("diff-tree", "--no-commit-id", "--name-only", "-r", "--root", "-z", sha)
	if err != nil {
		return false, err
	}
	bumped := []string{}
	if r.cfg.Version.Mode == config.ModeFile {
		bumped = append(bumped, r.cfg.Version.File)
		for _, f := range r.cfg.Version.Sync {
			bumped = append(bumped, f.Path)
		}
	}
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
	client *jira.Client
	prefix string
	// items maps a key to the name of its approved version, "" for none.
	items map[string]string
	// versions maps a version id to whether every approver approved it.
	versions map[string]bool
}

// approvedVersion names an approved version of one of the items, "" when
// none of them has one.
func (g *approvalGate) approvedVersion(ctx context.Context, keys []string) (string, error) {
	for _, key := range keys {
		name, ok := g.items[key]
		if !ok {
			var err error
			if name, err = g.itemVersion(ctx, key); err != nil {
				return "", err
			}
			g.items[key] = name
		}
		if name != "" {
			return name, nil
		}
	}
	return "", nil
}

// itemVersion names the item's fix version of the prefix that every
// approver approved, "" when it has none.
func (g *approvalGate) itemVersion(ctx context.Context, key string) (string, error) {
	issue, err := g.client.Issue(ctx, key)
	if err != nil {
		return "", err
	}
	for _, v := range issue.FixVersions {
		if !strings.HasPrefix(v.Name, g.prefix) {
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

// isRevert recognizes git's own subject (Revert "…") and the Conventional
// Commits revert type.
func isRevert(subject string) bool {
	s := strings.ToLower(subject)
	return strings.HasPrefix(s, "revert ") || strings.HasPrefix(s, "revert:") || strings.HasPrefix(s, "revert(") || strings.HasPrefix(s, "revert!")
}

type deliveredCommit struct {
	sha, subject, body string
}

// deliveredCommits lists the commits that go out, merges left out.
func deliveredCommits(repo *gitx.Repo, prev, head string) ([]deliveredCommit, error) {
	out, err := repo.Git("log", "--no-merges", "--format=%H%x1f%s%x1f%b%x1e", prev+".."+head)
	if err != nil {
		return nil, err
	}
	var commits []deliveredCommit
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x1f", 3)
		if len(f) != 3 {
			return nil, fmt.Errorf("unexpected git log record %q", rec)
		}
		commits = append(commits, deliveredCommit{sha: f[0], subject: f[1], body: f[2]})
	}
	return commits, nil
}

func uniqueSorted(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
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
