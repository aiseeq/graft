package main

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestDeployLeavesDormantReleaseNoteAlone(t *testing.T) {
	configure := func(state string, q func(string) string) string {
		cfg := deployConfig(state, q)
		cfg = strings.Replace(cfg, "      release_notes: {transitions: [{from: [In Progress], to: Testing}]}\n", "      release_notes: {transitions: [{from: [In Progress], to: Testing}, {from: [Testing], to: Done}]}\n      approved_release: {statuses: [Ready for release], prefix: 'Product v', dormant: [{key: PROJ-123, reason: test-stand}]}\n", 1)
		return cfg
	}
	d := newDeployFixture(t, configure)
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-123"] = "Testing"
	d.jira.statuses["PROJ-124"] = "In Progress"
	d.jira.parents = map[string]string{"PROJ-124": "PROJ-123"}
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "--unapproved", "test")
	d.write("dormant.txt", "disabled feature stays in build\n")
	d.mustFixtureCommit("feat: PROJ-124 dormant child work")
	out := d.mustGraft("deploy", "--unapproved", "test")
	comments, moved := d.jira.snapshot()
	if len(comments["PROJ-124"]) != 0 || slices.Contains(moved, "PROJ-124") {
		t.Fatalf("dormant item got note/transition: comments=%v moved=%v\n%s", comments, moved, out)
	}
	if !strings.Contains(out, "PROJ-124 ships dormant with its parent PROJ-123 (test-stand): left in In Progress, not moved") {
		t.Fatalf("missing dormant note line:\n%s", out)
	}
}

func TestDeployCheckShowsDormantLineAndStatusAdvice(t *testing.T) {
	configure := func(state string, q func(string) string) string {
		cfg := deployConfig(state, q)
		return strings.Replace(cfg, `approved_release: {prefix: "Product v", ignore_paths: ["docs/**", "*.md"]}`, `release_notes: {transitions: [{from: [In Progress], to: Testing}]}
      approved_release: {statuses: [Ready for release, Done], prefix: "Product v", dormant: [{key: PROJ-123, reason: ready-fixture}, {key: PROJ-124, reason: done-fixture}, {key: PROJ-125, reason: not-in-release}], ignore_paths: ["docs/**", "*.md"]}`, 1)
	}
	d := newDeployFixture(t, configure)
	d.mustGraft("deploy", "--unapproved", "live")
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-123": {"10030"}, "PROJ-124": {"10030"}})
	d.jira.setApprovers(map[string][]string{"10030": {"APPROVED", "APPROVED"}})
	d.jira.mu.Lock()
	maps.Copy(d.jira.statuses, map[string]string{"PROJ-123": "Ready for release", "PROJ-124": "Done"})
	d.jira.mu.Unlock()
	d.write("dormant-summary.txt", "fixture\n")
	d.mustFixtureCommit("feat: PROJ-123 PROJ-124 dormant work")
	out, code := d.graft("", "deploy", "check", "live")
	if code != 0 {
		t.Fatalf("deploy check refused dormant items: code=%d\n%s", code, out)
	}
	for _, want := range []string{
		"ships dormant: PROJ-123 — ready-fixture", "ships dormant: PROJ-124 — done-fixture",
		"PROJ-124 ships dormant (done-fixture): left in Done, not moved",
		"PROJ-123 is Ready for release: drop it from approved_release.dormant to let the deploy close it",
		"PROJ-124 is Done: drop it from approved_release.dormant",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "not-in-release") {
		t.Errorf("reported dormant item outside this release:\n%s", out)
	}
	if strings.Contains(out, "would refuse") {
		t.Errorf("status advice became a refusal:\n%s", out)
	}
	if strings.Contains(out, "note only") {
		t.Errorf("closed dormant task was previewed as note-only:\n%s", out)
	}
}
