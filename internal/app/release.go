package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/deploy"
	"github.com/aiseeq/graft/v2/internal/envs"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/jira"
)

// EnvNow, when set (RFC 3339), is the time deploy judges windows and holds
// by: for tests, and for rehearsing a cutoff.
const EnvNow = "GRAFT_NOW"

func deployNow() (time.Time, error) {
	v := os.Getenv(EnvNow)
	if v == "" {
		return time.Now(), nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s=%q: %w", EnvNow, v, err)
	}
	return t, nil
}

// Records graft keeps beside a target's deployed_sha file.
const (
	// withheldSuffix lists the commits a release branch reverted: the trunk
	// still has them, and the next release checks them.
	withheldSuffix = ".withheld"
	// candidateSuffix is the release candidate a target holds: the window's
	// date and the commit.
	candidateSuffix = ".candidate"
)

func sibling(rec *config.DeployedSHA, suffix string) *config.DeployedSHA {
	return &config.DeployedSHA{Path: rec.Path + suffix, Sudo: rec.Sudo}
}

func (r *deployRun) readWithheld(ctx context.Context) ([]string, error) {
	out, err := r.readSHA(ctx, r.env, sibling(r.target.DeployedSHA, withheldSuffix))
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// checkWindow refuses a target with a window outside it.
func (a *App) checkWindow(r *deployRun) error {
	w := r.target.Window
	if w == nil {
		return nil
	}
	if deploy.Open(w, r.now) {
		a.printf("release window %s is open", deploy.Describe(w))
		return nil
	}
	next := deploy.Next(w, r.now)
	hint := ""
	if h := r.cfg.Deploy.Hotfix; h != nil {
		hint = fmt.Sprintf("; a hotfix branch (%s) goes any time", strings.Join(h.Branches, ", "))
	}
	return fmt.Errorf("deploy %s: nothing was deployed: outside the release window (%s); the next one opens %s%s",
		r.name, deploy.Describe(w), next.Start.Format("Mon 02.01 15:04"), hint)
}

// checkHotfix lets a hotfix branch out: it starts from the commit the base
// target runs, and every commit names an item with the hotfix label.
func (a *App) checkHotfix(ctx context.Context, r *deployRun) error {
	h := r.cfg.Deploy.Hotfix
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("deploy %s: nothing was deployed: hotfix %s: "+format, append([]any{r.name, r.branch}, args...)...)
	}
	baseSHA, err := hotfixBase(ctx, r)
	if err != nil {
		return refuse("%w", err)
	}
	commits, err := deliveredCommits(r.repo, baseSHA, r.head)
	if err != nil {
		return refuse("%v", err)
	}
	if len(commits) == 0 {
		return refuse("no commits on top of %s, which %s runs", gitx.Short(baseSHA), h.Base)
	}
	outside, items, err := unlabelled(ctx, r, commits)
	if err != nil {
		return refuse("%v", err)
	}
	r.hotfixItems = items
	if len(outside) > 0 {
		return refuse("every commit must name an item labelled %s:\n  %s", h.Label, strings.Join(outside, "\n  "))
	}
	a.printf("hotfix %s: %d commits on top of %s, which %s runs, for %s; the release window, the hold and approved_release do not apply",
		r.branch, len(commits), gitx.Short(baseSHA), h.Base, strings.Join(items, ", "))
	return nil
}

// hotfixBase reads the commit the hotfix base target runs and checks that
// HEAD starts from it.
func hotfixBase(ctx context.Context, r *deployRun) (string, error) {
	base := r.cfg.Deploy.Hotfix.Base
	baseSHA := r.prev
	if base != r.name {
		bt := r.cfg.Deploy.Targets[base]
		env, err := envs.Get(r.cfg, r.repo.Root, bt.Env)
		if err != nil {
			return "", err
		}
		if baseSHA, err = r.readSHA(ctx, env, bt.DeployedSHA); err != nil {
			return "", err
		}
	}
	if baseSHA == "" {
		return "", fmt.Errorf("%s has no deployed commit recorded to start from", base)
	}
	known, err := r.repo.RevExists(baseSHA)
	if err != nil {
		return "", err
	}
	if !known {
		return "", fmt.Errorf("%s runs %s, unknown here (git fetch?)", base, gitx.Short(baseSHA))
	}
	from, err := r.repo.IsAncestor(baseSHA, r.head)
	if err != nil {
		return "", err
	}
	if !from {
		return "", fmt.Errorf("does not start from %s, which %s runs: branch from it (git switch -c %s %s) and bring the fix over", gitx.Short(baseSHA), base, r.branch, gitx.Short(baseSHA))
	}
	return baseSHA, nil
}

