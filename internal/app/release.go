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
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

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
	if migrations.unavailable != "" {
		fmt.Fprintf(a.Stdout, "# migration check unavailable: %s\n", migrations.unavailable)
	}
	printedStays := map[string]bool{}
	for _, grp := range groups {
		fmt.Fprintf(a.Stdout, "\n  # %s\n", grp.title)
		if grp.hasMigrationConflict(migrationConflicts) {
			fmt.Fprintln(a.Stdout, "  # A migration conflict below; keeping the whole task instead is weighed above.")
		}
		for _, f := range grp.failures {
			for _, path := range migrations.staysByCommit[f.commit.sha] {
				if printedStays[path] {
					continue
				}
				printedStays[path] = true
				n, _ := migrationNumber(filepath.Base(path))
				if strings.Contains(filepath.Base(path), ".down.") {
					fmt.Fprintf(a.Stdout, "  # %s stays in the release: its revert does not apply on the candidate\n", path)
				} else {
					fmt.Fprintf(a.Stdout, "  # %s stays in the release: its revert does not apply on the candidate, so number %d runs on prod; if it must not ship, resolve the revert by hand and keep every number up to %d as in the candidate\n", path, n, migrations.highest)
				}
			}
		}
		var body []string
		for _, f := range grp.failures {
			fmt.Fprintf(a.Stdout, "  # %s\n", f.line())
			conflicts := migrationConflicts[f.commit.sha]
			includeCommit := true
			if len(conflicts) == 0 {
				var omitted []string
				omitted = append(omitted, migrations.staysByCommit[f.commit.sha]...)
				if migrations.emptyByCommit[f.commit.sha] {
					fmt.Fprintln(a.Stdout, "  # nothing left to revert in this commit; leave it out of the graft commit below")
					includeCommit = false
				} else {
					fmt.Fprintf(a.Stdout, "  git diff %s %s^ %s -- .%s | git apply -R --3way --index\n", strings.Join(cutoffDiffFlags(), " "), f.commit.sha, f.commit.sha, excludes(r.cfg, omitted))
				}
			} else {
				includeCommit = printCutoffMigrationConflict(a.Stdout, f.commit.sha, conflicts, r.cfg)
			}
			if includeCommit {
				body = append(body, "-m "+envs.Quote("This reverts commit "+f.commit.sha+"."))
			}
		}
		if len(body) > 0 {
			fmt.Fprintf(a.Stdout, "  graft commit -m %s %s\n", envs.Quote("revert: "+grp.what+" not checked by the "+c.occ.Name()+" cutoff"), strings.Join(body, " "))
		}
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

func printCutoffKeepOptions(w io.Writer, groups []failureGroup, migrations cutoffMigrationPlan) {
	var lines []string
	for _, grp := range groups {
		if !grp.hasMigrationConflict(migrations.conflicts) {
			continue
		}
		if why := migrations.keepUnavailable[grp.what]; why != "" {
			lines = append(lines, fmt.Sprintf("#   %s: migration check unavailable if kept whole: %s.", grp.what, why))
			continue
		}
		others := migrations.keep[grp.what]
		fails := migrations.keepRevertFails[grp.what]
		if len(others) == 0 && len(fails) == 0 {
			lines = append(lines, fmt.Sprintf("#   %s: the other items' reverts stay as below.", grp.what))
			continue
		}
		var entries []string
		for _, o := range others {
			entries = append(entries, fmt.Sprintf("%s (number %d, %s)", o.path, o.number, o.item))
		}
		for _, o := range fails {
			entry := fmt.Sprintf("%s (number %d, %s)", o.path, o.number, o.item)
			if !slices.Contains(entries, entry) {
				entries = append(entries, entry)
			}
		}
		var line strings.Builder
		line.WriteString(fmt.Sprintf("#   %s: kept, it leaves the highest remaining number at %d", grp.what, migrations.keepHighest[grp.what]))
		if len(entries) > 0 {
			line.WriteString(", so also exclude from the other items' reverts " + strings.Join(entries, ", "))
		}
		for _, f := range fails {
			line.WriteString(fmt.Sprintf(" and the revert of %s (%s) does not apply then: it stays as in the candidate", f.path, f.item))
		}
		lines = append(lines, line.String()+".")
		emptyCommits := slices.Sorted(maps.Keys(migrations.keepEmpty[grp.what]))
		for _, sha := range emptyCommits {
			lines = append(lines, fmt.Sprintf("#   if keeping %s: nothing left to revert in %s's commit %s; leave it out of the graft commit", grp.what, itemForSHA(groups, sha), gitx.Short(sha)))
		}
	}
	if len(lines) == 0 {
		return
	}
	fmt.Fprintln(w, "# Decide first: an item with a migration conflict can be kept whole instead of reverted; skip its item: its git diff lines and its graft commit. Kept:")
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}
}

