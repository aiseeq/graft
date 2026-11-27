package main

import (
	"path/filepath"
	"strings"
	"testing"
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