// unlabelled lists the hotfix commits that name no item with the hotfix
// label, and the labelled items the others name.
func unlabelled(ctx context.Context, r *deployRun, commits []deliveredCommit) (outside, items []string, err error) {
	h := r.cfg.Deploy.Hotfix
	client, err := jiraClient()
	if err != nil {
		return nil, nil, err
	}
	keyRe := projectKeyRe(r.cfg)
	conf := r.cfg.Deploy.Targets[h.Base].ApprovedRelease
	for _, c := range commits {
		if isRevert(c.subject) {
			continue
		}
		keys := itemKeys(keyRe, conf, c)
		labelled, err := labelledItems(ctx, client, keys, h.Label)
		if err != nil {
			return nil, nil, err
		}
		if len(labelled) > 0 {
			for _, key := range labelled {
				if !slices.Contains(items, key) {
					items = append(items, key)
				}
			}
			continue
		}
		if conf != nil {
			ignored, err := shipsNothing(r.repo, r.cfg, conf, c.sha)
			if err != nil {
				return nil, nil, err
			}
			if ignored {
				continue
			}
		}
		why := "names no item"
		if len(keys) > 0 {
			why = fmt.Sprintf("%s without the label %s", strings.Join(keys, ", "), h.Label)
		}
		outside = append(outside, gitx.Short(c.sha)+" "+c.subject+": "+why)
	}
	return outside, items, nil
}

// labelledItems names every item with the label when each has it; none otherwise.
func labelledItems(ctx context.Context, client *jira.Client, keys []string, label string) ([]string, error) {
	var labelled []string
	for _, key := range keys {
		issue, err := client.Issue(ctx, key)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(issue.Labels, label) {
			return nil, nil
		} else {
			labelled = append(labelled, key)
		}
	}
	return labelled, nil
}

// checkHold keeps a release candidate on a target from a window's cutoff
// until the window ends or the windowed target runs it. Meanwhile only
// reverts of the candidate's work, changes to ignore_paths and merges of
// fixes already out may go there; a hotfix branch goes as well.
func (a *App) checkHold(ctx context.Context, r *deployRun) error {
	for _, wname := range r.cfg.Deploy.HoldingWindows(r.name) {
		if err := a.checkHeldFor(ctx, r, wname); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) checkHeldFor(ctx context.Context, r *deployRun, wname string) error {
	wt := r.cfg.Deploy.Targets[wname]
	occ, held := deploy.Hold(wt.Window, r.now)
	if !held {
		return nil
	}
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("deploy %s: nothing was deployed: "+format, append([]any{r.name}, args...)...)
	}
	wenv, err := envs.Get(r.cfg, r.repo.Root, wt.Env)
	if err != nil {
		return refuse("%v", err)
	}
	released, err := r.readSHA(ctx, wenv, wt.DeployedSHA)
	if err != nil {
		return refuse("%w", err)
	}
	candidate, err := r.heldCandidate(ctx, r.env, r.target.DeployedSHA, r.prev, occ)
	if err != nil {
		return refuse("%v", err)
	}
	// The hold is over once the windowed target runs the candidate, or work
	// on top of it. The candidate is recorded then: the next deploy here
	// runs on top of it, and is past the hold too.
	over, err := runsCandidate(r.repo, wname, released, candidate)
	if err != nil {
		return refuse("%v", err)
	}
	if over {
		if !r.dryRun {
			if err := recordCandidate(ctx, r, occ, candidate); err != nil {
				return refuse("%v", err)
			}
		}
		a.printf("%s already runs %s, the candidate of the %s window: the hold is over", wname, gitx.Short(candidate), occ.Name())
		return nil
	}
	if candidate == "" {
		return refuse("%s holds the release candidate of %s from %s, but runs no recorded commit to hold", r.name, wname, occ.Cutoff.Format("Mon 02.01 15:04"))
	}
	if !r.dryRun {
		if err := recordCandidate(ctx, r, occ, candidate); err != nil {
			return refuse("%v", err)
		}
	}
	if r.hotfix {
		a.printf("%s holds the candidate %s of the %s window; a hotfix passes the hold", r.name, gitx.Short(candidate), occ.Name())
		return nil
	}
	bad, err := a.outsideHold(r, wt, candidate, released)
	if err != nil {
		return refuse("%v", err)
	}
	if len(bad) > 0 {
		return refuse("%s holds the release candidate %s of the %s %s window (%s) from the cutoff %s until %s or until %s runs it.\nOnly commits on top of the candidate that revert its work, change only ignore_paths or merge a hotfix already on %s may go there; these do not:\n  %s\nsee graft deploy cutoff %s; a hotfix branch goes any time",
			r.name, gitx.Short(candidate), wname, occ.Name(), deploy.Describe(wt.Window), occ.Cutoff.Format("Mon 02.01 15:04"), occ.End.Format("Mon 02.01 15:04"), wname, wname, strings.Join(bad, "\n  "), wname)
	}
	a.printf("%s holds the candidate %s of the %s window: %s takes only its reverts and hotfixes", r.name, gitx.Short(candidate), occ.Name(), gitx.Short(r.head))
	if err := planVersionMove(r, wname, occ, candidate); err != nil {
		return refuse("%v", err)
	}
	return nil
}

