package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/deploy"
	"github.com/aiseeq/graft/internal/gitx"
	"github.com/aiseeq/graft/internal/jira"
)

// versionMove is what a deploy to a held target takes out of the window's
// version: the items whose commits it reverts wait for the next window.
type versionMove struct {
	window string
	occ    deploy.Occurrence
	keys   []string
}

// planVersionMove notes the items whose commits the deploy to a held target
// reverts, for the windowed target's versions.
func planVersionMove(r *deployRun, window string, occ deploy.Occurrence, candidate string) error {
	if !r.cfg.Deploy.Targets[window].ReleaseVersion.On() {
		return nil
	}
	commits, err := deliveredCommits(r.repo, candidate, r.head)
	if err != nil {
		return err
	}
	keyRe := projectKeyRe(r.cfg)
	var keys []string
	for _, c := range commits {
		if !isRevert(c.subject) {
			continue
		}
		for _, m := range revertsRe.FindAllStringSubmatch(c.body, -1) {
			reverted, err := oneCommit(r.repo, m[1])
			if err != nil {
				return fmt.Errorf("%s reverts %s: %w", gitx.Short(c.sha), m[1], err)
			}
			keys = append(keys, keyRe.FindAllString(reverted.subject+"\n"+reverted.body, -1)...)
		}
	}
	if len(keys) > 0 {
		r.moveOut = &versionMove{window: window, occ: occ, keys: uniqueSorted(keys)}
	}
	return nil
}

// windowVersions works on the window versions of one deploy, reading each
// project's versions once.
type windowVersions struct {
	app    *App
	client *jira.Client
	conf   *config.ReleaseVersion
	read   map[string][]jira.Version
}

func newWindowVersions(a *App, conf *config.ReleaseVersion) (*windowVersions, error) {
	client, err := jiraClient()
	if err != nil {
		return nil, err
	}
	return &windowVersions{app: a, client: client, conf: conf, read: map[string][]jira.Version{}}, nil
}

// versions reads a project's versions; with write, it first checks that the
// token's user may change them. A name that looks like a template but does
// not follow it is an error.
func (w *windowVersions) versions(ctx context.Context, project string, write bool) ([]jira.Version, error) {
	if write {
		if err := w.client.RequireVersionRights(ctx, project); err != nil {
			return nil, err
		}
	}
	if vs, ok := w.read[project]; ok {
		return vs, nil
	}
	vs, err := w.client.ProjectVersions(ctx, project)
	if err != nil {
		return nil, err
	}
	for _, t := range []*config.VersionName{w.conf.Template, w.conf.HotfixTemplate} {
		if t == nil {
			continue
		}
		if err := jira.SlipsOf(vs, t); err != nil {
			return nil, fmt.Errorf("%s: %w", project, err)
		}
	}
	w.read[project] = vs
	return vs, nil
}

// named finds the unreleased version of the name.
func named(versions []jira.Version, name string) (jira.Version, bool) {
	i := slices.IndexFunc(versions, func(v jira.Version) bool { return v.Name == name && !v.Released && !v.Archived })
	if i < 0 {
		return jira.Version{}, false
	}
	return versions[i], true
}

// ensure finds the unreleased version of the name or creates it.
func (w *windowVersions) ensure(ctx context.Context, project, name, date string) (jira.Version, error) {
	vs, err := w.versions(ctx, project, true)
	if err != nil {
		return jira.Version{}, err
	}
	if v, ok := named(vs, name); ok {
		return v, nil
	}
	id, err := w.client.ProjectID(ctx, project)
	if err != nil {
		return jira.Version{}, err
	}
	v, err := w.client.CreateVersion(ctx, id, name, date)
	if err != nil {
		return jira.Version{}, err
	}
	w.read[project] = append(vs, v)
	w.app.printf("version %s added to %s, release date %s", name, project, date)
	return v, nil
}

