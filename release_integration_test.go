package main

import (
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
	d.mustGraft("commit", "-m", msg)
	return d.git("rev-parse", "HEAD")
}

// runCommands runs the lines of out between the line containing from and the
// line containing to that start with git or graft, as a person would paste
// them.
func (d *deployFixture) runCommands(out, from, to string) {
	d.t.Helper()
	on := false
	for _, line := range strings.Split(out, "\n") {
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
	d.mustGraft("commit", "-m", "docs: notes")
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