// runsCandidate tells whether the windowed target wname, which runs
// released, has the candidate.
func runsCandidate(repo *gitx.Repo, wname, released, candidate string) (bool, error) {
	if released == "" || candidate == "" {
		return false, nil
	}
	if released == candidate {
		return true, nil
	}
	known, err := repo.RevExists(released)
	if err != nil {
		return false, err
	}
	if !known {
		return false, fmt.Errorf("%s runs %s, which is not in the local repository (git fetch?)", wname, gitx.Short(released))
	}
	return repo.IsAncestor(candidate, released)
}

// heldCandidate is the candidate recorded for the window, or, before any
// deploy since the cutoff recorded one, the commit the target runs.
func (r *deployRun) heldCandidate(ctx context.Context, env *envs.Env, rec *config.DeployedSHA, runs string, occ deploy.Occurrence) (string, error) {
	line, err := r.readSHA(ctx, env, sibling(rec, candidateSuffix))
	if err != nil {
		return "", err
	}
	if date, sha, ok := strings.Cut(line, " "); ok && date == occ.Date {
		return strings.TrimSpace(sha), nil
	}
	return runs, nil
}

func recordCandidate(ctx context.Context, r *deployRun, occ deploy.Occurrence, candidate string) error {
	return deploy.WriteDeployedSHA(ctx, r.env, sibling(r.target.DeployedSHA, candidateSuffix), occ.Date+" "+candidate)
}

// outsideHold lists the commits on top of the candidate a held target does
// not take.
func (a *App) outsideHold(r *deployRun, wt *config.DeployTarget, candidate, released string) ([]string, error) {
	from, err := r.repo.IsAncestor(candidate, r.head)
	if err != nil {
		return nil, err
	}
	if !from {
		return []string{gitx.Short(r.head) + " does not start from the candidate " + gitx.Short(candidate)}, nil
	}
	commits, err := deliveredCommits(r.repo, candidate, r.head)
	if err != nil {
		return nil, err
	}
	var bad []string
	for _, c := range commits {
		if isRevert(c.subject) {
			continue
		}
		if released != "" {
			out, err := r.repo.IsAncestor(c.sha, released)
			if err != nil {
				return nil, err
			}
			if out {
				continue
			}
		}
		if conf := wt.ApprovedRelease; conf != nil {
			ignored, err := shipsNothing(r.repo, r.cfg, conf, c.sha)
			if err != nil {
				return nil, err
			}
			if ignored {
				continue
			}
		}
		bad = append(bad, gitx.Short(c.sha)+" "+c.subject)
	}
	return bad, nil
}

// checkContainsDeployed refuses a release that would take back what the
// target runs: a hotfix or a release branch not merged into the branch
// deployed now.
func (a *App) checkContainsDeployed(r *deployRun) error {
	if r.prev == "" || r.prev == r.head {
		return nil
	}
	known, err := r.repo.RevExists(r.prev)
	if err != nil || !known {
		// checkApproved reports an unknown deployed commit.
		return err
	}
	in, err := r.repo.IsAncestor(r.prev, r.head)
	if err != nil || in {
		return err
	}
	out, err := r.repo.Git("branch", "-r", "--contains", r.prev, "--format=%(refname:short)")
	if err != nil {
		return err
	}
	var merges []string
	for b := range strings.FieldsSeq(out) {
		_, name, _ := strings.Cut(b, "/")
		switch {
		case r.cfg.Deploy.IsHotfix(name):
			merges = append(merges, "git merge --no-ff "+b+"   # a hotfix: its fix must reach "+r.branch)
		case config.BranchMatches(r.cfg.Version.Build, name):
			merges = append(merges, "git merge -s ours --no-edit "+b+"   # a release branch: "+r.branch+" keeps its own code, the reverted commits stay withheld")
		}
	}
	if len(merges) == 0 {
		merges = append(merges, "no hotfix or release branch on a remote has it: find where it came from (git branch -a --contains "+gitx.Short(r.prev)+")")
	}
	subject, err := r.repo.Git("log", "-1", "--format=%s", r.prev)
	if err != nil {
		return err
	}
	return fmt.Errorf("deploy %s: nothing was deployed: %s runs %s %s, which %s lacks; merge it first, then push:\n  %s\n(a conflict in the version files: keep %s's, git checkout --ours -- %s)",
		r.name, r.name, gitx.Short(r.prev), strings.TrimSpace(subject), r.branch, strings.Join(merges, "\n  "), r.branch, strings.Join(r.cfg.VersionFiles(), " "))
}