// release writes the build into the version's description and releases it
// on the day, renaming it when rename is not empty.
func (w *windowVersions) release(ctx context.Context, v jira.Version, rename, date, build string) error {
	fields := map[string]any{"released": true, "releaseDate": date, "description": strings.TrimSpace(v.Description + "\n" + build)}
	name := v.Name
	if rename != "" {
		fields["name"], name = rename, rename
	}
	if err := w.client.UpdateVersion(ctx, v.ID, fields); err != nil {
		return fmt.Errorf("version %s: %w", v.Name, err)
	}
	if rename != "" {
		w.app.printf("version %s renamed to %s: the window moved", v.Name, rename)
	}
	w.app.printf("version %s released on %s: %s", name, date, build)
	return nil
}

// projectsOf groups item keys by project.
func projectsOf(keys []string) map[string][]string {
	out := map[string][]string{}
	for _, k := range keys {
		project, _, _ := strings.Cut(k, "-")
		out[project] = append(out[project], k)
	}
	return out
}

// moveToNextWindow takes the reverted items out of the window's version and
// into the next window's.
func (a *App) moveToNextWindow(ctx context.Context, r *deployRun) error {
	m := r.moveOut
	wt := r.cfg.Deploy.Targets[m.window]
	conf := wt.ReleaseVersion
	w, err := newWindowVersions(a, conf)
	if err != nil {
		return err
	}
	next := deploy.Next(wt.Window, m.occ.End)
	from, to := conf.Template.Format(m.occ.Start), conf.Template.Format(next.Start)
	var errs []error
	groups := projectsOf(m.keys)
	for _, project := range slices.Sorted(maps.Keys(groups)) {
		keys := groups[project]
		target, err := w.ensure(ctx, project, to, next.Date)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		cur, hasCur := named(w.read[project], from)
		for _, key := range keys {
			if err := a.moveItem(ctx, w.client, key, cur, hasCur, target); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", key, err))
			}
		}
	}
	return errors.Join(errs...)
}

func (a *App) moveItem(ctx context.Context, client *jira.Client, key string, cur jira.Version, hasCur bool, target jira.Version) error {
	issue, err := client.Issue(ctx, key)
	if err != nil {
		return err
	}
	has := func(v jira.Version) bool {
		return slices.ContainsFunc(issue.FixVersions, func(f jira.Version) bool { return f.ID == v.ID })
	}
	if hasCur && has(cur) {
		if err := client.RemoveFixVersion(ctx, key, cur.ID); err != nil {
			return err
		}
	}
	if !has(target) {
		if err := client.AddFixVersion(ctx, key, target.ID); err != nil {
			return err
		}
	}
	a.printf("%s moved to version %s: its work is reverted from the candidate", key, target.Name)
	return nil
}

// checkVersionContents refuses a release whose window version holds items
// the release does not take out: QA approved what the version lists.
func (a *App) checkVersionContents(ctx context.Context, r *deployRun) error {
	conf := r.target.ReleaseVersion
	if !conf.On() || r.hotfix {
		return nil
	}
	w, err := newWindowVersions(a, conf)
	if err != nil {
		return err
	}
	name := conf.Template.Format(deploy.Next(r.target.Window, r.now).Start)
	for _, project := range r.cfg.ReleaseNotes.Jira.ProjectKeys {
		vs, err := w.versions(ctx, project, false)
		if err != nil {
			return fmt.Errorf("deploy %s: nothing was deployed: %w", r.name, err)
		}
		v, ok := named(vs, name)
		if !ok {
			continue
		}
		items, err := w.client.VersionItems(ctx, v.ID)
		if err != nil {
			return fmt.Errorf("deploy %s: nothing was deployed: %w", r.name, err)
		}
		var extra []string
		for _, k := range items {
			if !slices.Contains(r.passed, k) {
				extra = append(extra, k)
			}
		}
		if len(extra) > 0 {
			return fmt.Errorf("deploy %s: nothing was deployed: version %s lists %s, which this release does not take out; take them out of the version (they wait for the next window) or release their work", r.name, name, strings.Join(extra, ", "))
		}
		a.printf("version %s lists only items this release takes out", name)
	}
	return nil
}

