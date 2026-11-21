package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func reviewGuardFixture(t *testing.T, statuses []string) *deployFixture {
	t.Helper()
	return newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := deployConfig(state, q)
		old := "    uat:\n      env: local\n"
		guard := "    uat:\n      env: local\n      version: 'echo 1.2.10'\n      review_guard: {jira_version: 'Product v{version}', statuses: [" + strings.Join(statuses, ", ") + "]}\n"
		return strings.Replace(cfg, old, guard, 1)
	})
}

func setupGuardVersion(d *deployFixture, issues map[string][]string) {
	d.jira.setVersions(productVersions(), issues)
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-1"] = "Testing"
	d.jira.statuses["PROJ-5"] = "Testing"
	d.jira.assignees["PROJ-1"] = "anna"
	d.jira.assignees["PROJ-5"] = "anna"
	d.jira.mu.Unlock()
}

func TestDeployReviewGuardRefusesAndRequiresEveryReviewerPair(t *testing.T) {
	d := reviewGuardFixture(t, []string{"Testing", "In QA"})
	setupGuardVersion(d, map[string][]string{"PROJ-1": {"10010"}, "PROJ-5": {"10010"}})
	out, code := d.graft("", "deploy", "uat")
	if code == 0 || !strings.Contains(out, "PROJ-1") || !strings.Contains(out, "PROJ-5") || !strings.Contains(out, "anna") || !strings.Contains(out, "PROJ-1@anna") || !strings.Contains(out, "PROJ-5@anna") {
		t.Fatalf("guard refusal: exit %d\n%s", code, out)
	}
	if d.stateExists("uat-run") {
		t.Fatalf("deploy script ran despite review: %s", d.stateFile("uat-run"))
	}
	out, code = d.graft("", "deploy", "--reviewer-agreed", "PROJ-1@anna,PROJ-5@anna", "uat")
	if code != 0 || !strings.Contains(out, "deployed ") {
		t.Fatalf("reviewer agreement: exit %d\n%s", code, out)
	}
	if !d.stateExists("uat-run") {
		t.Fatal("deploy script did not run after both reviewers agreed")
	}
}

func TestDeployReviewGuardCommitKeysStopAtPreviousDeployment(t *testing.T) {
	d := reviewGuardFixture(t, []string{"Testing"})
	d.write("old-work.txt", "old\n")
	d.mustPlainCommit("older work PROJ-5")
	d.write("previous-release.txt", "previous\n")
	d.mustPlainCommit("previous deployment")
	previous := strings.TrimSpace(d.git("rev-parse", "HEAD"))
	writeFile(t, filepath.Join(d.state, "uat-sha"), previous+"\n")
	d.write("new-work.txt", "new\n")
	d.mustPlainCommit("new work PROJ-1")
	d.git("push", "-q", "-u", "origin", "HEAD")
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-5": {"10010"}})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-1"] = "Testing"
	d.jira.assignees["PROJ-1"] = "anna"
	d.jira.statuses["PROJ-5"] = "Testing"
	d.jira.assignees["PROJ-5"] = "anna"
	d.jira.mu.Unlock()

	out, code := d.graft("", "deploy", "uat")
	if code == 0 || !strings.Contains(out, "PROJ-5") || strings.Contains(out, "PROJ-1") {
		t.Fatalf("guard did not stop at the previous deployment SHA: exit %d\n%s", code, out)
	}
}

func TestDeployReviewGuardCheckReportsWithoutRefusing(t *testing.T) {
	d := reviewGuardFixture(t, []string{"In QA"})
	setupGuardVersion(d, map[string][]string{"PROJ-1": {"10010"}})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-1"] = "In QA"
	d.jira.mu.Unlock()
	out, code := d.graft("", "deploy", "check", "uat")
	if code != 0 || !strings.Contains(out, "PROJ-1") || !strings.Contains(out, "anna") || !strings.Contains(out, "PROJ-1@anna") || !strings.Contains(out, "would stop at the review guard") || strings.Contains(out, "passes its checks now") {
		t.Fatalf("deploy check report: exit %d\n%s", code, out)
	}
	if d.stateExists("uat-run") {
		t.Fatal("deploy check ran the script")
	}
}

