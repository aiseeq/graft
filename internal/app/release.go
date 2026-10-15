package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/deploy"
	"github.com/aiseeq/graft/internal/envs"
	"github.com/aiseeq/graft/internal/gitx"
	"github.com/aiseeq/graft/internal/jira"
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

func readWithheld(ctx context.Context, env *envs.Env, rec *config.DeployedSHA) ([]string, error) {
	out, err := deploy.ReadDeployedSHA(ctx, env, sibling(rec, withheldSuffix))
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
		return refuse("%v", err)
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
		if baseSHA, err = deploy.ReadDeployedSHA(ctx, env, bt.DeployedSHA); err != nil {
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
		keys := uniqueSorted(keyRe.FindAllString(c.subject+"\n"+c.body, -1))
		labelled, err := labelledItem(ctx, client, keys, h.Label)
		if err != nil {
			return nil, nil, err
		}
		if labelled != "" {
			if !slices.Contains(items, labelled) {
				items = append(items, labelled)
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

// labelledItem names the first of the items with the label, "" for none.
func labelledItem(ctx context.Context, client *jira.Client, keys []string, label string) (string, error) {
	for _, key := range keys {
		issue, err := client.Issue(ctx, key)
		if err != nil {
			return "", err
		}
		if slices.Contains(issue.Labels, label) {
			return key, nil
		}
	}
	return "", nil
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
	released, err := deploy.ReadDeployedSHA(ctx, wenv, wt.DeployedSHA)
	if err != nil {
		return refuse("%v", err)
	}
	if released != "" && released == r.prev {
		a.printf("%s already runs %s, the candidate of the %s window: the hold is over", wname, gitx.Short(released), occ.Name())
		return nil
	}
	candidate, err := heldCandidate(ctx, r.env, r.target.DeployedSHA, r.prev, occ)
	if err != nil {
		return refuse("%v", err)
	}
	if candidate == "" {
		return refuse("%s holds the release candidate of %s from %s, but runs no recorded commit to hold", r.name, wname, occ.Cutoff.Format("Mon 02.01 15:04"))
	}
	if err := recordCandidate(ctx, r, occ, candidate); err != nil {
		return refuse("%v", err)
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
	return nil
}

// heldCandidate is the candidate recorded for the window, or, before any
// deploy since the cutoff recorded one, the commit the target runs.
func heldCandidate(ctx context.Context, env *envs.Env, rec *config.DeployedSHA, runs string, occ deploy.Occurrence) (string, error) {
	line, err := deploy.ReadDeployedSHA(ctx, env, sibling(rec, candidateSuffix))
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
		r.name, r.name, gitx.Short(r.prev), strings.TrimSpace(subject), r.branch, strings.Join(merges, "\n  "), r.branch, strings.Join(versionFiles(r.cfg), " "))
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
		r.branch, r.name, r.cfg.Deploy.Remote, strings.Join(versionFiles(r.cfg), " "))
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
	withheld, err := readWithheld(ctx, r.env, r.target.DeployedSHA)
	if err != nil {
		return err
	}
	g, err := newGateRun(a, r.repo, r.cfg, r.target.ApprovedRelease)
	if err != nil {
		return err
	}
	out, err := g.run(ctx, r.prev, c.candidate, withheld, false)
	if err != nil {
		return err
	}
	a.printCutoff(r, c, out.failures)
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
	if r.prev, err = deploy.ReadDeployedSHA(ctx, r.env, r.target.DeployedSHA); err != nil {
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
	if c.candidate, err = deploy.ReadDeployedSHA(ctx, henv, ht.DeployedSHA); err != nil {
		return c, err
	}
	when := "the cutoff is " + occ.Cutoff.Format("Mon 02.01 15:04") + ": until then the candidate is whatever " + c.held + " runs"
	if held {
		when = "the cutoff was " + occ.Cutoff.Format("Mon 02.01 15:04") + ", " + c.held + " holds the candidate"
		if c.candidate, err = heldCandidate(ctx, henv, ht.DeployedSHA, c.candidate, occ); err != nil {
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
func (a *App) printCutoff(r *deployRun, c cutoff, failures []gateFailure) {
	branch := "release/" + c.occ.Name()
	remote := r.cfg.Deploy.Remote
	fmt.Fprintf(a.Stdout, "\nThe release goes out from %s, cut from the candidate:\n  git fetch %s\n  git switch -c %s %s\n", branch, remote, branch, c.candidate)
	if len(failures) == 0 {
		fmt.Fprintf(a.Stdout, "  git push -u %s %s\nEvery commit of the candidate passes. In the window:\n  graft deploy %s\n", remote, branch, r.name)
		return
	}
	fmt.Fprintf(a.Stdout, "These commits are not checked: their reverts go onto %s, one commit per item (a conflict: resolve it, git add, go on):\n", branch)
	for _, grp := range groupFailures(failures) {
		fmt.Fprintf(a.Stdout, "\n  # %s\n", grp.title)
		var body []string
		for _, f := range grp.failures {
			fmt.Fprintf(a.Stdout, "  # %s\n", f.line())
			fmt.Fprintf(a.Stdout, "  git diff %s^ %s -- .%s | git apply -R --3way --index\n", f.commit.sha, f.commit.sha, excludes(r.cfg))
			body = append(body, "-m "+envs.Quote("This reverts commit "+f.commit.sha+"."))
		}
		fmt.Fprintf(a.Stdout, "  graft commit -m %s %s\n", envs.Quote("revert: "+grp.what+" not checked by the "+c.occ.Name()+" cutoff"), strings.Join(body, " "))
	}
	fmt.Fprintf(a.Stdout, `
Then the candidate goes to %[1]s again (the hold takes reverts) and out in the window:
  graft deploy %[1]s
  graft deploy %[2]s
After the release, %[3]s records it without taking the reverts: it keeps the work, and the next release checks the withheld commits again:
  git switch %[3]s && git pull --ff-only %[5]s %[3]s
  git merge -s ours --no-edit %[4]s
  git push %[5]s %[3]s
`, c.held, r.name, r.branch, branch, remote)
}

// failureGroup is the failing commits of one item, or of no item.
type failureGroup struct {
	title, what string
	failures    []gateFailure
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
	for _, f := range versionFiles(cfg) {
		out.WriteString(" " + envs.Quote(":(exclude)"+f))
	}
	return out.String()
}