func printCutoffMigrationConflict(w io.Writer, sha string, conflicts []cutoffMigration, cfg *config.Config) bool {
	var names, omitted []string
	allApplied := true
	for _, c := range conflicts {
		names = append(names, fmt.Sprintf("%s (number %d)", c.path, c.number))
		if c.unapplied {
			allApplied = false
			fmt.Fprintf(w, "  # the revert of %s does not apply on the candidate, so it stays as in the candidate\n", c.path)
		} else {
			fmt.Fprintf(w, "  # The migrator applies only versions above the last applied version, so %s will be skipped on the next release.\n", c.path)
		}
		for _, p := range c.excludes {
			if !slices.Contains(omitted, p) {
				omitted = append(omitted, p)
			}
		}
	}
	fmt.Fprintf(w, "  # Migration cutoff conflict: %s; highest remaining migration number %d.\n", strings.Join(names, ", "), conflicts[0].highest)
	if conflicts[0].empty {
		fmt.Fprintln(w, "  # nothing left to revert in this commit; leave it out of the graft commit below")
		return false
	}
	fmt.Fprintln(w, "  # Keep the migration with this ready-to-run revert, which excludes the listed migration file(s):")
	var ex strings.Builder
	ex.WriteString(excludes(cfg, omitted))
	fmt.Fprintf(w, "  git diff %s %s^ %s -- .%s | git apply -R --3way --index\n", strings.Join(cutoffDiffFlags(), " "), sha, sha, ex.String())
	if allApplied {
		fmt.Fprintln(w, "  # Or revert and renumber it above the migrations that remain.")
	}
	return true
}

type cutoffMigration struct {
	path            string
	number, highest uint64
	excludes        []string
	empty           bool
	unapplied       bool
}
type cutoffKeepOther struct {
	path, item string
	number     uint64
}
type cutoffMigrationPlan struct {
	conflicts       map[string][]cutoffMigration
	keep            map[string][]cutoffKeepOther
	keepUnavailable map[string]string
	staysByCommit   map[string][]string
	emptyByCommit   map[string]bool
	keepRevertFails map[string][]cutoffKeepOther
	keepHighest     map[string]uint64
	keepEmpty       map[string]map[string]bool
	highest         uint64
	unavailable     string
}
type migrationTree map[uint64]map[string]string

