package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// releaseConfig has a test target and a prod target with Monday and Thursday
// windows in UTC+3, a cutoff at 18:00 the day before, items in Ready for
// release or Done going out, and hotfix branches from what prod runs.
func releaseConfig(state string, q func(string) string) string {
	return `schema: 1
version: {mode: file, build_branches: ['hotfix/*', 'release/*']}
envs:
  local: {}
deploy:
  remote: origin
  hotfix: {branches: ['hotfix/*'], label: hotfix, base: prod}
  targets:
    test:
      env: local
      run: [sh, deploy.sh]
      version: 'cat ` + filepath.Join(state, "test-version") + `'
      deployed_sha: {path: ` + q("test-sha") + `}
      release_notes: {transitions: [{from: [In Progress], to: Testing, assign: qa}]}
    prod:
      env: local
      run: [sh, deploy.sh]
      requires: test
      version: 'cat ` + filepath.Join(state, "prod-version") + `'
      deployed_sha: {path: ` + q("prod-sha") + `}
      window: {days: [mon, thu], at: "15:00", until: "19:00", tz: Etc/GMT-3, cutoff: {days_before: 1, at: "18:00"}}
      approved_release: {statuses: [Ready for release, Done], ignore_paths: ["docs/**"]}
      release_notes: {transitions: [{from: [Ready for release], to: Done}, {from: [In Progress], to: Testing, assign: qa}]}
release_notes:
  jira:
    project_keys: [PROJ]
`
}

// at sets the time graft judges windows by: 2026-10-19 is a Monday.
func (d *deployFixture) at(when string) {
	d.env = append(d.env, "GRAFT_NOW="+strings.Replace(when, " ", "T", 1)+":00+03:00")
}

func (d *deployFixture) setStatus(key, status string) {
	d.jira.mu.Lock()
	defer d.jira.mu.Unlock()
	d.jira.statuses[key] = status
}

func (d *deployFixture) status(key string) string {
	d.jira.mu.Lock()
	defer d.jira.mu.Unlock()
	return d.jira.statuses[key]
}

func (d *deployFixture) commitFile(name, msg string) string {
	d.write(name, msg+"\n")
	// The build branches of the deploy fixtures.
	branch := d.git("branch", "--show-current")
	d.mustVersionedCommit(d.work, msg, strings.HasPrefix(branch, "hotfix/") || strings.HasPrefix(branch, "release/"))
	return d.git("rev-parse", "HEAD")
}

func (d *deployFixture) plainCommitFile(name, msg string) string {
	d.write(name, msg+"\n")
	d.mustPlainCommit(msg)
	d.git("push", "-q", "origin", "HEAD")
	return d.git("rev-parse", "HEAD")
}

// runCommands runs the lines of out between the line containing from and the
// line containing to that start with git or graft, as a person would paste
// them.
func (d *deployFixture) runCommands(out, from, to string) {
	d.t.Helper()
	on := false
	for line := range strings.SplitSeq(out, "\n") {
		switch {
		case strings.Contains(line, from):
			on = true
		case strings.Contains(line, to):
			on = false
		}
		cmd := strings.TrimSpace(line)
		if !on || !(strings.HasPrefix(cmd, "git ") || strings.HasPrefix(cmd, "graft ")) {
			continue
		}
		if got, code := d.exec(d.work, "", "sh", "-c", cmd); code != 0 {
			d.t.Fatalf("%s: exit %d\n%s", cmd, code, got)
		}
	}
}

func releaseRepo(t *testing.T) *deployFixture {
	d := newDeployFixture(t, releaseConfig)
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-8"] = "Ready for release"
	d.jira.statuses["PROJ-9"] = "In Progress"
	d.jira.mu.Unlock()
	d.at("2026-10-16 10:00")
	d.mustGraft("deploy", "test")
	out, code := d.graft("", "deploy", "prod")
	if code == 0 || !strings.Contains(out, "outside the release window (mon, thu 15:00-19:00 Etc/GMT-3); the next one opens Mon 19.10 15:00; a hotfix branch (hotfix/*) goes any time") {
		t.Fatalf("prod outside the window: exit %d\n%s", code, out)
	}
	d.at("2026-10-19 15:30")
	d.mustGraft("deploy", "--unapproved", "prod")
	return d
}