func TestDeployReviewGuardUsesConfiguredStatuses(t *testing.T) {
	d := reviewGuardFixture(t, []string{"In QA"})
	setupGuardVersion(d, map[string][]string{"PROJ-1": {"10010"}})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-1"] = "Ready for QA"
	d.jira.mu.Unlock()
	out, code := d.graft("", "deploy", "uat")
	if code != 0 || !d.stateExists("uat-run") {
		t.Fatalf("unconfigured status should pass: exit %d\n%s", code, out)
	}
}

func TestDeployReviewGuardWithoutSHAOrTargetVersionRefusesUnknown(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := deployConfig(state, q)
		cfg = strings.Replace(cfg, "    uat:\n      env: local\n", "    uat:\n      env: local\n      review_guard: {jira_version: 'Product v{version}', statuses: [Testing]}\n", 1)
		cfg = strings.Replace(cfg, "      deployed_sha: {path: "+q("uat-sha")+"}\n", "", 1)
		return strings.Replace(cfg, `      release_notes: {transitions: [{from: [In Progress], to: Testing}], fix_version: {prefix: "Product v"}}`+"\n", "", 1)
	})
	d.jira.setVersions(productVersions(), nil)
	out, code := d.graft("", "deploy", "uat")
	if code == 0 || !strings.Contains(strings.ToLower(out), "unknown") || d.stateExists("uat-run") {
		t.Fatalf("missing target version should be unknown: exit %d\n%s", code, out)
	}
}

func TestDeployReviewGuardUnknownMyselfCannotBeAgreedAround(t *testing.T) {
	d := reviewGuardFixture(t, []string{"Testing"})
	setupGuardVersion(d, map[string][]string{"PROJ-1": {"10010"}})
	d.jira.mu.Lock()
	d.jira.failMyself = 503
	d.jira.mu.Unlock()
	out, code := d.graft("", "deploy", "--reviewer-agreed", "PROJ-1@anna", "uat")
	if code == 0 || !strings.Contains(out, "503") || d.stateExists("uat-run") {
		t.Fatalf("unknown user must refuse: exit %d\n%s", code, out)
	}
}

func TestDeployReviewGuardRejectsPartialAgreementAndJiraFailure(t *testing.T) {
	d := reviewGuardFixture(t, []string{"Testing"})
	setupGuardVersion(d, map[string][]string{"PROJ-1": {"10010"}, "PROJ-5": {"10010"}})
	out, code := d.graft("", "deploy", "--reviewer-agreed", "PROJ-1@anna", "uat")
	if code == 0 || !strings.Contains(out, "PROJ-5@anna") || d.stateExists("uat-run") {
		t.Fatalf("partial agreement passed: exit %d\n%s", code, out)
	}
	d.jira.mu.Lock()
	d.jira.failIssues = map[string]int{"PROJ-5": 429}
	d.jira.mu.Unlock()
	out, code = d.graft("", "deploy", "--reviewer-agreed", "PROJ-1@anna,PROJ-5@anna", "uat")
	if code == 0 || !strings.Contains(out, "429") || d.stateExists("uat-run") {
		t.Fatalf("Jira 429 was bypassed: exit %d\n%s", code, out)
	}
}

func TestDeployReviewGuardRepeatsImmediatelyBeforeScript(t *testing.T) {
	d := reviewGuardFixture(t, []string{"Testing"})
	setupGuardVersion(d, map[string][]string{"PROJ-1": {"10010"}})
	d.jira.mu.Lock()
	d.jira.changeIssueRead = map[string]int{"PROJ-1": 1}
	d.jira.changeAssignee = map[string]string{"PROJ-1": "boris"}
	d.jira.mu.Unlock()
	out, code := d.graft("", "deploy", "--reviewer-agreed", "PROJ-1@anna", "uat")
	if code == 0 || !strings.Contains(out, "PROJ-1@boris") || !strings.Contains(out, "boris") || d.stateExists("uat-run") {
		t.Fatalf("stale reviewer agreement passed: exit %d\n%s", code, out)
	}
}

func TestDeployReviewGuardFindsSubtaskWithoutACommit(t *testing.T) {
	d := reviewGuardFixture(t, []string{"Testing"})
	setupGuardVersion(d, map[string][]string{"PROJ-1": {"10010"}})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-1"] = "Done"
	d.jira.parents = map[string]string{"PROJ-10": "PROJ-1"}
	d.jira.statuses["PROJ-10"] = "Testing"
	d.jira.assignees["PROJ-10"] = "anna"
	d.jira.mu.Unlock()
	out, code := d.graft("", "deploy", "uat")
	if code == 0 || !strings.Contains(out, "PROJ-10") || !strings.Contains(out, "PROJ-10@anna") || d.stateExists("uat-run") {
		t.Fatalf("subtask review was missed: exit %d\n%s", code, out)
	}
}