func cutoffMigrationConflicts(repo *gitx.Repo, cfg *config.Config, candidate string, failures []gateFailure) (cutoffMigrationPlan, error) {
	plan := cutoffMigrationPlan{conflicts: map[string][]cutoffMigration{}, keep: map[string][]cutoffKeepOther{}, keepUnavailable: map[string]string{}, staysByCommit: map[string][]string{}, emptyByCommit: map[string]bool{}, keepRevertFails: map[string][]cutoffKeepOther{}, keepHighest: map[string]uint64{}, keepEmpty: map[string]map[string]bool{}}
	if cfg.TestDB == nil || cfg.TestDB.Migrations == "" || len(failures) == 0 {
		return plan, nil
	}
	dir := filepath.ToSlash(filepath.Clean(cfg.TestDB.Migrations))
	spec := migrationPathspec(dir)
	groups := groupFailures(failures)
	ordered := orderedCutoffFailures(groups)
	target, _, err := cutoffTree(repo, candidate, spec, dir, nil, nil, nil)
	if err != nil {
		plan.unavailable = err.Error()
		return plan, nil
	}
	paths := map[string]map[uint64][]string{}
	for _, f := range ordered {
		paths[f.commit.sha], err = cutoffTouched(repo, f.commit.sha, spec, dir)
		if err != nil {
			plan.unavailable = err.Error()
			return plan, nil
		}
	}
	remaining, excluded, unapplied, lost, err := cutoffFixedPoint(repo, candidate, spec, dir, ordered, paths, target, true)
	if err != nil {
		plan.unavailable = err.Error()
		return plan, nil
	}
	_ = lost
	highest := cutoffHighest(cutoffWithoutPaths(remaining, unapplied))
	plan.highest = highest
	for _, f := range ordered {
		for n, ps := range paths[f.commit.sha] {
			for _, p := range ps {
				if excluded[p] && n > highest {
					plan.staysByCommit[f.commit.sha] = append(plan.staysByCommit[f.commit.sha], p)
				}
			}
		}
	}
	conflicts := map[string][]cutoffMigration{}
	for _, f := range ordered {
		var nums []uint64
		var omitted []string
		for n, ps := range paths[f.commit.sha] {
			for _, p := range ps {
				if excluded[p] {
					omitted = append(omitted, p)
					if n <= highest {
						nums = append(nums, n)
					}
				}
			}
		}
		slices.Sort(nums)
		nums = slices.Compact(nums)
		sort.Strings(omitted)
		omitted = slices.Compact(omitted)
		if len(omitted) > 0 {
			empty, e := cutoffEmptyPatch(repo, f.commit.sha, omitted, cfg)
			if e != nil {
				plan.unavailable = e.Error()
				return plan, nil
			}
			plan.emptyByCommit[f.commit.sha] = empty
		}
		for _, n := range nums {
			for _, p := range paths[f.commit.sha][n] {
				if excluded[p] {
					conflicts[f.commit.sha] = append(conflicts[f.commit.sha], cutoffMigration{path: p, number: n, highest: highest, excludes: omitted, empty: plan.emptyByCommit[f.commit.sha], unapplied: unapplied[p]})
				}
			}
		}
	}
	plan.conflicts = conflicts
	itemBySHA := map[string]string{}
	for _, g := range groups {
		for _, f := range g.failures {
			itemBySHA[f.commit.sha] = g.what
		}
	}
	cutoffKeepPlans(repo, cfg, candidate, spec, dir, groups, ordered, target, paths, itemBySHA, &plan)
	return plan, nil
}