// TestReleaseCutoffHoldAndWithheld follows a release: the cutoff fixes the
// candidate on test, its unchecked work is reverted on a release branch,
// prod takes the branch in the window, and the trunk keeps the work, which
// the next release checks again.
func TestReleaseCutoffHoldAndWithheld(t *testing.T) {
	d := releaseRepo(t)
	trunk := d.git("branch", "--show-current")

	d.at("2026-10-20 10:00")
	d.commitFile("checked.txt", "feat: PROJ-8 checked work")
	unchecked := d.commitFile("unchecked.txt", "feat: PROJ-1 unchecked work")
	d.write("docs/notes.md", "n\n")
	d.mustFixtureCommit("docs: notes")
	d.mustGraft("deploy", "test")
	candidate := d.git("rev-parse", "HEAD")

	// After the cutoff test holds the candidate; work goes on in the trunk.
	d.at("2026-10-21 18:30")
	d.commitFile("more.txt", "feat: PROJ-1 more work")
	out, code := d.graft("", "deploy", "test")
	if code == 0 || !strings.Contains(out, "test holds the release candidate "+candidate[:12]+" of the prod 22.10.26 window") || !strings.Contains(out, "feat: PROJ-1 more work") {
		t.Fatalf("test took new work during the hold: exit %d\n%s", code, out)
	}

	out = d.mustGraft("deploy", "cutoff", "prod")
	for _, want := range []string{
		"the cutoff was Wed 21.10 18:00, test holds the candidate",
		"git switch -c release/22.10.26 " + candidate,
		"# PROJ-1 is Testing, not Ready for release or Done",
		"git diff " + unchecked + "^ " + unchecked + " -- . ':(exclude)VERSION' | git apply -R --3way --index",
		"graft commit -m 'revert: PROJ-1 not checked by the 22.10.26 cutoff' -m 'This reverts commit " + unchecked + ".'",
		"git merge -s ours --no-edit release/22.10.26",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cutoff: no %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "PROJ-8 checked work") || strings.Contains(out, "notes: names no item") {
		t.Errorf("cutoff lists commits that pass:\n%s", out)
	}
	d.runCommands(out, "goes out from release/22.10.26", "Then the candidate goes")
	if v := d.read("VERSION"); !strings.HasSuffix(strings.TrimSpace(v), ".1") {
		t.Errorf("the release branch build: VERSION %q", v)
	}
	if _, err := os.Stat(filepath.Join(d.work, "unchecked.txt")); err == nil {
		t.Error("the revert left unchecked.txt")
	}
	out = d.mustGraft("deploy", "test")
	if !strings.Contains(out, "takes only its reverts and hotfixes") {
		t.Errorf("the reverted candidate:\n%s", out)
	}

	d.at("2026-10-22 15:30")
	out = d.mustGraft("deploy", "prod")
	if !strings.Contains(out, "feat: PROJ-1 unchecked work: reverted by") || !strings.Contains(out, "1 commits withheld") {
		t.Errorf("the release from the branch:\n%s", out)
	}
	if got := strings.TrimSpace(d.stateFile("prod-sha.withheld")); got != unchecked {
		t.Errorf("withheld = %q, want %s", got, unchecked)
	}
	if d.status("PROJ-8") != "Done" || d.status("PROJ-1") != "Testing" {
		t.Errorf("PROJ-8 %s, PROJ-1 %s", d.status("PROJ-8"), d.status("PROJ-1"))
	}
	comments, _ := d.jira.snapshot()
	if slices.ContainsFunc(comments["PROJ-1"], func(c string) bool { return strings.Contains(c, "prod") }) {
		t.Errorf("PROJ-1 got a prod note for withheld work: %v", comments["PROJ-1"])
	}

	// Back on the trunk: the release branch is recorded, its reverts are not.
	d.git("switch", "-q", trunk)
	d.git("-c", "core.hooksPath=/dev/null", "merge", "-q", "-s", "ours", "--no-edit", "release/22.10.26")
	d.git("push", "-q", "origin", trunk)
	if _, err := os.Stat(filepath.Join(d.work, "unchecked.txt")); err != nil {
		t.Error("the trunk lost the withheld work")
	}
	d.at("2026-10-23 10:00")
	d.mustGraft("deploy", "test")

	d.at("2026-10-26 15:30")
	out, code = d.graft("", "deploy", "prod")
	if code == 0 || !strings.Contains(out, "feat: PROJ-1 unchecked work: withheld from an earlier release, goes out now") || !strings.Contains(out, "feat: PROJ-1 unchecked work: PROJ-1 is Testing") {
		t.Fatalf("the withheld commit is not checked: exit %d\n%s", code, out)
	}
	d.setStatus("PROJ-1", "Ready for release")
	d.mustGraft("deploy", "prod")
	if got := strings.TrimSpace(d.stateFile("prod-sha.withheld")); got != "" {
		t.Errorf("withheld after it passed = %q", got)
	}
}

