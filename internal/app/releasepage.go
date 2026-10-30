package app

import (
	"context"
	"errors"
	"fmt"
	"html"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/deploy"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/jira"
)

// publishReleasePage publishes what the release took out on the release
// page in Confluence. The release is live by now, so a failure is printed
// and returned for the exit status.
func (a *App) publishReleasePage(ctx context.Context, r *deployRun) error {
	if r.target.ReleasePage == nil {
		return nil
	}
	if err := a.releasePage(ctx, r, r.target.ReleasePage); err != nil {
		fmt.Fprintf(a.Stderr, "graft: release page NOT PUBLISHED: %v\n", err)
		return fmt.Errorf("the release page is not published: %w", err)
	}
	return nil
}

// pageRelease names the release by the window's version, the hotfix's or
// the approved versions it took items from, with its day.
func pageRelease(r *deployRun) ([]string, string, error) {
	rv := r.target.ReleaseVersion
	switch {
	case r.hotfix && rv != nil && rv.HotfixTemplate != nil:
		day := hotfixDay(r)
		return []string{rv.HotfixTemplate.Format(day)}, day.Format(time.DateOnly), nil
	case r.hotfix && rv != nil:
		return nil, "", errors.New("a hotfix has no release name: set release_version.hotfix")
	case rv != nil:
		occ := deploy.Next(r.target.Window, r.now)
		return []string{rv.Template.Format(occ.Start)}, occ.Date, nil
	case len(r.approvedVersions) > 0:
		return r.approvedVersions, r.now.Format(time.DateOnly), nil
	default:
		return nil, "", errors.New("no approved version names the release (deployed with --unapproved, or a hotfix?)")
	}
}

func (a *App) releasePage(ctx context.Context, r *deployRun, p *config.ReleasePage) error {
	names, date, err := pageRelease(r)
	if err != nil {
		return err
	}
	title := strings.ReplaceAll(p.Title, "{release}", strings.Join(names, ", "))
	client, err := jiraClient()
	if err != nil {
		return err
	}
	shipped := r.passed
	if r.hotfix {
		shipped = r.hotfixItems
	}
	moved, err := withheldItems(r, shipped)
	if err != nil {
		return err
	}
	section, err := buildSection(ctx, client, p, r, shipped, moved)
	if err != nil {
		return err
	}
	if frozen := r.freezeForOwnedTarget(); frozen != nil && frozen.branch == r.branch {
		journal, err := a.freezeJournal(r.repo, r.cfg, frozen)
		if err != nil {
			return err
		}
		section += journal
	}
	space, err := client.PageSpace(ctx, p.Parent)
	if err != nil {
		return err
	}
	page, found, err := client.FindPage(ctx, space, title)
	if err != nil {
		return err
	}
	if !found {
		header, err := pageHeader(ctx, client, p, r, names, date)
		if err != nil {
			return err
		}
		page, err = client.CreatePage(ctx, space, p.Parent, title, header+section)
		if err != nil {
			return err
		}
		a.printf("release page %q published: %s", title, page.URL)
		return nil
	}
	heading := buildHeading(p, r)
	if strings.Contains(page.Body, heading) {
		if strings.Contains(section, "<h2>Freeze journal</h2>") {
			body := replaceFreezeJournal(page.Body, section[strings.Index(section, "<h2>Freeze journal</h2>"):])
			if body != page.Body {
				if _, err := client.UpdatePage(ctx, page, body, "freeze journal "+r.version); err != nil {
					return err
				}
			}
		}
		a.printf("release page %q already lists build %s: %s", title, r.version, page.URL)
		return nil
	}
	if page, err = client.UpdatePage(ctx, page, page.Body+section, "build "+r.version); err != nil {
		return err
	}
	a.printf("release page %q: build %s added: %s", title, r.version, page.URL)
	return nil
}

func replaceFreezeJournal(body, journal string) string {
	const heading = "<h2>Freeze journal</h2>"
	start := strings.Index(body, heading)
	if start < 0 {
		return body + journal
	}
	end := strings.Index(body[start:], "</ul>")
	if end < 0 {
		return body[:start] + journal
	}
	return body[:start] + journal + body[start+end+len("</ul>"):]
}

func (a *App) freezeJournal(repo *gitx.Repo, cfg *config.Config, f *activeFreeze) (string, error) {
	tags, err := freezeLogTags(repo, cfg, f)
	if err != nil {
		return "", err
	}
	if len(tags) == 0 {
		return "", nil
	}
	names := make([]string, 0, len(tags))
	for tag := range tags {
		names = append(names, tag)
	}
	slices.Sort(names)
	var b strings.Builder
	b.WriteString("<h2>Freeze journal</h2><ul>")
	for _, tag := range names {
		refs := tags[tag]
		if len(refs) == 0 {
			return "", fmt.Errorf("freeze journal tag %s has no remote ref", tag)
		}
		contents, err := readRemoteTag(repo, refs[0])
		if err != nil {
			return "", err
		}
		kind := tag[strings.LastIndex(tag, "-")+1:]
		detail := strings.TrimSpace(strings.ReplaceAll(contents, "\n", "; "))
		b.WriteString("<li>" + html.EscapeString(kind+": "+detail) + "</li>")
	}
	b.WriteString("</ul>")
	return b.String(), nil
}