// recordWithheld writes the commits the release branch withheld; the
// release is live by then, so a failure is returned for the exit status.
func (a *App) recordWithheld(ctx context.Context, r *deployRun) error {
	if !r.gated {
		return nil
	}
	content := strings.Join(r.withheld, "\n")
	if err := deploy.WriteDeployedSHA(ctx, r.env, sibling(r.target.DeployedSHA, withheldSuffix), content); err != nil {
		return fmt.Errorf("the withheld commits are not recorded (the next release will not check them): %s: %w", strings.Join(r.withheld, " "), err)
	}
	if len(r.withheld) > 0 {
		a.printf("%d commits withheld: the next release checks them", len(r.withheld))
	}
	return nil
}

// hotfixDone tells where a hotfix that reached its base target goes next.
func (a *App) hotfixDone(r *deployRun) {
	if !r.hotfix || r.name != r.cfg.Deploy.Hotfix.Base {
		return
	}
	a.printf("hotfix %[1]s is on %[2]s; merge it into the trunk and into a held release branch before the next release, which refuses until then:\n  git switch <branch> && git pull --ff-only %[3]s <branch> && git merge --no-ff %[3]s/%[1]s\n  (a conflict in the version files: keep the branch's own, git checkout --ours -- %[4]s)",
		r.branch, r.name, r.cfg.Deploy.Remote, strings.Join(r.cfg.VersionFiles(), " "))
}

// DeployCutoff is graft deploy cutoff: what the release candidate of a
// window takes out that is not checked yet, and the commands that take it
// back out on a release branch.
func (a *App) DeployCutoff(ctx context.Context, target string) error {
	r, err := a.deployTarget(target)
	if err != nil {
		return err
	}
	w := r.target.Window
	if w == nil || w.Cutoff == nil || r.target.ApprovedRelease == nil {
		return fmt.Errorf("deploy.targets.%s: cutoff needs window.cutoff and approved_release", target)
	}
	if r.now, err = deployNow(); err != nil {
		return err
	}
	if r.branch, err = r.repo.Branch(); err != nil {
		return err
	}
	if r.cfg.Deploy.IsHotfix(r.branch) || config.BranchMatches(r.cfg.Version.Build, r.branch) {
		return fmt.Errorf("run cutoff on the trunk, not on %s: the commands merge the release branch back into the branch checked out", r.branch)
	}
	c, err := a.cutoffCandidate(ctx, r)
	if err != nil || c.candidate == r.prev {
		return err
	}
	withheld, err := r.readWithheld(ctx)
	if err != nil {
		return err
	}
	g, err := newGateRun(a, r.repo, r.cfg, r.target)
	if err != nil {
		return err
	}
	out, err := g.run(ctx, r.prev, c.candidate, withheld, false)
	if err != nil {
		return err
	}
	migrations, err := cutoffMigrationConflicts(r.repo, r.cfg, c.candidate, out.failures)
	if err != nil {
		return err
	}
	a.printCutoff(r, c, out.failures, migrations)
	return nil
}

// cutoff is the window a cutoff command is about and its candidate.
type cutoff struct {
	occ       deploy.Occurrence
	held      string
	candidate string
}