func TestDeployReviewGuardFindsReviewStartedBetweenChecks(t *testing.T) {
	d := reviewGuardFixture(t, []string{"Testing"})
	setupGuardVersion(d, map[string][]string{"PROJ-1": {"10010"}})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-1"] = "Done"
	d.jira.changeIssueRead = map[string]int{"PROJ-1": 1}
	d.jira.changeStatus = map[string]string{"PROJ-1": "Testing"}
	d.jira.mu.Unlock()
	out, code := d.graft("", "deploy", "uat")
	if code == 0 || !strings.Contains(out, "PROJ-1@anna") || d.stateExists("uat-run") {
		t.Fatalf("new review was not caught by second check: exit %d\n%s", code, out)
	}
}

func TestDeployReviewGuardUsesTheVersionOnThisTarget(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := deployConfig(state, q)
		cfg = strings.Replace(cfg, "    test:\n      env: local\n", "    test:\n      env: local\n      review_guard: {jira_version: 'Product v{version}', statuses: [Testing]}\n", 1)
		cfg = strings.Replace(cfg, "    uat:\n      env: local\n", "    uat:\n      env: local\n      version: 'echo 1.2.10'\n      review_guard: {jira_version: 'Product v{version}', statuses: [Testing]}\n", 1)
		return cfg
	})
	writeFile(t, filepath.Join(d.state, "test-version"), "Product v2.0.0\n")
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-1": {"10200"}})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-1"] = "Testing"
	d.jira.assignees["PROJ-1"] = "anna"
	d.jira.mu.Unlock()
	out, code := d.graft("", "deploy", "uat")
	if code != 0 || !d.stateExists("uat-run") {
		t.Fatalf("UAT was blocked by TEST version: exit %d\n%s", code, out)
	}
}

func TestDeployReviewGuardUsesJiraFixVersionAcrossRequiresRange(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := deployConfig(state, q)
		return strings.Replace(cfg, "    uat:\n      env: local\n", "    uat:\n      env: local\n      version: 'echo 2.23.38'\n      requires: test\n      review_guard: {jira_version: 'Product v{version}', statuses: [Testing, In QA]}\n", 1)
	})
	d.write("old.txt", "old\n")
	d.mustPlainCommit("old work PROJ-5")
	old := strings.TrimSpace(d.git("rev-parse", "HEAD"))
	d.write("base.txt", "base\n")
	d.mustPlainCommit("required target baseline")
	base := strings.TrimSpace(d.git("rev-parse", "HEAD"))
	d.write("release.txt", "release\n")
	d.mustPlainCommit("release PROJ-1")
	deployed := strings.TrimSpace(d.git("rev-parse", "HEAD"))
	d.git("push", "-q", "-u", "origin", "HEAD")
	writeFile(t, filepath.Join(d.state, "test-version"), "1.0.1\n")
	writeFile(t, filepath.Join(d.state, "test-sha"), base+"\n")
	writeFile(t, filepath.Join(d.state, "uat-sha"), deployed+"\n")
	if old == base || old == deployed {
		t.Fatal("fixture did not create an older commit outside the requires range")
	}
	d.jira.setVersions([]map[string]any{{"id": "223", "name": "Product v2.23.38", "released": false, "archived": false}}, map[string][]string{
		"PROJ-1": {"223"}, "PROJ-5": {"223"},
	})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-1"] = "Done"
	d.jira.statuses["PROJ-5"] = "Testing"
	d.jira.assignees["PROJ-5"] = "anna"
	d.jira.mu.Unlock()
	out, code := d.graft("", "deploy", "uat")
	if code == 0 || !strings.Contains(out, "PROJ-5") || !strings.Contains(out, "Product v2.23") || d.stateExists("uat-run") {
		t.Fatalf("Jira fix version and older version item were missed: exit %d\n%s", code, out)
	}
}

func reviewerReminderFixture(t *testing.T) *deployFixture {
	t.Helper()
	return newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := deployConfig(state, q)
		cfg = strings.Replace(cfg, "      release_notes: {transitions: [{from: [In Progress], to: Testing}], fix_version: {prefix: \"Product v\"}}\n", "      release_notes: {transitions: [{from: [In Progress], to: Testing}]}\n", 1)
		return strings.Replace(cfg, "    uat:\n      env: local\n", "    uat:\n      env: local\n      reviewer_reminder: {statuses: [Testing]}\n", 1)
	})
}

