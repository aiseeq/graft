package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func withheldHintFixture(t *testing.T, prefixGate bool) (*deployFixture, string, string, string) {
	t.Helper()
	cfg := func(state string, q func(string) string) string {
		if prefixGate {
			return strings.Replace(versionGateConfig(state, q),
				"approved_release: {statuses: [Ready for release, Done]}",
				`approved_release: {prefix: "Product", statuses: [Ready for release, Done], version: {match: '^Product'}}`, 1)
		}
		return strings.Replace(versionGateConfig(state, q),
			"approved_release: {statuses: [Ready for release, Done]}",
			"approved_release: {statuses: [Ready for release, Done], version: {match: '^Product'}}", 1)
	}
	d := newDeployFixture(t, cfg)
	prev := d.git("rev-parse", "HEAD")
	writeFile(t, filepath.Join(d.state, "gate-sha"), prev+"\n")

	d.git("switch", "-q", "-c", "withheld-work")
	d.write("withheld.txt", "synthetic withheld work\n")
	d.mustPlainCommit("feat: PROJ-1 withheld synthetic work")
	w := d.git("rev-parse", "HEAD")
	d.git("switch", "-q", "-c", "deploy-work", prev)

	marker := filepath.Join(d.state, "gate-sha") + ".withheld"
	writeFile(t, marker, w+"\n")
	d.jira.setVersions([]map[string]any{{"id": "target", "name": "Product 1.0"}}, map[string][]string{
		"PROJ-1": {"target"},
		"PROJ-2": {"target"},
	})
	d.jira.setApprovers(map[string][]string{"target": {"APPROVED"}})
	return d, w, marker, prev
}

func withheldHint(t *testing.T, d *deployFixture, key string, status string) string {
	t.Helper()
	d.jira.mu.Lock()
	d.jira.statuses[key] = status
	d.jira.mu.Unlock()
	d.write(strings.ToLower(key)+".txt", "synthetic release work\n")
	d.mustPlainCommit("feat: " + key + " release work")
	d.git("push", "-q", "-u", "origin", "HEAD")
	out, code := d.graft("", "deploy", "check", "prod")
	if code == 0 {
		t.Fatalf("deploy check unexpectedly passed:\n%s", out)
	}
	return out
}

func TestDeployCheckWithheldHintCommitItems(t *testing.T) {
	d, w, marker, _ := withheldHintFixture(t, true)
	// Keep PROJ-1 out of the approved version while PROJ-2 selects it.
	d.jira.mu.Lock()
	d.jira.fixVersions["PROJ-1"] = nil
	d.jira.statuses["PROJ-1"] = "Ready for release"
	d.jira.statuses["PROJ-2"] = "Ready for release"
	d.jira.mu.Unlock()
	out := withheldHint(t, d, "PROJ-2", "Ready for release")
	want := d.gitIn(d.work, "rev-parse", "--short=12", w) + " was withheld from an earlier release: if its change is already on the target, remove its line from " + marker + "."
	commitItems := strings.Index(out, "commit-items:")
	hint := strings.Index(out, want)
	versionTop := strings.Index(out, "version-top:")
	if commitItems < 0 || hint < commitItems || versionTop < 0 || hint > versionTop {
		t.Fatalf("commit-items refusal missing withheld hint %q:\n%s", want, out)
	}
	if got := readFile(t, marker); got != w+"\n" {
		t.Fatalf("withheld marker changed: got %q, want %q", got, w+"\n")
	}
}

func TestDeployCheckWithheldHintVersionTop(t *testing.T) {
	d, w, marker, _ := withheldHintFixture(t, false)
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-1"] = "In Progress"
	d.jira.statuses["PROJ-2"] = "Ready for release"
	d.jira.mu.Unlock()
	out := withheldHint(t, d, "PROJ-2", "Ready for release")
	want := d.gitIn(d.work, "rev-parse", "--short=12", w) + " was withheld from an earlier release: if its change is already on the target, remove its line from " + marker + "."
	if !strings.Contains(out, "version-top") || !strings.Contains(out, want) {
		t.Fatalf("version-top refusal missing withheld hint %q:\n%s", want, out)
	}
	if got := readFile(t, marker); got != w+"\n" {
		t.Fatalf("withheld marker changed: got %q, want %q", got, w+"\n")
	}
}

func TestDeployCheckWithheldHintMixedSelectionDoesNotNameOtherCommit(t *testing.T) {
	d, w, marker, _ := withheldHintFixture(t, false)
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-1"] = "Ready for release"
	d.jira.statuses["PROJ-2"] = "In Progress"
	d.jira.mu.Unlock()
	out := withheldHint(t, d, "PROJ-2", "In Progress")
	if !strings.Contains(out, "version-top") {
		t.Fatalf("missing version-top refusal:\n%s", out)
	}
	if strings.Contains(out, "withheld from an earlier release: if its change is already on the target") {
		t.Fatalf("unrelated failure was given a withheld hint:\n%s", out)
	}
	if got := readFile(t, marker); got != w+"\n" {
		t.Fatalf("withheld marker changed: got %q, want %q", got, w+"\n")
	}
}