func TestWithheldRevertPersistsOnNonBuildBranch(t *testing.T) {
	d := releaseRepo(t)
	d.at("2026-10-20 10:00")
	d.commitFile("checked.txt", "feat: PROJ-8 checked work")
	unchecked := d.commitFile("unchecked.txt", "feat: PROJ-1 unchecked work")
	d.mustGraft("deploy", "test")
	d.at("2026-10-21 18:30")
	d.runCommands(d.mustGraft("deploy", "cutoff", "prod"), "goes out from release/22.10.26", "Then the candidate goes")
	d.mustGraft("deploy", "test")
	d.at("2026-10-22 15:30")
	d.mustGraft("deploy", "prod")
	if got := strings.TrimSpace(d.stateFile("prod-sha.withheld")); got != unchecked {
		t.Fatalf("initial withheld = %q, want %s", got, unchecked)
	}

	// This branch contains the release's revert but is outside build_branches.
	d.git("switch", "-q", "-c", "maintenance/PROJ-8-followup")
	d.write("followup.txt", "feat: PROJ-8 followup\n")
	d.git("-c", "core.hooksPath=/dev/null", "add", "-A")
	d.git("-c", "core.hooksPath=/dev/null", "commit", "-m", "feat: PROJ-8 followup")
	d.git("push", "-q", "-u", "origin", "maintenance/PROJ-8-followup")
	d.setStatus("PROJ-1", "Ready for release")
	d.mustGraft("deploy", "test")
	out := d.mustGraft("deploy", "prod")
	if strings.Contains(out, "withheld from an earlier release, goes out now") {
		t.Errorf("a reverted withheld commit was released on another branch:\n%s", out)
	}
	if got := strings.TrimSpace(d.stateFile("prod-sha.withheld")); got != unchecked {
		t.Errorf("withheld after deploy = %q, want %s\n%s", got, unchecked, out)
	}
	if d.status("PROJ-1") != "Ready for release" {
		t.Errorf("PROJ-1 moved to %s", d.status("PROJ-1"))
	}
}

// TestReleaseHoldOverOncePushedOnTop: once prod runs the candidate, the hold
// is over for every later deploy to test in the window, not only for the
// first one on top of the candidate; whether a deploy during the hold
// recorded the candidate or none did.
func TestReleaseHoldOverOncePushedOnTop(t *testing.T) {
	for _, recorded := range []bool{true, false} {
		t.Run(fmt.Sprintf("recorded=%v", recorded), func(t *testing.T) {
			d := releaseRepo(t)

			d.at("2026-10-20 10:00")
			d.plainCommitFile("work.txt", "feat: PROJ-8 checked work")
			d.mustGraft("deploy", "test")
			candidate := d.git("rev-parse", "HEAD")
			if recorded {
				d.at("2026-10-21 18:30")
				d.mustGraft("deploy", "--redeploy", "test")
			}

			d.at("2026-10-22 15:26")
			d.mustGraft("deploy", "--unapproved", "prod")
			for i, at := range []string{"2026-10-22 15:46", "2026-10-22 16:08"} {
				d.at(at)
				d.plainCommitFile(fmt.Sprintf("next%d.txt", i), fmt.Sprintf("feat: PROJ-1 next work %d", i))
				out, code := d.graft("", "deploy", "test")
				if code != 0 || !strings.Contains(out, "prod already runs "+candidate[:12]+", the candidate of the 22.10.26 window: the hold is over") {
					t.Fatalf("deploy %d to test after prod took the candidate: exit %d\n%s", i+1, code, out)
				}
			}
		})
	}
}