func setupReminderCommits(t *testing.T, d *deployFixture, oldKey, newKey string) {
	t.Helper()
	d.write("reminder-old.txt", oldKey+"\n")
	d.mustPlainCommit("older work " + oldKey)
	previous := strings.TrimSpace(d.git("rev-parse", "HEAD"))
	writeFile(t, filepath.Join(d.state, "uat-sha"), previous+"\n")
	d.write("reminder-new.txt", newKey+"\n")
	d.mustPlainCommit("this release " + newKey)
	d.git("push", "-q", "-u", "origin", "HEAD")
	d.jira.mu.Lock()
	d.jira.statuses[oldKey] = "Testing"
	d.jira.assignees[oldKey] = "anna"
	d.jira.mu.Unlock()
}

func TestDeployReviewerReminderReportsUnmentionedTaskAndSkipsThisRelease(t *testing.T) {
	t.Run("older task", func(t *testing.T) {
		d := reviewerReminderFixture(t)
		setupReminderCommits(t, d, "PROJ-9", "PROJ-1")
		out, code := d.graft("", "deploy", "uat")
		want := "graft: PROJ-9: under review by anna (Testing); post what changed on uat in the task"
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("reminder missing: exit %d\n%s", code, out)
		}
	})
	t.Run("named in release", func(t *testing.T) {
		d := reviewerReminderFixture(t)
		setupReminderCommits(t, d, "PROJ-1", "PROJ-1")
		d.jira.mu.Lock()
		d.jira.assignees["PROJ-1"] = "anna"
		d.jira.mu.Unlock()
		out, code := d.graft("", "deploy", "uat")
		if code != 0 || strings.Contains(out, "under review by") {
			t.Fatalf("named task got reminder: exit %d\n%s", code, out)
		}
	})
}

func TestDeployReviewerReminderFiltersAssigneeAndStatus(t *testing.T) {
	for _, tc := range []struct {
		name, assignee, status string
	}{
		{name: "graft account", assignee: "me", status: "Testing"},
		{name: "other status", assignee: "anna", status: "Done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := reviewerReminderFixture(t)
			setupReminderCommits(t, d, "PROJ-9", "PROJ-1")
			d.jira.mu.Lock()
			d.jira.assignees["PROJ-9"] = tc.assignee
			d.jira.statuses["PROJ-9"] = tc.status
			d.jira.mu.Unlock()
			out, code := d.graft("", "deploy", "uat")
			if code != 0 || strings.Contains(out, "under review by") {
				t.Fatalf("filtered task got reminder: exit %d\n%s", code, out)
			}
		})
	}
}

func TestDeployReviewerReminderDoesNotIncludeUncommittedSubtasks(t *testing.T) {
	d := reviewerReminderFixture(t)
	setupReminderCommits(t, d, "PROJ-9", "PROJ-1")
	d.jira.mu.Lock()
	d.jira.parents = map[string]string{"PROJ-10": "PROJ-9"}
	d.jira.statuses["PROJ-10"] = "Testing"
	d.jira.assignees["PROJ-10"] = "anna"
	d.jira.mu.Unlock()
	out, code := d.graft("", "deploy", "uat")
	if code != 0 || strings.Contains(out, "PROJ-10: under review by") {
		t.Fatalf("uncommitted subtask got a reminder: exit %d\n%s", code, out)
	}
}

func TestDeployReviewerReminderExcludesCurrentReleaseParent(t *testing.T) {
	d := reviewerReminderFixture(t)
	setupReminderCommits(t, d, "PROJ-9", "PROJ-1")
	d.jira.mu.Lock()
	d.jira.parents = map[string]string{"PROJ-9": "PROJ-1"}
	d.jira.statuses["PROJ-9"] = "Done"
	d.jira.statuses["PROJ-1"] = "Testing"
	d.jira.assignees["PROJ-1"] = "anna"
	d.jira.mu.Unlock()
	out, code := d.graft("", "deploy", "uat")
	if code != 0 || strings.Contains(out, "PROJ-1: under review by") {
		t.Fatalf("current release parent got a reminder: exit %d\n%s", code, out)
	}
}