// cutoffCandidate finds the window (the one holding, or the next), what the
// windowed target runs (into r.prev) and the candidate.
func (a *App) cutoffCandidate(ctx context.Context, r *deployRun) (cutoff, error) {
	c := cutoff{held: r.target.Requires}
	occ, held := deploy.Hold(r.target.Window, r.now)
	if !held {
		occ = deploy.Next(r.target.Window, r.now)
	}
	c.occ = occ
	var err error
	if r.prev, err = r.readSHA(ctx, r.env, r.target.DeployedSHA); err != nil {
		return c, err
	}
	if r.prev == "" {
		return c, fmt.Errorf("%s has no deployed commit recorded: what the candidate takes out is unknown", r.name)
	}
	ht := r.cfg.Deploy.Targets[c.held]
	henv, err := envs.Get(r.cfg, r.repo.Root, ht.Env)
	if err != nil {
		return c, err
	}
	if c.candidate, err = r.readSHA(ctx, henv, ht.DeployedSHA); err != nil {
		return c, err
	}
	when := "the cutoff is " + occ.Cutoff.Format("Mon 02.01 15:04") + ": until then the candidate is whatever " + c.held + " runs"
	if held {
		when = "the cutoff was " + occ.Cutoff.Format("Mon 02.01 15:04") + ", " + c.held + " holds the candidate"
		if c.candidate, err = r.heldCandidate(ctx, henv, ht.DeployedSHA, c.candidate, occ); err != nil {
			return c, err
		}
	}
	a.printf("%s window %s, %s-%s; %s", r.name, occ.Name(), occ.Start.Format("Mon 02.01 15:04"), occ.End.Format("15:04"), when)
	if c.candidate == "" {
		return c, fmt.Errorf("%s runs no recorded commit: there is no candidate", c.held)
	}
	for _, sha := range []string{r.prev, c.candidate} {
		if known, err := r.repo.RevExists(sha); err != nil || !known {
			return c, errors.Join(err, fmt.Errorf("%s is unknown here (git fetch?)", sha))
		}
	}
	subject, err := r.repo.Git("log", "-1", "--format=%s", c.candidate)
	if err != nil {
		return c, err
	}
	a.printf("candidate %s %s; %s runs %s", gitx.Short(c.candidate), strings.TrimSpace(subject), r.name, gitx.Short(r.prev))
	if c.candidate == r.prev {
		a.printf("%s runs the candidate already: nothing to release", r.name)
		return c, nil
	}
	in, err := r.repo.IsAncestor(r.prev, c.candidate)
	if err != nil {
		return c, err
	}
	if !in {
		a.warn("%s runs %s, which the candidate lacks: a hotfix not merged yet. Merge it into the release branch below before graft deploy %s, or that deploy refuses", r.name, gitx.Short(r.prev), c.held)
	}
	return c, nil
}

// printCutoff prints the commands that take the failing commits out of the
// candidate on a release branch, deploy it and record it on the trunk.
func (a *App) printCutoff(r *deployRun, c cutoff, failures []gateFailure, migrations cutoffMigrationPlan) {
	migrationConflicts := migrations.conflicts
	branch := "release/" + c.occ.Name()
	remote := r.cfg.Deploy.Remote
	fmt.Fprintf(a.Stdout, "\n# The release goes out from %s, cut from the candidate:\n  git fetch %s\n  git switch -c %s %s\n", branch, remote, branch, c.candidate)
	if len(failures) == 0 {
		fmt.Fprintf(a.Stdout, "  git push -u %s %s\n# Every commit of the candidate passes. In the window:\n  graft deploy %s\n", remote, branch, r.name)
		return
	}
	fmt.Fprintf(a.Stdout, "# These commits are not checked: their reverts go onto %s, one commit per item (a conflict: resolve it, git add, go on):\n", branch)
	groups := groupFailures(failures)
	printCutoffKeepOptions(a.Stdout, groups, migrations)
	for _, grp := range groups {
		fmt.Fprintf(a.Stdout, "\n  # %s\n", grp.title)
		if grp.hasMigrationConflict(migrationConflicts) {
			fmt.Fprintln(a.Stdout, "  # A migration conflict below; keeping the whole task instead is weighed above.")
		}
		var body []string
		for _, f := range grp.failures {
			fmt.Fprintf(a.Stdout, "  # %s\n", f.line())
			conflicts := migrationConflicts[f.commit.sha]
			if len(conflicts) == 0 {
				fmt.Fprintf(a.Stdout, "  git diff %s^ %s -- .%s | git apply -R --3way --index\n", f.commit.sha, f.commit.sha, excludes(r.cfg))
			} else {
				printCutoffMigrationConflict(a.Stdout, f.commit.sha, conflicts, r.cfg)
			}
			body = append(body, "-m "+envs.Quote("This reverts commit "+f.commit.sha+"."))
		}
		fmt.Fprintf(a.Stdout, "  graft commit -m %s %s\n", envs.Quote("revert: "+grp.what+" not checked by the "+c.occ.Name()+" cutoff"), strings.Join(body, " "))
	}
	fmt.Fprintf(a.Stdout, `
# Then the candidate goes to %[1]s again (the hold takes reverts) and out in the window:
  graft deploy %[1]s
  graft deploy %[2]s
# After the release, %[3]s records it without taking the reverts: it keeps the work, and the next release checks the withheld commits again:
  git switch %[3]s && git pull --ff-only %[5]s %[3]s
  git merge -s ours --no-edit %[4]s
  git push %[5]s %[3]s
`, c.held, r.name, r.branch, branch, remote)
}

