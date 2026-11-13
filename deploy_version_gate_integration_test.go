package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func versionGateConfig(state string, q func(string) string) string {
	cfg := deployConfig(state, q)
	cfg = strings.Replace(cfg, "    prod:\n      env: local\n      run: [sh, deploy.sh]\n      requires: test\n      confirm: sudo\n", "", 1)
	return strings.Replace(cfg, "    gate:\n", "    prod:\n", 1)
}

func TestDeployReleaseVersion(t *testing.T) {
	cfg := func(state string, q func(string) string) string {
		return strings.Replace(versionGateConfig(state, q), "approved_release: {statuses: [Ready for release, Done]}", `approved_release: {statuses: [Ready for release, Done], label_statuses: {internal: [Ready for release]}, ignore_paths: ["docs/**"], version: {match: '^Product[ .]?\d\d\.\d\d\.\d\d$', manual: ['QA smoke on TEST passed'], refs: {version-subtasks: '2.2'}}}`, 1)
	}
	d := newDeployFixture(t, cfg)
	writeFile(t, filepath.Join(d.state, "gate-sha"), d.git("rev-parse", "HEAD")+"\n")
	d.jira.setVersions([]map[string]any{{"id": "1", "name": "Product 05.11.26"}, {"id": "2", "name": "Product.12.11.26"}, {"id": "3", "name": "Other"}}, map[string][]string{"PROJ-10": {"1"}})
	d.jira.mu.Lock()
	d.jira.parents = map[string]string{"PROJ-11": "PROJ-10", "PROJ-12": "PROJ-10"}
	d.jira.statuses["PROJ-10"] = "Ready for release"
	d.jira.statuses["PROJ-11"] = "Done"
	d.jira.statuses["PROJ-12"] = "Done"
	d.jira.mu.Unlock()
	d.jira.setApprovers(map[string][]string{"1": {"APPROVED", "APPROVED"}, "2": {"APPROVED"}})
	d.commitFile("code.txt", "feat: PROJ-11 work")
	d.commitFile("parent.txt", "feat: PROJ-10 work")
	d.plainCommitFile("docs/readme.txt", "docs: update")
	check := func(wantCode bool, wants ...string) string {
		t.Helper()
		out, code := d.graft("", "deploy", "check", "prod")
		if (code == 0) != wantCode {
			t.Errorf("exit %d, want success %v:\n%s", code, wantCode, out)
		}
		for _, want := range append(wants, "QA smoke on TEST passed") {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q:\n%s", want, out)
			}
		}
		return out
	}
	// A passed approval names every approver; an unread name gives the account.
	t.Log("pass output:\n" + check(true, "passes its checks now", "approved release: version Product 05.11.26 (1) approved by approver0, Name of approver1"))
	d.setStatus("PROJ-10", "Testing")
	d.setStatus("PROJ-12", "To Do")
	refusal := check(false, "version-top", "PROJ-10 is Testing, fix versions Product 05.11.26: move it to Ready for release or Done", "version-umbrella", "PROJ-12", "version-status")
	if strings.Contains(refusal, "add it to") {
		t.Errorf("a task already in the version is told to be added to it:\n%s", refusal)
	}
	t.Log("refusal output:\n" + refusal)
	d.setStatus("PROJ-10", "Done")
	check(false, "version-umbrella", "PROJ-12")
	d.setStatus("PROJ-10", "Ready for release")
	d.setStatus("PROJ-12", "Ready for release")
	d.jira.mu.Lock()
	if d.jira.labels == nil {
		d.jira.labels = map[string][]string{}
	}
	d.jira.labels["PROJ-12"] = []string{"internal"}
	d.jira.mu.Unlock()
	check(false, "version-umbrella", "PROJ-10 is Ready for release with unfinished subtasks PROJ-12 (Ready for release); move them to Done")
	d.jira.mu.Lock()
	d.jira.fixVersions["PROJ-11"] = []string{"1"}
	d.jira.statuses["PROJ-20"] = "Testing"
	d.jira.fixVersions["PROJ-20"] = []string{"1"}
	d.jira.versionPageSize = 1
	d.jira.mu.Unlock()
	check(false, "version-subtasks (rule 2.2)", "PROJ-11", "version-status", "PROJ-20", "without code in the range")
	d.jira.mu.Lock()
	delete(d.jira.fixVersions, "PROJ-11")
	delete(d.jira.fixVersions, "PROJ-20")
	d.jira.mu.Unlock()
	d.jira.setApprovers(map[string][]string{"1": {"APPROVED", "PENDING"}})
	check(false, "version-approval", "Name of approver1 (PENDING)")
	// An approver whose name cannot be read is named by the account.
	d.jira.setApprovers(map[string][]string{"1": {"PENDING", "APPROVED"}})
	if out := check(false, "version-approval", "approver0 (PENDING)"); strings.Contains(out, "Name of approver0") {
		t.Errorf("an unread name is shown:\n%s", out)
	}
	d.jira.setApprovers(map[string][]string{})
	check(false, "version-approval", "no approvers")
	d.jira.setApprovers(map[string][]string{"1": {"APPROVED", "APPROVED"}, "2": {"APPROVED"}})
	d.jira.mu.Lock()
	d.jira.fixVersions["PROJ-10"] = []string{"1", "3"}
	d.jira.mu.Unlock()
	d.setStatus("PROJ-12", "Done")
	check(true)
	d.jira.mu.Lock()
	d.jira.fixVersions["PROJ-10"] = []string{"1", "2"}
	d.jira.mu.Unlock()
	if out := check(false, "version-select", "Product 05.11.26", "Product.12.11.26"); strings.Contains(out, "version-top") {
		t.Errorf("a ready top task is refused when no version is chosen:\n%s", out)
	}
	// A status refusal is listed though no version could be chosen.
	d.setStatus("PROJ-10", "Testing")
	check(false, "version-select", "version-top", "PROJ-10 is Testing")
	d.setStatus("PROJ-10", "Ready for release")
	d.jira.mu.Lock()
	d.jira.fixVersions["PROJ-10"] = nil
	d.jira.statuses["PROJ-12"] = "To Do"
	d.jira.mu.Unlock()
	check(false, "version-select", "version-umbrella", "PROJ-12")
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-10"] = "In Progress"
	d.jira.statuses["PROJ-30"] = "Ready for release"
	d.jira.fixVersions["PROJ-30"] = []string{"2"}
	d.jira.mu.Unlock()
	d.commitFile("other.txt", "feat: PROJ-30 work")
	check(false, "version-top", "PROJ-10 is In Progress, fix versions none: add it to Product.12.11.26 and move it to Ready for release or Done", "version-umbrella")
	out, code := d.graft("", "deploy", "--unapproved", "prod")
	if code != 0 || !strings.Contains(out, "approved release not checked (--unapproved)") {
		t.Errorf("bypass: exit %d\n%s", code, out)
	}
	d.commitFile("more.txt", "feat: PROJ-30 more work")
	d.jira.setFailing()
	check(false, "down for maintenance")
}