func TestDeployReviewerReminderKeepsParentOfNamedSubtask(t *testing.T) {
	d := reviewerReminderFixture(t)
	setupReminderCommits(t, d, "PROJ-9", "PROJ-1")
	d.jira.mu.Lock()
	d.jira.parents = map[string]string{"PROJ-1": "PROJ-9"}
	d.jira.mu.Unlock()
	out, code := d.graft("", "deploy", "uat")
	if code != 0 || !strings.Contains(out, "graft: PROJ-9: under review by anna (Testing)") {
		t.Fatalf("parent without a release note was excluded: exit %d\n%s", code, out)
	}
}

func TestDeployReviewerReminderWithoutKeysAndOnJiraFailure(t *testing.T) {
	t.Run("no keys", func(t *testing.T) {
		d := reviewerReminderFixture(t)
		d.write("reminder-no-key.txt", "change\n")
		d.mustPlainCommit("change without a task key")
		d.git("push", "-q", "-u", "origin", "HEAD")
		out, code := d.graft("", "deploy", "uat")
		d.jira.mu.Lock()
		requests := d.jira.requests
		d.jira.mu.Unlock()
		if code != 0 || strings.Contains(out, "under review by") || requests != 0 {
			t.Fatalf("empty task range changed deploy: exit %d, Jira requests %d\n%s", code, requests, out)
		}
	})
	t.Run("Jira unavailable", func(t *testing.T) {
		d := reviewerReminderFixture(t)
		setupReminderCommits(t, d, "PROJ-9", "PROJ-1")
		d.jira.mu.Lock()
		if d.jira.failIssues == nil {
			d.jira.failIssues = map[string]int{}
		}
		d.jira.failIssues["PROJ-9"] = 503
		d.jira.mu.Unlock()
		out, code := d.graft("", "deploy", "uat")
		if code != 0 || !d.stateExists("uat-run") || !strings.Contains(out, "graft: warning: reviewer reminder skipped:") {
			t.Fatalf("Jira failure changed deploy result: exit %d\n%s", code, out)
		}
	})
}

func TestDeployReviewGuardTemplateMatchesFullJiraVersion(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := deployConfig(state, q)
		return strings.Replace(cfg, "    uat:\n      env: local\n", "    uat:\n      env: local\n      version: 'echo 2.23.38'\n      review_guard: {jira_version: 'Product v{version}', statuses: [Testing]}\n", 1)
	})
	d.jira.setVersions([]map[string]any{{"id": "238", "name": "Product v2.23.38", "released": false, "archived": false}}, map[string][]string{"PROJ-2": {"238"}})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-2"] = "Testing"
	d.jira.assignees["PROJ-2"] = "anna"
	d.jira.mu.Unlock()
	out, code := d.graft("", "deploy", "uat")
	if code == 0 || !strings.Contains(out, "Product v2.23.38") || !strings.Contains(out, "PROJ-2") || !strings.Contains(out, "anna") || d.stateExists("uat-run") {
		t.Fatalf("version template did not guard exact Jira release: exit %d\n%s", code, out)
	}
}

func TestDeployReviewGuardUnknownShowsComputedVersionAndNearbyVersions(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := deployConfig(state, q)
		return strings.Replace(cfg, "    uat:\n      env: local\n", "    uat:\n      env: local\n      version: 'echo 2.23.38'\n      review_guard: {jira_version: 'Product v{version}', statuses: [Testing]}\n", 1)
	})
	d.jira.setVersions([]map[string]any{{"id": "237", "name": "Product v2.23.37", "released": false, "archived": false}}, nil)
	out, code := d.graft("", "deploy", "uat")
	if code == 0 || !strings.Contains(out, "unknown") || !strings.Contains(out, "Product v2.23.38") || !strings.Contains(out, "Product v2.23.37") || d.stateExists("uat-run") {
		t.Fatalf("unknown version diagnostic: exit %d\n%s", code, out)
	}
}