func cutoffKeepPlans(repo *gitx.Repo, cfg *config.Config, candidate, spec, dir string, groups []failureGroup, ordered []gateFailure, target migrationTree, paths map[string]map[uint64][]string, itemBySHA map[string]string, plan *cutoffMigrationPlan) {
	for _, g := range groups {
		if !g.hasMigrationConflict(plan.conflicts) {
			continue
		}
		keptFailures := withoutCutoffGroup(ordered, g)
		after, _, failedPaths, newly, e := cutoffFixedPoint(repo, candidate, spec, dir, keptFailures, paths, target, false)
		if e != nil {
			plan.keepUnavailable[g.what] = e.Error()
			continue
		}
		keptNumbers := map[uint64]bool{}
		for _, f := range g.failures {
			for n := range paths[f.commit.sha] {
				keptNumbers[n] = true
			}
		}
		alreadyConflicted := map[uint64]bool{}
		for _, cs := range plan.conflicts {
			for _, c := range cs {
				alreadyConflicted[c.number] = true
			}
		}
		var ns []uint64
		for n := range newly {
			if !alreadyConflicted[n] || keptNumbers[n] {
				ns = append(ns, n)
			}
		}
		slices.Sort(ns)
		plan.keepHighest[g.what] = cutoffHighest(after)
		plan.keep[g.what] = cutoffOtherMigrationPaths(ordered, paths, itemBySHA, g.what, ns)
		seenOther := map[string]bool{}
		seenFailure := map[string]bool{}
		for _, other := range plan.keep[g.what] {
			seenOther[other.path+"\x00"+other.item] = true
		}
		for sha, byNumber := range paths {
			item := itemBySHA[sha]
			if item == g.what {
				continue
			}
			for n, ps := range byNumber {
				for _, p := range ps {
					if failedPaths[p] {
						entry := cutoffKeepOther{path: p, item: item, number: n}
						key := p + "\x00" + item
						if !seenFailure[key] {
							plan.keepRevertFails[g.what] = append(plan.keepRevertFails[g.what], entry)
							seenFailure[key] = true
						}
						if !seenOther[key] {
							plan.keep[g.what] = append(plan.keep[g.what], entry)
							seenOther[key] = true
						}
					}
				}
			}
		}
		plan.keepEmpty[g.what] = map[string]bool{}
		for _, f := range ordered {
			if itemBySHA[f.commit.sha] == g.what {
				continue
			}
			var omit []string
			for n, ps := range paths[f.commit.sha] {
				for _, p := range ps {
					if slices.Contains(ns, n) || failedPaths[p] {
						omit = append(omit, p)
					}
				}
			}
			for _, conflict := range plan.conflicts[f.commit.sha] {
				omit = append(omit, conflict.path)
			}
			if len(omit) == 0 {
				continue
			}
			empty, err := cutoffEmptyPatch(repo, f.commit.sha, omit, cfg)
			if err != nil {
				plan.keepUnavailable[g.what] = err.Error()
				break
			}
			if empty {
				plan.keepEmpty[g.what][f.commit.sha] = true
			}
		}
	}
}

func itemForSHA(groups []failureGroup, sha string) string {
	for _, group := range groups {
		for _, failure := range group.failures {
			if failure.commit.sha == sha {
				return group.what
			}
		}
	}
	return "other item"
}

func cutoffOtherMigrationPaths(ordered []gateFailure, paths map[string]map[uint64][]string, itemBySHA map[string]string, kept string, nums []uint64) []cutoffKeepOther {
	seen := map[string]bool{}
	var out []cutoffKeepOther
	for _, f := range ordered {
		item := itemBySHA[f.commit.sha]
		if item == kept {
			continue
		}
		touched := paths[f.commit.sha]
		for _, n := range nums {
			for _, path := range touched[n] {
				key := path + "\x00" + item
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, cutoffKeepOther{path: path, item: item, number: n})
			}
		}
	}
	return out
}
func orderedCutoffFailures(gs []failureGroup) []gateFailure {
	var out []gateFailure
	for _, g := range gs {
		out = append(out, g.failures...)
	}
	return out
}
func withoutCutoffGroup(fs []gateFailure, g failureGroup) []gateFailure {
	out := make([]gateFailure, 0, len(fs))
	for _, f := range fs {
		if !slices.ContainsFunc(g.failures, func(x gateFailure) bool { return x.commit.sha == f.commit.sha }) {
			out = append(out, f)
		}
	}
	return out
}