func TestDeployReleaseVersionSkippedParentAndIgnoredCommits(t *testing.T) {
	cfg := func(state string, q func(string) string) string {
		return strings.Replace(versionGateConfig(state, q), "approved_release: {statuses: [Ready for release, Done]}", `approved_release: {statuses: [Ready for release, Done], ignore_paths: ["docs/**"], version: {}}`, 1)
	}
	d := newDeployFixture(t, cfg)
	writeFile(t, filepath.Join(d.state, "gate-sha"), d.git("rev-parse", "HEAD")+"\n")
	d.plainCommitFile("docs/readme.txt", "docs: update")
	out, code := d.graft("", "deploy", "check", "prod")
	if code != 0 || !strings.Contains(out, "no active commits") {
		t.Errorf("ignored-only: exit %d\n%s", code, out)
	}
	d.jira.setVersions([]map[string]any{{"id": "1", "name": "Product 05.11.26"}}, map[string][]string{"PROJ-40": {"1"}})
	d.jira.setApprovers(map[string][]string{"1": {"APPROVED"}})
	d.jira.mu.Lock()
	d.jira.parents = map[string]string{"PROJ-41": "PROJ-40"}
	d.jira.statuses["PROJ-40"] = "Ready for release"
	d.jira.statuses["PROJ-41"] = "To Do"
	d.jira.mu.Unlock()
	d.write("docs/more.txt", "docs\n")
	d.write("code.txt", "code\n")
	d.mustFixtureCommit("feat: PROJ-41 mixed")
	out, code = d.graft("", "deploy", "check", "prod")
	if code != 0 || !strings.Contains(out, "passes its checks now") {
		t.Errorf("skipped umbrella: exit %d\n%s", code, out)
	}
	d.setStatus("PROJ-40", "Testing")
	out, code = d.graft("", "deploy", "check", "prod")
	if code == 0 || !strings.Contains(out, "version-top") || !strings.Contains(out, "PROJ-40") {
		t.Errorf("skip must not exempt status: exit %d\n%s", code, out)
	}
}

func TestDeployReleaseVersionHotfix(t *testing.T) {
	cfg := func(state string, q func(string) string) string {
		return strings.Replace(releaseConfig(state, q), "approved_release: {statuses: [Ready for release, Done], ignore_paths: [\"docs/**\"]}", "approved_release: {statuses: [Ready for release, Done], ignore_paths: [\"docs/**\"], version: {manual: ['manual release check']}}", 1)
	}
	d := newDeployFixture(t, cfg)
	d.at("2026-10-16 10:00")
	d.mustGraft("deploy", "test")
	d.at("2026-10-19 15:30")
	d.mustGraft("deploy", "--unapproved", "prod")
	d.git("switch", "-q", "-c", "hotfix/PROJ-9")
	d.jira.mu.Lock()
	d.jira.labels = map[string][]string{"PROJ-9": {"hotfix"}}
	d.jira.statuses["PROJ-9"] = "In Progress"
	d.jira.mu.Unlock()
	d.at("2026-10-20 10:00")
	d.commitFile("fix.txt", "fix: PROJ-9 urgent")
	d.mustGraft("deploy", "test")
	out, code := d.graft("", "deploy", "check", "prod")
	if code != 0 || !strings.Contains(out, "approved_release do not apply") || strings.Contains(out, "manual release check") || strings.Contains(out, "version-select") {
		t.Errorf("hotfix: exit %d\n%s", code, out)
	}
}