// TestReleaseHotfix: a hotfix branch from what prod runs goes through test
// during the hold and out of the window; the next release takes it only once
// the trunk has merged it.
func TestReleaseHotfix(t *testing.T) {
	d := releaseRepo(t)
	trunk := d.git("branch", "--show-current")
	prod := strings.TrimSpace(d.stateFile("prod-sha"))

	d.at("2026-10-20 10:00")
	d.commitFile("work.txt", "feat: PROJ-1 trunk work")
	d.mustGraft("deploy", "test")

	d.at("2026-10-21 18:30")
	d.git("switch", "-q", "-c", "hotfix/PROJ-9-wrong-base")
	d.commitFile("fix.txt", "fix: PROJ-9 urgent")
	// Branched from the trunk, it would take the trunk's work along.
	if out, code := d.graft("", "deploy", "test"); code == 0 || !strings.Contains(out, "feat: PROJ-1 trunk work: PROJ-1 without the label hotfix") {
		t.Errorf("a hotfix from the trunk: exit %d\n%s", code, out)
	}
	d.git("switch", "-q", "-c", "hotfix/PROJ-9", prod)
	d.commitFile("fix.txt", "fix: PROJ-9 urgent")
	if v := strings.TrimSpace(d.read("VERSION")); v != "1.0.1.1" {
		t.Errorf("hotfix build VERSION %q", v)
	}
	if out, code := d.graft("", "deploy", "test"); code == 0 || !strings.Contains(out, "PROJ-9 without the label hotfix") {
		t.Errorf("an unlabelled hotfix: exit %d\n%s", code, out)
	}
	d.jira.mu.Lock()
	d.jira.labels = map[string][]string{"PROJ-9": {"backend", "hotfix"}}
	d.jira.mu.Unlock()
	if out := d.mustGraft("deploy", "test"); !strings.Contains(out, "a hotfix passes the hold") {
		t.Errorf("hotfix to test:\n%s", out)
	}
	out := d.mustGraft("deploy", "prod")
	if !strings.Contains(out, "the release window, the hold and approved_release do not apply") || !strings.Contains(out, "git merge --no-ff origin/hotfix/PROJ-9") {
		t.Errorf("hotfix to prod:\n%s", out)
	}
	if d.status("PROJ-9") != "Testing" {
		t.Errorf("PROJ-9 is %s", d.status("PROJ-9"))
	}

	// The window: the trunk without the hotfix would take it back.
	d.at("2026-10-22 15:30")
	d.git("switch", "-q", trunk)
	d.mustGraft("deploy", "test")
	out, code := d.graft("", "deploy", "prod")
	if code == 0 || !strings.Contains(out, "git merge --no-ff origin/hotfix/PROJ-9   # a hotfix") {
		t.Fatalf("a release without the hotfix: exit %d\n%s", code, out)
	}
	if _, code := d.exec(d.work, "", "git", "-c", "core.hooksPath=/dev/null", "merge", "-q", "--no-ff", "--no-edit", "origin/hotfix/PROJ-9"); code == 0 {
		t.Fatal("the version files did not conflict")
	}
	d.git("checkout", "--ours", "--", "VERSION")
	d.git("add", "VERSION")
	d.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "--no-edit")
	d.git("push", "-q", "origin", trunk)
	if out := d.mustGraft("deploy", "test"); !strings.Contains(out, "takes only its reverts and hotfixes") {
		t.Errorf("the candidate with the hotfix merged:\n%s", out)
	}
	if out, code := d.graft("", "deploy", "prod"); code == 0 || !strings.Contains(out, "feat: PROJ-1 trunk work: PROJ-1 is Testing") {
		t.Errorf("unchecked trunk work: exit %d\n%s", code, out)
	}
	d.setStatus("PROJ-1", "Ready for release")
	d.mustGraft("deploy", "prod")
}

// versionConfig is releaseConfig with the window versions kept and the test
// target adding the window version to the items it moves to Testing.
func versionConfig(state string, q func(string) string) string {
	cfg := releaseConfig(state, q)
	cfg = strings.Replace(cfg, "release_notes: {transitions: [{from: [In Progress], to: Testing, assign: qa}]}",
		`release_notes: {transitions: [{from: [In Progress], to: Testing, assign: qa}], fix_version: {name: "Product {dd}.{mm}.{yy}"}}`, 1)
	return strings.Replace(cfg, "      approved_release:",
		`      release_version: {enabled: true, name: "Product {dd}.{mm}.{yy}", hotfix: "Product {dd}.{mm}.{yy} hotfix"}
      release_page: {parent: "500", labels: {moved: "Next window"}}
      approved_release:`, 1)
}

func (d *deployFixture) fixVersionsOf(key string) []string {
	d.jira.mu.Lock()
	defer d.jira.mu.Unlock()
	return slices.Clone(d.jira.fixVersions[key])
}

