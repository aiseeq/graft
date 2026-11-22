package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func versionGateConfig(state string, q func(string) string) string {
	cfg := deployConfig(state, q)
	cfg = strings.Replace(cfg, "    prod:\n      env: local\n      run: [sh, deploy.sh]\n      requires: test\n      confirm: sudo\n", "", 1)
	return strings.Replace(cfg, "    gate:\n", "    prod:\n", 1)
}

func TestDeployVersionGateVersionOnlyBumpAfterMerge(t *testing.T) {
	cfg := func(state string, q func(string) string) string {
		out := strings.Replace(deployConfig(state, q), "version: {mode: file}", `version: {mode: file, file: VERSION, sync: [{path: package.json, format: json, key: [version]}]}`, 1)
		return strings.Replace(out, "approved_release: {statuses: [Ready for release, Done]}", `approved_release: {statuses: [Ready for release, Done], ignore_paths: ["docs/**"]}`, 1)
	}
	d := newDeployFixture(t, cfg)
	d.write("package.json", `{"version":"1.0.1","dependencies":{}}`+"\n")
	d.mustPlainCommit("chore: add synced package version")
	writeFile(t, filepath.Join(d.state, "gate-sha"), d.git("rev-parse", "HEAD")+"\n")

	d.write("VERSION", "1.0.2\n")
	d.write("package.json", `{"version":"1.0.2","dependencies":{}}`+"\n")
	d.mustPlainCommit("chore: bump version after merge")
	d.git("push", "-q", "origin", "HEAD")
	out, code := d.graft("", "deploy", "check", "gate")
	if code != 0 || !strings.Contains(out, "changes only the version files, passes") {
		t.Fatalf("version-only deploy check: exit %d\n%s", code, out)
	}

	d.write("VERSION", "1.0.3\n")
	d.write("package.json", `{"version":"1.0.3","dependencies":{"demo":"1.0.0"}}`+"\n")
	d.mustPlainCommit("chore: bump version and package dependency")
	d.git("push", "-q", "origin", "HEAD")
	out, code = d.graft("", "deploy", "check", "gate")
	if code == 0 || !strings.Contains(out, "names no item") {
		t.Fatalf("mixed version and package data deploy check: exit %d, want names no item\n%s", code, out)
	}
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
	d.jira.mu.Lock()
	if d.jira.fixChanges == nil {
		d.jira.fixChanges = map[string][]fakeFixChange{}
	}
	d.jira.fixChanges["PROJ-10"] = []fakeFixChange{{at: time.Date(2026, 11, 16, 15, 55, 0, 0, time.UTC), id: "2", added: false, author: "Jane Doe"}}
	d.jira.mu.Unlock()
	out := check(false, "version-top", "PROJ-10 is In Progress, fix versions none: not in Product.12.11.26", "taken out of it by Jane Doe", "version-umbrella")
	if strings.Contains(out, "add it to") {
		t.Errorf("refusal suggests adding the item back:\n%s", out)
	}
	d.jira.mu.Lock()
	d.jira.changelogDown = true
	d.jira.mu.Unlock()
	check(false, "version-top", "Jira history not read")
	out, code := d.graft("", "deploy", "--unapproved", "prod")
	if code != 0 || !strings.Contains(out, "collecting approved release (--unapproved)") {
		t.Errorf("bypass: exit %d\n%s", code, out)
	}
	d.commitFile("more.txt", "feat: PROJ-30 more work")
	d.jira.setFailing()
	check(false, "down for maintenance")
}