func TestDeployReviewGuardIncludesCommitKeysWithoutFixVersions(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := deployConfig(state, q)
		return strings.Replace(cfg, "    uat:\n      env: local\n", "    uat:\n      env: local\n      version: 'echo 1.2.10'\n      requires: test\n      review_guard: {jira_version: 'Product v{version}', statuses: [Testing]}\n", 1)
	})
	base := strings.TrimSpace(d.git("rev-parse", "HEAD"))
	d.write("change.txt", "change\n")
	d.mustPlainCommit("done work PROJ-1")
	deployed := strings.TrimSpace(d.git("rev-parse", "HEAD"))
	d.git("push", "-q", "-u", "origin", "HEAD")
	writeFile(t, filepath.Join(d.state, "test-sha"), base+"\n")
	writeFile(t, filepath.Join(d.state, "test-version"), "1.0.1\n")
	writeFile(t, filepath.Join(d.state, "uat-sha"), deployed+"\n")
	d.jira.setVersions(productVersions(), nil)
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-1"] = "Testing"
	d.jira.assignees["PROJ-1"] = "anna"
	d.jira.mu.Unlock()
	out, code := d.graft("", "deploy", "uat")
	if code == 0 || !strings.Contains(out, "PROJ-1@anna") || !strings.Contains(out, "PROJ-1") || d.stateExists("uat-run") {
		t.Fatalf("commit key without fixVersions was not checked: exit %d\n%s", code, out)
	}
}

func TestDeployReviewGuardFindsVersionTaskBesideDoneCommitKey(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := deployConfig(state, q)
		return strings.Replace(cfg, "    uat:\n      env: local\n", "    uat:\n      env: local\n      version: 'echo 1.2.10'\n      requires: test\n      review_guard: {jira_version: 'Product v{version}', statuses: [Testing]}\n", 1)
	})
	base := strings.TrimSpace(d.git("rev-parse", "HEAD"))
	d.write("done.txt", "done\n")
	d.mustPlainCommit("done work PROJ-1")
	deployed := strings.TrimSpace(d.git("rev-parse", "HEAD"))
	d.git("push", "-q", "-u", "origin", "HEAD")
	writeFile(t, filepath.Join(d.state, "test-sha"), base+"\n")
	writeFile(t, filepath.Join(d.state, "test-version"), "1.0.1\n")
	writeFile(t, filepath.Join(d.state, "uat-sha"), deployed+"\n")
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-2": {"10010"}})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-1"] = "Done"
	d.jira.statuses["PROJ-2"] = "Testing"
	d.jira.assignees["PROJ-2"] = "anna"
	d.jira.mu.Unlock()
	out, code := d.graft("", "deploy", "uat")
	if code == 0 || !strings.Contains(out, "PROJ-2@anna") || strings.Contains(out, "PROJ-1:") || d.stateExists("uat-run") {
		t.Fatalf("version search missed review while handling a completed range key: exit %d\n%s", code, out)
	}
}

func TestDeployReviewGuardUsesAtMostSixHTTPRequestsForFiftyVersionItems(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := deployConfig(state, q)
		return strings.Replace(cfg, "    uat:\n      env: local\n", "    uat:\n      env: local\n      version: 'echo 1.2.10'\n      requires: test\n      review_guard: {jira_version: 'Product v{version}', statuses: [Testing]}\n", 1)
	})
	base := strings.TrimSpace(d.git("rev-parse", "HEAD"))
	d.write("range.txt", "range\n")
	d.mustPlainCommit("range PROJ-999")
	deployed := strings.TrimSpace(d.git("rev-parse", "HEAD"))
	d.git("push", "-q", "-u", "origin", "HEAD")
	writeFile(t, filepath.Join(d.state, "test-sha"), base+"\n")
	writeFile(t, filepath.Join(d.state, "test-version"), "1.0.1\n")
	writeFile(t, filepath.Join(d.state, "uat-sha"), deployed+"\n")
	fixVersions := map[string][]string{}
	for i := range 50 {
		key := fmt.Sprintf("PROJ-%d", 100+i)
		fixVersions[key] = []string{"10010"}
		d.jira.mu.Lock()
		d.jira.statuses[key] = "Testing"
		d.jira.assignees[key] = "anna"
		d.jira.mu.Unlock()
	}
	d.jira.setVersions(productVersions(), fixVersions)
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-999"] = "Done"
	d.jira.assignees["PROJ-999"] = "me"
	d.jira.mu.Unlock()
	_, code := d.graft("", "deploy", "check", "uat")
	if code != 0 {
		t.Fatalf("deploy check failed: %d", code)
	}
	d.jira.mu.Lock()
	searches, requests := d.jira.reviewSearchRequests, d.jira.requests
	d.jira.mu.Unlock()
	if got := searches; got != 4 {
		t.Fatalf("one guard pass made %d Jira search requests for 50 version items, want commit, status, version-key and parent searches", got)
	}
	if got := requests; got != 6 {
		t.Fatalf("one guard pass made %d total Jira HTTP requests, want the six-request budget", got)
	}
}