func cutoffFixedPoint(repo *gitx.Repo, base, spec, dir string, fs []gateFailure, paths map[string]map[uint64][]string, target migrationTree, excludeLosses bool) (migrationTree, map[string]bool, map[string]bool, map[uint64]bool, error) {
	excluded := map[string]bool{}
	unapplied := map[string]bool{}
	var tree migrationTree
	var lost map[uint64]bool
	for {
		failed := []string(nil)
		var err error
		tree, failed, err = cutoffTree(repo, base, spec, dir, fs, paths, excluded)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		changed := false
		for _, p := range failed {
			unapplied[p] = true
			if !excluded[p] {
				excluded[p] = true
				changed = true
			}
		}
		lost = cutoffLosses(target, tree)
		if excludeLosses {
			for _, touched := range paths {
				for n := range lost {
					for _, p := range touched[n] {
						if !excluded[p] {
							excluded[p] = true
							changed = true
						}
					}
				}
			}
		}
		if !changed {
			return tree, excluded, unapplied, lost, nil
		}
	}
}

func cutoffWithoutPaths(tree migrationTree, paths map[string]bool) migrationTree {
	out := migrationTree{}
	for n, files := range tree {
		for path, blob := range files {
			if paths[path] {
				continue
			}
			if out[n] == nil {
				out[n] = map[string]string{}
			}
			out[n][path] = blob
		}
		if len(out[n]) == 0 {
			delete(out, n)
		}
	}
	return out
}

func cutoffTree(repo *gitx.Repo, base, spec, dir string, fs []gateFailure, touchedByCommit map[string]map[uint64][]string, excluded map[string]bool) (migrationTree, []string, error) {
	tmp, e := os.MkdirTemp("", "graft-cutoff-")
	if e != nil {
		return nil, nil, e
	}
	defer os.RemoveAll(tmp)
	// LC_ALL=C: cutoffPatchConflict reads git apply's English messages.
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index"), "LC_ALL=C"}
	if _, e = repo.GitEnv(env, "read-tree", base); e != nil {
		return nil, nil, e
	}
	var failed []string
	for _, f := range fs {
		touched := touchedByCommit[f.commit.sha]
		var paths []string
		for _, ps := range touched {
			paths = append(paths, ps...)
		}
		sort.Strings(paths)
		for _, p := range paths {
			if excluded[p] {
				continue
			}
			args := append([]string{"diff"}, cutoffDiffFlags()...)
			args = append(args, f.commit.sha+"^", f.commit.sha, "--", ":(top,literal)"+p)
			d, x := repo.Git(args...)
			if x != nil {
				return nil, nil, x
			}
			if d != "" {
				if _, x = repo.GitInput(strings.NewReader(d), env, "apply", "-R", "--cached"); x != nil {
					if !cutoffPatchConflict(x) {
						return nil, nil, x
					}
					failed = append(failed, p)
				}
			}
		}
	}
	out, e := repo.GitEnv(env, "ls-files", "-s", "-z")
	if e != nil {
		return nil, nil, e
	}
	tree := migrationTree{}
	for entry := range strings.SplitSeq(out, "\x00") {
		meta, path, ok := strings.Cut(entry, "\t")
		if !ok || !isTopLevelMigration(path, dir) {
			continue
		}
		fields := strings.Fields(meta)
		// git apply --cached without --3way either applies a path at stage 0 or
		// leaves the index untouched on failure. Count only file and symlink
		// entries; a gitlink is not a migration file.
		if len(fields) < 3 || !(strings.HasPrefix(fields[0], "100") || fields[0] == "120000") || strings.Contains(filepath.Base(filepath.FromSlash(path)), ".down.") {
			continue
		}
		n, ok := migrationNumber(filepath.Base(filepath.FromSlash(path)))
		if !ok {
			continue
		}
		if tree[n] == nil {
			tree[n] = map[string]string{}
		}
		tree[n][path] = fields[1]
	}
	return tree, slices.Compact(failed), nil
}

func cutoffPatchConflict(err error) bool {
	var gitErr *gitx.Error
	if !errors.As(err, &gitErr) || gitx.ExitCode(err) != 1 {
		return false
	}
	message := strings.ToLower(gitErr.Stderr)
	return strings.Contains(message, "patch does not apply") || strings.Contains(message, "patch failed:") || strings.Contains(message, "patch failed at") || strings.Contains(message, "already exists in index") || strings.Contains(message, "does not exist in index")
}