// TestReleaseVersions: the window version takes the items test moves to
// Testing, loses the ones the cutoff reverts to the next window's, and is
// released with its build; a hotfix gets a version of its own.
func TestReleaseVersions(t *testing.T) {
	d := newDeployFixture(t, versionConfig)
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-8"] = "In Progress"
	d.jira.statuses["PROJ-9"] = "In Progress"
	d.jira.types = map[string]string{"PROJ-8": "Bug"}
	d.jira.versions = []map[string]any{
		{"id": "20019", "name": "Product 19.10.26", "released": false, "archived": false, "releaseDate": "2026-10-19"},
		{"id": "20022", "name": "Product.22.10.26", "released": false, "archived": false, "releaseDate": "2026-10-22"},
		{"id": "10009", "name": "Product v1.2.9", "released": false, "archived": false},
	}
	d.jira.mu.Unlock()
	d.at("2026-10-16 10:00")
	d.mustGraft("deploy", "test")

	// Without the permission, and with a name that slips from the template,
	// the release is live but its version is not kept.
	d.at("2026-10-19 15:30")
	out, code := d.graft("", "deploy", "--unapproved", "prod")
	if code == 0 || !strings.Contains(out, "needs the Manage versions permission (MANAGE_VERSIONS); in a team-managed project it comes with the Administrator role") {
		t.Fatalf("no permission: exit %d\n%s", code, out)
	}
	d.jira.mu.Lock()
	d.jira.manageVersions = true
	d.jira.mu.Unlock()
	out, code = d.graft("", "deploy", "--unapproved", "--redeploy", "prod")
	if code == 0 || !strings.Contains(out, `versions "Product.22.10.26" look like the version name template but do not follow it`) {
		t.Fatalf("a slipped name: exit %d\n%s", code, out)
	}
	d.jira.mu.Lock()
	d.jira.versions[1]["name"] = "Product 22.10.26"
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "--unapproved", "--redeploy", "prod")
	if v := d.jira.version("20019"); v["released"] != true || v["releaseDate"] != "2026-10-19" || !strings.Contains(v["description"].(string), "build 1.0.1, commit ") {
		t.Errorf("the Monday version: %v", v)
	}
	if body := d.jira.page("Product 19.10.26: release notes"); !strings.Contains(body, `<a href="`+d.jiraURL()+`/projects/PROJ/versions/20019">Product 19.10.26</a>`) ||
		!strings.Contains(body, "<th>Date</th><td>2026-10-19</td>") || strings.Count(body, "<h2>Build 1.0.1, commit ") != 1 || !strings.Contains(body, "<p>No items.</p>") {
		t.Errorf("the Monday page, published once over three deploys:\n%s", body)
	}

	// Test moves the work to Testing into the Thursday version.
	d.at("2026-10-20 10:00")
	d.commitFile("checked.txt", "feat: PROJ-8 checked work")
	d.commitFile("unchecked.txt", "feat: PROJ-1 unchecked work")
	d.mustGraft("deploy", "test")
	for _, key := range []string{"PROJ-1", "PROJ-8"} {
		if got := d.fixVersionsOf(key); !slices.Equal(got, []string{"20022"}) {
			t.Errorf("%s fix versions = %v", key, got)
		}
	}
	d.setStatus("PROJ-8", "Ready for release")

	// The cutoff reverts PROJ-1: its version becomes the next window's.
	d.at("2026-10-21 18:30")
	d.runCommands(d.mustGraft("deploy", "cutoff", "prod"), "goes out from release/22.10.26", "Then the candidate goes")
	out = d.mustGraft("deploy", "test")
	next := d.jira.versionNamed("Product 26.10.26")
	if next == nil || next["releaseDate"] != "2026-10-26" {
		t.Fatalf("no next window version:\n%s", out)
	}
	if got := d.fixVersionsOf("PROJ-1"); !slices.Equal(got, []string{next["id"].(string)}) || !strings.Contains(out, "PROJ-1 moved to version Product 26.10.26") {
		t.Errorf("PROJ-1 fix versions = %v\n%s", got, out)
	}

	// The release refuses while the version lists work it does not take out.
	d.at("2026-10-22 15:30")
	d.jira.mu.Lock()
	d.jira.fixVersions["PROJ-5"] = []string{"20022"}
	d.jira.mu.Unlock()
	if out, code := d.graft("", "deploy", "prod"); code == 0 || !strings.Contains(out, "version Product 22.10.26 lists PROJ-5, which this release does not take out") {
		t.Errorf("a version with more than the release: exit %d\n%s", code, out)
	}
	d.jira.mu.Lock()
	delete(d.jira.fixVersions, "PROJ-5")
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "prod")
	if v := d.jira.version("20022"); v["released"] != true || v["releaseDate"] != "2026-10-22" || !strings.Contains(v["description"].(string), "build 1.0.3.1") {
		t.Errorf("the Thursday version: %v", v)
	}
	body := d.jira.page("Product 22.10.26: release notes")
	changes, moved, _ := strings.Cut(body, "<h3>Next window</h3>")
	if !strings.Contains(changes, "<h2>Build 1.0.3.1, commit ") || !strings.Contains(changes, "<h3>Fixes</h3>") ||
		!strings.Contains(changes, `<td><a href="`+d.jiraURL()+`/browse/PROJ-8">PROJ-8</a></td><td>Summary of PROJ-8</td><td>Bug</td>`) ||
		!strings.Contains(moved, ">PROJ-1</a>") || strings.Contains(changes, ">PROJ-1</a>") || strings.Contains(body, "<h3>Changes</h3>") {
		t.Errorf("the Thursday page:\n%s", body)
	}

	// A hotfix on Friday: its own version, released with it.
	d.at("2026-10-23 11:00")
	d.git("switch", "-q", "-c", "hotfix/PROJ-9", strings.TrimSpace(d.stateFile("prod-sha")))
	d.commitFile("fix.txt", "fix: PROJ-9 urgent")
	d.jira.mu.Lock()
	d.jira.labels = map[string][]string{"PROJ-9": {"hotfix"}}
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "test")
	d.mustGraft("deploy", "prod")
	hv := d.jira.versionNamed("Product 23.10.26 hotfix")
	if hv == nil || hv["released"] != true || !strings.Contains(hv["description"].(string), "build 1.0.3.2") {
		t.Fatalf("hotfix version: %v", hv)
	}
	if got := d.fixVersionsOf("PROJ-9"); !slices.Contains(got, hv["id"].(string)) {
		t.Errorf("PROJ-9 fix versions = %v", got)
	}
	if body := d.jira.page("Product 23.10.26 hotfix: release notes"); !strings.Contains(body, "<h3>Changes</h3>") || !strings.Contains(body, ">PROJ-9</a>") || !strings.Contains(body, "<th>Date</th><td>2026-10-23</td>") {
		t.Errorf("the hotfix page:\n%s", body)
	}
}