func TestDeployReviewGuardSearchFindsSecondPageReview(t *testing.T) {
	d := reviewGuardFixture(t, []string{"Testing"})
	d.jira.mu.Lock()
	d.jira.versionPageSize = 1
	d.jira.mu.Unlock()
	d.jira.setVersions(productVersions(), map[string][]string{
		"PROJ-101": {"10010"}, "PROJ-102": {"10010"},
	})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-101"] = "Testing"
	d.jira.statuses["PROJ-102"] = "Testing"
	d.jira.assignees["PROJ-101"] = "me"
	d.jira.assignees["PROJ-102"] = "anna"
	d.jira.mu.Unlock()
	out, code := d.graft("", "deploy", "uat")
	if code == 0 || !strings.Contains(out, "PROJ-102@anna") || d.stateExists("uat-run") {
		t.Fatalf("review on second page was missed: exit %d\n%s", code, out)
	}
	d.jira.mu.Lock()
	searches := d.jira.reviewSearchRequests
	d.jira.mu.Unlock()
	if searches < 4 {
		t.Fatalf("only %d paginated searches; expected second pages for version tasks and keys", searches)
	}
}

func TestDeployReviewGuardIgnoresMyselfAndUnassigned(t *testing.T) {
	for _, assignee := range []string{"me", ""} {
		t.Run(map[string]string{"me": "myself", "": "unassigned"}[assignee], func(t *testing.T) {
			d := reviewGuardFixture(t, []string{"Testing"})
			setupGuardVersion(d, map[string][]string{"PROJ-1": {"10010"}})
			d.jira.mu.Lock()
			d.jira.statuses["PROJ-1"] = "Testing"
			d.jira.assignees["PROJ-1"] = assignee
			d.jira.mu.Unlock()
			out, code := d.graft("", "deploy", "uat")
			if code != 0 || !d.stateExists("uat-run") {
				t.Fatalf("assignee %q should not block: exit %d\n%s", assignee, code, out)
			}
		})
	}
}

func TestDeployReviewGuardReadsVersionFromDeployedSHA(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := deployConfig(state, q)
		return strings.Replace(cfg, "    uat:\n      env: local\n", "    uat:\n      env: local\n      review_guard: {jira_version: 'Product v{version}', statuses: [Testing]}\n", 1)
	})
	deployed := strings.TrimSpace(d.git("rev-parse", "HEAD"))
	writeFile(t, filepath.Join(d.state, "uat-sha"), deployed+"\n")
	d.jira.setVersions([]map[string]any{{"id": "10101", "name": "Product v1.0.1", "released": false, "archived": false}}, map[string][]string{"PROJ-1": {"10101"}})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-1"] = "Testing"
	d.jira.assignees["PROJ-1"] = "anna"
	d.jira.mu.Unlock()
	out, code := d.graft("", "deploy", "uat")
	if code == 0 || !strings.Contains(out, "Product v1.0.1") || !strings.Contains(out, "PROJ-1@anna") {
		t.Fatalf("version from deployed SHA was not used: exit %d\n%s", code, out)
	}
}

func TestDeployReviewGuardRequiresVersionPlaceholder(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := deployConfig(state, q)
		return strings.Replace(cfg, "    uat:\n      env: local\n", "    uat:\n      env: local\n      review_guard: {jira_version: 'Product v1.0.1', statuses: [Testing]}\n", 1)
	})
	out, code := d.graft("", "deploy", "check", "uat")
	if code == 0 || !strings.Contains(out, "review_guard.jira_version") || !strings.Contains(out, "{version}") {
		t.Fatalf("template without placeholder was accepted: exit %d\n%s", code, out)
	}
}

func TestDeployReviewGuardEscapesStatusInJQL(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := deployConfig(state, q)
		return strings.Replace(cfg, "    uat:\n      env: local\n", "    uat:\n      env: local\n      version: 'echo 1.2.10'\n      review_guard: {jira_version: 'Product v{version}', statuses: ['Testing \"review\" \\ lane']}\n", 1)
	})
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-1": {"10010"}})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-1"] = `Testing "review" \ lane`
	d.jira.assignees["PROJ-1"] = "anna"
	d.jira.mu.Unlock()
	_, code := d.graft("", "deploy", "check", "uat")
	if code != 0 {
		t.Fatal("deploy check rejected escaped JQL status")
	}
	want := `fixVersion = 10010 AND status in ("Testing \"review\" \\ lane")`
	d.jira.mu.Lock()
	jql := slices.Clone(d.jira.reviewSearchJQL)
	d.jira.mu.Unlock()
	if !slices.Contains(jql, want) {
		t.Fatalf("Jira query missing exact escaped status; got %#v, want %q", jql, want)
	}
}