func cutoffLosses(target, remaining migrationTree) map[uint64]bool {
	loss := map[uint64]bool{}
	if len(remaining) == 0 {
		return loss
	}
	highest := cutoffHighest(remaining)
	for n, files := range target {
		if n <= highest && !sameCutoffBlobs(files, remaining[n]) {
			loss[n] = true
		}
	}
	return loss
}
func sameCutoffBlobs(a, b map[string]string) bool {
	// A multiset: two files of a number with the same content are two
	// migrations, and losing one of them is a loss.
	blobs := func(files map[string]string) []string {
		list := make([]string, 0, len(files))
		for _, hash := range files {
			list = append(list, hash)
		}
		sort.Strings(list)
		return list
	}
	return slices.Equal(blobs(a), blobs(b))
}
func cutoffHighest(t migrationTree) uint64 {
	var n uint64
	for k := range t {
		if k > n {
			n = k
		}
	}
	return n
}
func cutoffTouched(repo *gitx.Repo, sha, spec, dir string) (map[uint64][]string, error) {
	args := append([]string{"diff", "--name-only", "-z"}, cutoffDiffFlags()...)
	args = append(args, sha+"^", sha)
	if spec == "" {
		args = append(args, "--", ".")
	} else {
		args = append(args, "--", spec)
	}
	out, e := repo.Git(args...)
	if e != nil {
		return nil, e
	}
	m := map[uint64][]string{}
	for p := range strings.SplitSeq(out, "\x00") {
		if p == "" || !isTopLevelMigration(p, dir) {
			continue
		}
		// The plan names migration paths in shell comments, where a line
		// break would end the comment.
		if strings.ContainsFunc(p, unicode.IsControl) {
			return nil, fmt.Errorf("migration path %q holds a control character; check the reverts of %s by hand", p, gitx.Short(sha))
		}
		n, ok := migrationNumber(filepath.Base(filepath.FromSlash(p)))
		if ok && !slices.Contains(m[n], p) {
			m[n] = append(m[n], p)
		}
	}
	for n := range m {
		sort.Strings(m[n])
	}
	return m, nil
}
func cutoffEmptyPatch(repo *gitx.Repo, sha string, paths []string, cfg *config.Config) (bool, error) {
	// The flags of the printed command: an external diff must not tell an
	// empty patch.
	args := append(append([]string{"diff", "--quiet"}, cutoffDiffFlags()...), sha+"^", sha, "--", ".")
	for _, p := range cfg.VersionFiles() {
		args = append(args, ":(exclude,literal)"+p)
	}
	for _, p := range paths {
		args = append(args, ":(exclude,literal)"+p)
	}
	_, e := repo.Git(args...)
	if e == nil {
		return true, nil
	}
	if gitx.ExitCode(e) == 1 {
		return false, nil
	}
	return false, e
}
func migrationPathspec(dir string) string {
	if dir == "." {
		return ""
	}
	return ":(top,literal)" + dir
}
func isTopLevelMigration(path, dir string) bool {
	return filepath.ToSlash(filepath.Dir(filepath.FromSlash(path))) == dir
}
func migrationNumber(name string) (uint64, bool) {
	i := 0
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, e := strconv.ParseUint(name[:i], 10, 64)
	return n, e == nil
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
func excludes(cfg *config.Config, paths []string) string {
	var out strings.Builder
	for _, f := range cfg.VersionFiles() {
		out.WriteString(" " + envs.Quote(":(exclude,literal)"+f))
	}
	for _, p := range paths {
		out.WriteString(" " + envs.Quote(":(exclude,literal)"+p))
	}
	return out.String()
}

func cutoffDiffFlags() []string {
	return []string{"--binary", "--no-renames", "--no-color", "--no-ext-diff", "--no-textconv", "--src-prefix=a/", "--dst-prefix=b/"}
}
