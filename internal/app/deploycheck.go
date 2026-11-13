package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/deploy"
	"github.com/aiseeq/graft/v2/internal/envs"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/proc"
)

// DeployCheck is graft deploy check: the checks graft deploy runs before the
// deps and the script, read only, every refusal listed. Nothing is deployed
// or recorded, and no sudo password is asked for.
func (a *App) DeployCheck(ctx context.Context, target string) error {
	r, err := a.deployTarget(target)
	if err != nil {
		return err
	}
	r.dryRun = true
	var refusals []error
	sudoRefused := false
	refuse := func(err error) {
		if err == nil {
			return
		}
		// Sudo refused once is refused for every record read under it.
		if refused := (*proc.RefusedError)(nil); errors.As(err, &refused) && refused.Path == "sudo" {
			if sudoRefused {
				return
			}
			sudoRefused = true
		}
		refusals = append(refusals, err)
	}
	refuse(deploy.CheckTree(r.repo, r.cfg.Deploy))
	sudoRefusal := refusedSudo(r.target, r.env)
	refuse(sudoRefusal)
	if r.head, err = deploy.PublishedHead(r.repo, r.cfg.Deploy); err != nil {
		refuse(err)
		head, err := r.repo.Git("rev-parse", "HEAD")
		if err != nil {
			return err
		}
		r.head = strings.TrimSpace(head)
	}
	if r.cfg.Version.Mode != config.ModeNone {
		if r.version, err = projectVersion(r.repo, r.cfg); err != nil {
			return err
		}
	}
	a.printf("checking graft deploy %s for %s (version %s) on %s; nothing is deployed", r.name, gitx.Short(r.head), r.version, r.env.Destination())
	if r.now, err = deployNow(); err != nil {
		return err
	}
	if r.branch, err = r.repo.Branch(); err != nil {
		return err
	}
	if r.freeze, err = activeFreezeForRun(r); err != nil {
		if !errors.Is(err, errNoActiveFreeze) {
			return err
		}
	}
	if r.hotfix = r.cfg.Deploy.IsHotfix(r.branch); !r.hotfix {
		refuse(a.checkWindow(r))
	}
	defer a.printDeploySummary(ctx, r)
	defer a.printVersionManual(r)
	refuse(a.requireDeployed(ctx, r))
	if err := a.checkDeployed(ctx, r, sudoRefusal, refuse); err != nil {
		return err
	}
	// Freeze refusals are useful even when an earlier check already failed.
	refuse(a.checkFreeze(ctx, r, DeployOptions{}))
	if len(refusals) == 0 {
		a.printf("graft deploy %s passes its checks now; deps, integration checks and confirmation run during the deploy", r.name)
		return nil
	}
	fmt.Fprintf(a.Stdout, "\ngraft deploy %s would refuse:\n", r.name)
	for _, err := range refusals {
		fmt.Fprintf(a.Stdout, "- %s\n", strings.ReplaceAll(err.Error(), "\n", "\n  "))
	}
	return fmt.Errorf("graft deploy %s would refuse, see above", r.name)
}

func (a *App) printVersionManual(r *deployRun) {
	if r.hotfix || r.target.ApprovedRelease == nil || r.target.ApprovedRelease.Version == nil {
		return
	}
	manual := r.target.ApprovedRelease.Version.Manual
	if len(manual) == 0 {
		return
	}
	fmt.Fprintln(a.Stdout, "check by hand before the deploy:")
	for _, line := range manual {
		fmt.Fprintf(a.Stdout, "- %s\n", line)
	}
}

func (a *App) checkDeployed(ctx context.Context, r *deployRun, sudoRefusal error, refuse func(error)) error {
	if r.target.DeployedSHA == nil {
		return a.showDeployed(ctx, r)
	}
	if sudoRefusal != nil && recordUsesSudo(r.target.DeployedSHA, r.env) {
		return nil
	}
	prev, err := r.readSHA(ctx, r.env, r.target.DeployedSHA)
	if err != nil {
		refuse(err)
		return nil
	}
	r.prev = prev
	if err := a.showDeployed(ctx, r); err != nil {
		return err
	}
	if r.prev == r.head {
		refuse(fmt.Errorf("%s already runs %s; graft deploy --redeploy %s deploys it again", r.name, gitx.Short(r.head), r.name))
		return nil
	}
	var listRefusals func(error)
	listRefusals = func(err error) {
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				listRefusals(child)
			}
			return
		}
		refuse(err)
	}
	listRefusals(a.checkReleaseFlow(ctx, r, false))
	return nil
}