// releaseVersions releases the window's version, or the hotfix's, once the
// release is live, and adds the next window's version.
func (a *App) releaseVersions(ctx context.Context, r *deployRun) error {
	conf := r.target.ReleaseVersion
	if !conf.On() || (r.hotfix && conf.HotfixTemplate == nil) {
		return nil
	}
	w, err := newWindowVersions(a, conf)
	if err != nil {
		return err
	}
	build := fmt.Sprintf("build %s, commit %s", r.version, gitx.Short(r.head))
	if r.hotfix {
		return a.releaseHotfixVersion(ctx, r, w, build)
	}
	occ := deploy.Next(r.target.Window, r.now)
	next := deploy.Next(r.target.Window, occ.End)
	var errs []error
	for _, project := range r.cfg.ReleaseNotes.Jira.ProjectKeys {
		if err := a.releaseWindowVersion(ctx, w, project, occ, build); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := w.ensure(ctx, project, conf.Template.Format(next.Start), next.Date); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// releaseWindowVersion releases the version of the window: the one named
// for its day, or the earliest unreleased window version renamed to that
// day, or a new one.
func (a *App) releaseWindowVersion(ctx context.Context, w *windowVersions, project string, occ deploy.Occurrence, build string) error {
	vs, err := w.versions(ctx, project, true)
	if err != nil {
		return err
	}
	name := w.conf.Template.Format(occ.Start)
	if v, ok := named(vs, name); ok {
		return w.release(ctx, v, "", occ.Date, build)
	}
	if v, err := jira.NextDatedVersion(project, vs, w.conf.Template); err == nil {
		return w.release(ctx, v, name, occ.Date, build)
	}
	v, err := w.ensure(ctx, project, name, occ.Date)
	if err != nil {
		return err
	}
	return w.release(ctx, v, "", occ.Date, build)
}

// releaseHotfixVersion puts the hotfix items into the hotfix version of the
// day and releases it.
func (a *App) releaseHotfixVersion(ctx context.Context, r *deployRun, w *windowVersions, build string) error {
	day := hotfixDay(r)
	date := day.Format(time.DateOnly)
	name := w.conf.HotfixTemplate.Format(day)
	var errs []error
	groups := projectsOf(r.hotfixItems)
	for _, project := range slices.Sorted(maps.Keys(groups)) {
		keys := groups[project]
		v, err := w.ensure(ctx, project, name, date)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, key := range keys {
			if err := w.client.AddFixVersion(ctx, key, v.ID); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", key, err))
				continue
			}
			a.printf("fix version %s added to %s", name, key)
		}
		if err := w.release(ctx, v, "", date, build); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// hotfixDay is the day a hotfix goes out on, in the window's zone.
func hotfixDay(r *deployRun) time.Time {
	return r.now.In(r.target.Window.Location)
}

// recordVersions keeps the Jira versions after a deploy: the release is live,
// so a failure is returned for the exit status and the rest goes on.
func (a *App) recordVersions(ctx context.Context, r *deployRun) error {
	var errs []error
	if r.moveOut != nil {
		if err := a.moveToNextWindow(ctx, r); err != nil {
			errs = append(errs, fmt.Errorf("the reverted items are not moved to the next window's version: %w", err))
		}
	}
	if r.target.Window != nil {
		if err := a.releaseVersions(ctx, r); err != nil {
			errs = append(errs, fmt.Errorf("the release version is not kept: %w", err))
		}
	}
	for _, err := range errs {
		fmt.Fprintf(a.Stderr, "graft: versions NOT KEPT: %v\n", err)
	}
	return errors.Join(errs...)
}