func TestDeployReviewGuardSeparatesRealTargetVersionsAndCommitKeys(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := deployConfig(state, q)
		return strings.Replace(cfg, "    uat:\n      env: local\n", "    uat:\n      env: local\n      version: 'echo 1.2.10'\n      review_guard: {jira_version: 'Product v{version}', statuses: [Testing]}\n", 1)
	})
	d.write("feature.txt", "feature\n")
	d.mustPlainCommit("feature PROJ-1")
	deployed := strings.TrimSpace(d.git("rev-parse", "HEAD"))
	d.git("push", "-q", "-u", "origin", "HEAD")
	writeFile(t, filepath.Join(d.state, "uat-sha"), deployed+"\n")
	writeFile(t, filepath.Join(d.state, "test-version"), "Product v2.0.0\n")
	writeFile(t, filepath.Join(d.state, "test-sha"), deployed+"\n")
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-1": {"10200"}})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-1"] = "Testing"
	d.jira.assignees["PROJ-1"] = "anna"
	d.jira.mu.Unlock()
	out, code := d.graft("", "deploy", "uat")
	if code != 0 || !d.stateExists("uat-run") {
		t.Fatalf("TEST version and commit key blocked UAT version: exit %d\n%s", code, out)
	}
}

func TestDeployReviewGuardChecksEveryProjectKey(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := deployConfig(state, q)
		cfg = strings.Replace(cfg, "project_keys: [PROJ]", "project_keys: [PROJ, OTHER]", 1)
		cfg = strings.Replace(cfg, "    uat:\n      env: local\n", "    uat:\n      env: local\n      version: 'echo Other v1'\n      review_guard: {jira_version: '{version}', statuses: [Testing]}\n", 1)
		return strings.Replace(cfg, `, fix_version: {prefix: "Product v"}`, "", 1)
	})
	d.write("other.txt", "other\n")
	d.mustPlainCommit("feature OTHER-1")
	deployed := strings.TrimSpace(d.git("rev-parse", "HEAD"))
	d.git("push", "-q", "-u", "origin", "HEAD")
	writeFile(t, filepath.Join(d.state, "uat-sha"), deployed+"\n")
	d.jira.mu.Lock()
	d.jira.otherVersions = []map[string]any{{"id": "other-1", "name": "Other v1", "released": false, "archived": false}}
	d.jira.mu.Unlock()
	d.jira.setVersions(productVersions(), nil)
	d.jira.mu.Lock()
	d.jira.statuses["OTHER-1"] = "Testing"
	d.jira.assignees["OTHER-1"] = "anna"
	d.jira.mu.Unlock()
	out, code := d.graft("", "deploy", "uat")
	if code == 0 || !strings.Contains(out, "OTHER-1") || d.stateExists("uat-run") {
		t.Fatalf("second project was ignored: exit %d\n%s", code, out)
	}
}

func TestDeployReviewGuardDeployRefusesInQA(t *testing.T) {
	d := reviewGuardFixture(t, []string{"In QA"})
	setupGuardVersion(d, map[string][]string{"PROJ-1": {"10010"}})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-1"] = "In QA"
	d.jira.mu.Unlock()
	out, code := d.graft("", "deploy", "uat")
	if code == 0 || !strings.Contains(out, "In QA") || d.stateExists("uat-run") {
		t.Fatalf("deploy did not refuse In QA: exit %d\n%s", code, out)
	}
}

func TestDeployWithoutReviewGuardAddsNoJiraReads(t *testing.T) {
	d := deployRepo(t)
	writeFile(t, filepath.Join(d.state, "test-version"), "1.0.1\n")
	out, code := d.graft("", "deploy", "stage")
	if code != 0 {
		t.Fatalf("deploy: exit %d\n%s", code, out)
	}
	d.jira.mu.Lock()
	requests := d.jira.requests
	d.jira.mu.Unlock()
	if requests != 0 {
		t.Fatalf("target without review_guard made %d Jira requests", requests)
	}
	if !d.stateExists("stage-run") {
		t.Fatal("deploy script did not run without review_guard")
	}
}