// TestReleasePageApprovedVersions: without windows the approved versions the
// release took its items from name the page; without them nothing does.
func TestReleasePageApprovedVersions(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return strings.Replace(deployConfig(state, q), `approved_release: {prefix: "Product v", ignore_paths: ["docs/**", "*.md"]}`,
			`approved_release: {prefix: "Product v", ignore_paths: ["docs/**", "*.md"]}
      release_page: {parent: "500", title: "{release}: notes"}`, 1)
	})
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-1": {"10010"}})
	d.jira.setApprovers(map[string][]string{"10010": {"APPROVED", "APPROVED"}})
	if out, code := d.graft("", "deploy", "--unapproved", "live"); code != 1 || !strings.Contains(out, "graft: release page NOT PUBLISHED: no approved version names the release") {
		t.Errorf("an unapproved release: exit %d\n%s", code, out)
	}
	d.write("a.txt", "a\n")
	d.mustFixtureCommit("feat: PROJ-1 approved thing")
	out := d.mustGraft("deploy", "live")
	body := d.jira.page("Product v1.2.10: notes")
	if !strings.Contains(out, `release page "Product v1.2.10: notes" published: `+d.jiraURL()+"/wiki/spaces/S/pages/") ||
		!strings.Contains(body, "/projects/PROJ/versions/10010\">Product v1.2.10</a>") || !strings.Contains(body, "<th>Target</th><td>live</td>") || !strings.Contains(body, ">PROJ-1</a>") {
		t.Errorf("the page:\n%s\n%s", body, out)
	}
}

// TestReleaseNameWithoutVersions: release_version with enabled false names
// the release by its window for the release page and the notes, and leaves
// the Jira versions alone: no permission is needed.
func TestReleaseNameWithoutVersions(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := strings.Replace(releaseConfig(state, q), "      approved_release:",
			`      release_version: {enabled: false, name: "Product {dd}.{mm}.{yy}", hotfix: "Product {dd}.{mm}.{yy} hotfix"}
      release_page: {parent: "500"}
      approved_release:`, 1)
		return strings.Replace(cfg, "release_notes: {transitions: [{from: [Ready for release], to: Done}",
			"release_notes: {comment: 'Released {release} to {target}, build {version}', transitions: [{from: [Ready for release], to: Done}", 1)
	})
	d.at("2026-10-16 10:00")
	d.mustGraft("deploy", "test")
	d.at("2026-10-19 15:30")
	d.mustGraft("deploy", "--unapproved", "prod")
	d.at("2026-10-20 10:00")
	d.commitFile("checked.txt", "feat: PROJ-8 checked work")
	d.setStatus("PROJ-8", "In Progress")
	d.mustGraft("deploy", "test")
	d.setStatus("PROJ-8", "Ready for release")
	d.at("2026-10-22 15:30")
	out := d.mustGraft("deploy", "prod")
	if body := d.jira.page("Product 22.10.26: release notes"); !strings.Contains(body, ">PROJ-8</a>") || !strings.Contains(body, "<th>Release</th><td>Product 22.10.26</td>") {
		t.Errorf("the page:\n%s\n%s", body, out)
	}
	d.jira.mu.Lock()
	comments, versions := d.jira.comments["PROJ-8"], d.jira.versions
	d.jira.mu.Unlock()
	if !slices.Contains(comments, "Released Product 22.10.26 to prod, build 1.0.2") || len(versions) != 0 || strings.Contains(out, "MANAGE_VERSIONS") {
		t.Errorf("comments %q, versions %v:\n%s", comments, versions, out)
	}
}