func TestDeployAndCheckReportAllVersionTopBlockers(t *testing.T) {
	d, withheld, _, _ := withheldHintFixture(t, false)
	d.jira.setVersions([]map[string]any{
		{"id": "current", "name": "Product 1.0"},
	}, map[string][]string{
		"PROJ-1": {"current"},
		"PROJ-2": {"current"},
	})
	d.jira.setApprovers(map[string][]string{"current": {"APPROVED"}})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-1"] = "Testing"
	d.jira.statuses["PROJ-2"] = "Testing"
	d.jira.mu.Unlock()
	d.write("candidate.txt", "synthetic candidate work\n")
	d.mustPlainCommit("feat: PROJ-2 candidate work")
	d.git("push", "-q", "-u", "origin", "HEAD")

	for _, args := range [][]string{{"deploy", "check", "prod"}, {"deploy", "prod"}} {
		out, code := d.graft("", args...)
		if code == 0 {
			t.Fatalf("%v unexpectedly passed:\n%s", args, out)
		}
		for _, want := range []string{
			"version-top: PROJ-2 is Testing",
			"version-top: PROJ-1 is Testing",
			withheld[:12] + " was withheld from an earlier release",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%v missing %q:\n%s", args, want, out)
			}
		}
	}
}

func TestDeployAndCheckReportCommitStatusHumanAndWindowBlockersTogether(t *testing.T) {
	cfg := func(state string, q func(string) string) string {
		out := versionConfig(state, q)
		return strings.Replace(out,
			"approved_release: {statuses: [Ready for release, Done], ignore_paths: [\"docs/**\"]}",
			`approved_release: {prefix: "Product", statuses: [Ready for release, Done], ignore_paths: ["docs/**"], version: {match: '^Product', human_check: {from: [Testing], labels: [noqa]}}}`, 1)
	}
	d := newDeployFixture(t, cfg)
	d.at("2026-10-22 15:30")
	prev := d.git("rev-parse", "HEAD")
	writeFile(t, filepath.Join(d.state, "prod-sha"), prev+"\n")
	writeFile(t, filepath.Join(d.state, "prod-version"), "1.0.1\n")
	writeFile(t, filepath.Join(d.state, "test-sha"), prev+"\n")
	writeFile(t, filepath.Join(d.state, "test-version"), "1.0.1\n")
	d.jira.setVersions([]map[string]any{{"id": "window", "name": "Product 22.10.26"}}, map[string][]string{
		"PROJ-1": {"window"}, "PROJ-2": {"window"}, "PROJ-3": {"window"},
	})
	d.jira.setApprovers(map[string][]string{"window": {"APPROVED"}})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-1"] = "Testing"
	d.jira.statuses["PROJ-2"] = "Ready for release"
	d.jira.statuses["PROJ-3"] = "Ready for release"
	d.jira.statuses["PROJ-4"] = "In Progress"
	d.jira.statusMoves = map[string][]fakeStatusMove{"PROJ-2": {{
		at: time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC), from: "Testing", to: "Ready for release", authorID: "me", authorName: "Synthetic bot",
	}}}
	d.jira.mu.Unlock()
	d.write("one.txt", "synthetic task one\n")
	d.mustPlainCommit("feat: PROJ-1 synthetic work")
	d.write("two.txt", "synthetic task two\n")
	d.mustPlainCommit("feat: PROJ-2 synthetic work")
	d.write("four.txt", "synthetic task four\n")
	d.mustPlainCommit("feat: PROJ-4 synthetic work")
	d.git("push", "-q", "-u", "origin", "HEAD")

	for _, args := range [][]string{{"deploy", "check", "prod"}, {"deploy", "prod"}} {
		out, code := d.graft("", args...)
		if code == 0 {
			t.Errorf("%v unexpectedly passed:\n%s", args, out)
		}
		// Each refusal is matched with the task it names on its own line.
		for _, want := range [][]string{
			{"commit-items:"},
			{"feat: PROJ-4 synthetic work: PROJ-4 not in an approved \"Product\" version"},
			{"version-top: PROJ-1 is Testing"},
			{"version-status: Product 22.10.26: PROJ-1 (Testing)"},
			{"version-human-check:", "PROJ-2:"},
			{"version Product 22.10.26 in project PROJ lists PROJ-3, which this release does not take out"},
		} {
			if !lineWith(out, want...) {
				t.Errorf("%v missing a line with %q:\n%s", args, want, out)
			}
		}
		// PROJ-1 and PROJ-2 go out with this release: refused by the
		// approved release, they are not missing from it as well.
		if lineWith(out, "does not take out", "PROJ-1") || lineWith(out, "does not take out", "PROJ-2") {
			t.Errorf("%v refuses the release's own items as not taken out:\n%s", args, out)
		}
	}
}