func TestDeployUnapprovedVersionFailuresOnPage(t *testing.T) {
	for _, tc := range []struct {
		name, parentStatus string
		approvers          []string
		openSubtask        bool
		reason             string
	}{
		{"approval", "Ready for release", []string{"PENDING"}, false, "version-approval"},
		{"top", "Testing", []string{"APPROVED"}, false, "version-top"},
		{"umbrella", "Ready for release", []string{"APPROVED"}, true, "version-umbrella"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDeployFixture(t, func(state string, q func(string) string) string {
				return strings.Replace(releaseConfig(state, q), `approved_release: {statuses: [Ready for release, Done], ignore_paths: ["docs/**"]}`,
					`release_version: {enabled: false, name: "Product {dd}.{mm}.{yy}"}
      release_page: {parent: "500"}
      approved_release: {statuses: [Ready for release, Done], ignore_paths: ["docs/**"], version: {match: '^Product'}}`, 1)
			})
			writeFile(t, filepath.Join(d.state, "prod-sha"), d.git("rev-parse", "HEAD")+"\n")
			d.jira.setVersions([]map[string]any{{"id": "100", "name": "Product 19.10.26"}}, map[string][]string{"PROJ-10": {"100"}, "PROJ-20": {"100"}})
			d.jira.setApprovers(map[string][]string{"100": tc.approvers})
			d.jira.mu.Lock()
			d.jira.parents = map[string]string{"PROJ-11": "PROJ-10"}
			d.jira.statuses["PROJ-11"] = "Done"
			d.jira.statuses["PROJ-20"] = "Ready for release"
			if tc.openSubtask {
				d.jira.parents["PROJ-12"] = "PROJ-10"
				d.jira.statuses["PROJ-12"] = "In Progress"
			}
			d.jira.mu.Unlock()
			d.commitFile("subtask.txt", "feat: PROJ-11 work")
			d.commitFile("independent.txt", "feat: PROJ-20 work")
			d.at("2026-10-16 10:00")
			d.mustGraft("deploy", "test")
			d.setStatus("PROJ-10", tc.parentStatus)
			d.at("2026-10-19 15:30")
			out := d.mustGraft("deploy", "--unapproved", "prod")
			body := d.jira.page("Product 19.10.26: release notes")
			ordinary, exceptions, ok := strings.Cut(body, "<h3>Shipped by exception (--unapproved)</h3>")
			if !ok || strings.Contains(ordinary, ">PROJ-11</a>") || !strings.Contains(exceptions, "PROJ-11") || !strings.Contains(exceptions, tc.reason) {
				t.Errorf("version failure should put the shipped task and reason in exceptions:\n%s\n%s", body, out)
			}
			if tc.name == "approval" {
				if strings.Contains(ordinary, ">PROJ-20</a>") || !strings.Contains(exceptions, "PROJ-20") {
					t.Errorf("version approval affects both tasks:\n%s", body)
				}
			} else if !strings.Contains(ordinary, ">PROJ-20</a>") || strings.Contains(exceptions, "PROJ-20") {
				t.Errorf("unrelated task should remain in changes:\n%s", body)
			}
			comments, _ := d.jira.snapshot()
			if len(comments["PROJ-11"]) == 0 || d.status("PROJ-20") != "Done" {
				t.Errorf("release notes or transitions changed: comments %v, PROJ-20 %s", comments, d.status("PROJ-20"))
			}
		})
	}
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