// TestReleaseAttribute: a commit that names no item, attributed to an
// unchecked item, is reverted by the cutoff under that item, withheld by the
// release and listed as the item's on the release page; the next release
// checks it again and takes it out as the item's.
func TestReleaseAttribute(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return strings.Replace(releaseConfig(state, q), "      approved_release:",
			`      release_version: {enabled: false, name: "Product {dd}.{mm}.{yy}"}
      release_page: {parent: "500"}
      approved_release:`, 1)
	})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-8"] = "Ready for release"
	d.jira.mu.Unlock()
	trunk := d.git("branch", "--show-current")
	d.at("2026-10-16 10:00")
	d.mustGraft("deploy", "test")
	d.at("2026-10-19 15:30")
	d.mustGraft("deploy", "--unapproved", "prod")

	d.at("2026-10-20 10:00")
	d.commitFile("checked.txt", "feat: PROJ-8 checked work")
	keyless := d.commitFile("keyless.txt", "feat: forgot the key")
	d.write(".graft.yaml", strings.Replace(d.read(".graft.yaml"), `ignore_paths: ["docs/**"]}`,
		`ignore_paths: ["docs/**"], attribute: [{commit: `+keyless+`, item: PROJ-1, reason: key left out}]}`, 1))
	d.mustFixtureCommit("chore: PROJ-8 attribute the keyless commit")
	d.mustGraft("deploy", "test")

	d.at("2026-10-21 18:30")
	out := d.mustGraft("deploy", "cutoff", "prod")
	for _, want := range []string{
		"# PROJ-1 is In Progress, not Ready for release or Done",
		"git diff " + keyless + "^ " + keyless + " -- . ':(exclude)VERSION' | git apply -R --3way --index",
		"graft commit -m 'revert: PROJ-1 not checked by the 22.10.26 cutoff' -m 'This reverts commit " + keyless + ".'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cutoff: no %q\n%s", want, out)
		}
	}
	d.runCommands(out, "goes out from release/22.10.26", "Then the candidate goes")
	d.mustGraft("deploy", "test")

	d.at("2026-10-22 15:30")
	out = d.mustGraft("deploy", "prod")
	if got := strings.TrimSpace(d.stateFile("prod-sha.withheld")); got != keyless || !strings.Contains(out, "feat: forgot the key: reverted by") {
		t.Errorf("withheld = %q, want %s\n%s", got, keyless, out)
	}
	changes, moved, _ := strings.Cut(d.jira.page("Product 22.10.26: release notes"), "<h3>Moved to the next release</h3>")
	if !strings.Contains(changes, ">PROJ-8</a>") || strings.Contains(changes, ">PROJ-1</a>") || !strings.Contains(moved, ">PROJ-1</a>") {
		t.Errorf("the page: changes\n%s\nmoved\n%s", changes, moved)
	}
	comments, _ := d.jira.snapshot()
	if slices.ContainsFunc(comments["PROJ-1"], func(c string) bool { return strings.Contains(c, "prod") }) {
		t.Errorf("PROJ-1 got a prod note for withheld work: %v", comments["PROJ-1"])
	}

	d.git("switch", "-q", trunk)
	d.git("-c", "core.hooksPath=/dev/null", "merge", "-q", "-s", "ours", "--no-edit", "release/22.10.26")
	d.git("push", "-q", "origin", trunk)
	d.at("2026-10-23 10:00")
	d.mustGraft("deploy", "test")
	d.setStatus("PROJ-1", "Ready for release")
	d.at("2026-10-26 15:30")
	out = d.mustGraft("deploy", "prod")
	if !strings.Contains(out, "feat: forgot the key: withheld from an earlier release, goes out now") || !strings.Contains(out, "feat: forgot the key: attributed to PROJ-1: key left out") {
		t.Errorf("the withheld commit:\n%s", out)
	}
	// It went out now, outside the deployed range: its item gets the notes.
	comments, _ = d.jira.snapshot()
	if got := strings.TrimSpace(d.stateFile("prod-sha.withheld")); got != "" || d.status("PROJ-1") != "Done" ||
		!slices.ContainsFunc(comments["PROJ-1"], func(c string) bool { return strings.Contains(c, "Deployed to prod") }) {
		t.Errorf("withheld = %q, PROJ-1 %s, comments %v", got, d.status("PROJ-1"), comments["PROJ-1"])
	}
	if body := d.jira.page("Product 26.10.26: release notes"); !strings.Contains(body, ">PROJ-1</a>") {
		t.Errorf("the next page:\n%s", body)
	}
}

// TestReleaseMergeIntoTrunk: with merge_into_trunk, the release from a
// release branch is recorded on the trunk by the deploy itself, keeping the
// trunk's tree, the withheld work with it; a fix made on the release branch
// that the trunk lacks stops the merge, which would drop it.
func TestReleaseMergeIntoTrunk(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return strings.Replace(releaseConfig(state, q), "      deployed_sha: {path: "+q("prod-sha")+"}\n",
			"      deployed_sha: {path: "+q("prod-sha")+"}\n      merge_into_trunk: true\n", 1)
	})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-8"] = "Ready for release"
	d.jira.mu.Unlock()
	trunk := d.git("branch", "--show-current")
	d.git("remote", "set-head", "origin", trunk)
	d.at("2026-10-16 10:00")
	d.mustGraft("deploy", "test")
	d.at("2026-10-19 15:30")
	d.mustGraft("deploy", "--unapproved", "prod")

	d.at("2026-10-20 10:00")
	d.commitFile("checked.txt", "feat: PROJ-8 checked work")
	d.commitFile("unchecked.txt", "feat: PROJ-1 unchecked work")
	d.mustGraft("deploy", "test")
	d.at("2026-10-21 18:30")
	d.runCommands(d.mustGraft("deploy", "cutoff", "prod"), "goes out from release/22.10.26", "Then the candidate goes")
	d.mustGraft("deploy", "test")
	release := d.git("rev-parse", "HEAD")
	before := d.git("rev-parse", "origin/"+trunk+"^{tree}")

	d.at("2026-10-22 15:30")
	out := d.mustGraft("deploy", "prod")
	if !strings.Contains(out, "merged release/22.10.26 into "+trunk) {
		t.Errorf("no merge into the trunk:\n%s", out)
	}
	d.git("fetch", "-q", "origin")
	if got := d.git("rev-parse", "origin/"+trunk+"^{tree}"); got != before {
		t.Errorf("the merge changed the trunk's tree: %s, was %s", got, before)
	}
	if got := d.git("rev-parse", "origin/"+trunk+"^2"); got != release {
		t.Errorf("the trunk's merge has %s as its second parent, want %s", got, release)
	}
	if got := d.git("rev-parse", trunk); got != d.git("rev-parse", "origin/"+trunk) {
		t.Errorf("the local trunk %s did not follow", got)
	}

}