// withheldItems are the items of the commits this release withheld, the
// ones it takes out aside: they wait for the next release.
func withheldItems(r *deployRun, shipped []string) ([]string, error) {
	keyRe := projectKeyRe(r.cfg)
	var keys []string
	for _, sha := range r.withheld {
		c, err := oneCommit(r.repo, sha)
		if err != nil {
			return nil, fmt.Errorf("withheld commit %s: %w", gitx.Short(sha), err)
		}
		for _, k := range commitKeys(keyRe, r.target.ApprovedRelease, c) {
			if !slices.Contains(shipped, k) {
				keys = append(keys, k)
			}
		}
	}
	return uniqueSorted(keys), nil
}

// pageHeader is the table a new page starts with: the release, its day and
// the target.
func pageHeader(ctx context.Context, client *jira.Client, p *config.ReleasePage, r *deployRun, names []string, date string) (string, error) {
	var links []string
	for _, name := range names {
		link := html.EscapeString(name)
		project, v, found, err := findProjectVersion(ctx, client, r.cfg.ReleaseNotes.Jira.ProjectKeys, name)
		if err != nil {
			return "", err
		}
		if found {
			link = anchor(client.VersionURL(project, v.ID), name)
		}
		links = append(links, link)
	}
	var b strings.Builder
	b.WriteString("<table><tbody>")
	for _, row := range [][2]string{{"release", strings.Join(links, ", ")}, {"date", html.EscapeString(date)}, {"target", html.EscapeString(r.name)}} {
		b.WriteString("<tr><th>" + html.EscapeString(p.Label(row[0])) + "</th><td>" + row[1] + "</td></tr>")
	}
	b.WriteString("</tbody></table>")
	return b.String(), nil
}

func findProjectVersion(ctx context.Context, client *jira.Client, projects []string, name string) (string, jira.Version, bool, error) {
	for _, project := range projects {
		versions, err := client.ProjectVersions(ctx, project)
		if err != nil {
			return "", jira.Version{}, false, err
		}
		if i := slices.IndexFunc(versions, func(v jira.Version) bool { return v.Name == name }); i >= 0 {
			return project, versions[i], true, nil
		}
	}
	return "", jira.Version{}, false, nil
}

// buildHeading names the build a section is about.
func buildHeading(p *config.ReleasePage, r *deployRun) string {
	text := strings.NewReplacer("{version}", r.version, "{short}", gitx.Short(r.head)).Replace(p.Label("build"))
	return "<h2>" + html.EscapeString(text) + "</h2>"
}

// buildSection lists the items of one build: changes, fixes and the items
// that wait for the next release.
func buildSection(ctx context.Context, client *jira.Client, p *config.ReleasePage, r *deployRun, shipped, moved []string) (string, error) {
	read := func(keys []string) ([]jira.Issue, error) {
		var out []jira.Issue
		for _, k := range keys {
			issue, err := client.Issue(ctx, k)
			if err != nil {
				return nil, err
			}
			out = append(out, issue)
		}
		return out, nil
	}
	items, err := read(shipped)
	if err != nil {
		return "", err
	}
	waiting, err := read(moved)
	if err != nil {
		return "", err
	}
	var changes, fixes []jira.Issue
	for k := range items {
		if slices.Contains(p.FixTypes, items[k].Type) {
			fixes = append(fixes, items[k])
		} else {
			changes = append(changes, items[k])
		}
	}
	var b strings.Builder
	b.WriteString(buildHeading(p, r))
	for _, part := range []struct {
		label  string
		issues []jira.Issue
	}{{"changes", changes}, {"fixes", fixes}, {"moved", waiting}} {
		if len(part.issues) == 0 {
			continue
		}
		b.WriteString("<h3>" + html.EscapeString(p.Label(part.label)) + "</h3>")
		b.WriteString(issueTable(client, p, part.issues))
	}
	if len(items) == 0 && len(waiting) == 0 {
		b.WriteString("<p>" + html.EscapeString(p.Label("none")) + "</p>")
	}
	return b.String(), nil
}

func issueTable(client *jira.Client, p *config.ReleasePage, issues []jira.Issue) string {
	var b strings.Builder
	b.WriteString("<table><tbody><tr>")
	for _, h := range []string{"issue", "summary", "type"} {
		b.WriteString("<th>" + html.EscapeString(p.Label(h)) + "</th>")
	}
	b.WriteString("</tr>")
	for k := range issues {
		i := &issues[k]
		b.WriteString("<tr><td>" + anchor(client.IssueURL(i.Key), i.Key) + "</td><td>" + html.EscapeString(i.Summary) + "</td><td>" + html.EscapeString(i.Type) + "</td></tr>")
	}
	b.WriteString("</tbody></table>")
	return b.String()
}

func anchor(href, text string) string {
	return `<a href="` + html.EscapeString(href) + `">` + html.EscapeString(text) + "</a>"
}