// printDeploySummary describes configured effects even when a check refused.
func (a *App) printDeploySummary(ctx context.Context, r *deployRun) {
	t := r.target
	fmt.Fprintln(a.Stdout, "\nwhat the deploy will do:")
	if len(t.Deps) == 0 {
		a.printf("- deps: none")
	} else {
		a.printf("- deps before the script: %s", strings.Join(t.Deps, ", "))
	}
	if t.Confirm == "" {
		a.printf("- confirmation: none")
	} else {
		a.printf("- confirmation: %s", t.Confirm)
	}
	if n := t.ReleaseNotes; n != nil {
		a.printf("- release_notes: Jira comment %q", r.cfg.NoteComment(t))
		if n.Glint != "" {
			a.printf("- release_notes: glint %s and above holds transitions", n.Glint)
		}
		for _, tr := range n.Transitions {
			a.printf("- release_notes: %s", transitionSummary(tr))
		}
		if n.FixVersion != nil {
			a.printf("- fix_version: prefix %q, name %q", n.FixVersion.Prefix, n.FixVersion.Name)
		}
		if n.Subtasks {
			a.printf("- subtasks: follow their parent")
		}
		if n.Sweep != nil {
			a.printf("- sweep: done %s, days %d, stale days %d, apply %t", n.Sweep.Done, n.Sweep.Days, n.Sweep.StaleDays, n.Sweep.Apply)
		}
		a.previewNotes(ctx, r)
	}
	if t.MergeIntoTrunk {
		a.printf("- merge_into_trunk: record the release branch on the trunk")
	}
	if v := t.ReleaseVersion; v != nil && v.Enabled != nil && *v.Enabled {
		a.printf("- release_version: %q; hotfix %q", v.Name, v.Hotfix)
	}
	if p := t.ReleasePage; p != nil {
		a.printf("- release_page: Confluence parent %s; title %q", p.Parent, p.Title)
	}
	a.printf("- command: graft deploy %s", r.name)
}

// transitionSummary is a transition of release_notes in one line, naming
// only the settings it has.
func transitionSummary(tr config.NoteTransition) string {
	parts := []string{strings.Join(tr.From, ", ") + " → " + tr.To}
	if len(tr.Types) > 0 {
		parts = append(parts, "types "+strings.Join(tr.Types, ", "))
	}
	if tr.Assign != "" {
		parts = append(parts, "assign "+tr.Assign)
	}
	if len(tr.KeepAssigneeLabels) > 0 {
		parts = append(parts, "keeps the assignee of "+strings.Join(tr.KeepAssigneeLabels, ", "))
	}
	if tr.RequireComment != "" {
		parts = append(parts, fmt.Sprintf("needs a comment with %q", tr.RequireComment))
	}
	return strings.Join(parts, "; ")
}

// previewKeys are the keys of the commits the deploy brings, or why they are
// unknown.
func previewKeys(r *deployRun) ([]string, string) {
	if r.prev == "" {
		return nil, "no previously deployed commit recorded"
	}
	known, err := r.repo.RevExists(r.prev)
	switch {
	case err != nil:
		return nil, fmt.Sprintf("the previously deployed commit %s is unreadable: %v", gitx.Short(r.prev), err)
	case !known:
		return nil, fmt.Sprintf("the previously deployed commit %s is not in this repository", gitx.Short(r.prev))
	}
	keys, _, _, err := deliveredKeys(r)
	if err != nil {
		return nil, err.Error()
	}
	return keys, ""
}

// previewNotes uses the deploy's key selection and move decision without writes.
func (a *App) previewNotes(ctx context.Context, r *deployRun) {
	keys, why := previewKeys(r)
	if why != "" {
		a.printf("- keys of the new commits not checked: %s", why)
	}
	// The deploy tries again the items earlier deploys held, new commits or not.
	held, err := readHeld(r)
	if err != nil {
		a.printf("- items held by earlier deploys not checked: %v", err)
	}
	if len(keys) == 0 && len(held) == 0 {
		return
	}
	client, err := jiraClient()
	if err != nil {
		a.printf("- keys not checked: %v", err)
		return
	}
	me, err := client.Myself(ctx)
	if err != nil {
		a.printf("- keys not checked: %v", err)
		return
	}
	glint, err := newGlintCheck(r, "", func(context.Context) (string, error) { return r.head, nil })
	if err != nil {
		a.printf("- keys not checked: %v", err)
		return
	}
	if glint.severity != "" {
		clean, cleanErr := r.repo.IsClean()
		if cleanErr != nil || !clean {
			glint.ran = true
			glint.err = errors.New("findings cannot be checked on a dirty work tree")
		}
	}
	m := &noteMover{app: a, client: client, conf: r.target.ReleaseNotes, me: me, target: r.name, glint: glint, standing: r.cfg.ReleaseNotes.Jira.SkipKeys, readOnly: true}
	for _, key := range keys {
		if slices.Contains(r.cfg.ReleaseNotes.Jira.SkipKeys, key) || (r.moveOut != nil && slices.Contains(r.moveOut.keys, key)) {
			a.printf("- %s: left alone", key)
			continue
		}
		issue, err := client.Issue(ctx, key)
		if err != nil {
			a.printf("- %s: not checked: %v", key, err)
			continue
		}
		yours, err := a.noteable(ctx, client, r.cfg.ReleaseNotes.Jira, me, issue)
		if err != nil {
			a.printf("- %s: not checked: %v", key, err)
			continue
		}
		if !yours {
			a.printf("- %s: %s → %s (left alone)", key, issue.Status, issue.Status)
			continue
		}
		if issue.Done {
			a.printf("- %s: %s → %s (note only, if not quiet)", key, issue.Status, issue.Status)
			continue
		}
		if status := m.delivered(ctx, issue); status == issue.Status {
			a.printf("- %s: %s → %s (stays)", key, issue.Status, status)
		}
	}
	a.retryHeld(ctx, &subtaskRun{mover: m, reached: map[string]string{}}, held, keys)
}