// printCutoffKeepOptions says, before any revert, that an item with a
// migration conflict may be kept whole instead and what that does to the
// other items' reverts: once one of them is applied, keeping the item means
// undoing it.
func printCutoffKeepOptions(w io.Writer, groups []failureGroup, migrations cutoffMigrationPlan) {
	var lines []string
	for _, grp := range groups {
		if !grp.hasMigrationConflict(migrations.conflicts) {
			continue
		}
		keep := migrations.keep[grp.what]
		if len(keep.others) == 0 {
			lines = append(lines, fmt.Sprintf("#   %s: the other items' reverts stay as below.", grp.what))
			continue
		}
		var others []string
		for _, o := range keep.others {
			others = append(others, fmt.Sprintf("%s (number %d, %s)", o.path, o.number, o.item))
		}
		lines = append(lines, fmt.Sprintf("#   %s: its migrations raise the highest remaining number to %d, so also exclude from the other items' reverts %s.", grp.what, keep.highest, strings.Join(others, ", ")))
	}
	if len(lines) == 0 {
		return
	}
	fmt.Fprintln(w, "# Decide first: an item with a migration conflict can be kept whole instead of reverted; skip its item: its git diff lines and its graft commit. Kept:")
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}
}

func printCutoffMigrationConflict(w io.Writer, sha string, conflicts []cutoffMigration, cfg *config.Config) {
	var migrations []string
	var excluded strings.Builder
	excluded.WriteString(excludes(cfg))
	for _, conflict := range conflicts {
		migrations = append(migrations, fmt.Sprintf("%s (number %d)", conflict.path, conflict.number))
		excluded.WriteString(" " + envs.Quote(":(exclude)"+conflict.path))
		if conflict.excludePath != "" {
			excluded.WriteString(" " + envs.Quote(":(exclude)"+conflict.excludePath))
		}
	}
	highest := conflicts[0].highest
	fmt.Fprintf(w, "  # Migration cutoff conflict: %s; highest remaining migration number %d.\n", strings.Join(migrations, ", "), highest)
	fmt.Fprintln(w, "  # The migrator applies only versions above the last applied version, so this migration will be skipped on the next release.")
	fmt.Fprintln(w, "  # Keep the migration with this ready-to-run revert, which excludes the listed migration file(s):")
	fmt.Fprintf(w, "  git diff %s^ %s -- .%s | git apply -R --3way --index\n", sha, sha, excluded.String())
	fmt.Fprintln(w, "  # Or revert and renumber it above the migrations that remain.")
}

type cutoffMigration struct {
	path, excludePath string
	number, highest   uint64
	deleted           bool
}

// cutoffMigrationConflicts finds reverted migrations whose number is below
// the highest migration that remains in the candidate after the planned
// reverts remove migrations introduced by those commits.
// cutoffMigrationPlan holds the migration conflicts by commit and, for each
// item with one, what keeping that item whole does to the other items'
// reverts.
type cutoffMigrationPlan struct {
	conflicts map[string][]cutoffMigration
	keep      map[string]cutoffKeep
}

// cutoffKeep: kept whole, an item's added migrations stay, so the highest
// remaining number may rise to highest, and the other items' reverts of
// migrations below it (not already conflicts) would skip them too.
type cutoffKeep struct {
	highest uint64
	others  []cutoffKeepOther
}

type cutoffKeepOther struct {
	path, item string
	number     uint64
}