// TestDeployMergeIntoTrunkKeepsFixes: a fix made on a release branch that
// the trunk lacks stops the merge into the trunk, which would drop it; once
// the trunk has it by content, a commit of its own, the next deploy merges,
// and so does a port of trunk work that names the item the trunk worked on.
func TestDeployMergeIntoTrunkKeepsFixes(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return `schema: 1
version: {mode: file, build_branches: ['release/*']}
ticket: {pattern: 'PROJ-[0-9]+'}
envs:
  local: {}
deploy:
  remote: origin
  targets:
    prod:
      env: local
      run: [sh, deploy.sh]
      deployed_sha: {path: ` + q("prod-sha") + `}
      merge_into_trunk: true
`
	})
	trunk := d.git("branch", "--show-current")
	d.git("remote", "set-head", "origin", trunk)
	d.git("switch", "-q", "-c", "release/1")
	d.git("push", "-q", "-u", "origin", "release/1")
	fix := d.commitFile("fix.txt", "fix: found in the release")
	out, code := d.graft("", "deploy", "prod")
	if code == 0 || !strings.Contains(out, "deployed "+fix[:12]+" to prod") ||
		!strings.Contains(out, "release/1 is not merged into "+trunk+": "+trunk+" lacks these of its commits, by content and by the items they name") ||
		!strings.Contains(out, fix[:12]+" fix: found in the release") {
		t.Errorf("a fix the trunk lacks: exit %d\n%s", code, out)
	}
	if got := strings.TrimSpace(d.stateFile("prod-sha")); got != fix {
		t.Errorf("the deploy is not recorded: prod-sha %q", got)
	}
	d.git("fetch", "-q", "origin")
	if _, code := d.exec(d.work, "", "git", "merge-base", "--is-ancestor", fix, "origin/"+trunk); code == 0 {
		t.Fatal("the release branch with the fix was merged into the trunk")
	}

	// The trunk takes the fix as a commit of its own; the branch builds again.
	d.git("switch", "-q", trunk)
	d.commitFile("fix.txt", "fix: found in the release")
	d.commitFile("b.txt", "feat: b")
	d.git("switch", "-q", "release/1")
	head := d.commitFile("b.txt", "feat: b")
	if out := d.mustGraft("deploy", "prod"); !strings.Contains(out, "merged release/1 into "+trunk) {
		t.Errorf("no merge once the trunk has the fix:\n%s", out)
	}
	d.git("fetch", "-q", "origin")
	if got := d.git("rev-parse", "origin/"+trunk+"^2"); got != head {
		t.Errorf("the trunk's merge has %s as its second parent, want %s", got, head)
	}

	// A port of trunk work the trunk has since reworked: by content it
	// conflicts, by its item it is in the trunk, and the merge says so.
	d.git("switch", "-q", trunk)
	d.git("pull", "-q", "--ff-only", "origin", trunk)
	d.commitFile("c.txt", "feat: PROJ-7 first take")
	d.git("switch", "-q", "-c", "release/2")
	d.git("push", "-q", "-u", "origin", "release/2")
	d.git("switch", "-q", trunk)
	d.commitFile("c.txt", "feat: PROJ-7 reworked")
	d.git("switch", "-q", "release/2")
	port := d.commitFile("c.txt", "fix: PROJ-7 ported from the trunk")
	out = d.mustGraft("deploy", "prod")
	if !strings.Contains(out, "merged release/2 into "+trunk) ||
		!strings.Contains(out, "1 commits of release/2 count as ports of "+trunk+"'s work by the items they name, not by content") ||
		!strings.Contains(out, port[:12]+" fix: PROJ-7 ported from the trunk") {
		t.Errorf("a port by its item:\n%s", out)
	}
}