func TestDeployReleaseVersionAllowsDormantTestingParentWithOpenSubtask(t *testing.T) {
	cfg := func(state string, q func(string) string) string {
		return strings.Replace(versionGateConfig(state, q), "approved_release: {statuses: [Ready for release, Done]}", `approved_release: {statuses: [Ready for release, Done], dormant: [{key: PROJ-123, reason: disabled parent}], version: {match: '^Product'}}`, 1)
	}
	d := newDeployFixture(t, cfg)
	writeFile(t, filepath.Join(d.state, "gate-sha"), d.git("rev-parse", "HEAD")+"\n")
	d.jira.setVersions([]map[string]any{{"id": "10030", "name": "Product 05.11.26"}}, map[string][]string{"PROJ-123": {"10030"}})
	d.jira.setApprovers(map[string][]string{"10030": {"APPROVED", "APPROVED"}})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-123"] = "Testing"
	d.jira.statuses["PROJ-124"] = "In Progress"
	d.jira.parents = map[string]string{"PROJ-124": "PROJ-123"}
	d.jira.mu.Unlock()
	d.commitFile("dormant-parent.txt", "feat: PROJ-123 disabled parent")
	out, code := d.graft("", "deploy", "check", "prod")
	if code != 0 {
		t.Fatalf("dormant parent with open subtask refused: code=%d\n%s", code, out)
	}
	for _, want := range []string{"ships dormant: PROJ-123 — disabled parent", "passes its checks now"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestDeployReleaseVersionOutsideDormantEntryIsLinkedToLiveVersion(t *testing.T) {
	cfg := func(state string, q func(string) string) string {
		base := versionGateConfig(state, q)
		return strings.Replace(base, "approved_release: {statuses: [Ready for release, Done]}", `approved_release: {statuses: [Ready for release, Done], version: {match: '^Product'}, dormant: [{key: PROJ-60, reason: disabled checkout, disabled_by: checkout.v2 is off, outside_version: 'Product 05.11.26'}]}`, 1)
	}
	d := newDeployFixture(t, cfg)
	writeFile(t, filepath.Join(d.state, "gate-sha"), d.git("rev-parse", "HEAD")+"\n")
	d.jira.setVersions([]map[string]any{{"id": "1", "name": "Product 05.11.26"}}, map[string][]string{"PROJ-10": {"1"}})
	d.jira.setApprovers(map[string][]string{"1": {"APPROVED", "APPROVED"}})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-10"] = "Ready for release"
	d.jira.statuses["PROJ-60"] = "Testing"
	d.jira.mu.Unlock()
	d.commitFile("live.txt", "feat: PROJ-10 release work")
	d.commitFile("disabled.txt", "feat: PROJ-60 disabled checkout")
	out, code := d.graft("", "deploy", "check", "prod")
	if code != 0 || !strings.Contains(out, "ships dormant outside Product 05.11.26: PROJ-60 — disabled checkout; disabled by checkout.v2 is off") {
		t.Fatalf("linked outside dormant item refused or not summarized: exit=%d\n%s", code, out)
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

func TestDeployCheckParentVersionFiles(t *testing.T) {
	cfg := func(state string, q func(string) string) string {
		out := strings.Replace(deployConfig(state, q), "version: {mode: file}", `version: {mode: file, file: VERSION, sync: [{path: frontend/package.json, format: json, key: [version]}, {path: frontend/package-lock.json, format: json, key: [version]}, {path: frontend/package-lock.json, format: json, key: [packages, '', version]}]}`, 1)
		return strings.Replace(out, "approved_release: {statuses: [Ready for release, Done]}", `approved_release: {statuses: [Ready for release, Done], ignore_paths: [".graft.yaml", "docs/**", "*.md", ".claude/**"], non_release_parents: [PROJ-1]}`, 1)
	}
	d := newDeployFixture(t, cfg)
	d.write("frontend/package.json", `{"version":"1.0.1","name":"projectA"}`+"\n")
	d.write("frontend/package-lock.json", `{"name":"projectA","version":"1.0.1","packages":{"":{"name":"projectA","version":"1.0.1","dependencies":{"projectA":"1.0.0"}}}}`+"\n")
	d.mustPlainCommit("chore: setup")
	writeFile(t, filepath.Join(d.state, "gate-sha"), d.git("rev-parse", "HEAD")+"\n")
	d.statuses(map[string]string{"PROJ-1": "In Progress", "PROJ-2": "Done"}, map[string]string{"PROJ-2": "PROJ-1"})
	d.write(".graft.yaml", d.read(".graft.yaml")+"\n# update\n")
	d.write("VERSION", "1.0.2\n")
	d.write("frontend/package.json", `{"version":"1.0.2","name":"projectA"}`+"\n")
	d.write("frontend/package-lock.json", `{"name":"projectA","version":"1.0.2","packages":{"":{"name":"projectA","version":"1.0.2","dependencies":{"projectA":"1.0.0"}}}}`+"\n")
	d.mustPlainCommit("docs: PROJ-2 configuration")
	d.git("push", "-q", "origin", "HEAD")
	out, code := d.graft("", "deploy", "check", "gate")
	if code != 0 || strings.Contains(out, "goes out with its parent") {
		t.Fatalf("ignored and version files: exit %d\n%s", code, out)
	}
	d.write("src.go", "package projectA\n")
	d.mustPlainCommit("feat: PROJ-2 code")
	d.git("push", "-q", "origin", "HEAD")
	out, code = d.graft("", "deploy", "check", "gate")
	if code == 0 || !strings.Contains(out, "PROJ-2 goes out with its parent PROJ-1") {
		t.Fatalf("code: exit %d\n%s", code, out)
	}
}