func cutoffMigrationConflicts(repo *gitx.Repo, cfg *config.Config, candidate string, failures []gateFailure) (cutoffMigrationPlan, error) {
	if cfg.TestDB == nil || cfg.TestDB.Migrations == "" || len(failures) == 0 {
		return cutoffMigrationPlan{}, nil
	}
	migrationsDir := filepath.ToSlash(filepath.Clean(cfg.TestDB.Migrations))
	pathspec := migrationPathspec(migrationsDir)
	candidateFiles, err := candidateMigrationFiles(repo, candidate, pathspec, migrationsDir)
	if err != nil {
		return cutoffMigrationPlan{}, err
	}
	changesByCommit, addedByCommit, err := cutoffMigrationChanges(repo, pathspec, migrationsDir, failures)
	if err != nil {
		return cutoffMigrationPlan{}, err
	}
	removedOnRevert := map[string]bool{}
	for _, added := range addedByCommit {
		for path := range added {
			removedOnRevert[path] = true
		}
	}
	highest := highestRemainingMigration(candidateFiles, removedOnRevert)
	conflicts := make(map[string][]cutoffMigration)
	for sha, changes := range changesByCommit {
		for _, changed := range changes {
			if !changed.deleted && changed.number < highest {
				changed.highest = highest
				conflicts[sha] = append(conflicts[sha], changed)
			}
		}
	}
	groups := groupFailures(failures)
	keep := cutoffKeepPlans(groups, conflicts, changesByCommit, addedByCommit, candidateFiles, removedOnRevert, highest)
	return cutoffMigrationPlan{conflicts: conflicts, keep: keep}, nil
}

// cutoffKeepPlans works out, for each item with a migration conflict, what
// keeping it whole does: its added migrations stay, and the other items'
// reverted migrations at or above highest but below the new highest would be
// skipped too.
func cutoffKeepPlans(groups []failureGroup, conflicts, changesByCommit map[string][]cutoffMigration, addedByCommit map[string]map[string]bool, candidateFiles, removedOnRevert map[string]bool, highest uint64) map[string]cutoffKeep {
	above := revertedAtOrAbove(groups, changesByCommit, highest)
	plans := map[string]cutoffKeep{}
	for _, grp := range groups {
		if !grp.hasMigrationConflict(conflicts) {
			continue
		}
		keep := cutoffKeep{highest: highestRemainingMigration(candidateFiles, withoutAddedBy(grp, addedByCommit, removedOnRevert))}
		if keep.highest <= highest {
			continue
		}
		keep.others = slices.DeleteFunc(slices.Clone(above), func(o cutoffKeepOther) bool {
			return o.item == grp.what || o.number >= keep.highest
		})
		plans[grp.what] = keep
	}
	return plans
}

// withoutAddedBy is removedOnRevert less the migration files grp's commits
// add: what the reverts remove once grp is kept.
func withoutAddedBy(grp failureGroup, addedByCommit map[string]map[string]bool, removedOnRevert map[string]bool) map[string]bool {
	removed := maps.Clone(removedOnRevert)
	for _, f := range grp.failures {
		for path := range addedByCommit[f.commit.sha] {
			delete(removed, path)
		}
	}
	return removed
}

// revertedAtOrAbove lists, in group order, the migrations the groups' reverts
// change (not restore) numbered at or above highest: no conflict while every
// item is reverted.
func revertedAtOrAbove(groups []failureGroup, changesByCommit map[string][]cutoffMigration, highest uint64) []cutoffKeepOther {
	var out []cutoffKeepOther
	for _, grp := range groups {
		for _, f := range grp.failures {
			for _, changed := range changesByCommit[f.commit.sha] {
				if !changed.deleted && changed.number >= highest {
					out = append(out, cutoffKeepOther{path: changed.path, item: grp.what, number: changed.number})
				}
			}
		}
	}
	return out
}

func migrationPathspec(migrationsDir string) string {
	if migrationsDir == "." {
		return ""
	}
	return ":(top,literal)" + migrationsDir
}

func candidateMigrationFiles(repo *gitx.Repo, candidate, pathspec, migrationsDir string) (map[string]bool, error) {
	args := []string{"ls-tree", "-r", "--name-only", "-z", candidate}
	if pathspec != "" {
		args = append(args, "--", pathspec)
	}
	tree, err := repo.Git(args...)
	if err != nil {
		return nil, fmt.Errorf("listing candidate migrations: %w", err)
	}
	files := map[string]bool{}
	for path := range strings.SplitSeq(tree, "\x00") {
		if path != "" && isTopLevelMigration(path, migrationsDir) {
			files[path] = true
		}
	}
	return files, nil
}

// cutoffMigrationChanges lists by commit the migrations its revert changes
// and the migration files it added, which its revert removes.
func cutoffMigrationChanges(repo *gitx.Repo, pathspec, migrationsDir string, failures []gateFailure) (map[string][]cutoffMigration, map[string]map[string]bool, error) {
	changesByCommit := make(map[string][]cutoffMigration, len(failures))
	addedByCommit := make(map[string]map[string]bool, len(failures))
	for _, f := range failures {
		args := []string{"diff", "--name-status", "-z", "--no-renames", f.commit.sha + "^", f.commit.sha}
		if pathspec != "" {
			args = append(args, "--", pathspec)
		}
		out, err := repo.Git(args...)
		if err != nil {
			return nil, nil, fmt.Errorf("listing migrations changed by %s: %w", f.commit.sha, err)
		}
		changes, add := cutoffMigrationChangesForCommit(out, migrationsDir)
		changesByCommit[f.commit.sha] = changes
		addedByCommit[f.commit.sha] = add
	}
	return changesByCommit, addedByCommit, nil
}