// DeployVersions is graft deploy versions: the version and the commit each
// target runs, read without a sudo password.
func (a *App) DeployVersions(ctx context.Context) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	if cfg.Deploy == nil {
		return errors.New("deploy is not configured: add a deploy section to .graft.yaml")
	}
	var failed []string
	for _, name := range targetOrder(cfg.Deploy.Targets) {
		t := cfg.Deploy.Targets[name]
		env, err := envs.Get(cfg, repo.Root, t.Env)
		if err != nil {
			return err
		}
		var sha string
		var shaErr error
		if t.DeployedSHA != nil {
			sha, shaErr = deploy.PeekDeployedSHA(ctx, env, t.DeployedSHA)
		}
		s, err := readDeployed(ctx, repo, cfg, env, t, sha)
		if err != nil {
			return err
		}
		if shaErr != nil {
			s.commit, s.failed = "unknown: "+shaErr.Error(), true
		}
		if s.failed {
			failed = append(failed, name)
		}
		fmt.Fprintf(a.Stdout, "%s (%s): version %s; commit %s\n", name, env.Destination(), s.version, s.commit)
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not read %s", strings.Join(slices.Compact(failed), ", "))
	}
	return nil
}

// deployedState is what a target runs: its version and its commit, each
// read or said why not; failed tells that one could not be read.
type deployedState struct {
	version, commit string
	failed          bool
}

// readDeployed describes what a target runs, sha being the commit its
// deployed_sha records (empty: none recorded, or no deployed_sha). The
// version comes from the target's version command, or else from the version
// files of that commit; the commit is described with the remote branches it
// is on.
func readDeployed(ctx context.Context, repo *gitx.Repo, cfg *config.Config, env *envs.Env, t *config.DeployTarget, sha string) (deployedState, error) {
	s := deployedState{version: "no version command", commit: "no deployed_sha"}
	known := false
	if t.DeployedSHA != nil {
		s.commit = "none recorded"
		if sha != "" {
			var err error
			if known, err = repo.RevExists(sha); err != nil {
				return s, err
			}
			if s.commit, err = describeDeployedCommit(repo, sha, known); err != nil {
				return s, err
			}
		}
	}
	switch {
	case t.Version.IsSet():
		v, err := deploy.DeployedVersion(ctx, env, t.Version)
		if err != nil {
			s.version, s.failed = "unknown: "+err.Error(), true
		} else {
			s.version = v
		}
	case known && cfg.Version.Mode != config.ModeNone:
		v, err := versionAt(repo, cfg, sha)
		if err != nil {
			s.version, s.failed = "unknown: "+err.Error(), true
		} else {
			s.version = v + " by its commit"
		}
	}
	return s, nil
}

// describeDeployedCommit is the commit a target runs, from the local
// repository: subject, date and the remote branches at it, or else those
// that contain it.
func describeDeployedCommit(repo *gitx.Repo, sha string, known bool) (string, error) {
	if !known {
		return gitx.Short(sha) + ", not in the local repository (git fetch?)", nil
	}
	line, err := repo.Git("log", "-1", "--format=%h %s (%cs)", sha)
	if err != nil {
		return "", err
	}
	branches, err := remoteBranches(repo, sha)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line) + ", " + branches, nil
}

// remoteBranches says which remote branches are at sha, or else contain it,
// three named at most.
func remoteBranches(repo *gitx.Repo, sha string) (string, error) {
	for _, how := range []struct{ flag, word string }{{"--points-at", "at"}, {"--contains", "in"}} {
		out, err := repo.Git("for-each-ref", how.flag, sha, "--format=%(refname:short)", "refs/remotes")
		if err != nil {
			return "", err
		}
		var names []string
		for n := range strings.FieldsSeq(out) {
			// refs/remotes/<remote>/HEAD is short for the remote alone.
			if strings.Contains(n, "/") && !strings.HasSuffix(n, "/HEAD") {
				names = append(names, n)
			}
		}
		if len(names) > 3 {
			names = append(names[:3], fmt.Sprintf("%d more", len(names)-3))
		}
		if len(names) > 0 {
			return how.word + " " + strings.Join(names, ", "), nil
		}
	}
	return "on no remote branch", nil
}

// targetOrder lists targets after the ones they require, by name otherwise:
// test, then uat, then prod.
func targetOrder(targets map[string]*config.DeployTarget) []string {
	depth := func(name string) int {
		n := 0
		for seen := map[string]bool{}; targets[name] != nil && targets[name].Requires != "" && !seen[name]; n++ {
			seen[name] = true
			name = targets[name].Requires
		}
		return n
	}
	return slices.SortedFunc(maps.Keys(targets), func(x, y string) int {
		return cmp.Or(cmp.Compare(depth(x), depth(y)), cmp.Compare(x, y))
	})
}