type migrationFileChange struct {
	status byte
	path   string
	number uint64
}

func cutoffMigrationChangesForCommit(out, migrationsDir string) ([]cutoffMigration, map[string]bool) {
	fields := strings.Split(out, "\x00")
	var changes []migrationFileChange
	for i := 0; i+1 < len(fields); {
		status, path := fields[i], fields[i+1]
		i += 2
		if status == "" || path == "" || (status[0] != 'A' && status[0] != 'M' && status[0] != 'D') || !isTopLevelMigration(path, migrationsDir) {
			continue
		}
		number, ok := migrationNumber(filepath.Base(filepath.FromSlash(path)))
		if ok {
			changes = append(changes, migrationFileChange{status: status[0], path: path, number: number})
		}
	}
	var migrations []cutoffMigration
	added := map[string]bool{}
	pairedAdds := map[int]bool{}
	pairedDeletes := map[int]bool{}
	addedByNumber := map[uint64][]int{}
	for i, change := range changes {
		if change.status == 'A' {
			addedByNumber[change.number] = append(addedByNumber[change.number], i)
		}
	}
	nextAdded := map[uint64]int{}
	for i, change := range changes {
		if change.status != 'D' {
			continue
		}
		indices := addedByNumber[change.number]
		next := nextAdded[change.number]
		if next >= len(indices) {
			continue
		}
		j := indices[next]
		nextAdded[change.number] = next + 1
		migrations = append(migrations, cutoffMigration{path: change.path, excludePath: changes[j].path, number: change.number})
		pairedAdds[j] = true
		pairedDeletes[i] = true
	}
	for i, change := range changes {
		switch change.status {
		case 'A':
			added[change.path] = true
			if !pairedAdds[i] {
				migrations = append(migrations, cutoffMigration{path: change.path, number: change.number})
			}
		case 'M':
			migrations = append(migrations, cutoffMigration{path: change.path, number: change.number})
		case 'D':
			if !pairedDeletes[i] {
				migrations = append(migrations, cutoffMigration{path: change.path, number: change.number, deleted: true})
			}
		}
	}
	return migrations, added
}

func isTopLevelMigration(path, migrationsDir string) bool {
	return filepath.ToSlash(filepath.Dir(filepath.FromSlash(path))) == migrationsDir
}

func highestRemainingMigration(candidateFiles, removedOnRevert map[string]bool) uint64 {
	var highest uint64
	for path := range candidateFiles {
		if removedOnRevert[path] {
			continue
		}
		number, ok := migrationNumber(filepath.Base(filepath.FromSlash(path)))
		if ok && number > highest {
			highest = number
		}
	}
	return highest
}

// migrationNumber reads the leading decimal digits of a migration filename.
func migrationNumber(name string) (uint64, bool) {
	i := 0
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, err := strconv.ParseUint(name[:i], 10, 64)
	return n, err == nil
}

// failureGroup is the failing commits of one item, or of no item.
type failureGroup struct {
	title, what string
	failures    []gateFailure
}

func (g failureGroup) hasMigrationConflict(conflicts map[string][]cutoffMigration) bool {
	return slices.ContainsFunc(g.failures, func(f gateFailure) bool { return len(conflicts[f.commit.sha]) > 0 })
}

// groupFailures groups the failing commits by the first item each names, in
// the order the newest commit of each group comes (git log order).
func groupFailures(failures []gateFailure) []failureGroup {
	var groups []failureGroup
	index := map[string]int{}
	for _, f := range failures {
		key, title, what := "", "commits naming no item", "commits naming no item"
		if len(f.keys) > 0 {
			key, title, what = f.keys[0], f.why, f.keys[0]
		}
		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			groups = append(groups, failureGroup{title: title, what: what})
		}
		groups[i].failures = append(groups[i].failures, f)
	}
	return groups
}

// excludes keeps the version files out of a reverse patch: every commit
// bumps them, and the release branch counts its own builds.
func excludes(cfg *config.Config) string {
	var out strings.Builder
	for _, f := range cfg.VersionFiles() {
		out.WriteString(" " + envs.Quote(":(exclude)"+f))
	}
	return out.String()
}
