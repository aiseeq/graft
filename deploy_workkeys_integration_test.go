package main

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// workKeysRepo is a deploy fixture whose project counts commits by their work.
func workKeysRepo(t *testing.T) *deployFixture {
	t.Helper()
	d := deployRepo(t)
	d.write(".graft.yaml", strings.Replace(d.read(".graft.yaml"), "    skip_keys: [PROJ-40]\n", `    skip_keys: [PROJ-40]
    work_keys: true
    project: Widget
    comment: "{project}: deployed to {target}"
`, 1))
	d.mustFixtureCommit("docs: PROJ-2 the release notes count work")
	return d
}

func (d *deployFixture) commits(msgs ...string) {
	d.t.Helper()
	for _, msg := range msgs {
		d.write("c"+strconv.Itoa(len(d.git("rev-list", "--all")))+".txt", msg+"\n")
		d.mustFixtureCommit(msg)
	}
}

func (d *deployFixture) statuses(s map[string]string, parents map[string]string) {
	d.jira.mu.Lock()
	defer d.jira.mu.Unlock()
	maps.Copy(d.jira.statuses, s)
	d.jira.parents = parents
}

func wantLines(t *testing.T, out string, wants, unwanted []string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}
	for _, not := range unwanted {
		if strings.Contains(out, not) {
			t.Errorf("%q is listed:\n%s", not, out)
		}
	}
}

// The gate counts the work of a commit, all of it, and a key it only mentions
// lets nothing out.
func TestDeployWorkKeysGate(t *testing.T) {
	d := workKeysRepo(t)
	d.statuses(map[string]string{"PROJ-8": "Ready for release"}, nil)
	d.mustGraft("deploy", "--unapproved", "gate")
	d.commits(
		"chore: mentions only\n\nAfter PROJ-8.",
		"feat: PROJ-8 PROJ-5 PROJ-2 three at once",
		"feat: PROJ-8 first; PROJ-1 second",
		"fix: PROJ-8 own work\n\nAfter PROJ-5.",
		"fix: PROJ-8 own work after PROJ-5",
		"fix: PROJ-5 unchecked work\n\nAfter PROJ-8.",
	)
	out, code := d.graft("", "deploy", "gate")
	if code == 0 {
		t.Fatalf("exit 0:\n%s", out)
	}
	wantLines(t, out, []string{
		"chore: mentions only: names no item",
		"feat: PROJ-8 PROJ-5 PROJ-2 three at once: PROJ-5 is Testing, not Ready for release or Done",
		"feat: PROJ-8 first; PROJ-1 second: PROJ-1 is In Progress",
		"fix: PROJ-5 unchecked work: PROJ-5 is Testing",
	}, []string{"own work:", "PROJ-8 Ready"})
}

// The same commits pass by any key they name without work_keys: other projects
// stay as they are until they turn it on.
func TestDeployWithoutWorkKeysCountsMentions(t *testing.T) {
	d := deployRepo(t)
	d.statuses(map[string]string{"PROJ-8": "Ready for release", "PROJ-9": "In Progress"}, map[string]string{"PROJ-9": "PROJ-8"})
	d.mustGraft("deploy", "--unapproved", "gate")
	d.commits(
		"chore: mentions only\n\nAfter PROJ-8.",
		"feat: PROJ-8 PROJ-5 two at once",
		"fix: PROJ-9 subtask of a parent with open subtasks",
	)
	if out, code := d.graft("", "deploy", "gate"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// An attribution ties a commit that only mentions the item to it, and passes
// in spite of the neighbour its subject names.
func TestDeployWorkKeysAttribute(t *testing.T) {
	d := workKeysRepo(t)
	d.statuses(map[string]string{"PROJ-8": "Ready for release"}, nil)
	d.mustGraft("deploy", "--unapproved", "gate")
	d.commits("chore: the work of PROJ-8 by mention only\n\nAfter PROJ-8.", "feat: PROJ-5 is not the work here")
	named := d.git("rev-parse", "HEAD")
	d.write(".graft.yaml", strings.Replace(d.read(".graft.yaml"), "approved_release: {statuses: [Ready for release, Done]}",
		"approved_release: {statuses: [Ready for release, Done], attribute: [{commit: "+named+", item: PROJ-8, reason: the key is wrong}]}", 1))
	d.mustFixtureCommit("docs: PROJ-2 attribute")
	out := d.mustGraft("deploy", "gate")
	wantLines(t, out, []string{"attributed to PROJ-8 instead of PROJ-5: the key is wrong"}, nil)
}

// The note goes to the work of the deploy, a done item too, whose status and
// versions stay; a mention gets none, and {project} names the product.
func TestDeployWorkKeysNotes(t *testing.T) {
	d := workKeysRepo(t)
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-2": {"10030"}})
	d.mustGraft("deploy", "--unapproved", "uat")
	d.commits(
		"fix: PROJ-2 the part of a done item\n\nAfter PROJ-3.",
		"feat: PROJ-1 in progress",
	)
	d.mustGraft("deploy", "uat")
	comments, _ := d.jira.snapshot()
	for _, key := range []string{"PROJ-1", "PROJ-2"} {
		if !slices.Contains(comments[key], "Widget: deployed to uat") {
			t.Errorf("%s has no note: %v", key, comments)
		}
	}
	if len(comments["PROJ-3"]) > 0 {
		t.Errorf("a mention got a note: %v", comments["PROJ-3"])
	}
	d.jira.mu.Lock()
	defer d.jira.mu.Unlock()
	if d.jira.statuses["PROJ-2"] != "Done" || d.jira.statuses["PROJ-1"] != "Testing" {
		t.Errorf("statuses: %v", d.jira.statuses)
	}
	if !slices.Equal(d.jira.fixVersions["PROJ-2"], []string{"10030"}) {
		t.Errorf("the versions of the done item changed: %v", d.jira.fixVersions["PROJ-2"])
	}
	if len(d.jira.fixVersions["PROJ-1"]) == 0 {
		t.Errorf("the item in work got no version: %v", d.jira.fixVersions)
	}
}

// Without work_keys a done item stays without a note, as it was.
func TestDeployWithoutWorkKeysDoneGetsNoNote(t *testing.T) {
	d := deployRepo(t)
	d.mustGraft("deploy", "--unapproved", "uat")
	d.commits("fix: PROJ-2 the part of a done item")
	out := d.mustGraft("deploy", "uat")
	wantLines(t, out, []string{"release note for PROJ-2 not posted, PROJ-2 left alone: it is Done"}, nil)
	if comments, _ := d.jira.snapshot(); len(comments["PROJ-2"]) > 0 {
		t.Errorf("a note on the done item: %v", comments["PROJ-2"])
	}
}

// {project} without the name is a config error, in the target's comment too.
func TestDeployProjectPlaceholderNeedsName(t *testing.T) {
	d := deployRepo(t)
	for _, replaced := range []string{
		strings.Replace(d.read(".graft.yaml"), "    skip_keys: [PROJ-40]\n", "    skip_keys: [PROJ-40]\n    comment: \"{project}\"\n", 1),
		strings.Replace(d.read(".graft.yaml"), "release_notes: {transitions: [{from: [In Progress], to: Testing}], fix_version", "release_notes: {comment: \"{project}\", transitions: [{from: [In Progress], to: Testing}], fix_version", 1),
	} {
		d.write(".graft.yaml", replaced)
		if out, code := d.graft("", "deploy", "check", "uat"); code == 0 || !strings.Contains(out, "{project} needs release_notes.jira.project") {
			t.Errorf("exit %d:\n%s", code, out)
		}
	}
}

// A parent ready with open subtasks keeps itself and them in; a subtask ready
// itself goes, a closed one does not count, and a standing item is left alone.
func TestDeployWorkKeysOpenParent(t *testing.T) {
	d := workKeysRepo(t)
	d.statuses(map[string]string{
		"PROJ-8": "Ready for release", "PROJ-9": "In Progress", "PROJ-10": "Ready for release",
		"PROJ-11": "Ready for release", "PROJ-12": "Ready for release", "PROJ-13": "Won't do",
		"PROJ-40": "Ready for release", "PROJ-41": "In Progress",
		"PROJ-43": "Done", "PROJ-44": "In Progress",
		"PROJ-46": "In Progress", "PROJ-47": "Ready for release",
	}, map[string]string{
		"PROJ-9": "PROJ-8", "PROJ-10": "PROJ-8", "PROJ-12": "PROJ-11", "PROJ-13": "PROJ-11",
		"PROJ-41": "PROJ-40", "PROJ-44": "PROJ-43", "PROJ-47": "PROJ-46",
	})
	d.mustGraft("deploy", "--unapproved", "gate")
	d.commits(
		"feat: PROJ-8 the parent",
		"feat: PROJ-9 the open subtask",
		"feat: PROJ-10 the subtask that is ready",
		"feat: PROJ-11 the parent of closed subtasks",
		"feat: PROJ-12 ready under it",
		"feat: PROJ-41 under a standing item",
		"feat: PROJ-44 open under a done parent",
		"feat: PROJ-47 ready under a parent in work",
	)
	out, code := d.graft("", "deploy", "gate")
	if code == 0 {
		t.Fatalf("exit 0:\n%s", out)
	}
	wantLines(t, out, []string{
		"feat: PROJ-8 the parent: PROJ-8 is Ready for release with open subtasks PROJ-9 (In Progress)",
		"feat: PROJ-9 the open subtask: PROJ-8 is Ready for release with open subtasks PROJ-9 (In Progress)",
		"feat: PROJ-44 open under a done parent: PROJ-43 is Done with open subtasks PROJ-44 (In Progress)",
	}, []string{"PROJ-10 the subtask that is ready:", "PROJ-11 the parent", "PROJ-12 ready under it:", "PROJ-41 under a standing item:", "PROJ-47 ready under"})

	d.statuses(map[string]string{"PROJ-9": "Ready for release", "PROJ-44": "Done"}, map[string]string{
		"PROJ-9": "PROJ-8", "PROJ-10": "PROJ-8", "PROJ-12": "PROJ-11", "PROJ-13": "PROJ-11",
		"PROJ-41": "PROJ-40", "PROJ-44": "PROJ-43", "PROJ-47": "PROJ-46",
	})
	d.mustGraft("deploy", "gate")
}

// A hotfix goes out by its work: a mention is no way in, and every item of its
// work has the label.
func TestReleaseHotfixWorkKeys(t *testing.T) {
	d := releaseRepo(t)
	prod := strings.TrimSpace(d.stateFile("prod-sha"))
	d.at("2026-10-21 18:30")
	d.git("switch", "-q", "-c", "hotfix/PROJ-9", prod)
	d.write(".graft.yaml", strings.Replace(d.read(".graft.yaml"), "project_keys: [PROJ]\n", "project_keys: [PROJ]\n    work_keys: true\n", 1))
	d.commitFile("fix.txt", "fix: PROJ-9 urgent\n\nAfter PROJ-8.")
	d.jira.mu.Lock()
	d.jira.labels = map[string][]string{"PROJ-9": {"hotfix"}}
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "test")

	d.commitFile("fix2.txt", "fix: PROJ-9 PROJ-8 the other half")
	if out, code := d.graft("", "deploy", "test"); code == 0 || !strings.Contains(out, "PROJ-8, PROJ-9 without the label hotfix") {
		t.Errorf("one item of two labelled: exit %d\n%s", code, out)
	}
	d.jira.mu.Lock()
	d.jira.labels["PROJ-8"] = []string{"hotfix"}
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "test")
}

// In a freeze every item of a commit's work is in the release, not one.
func TestFreezeWorkKeysAllItemsInTheRelease(t *testing.T) {
	d := freezeRepoWith(t, func(state string, q func(string) string) string {
		return strings.Replace(freezeConfig(state, q), "project_keys: [PROJ]\n", "project_keys: [PROJ]\n    work_keys: true\n", 1)
	})
	d.commitFile("one.txt", "feat: PROJ-1 accepted work")
	d.freeze()
	d.commitFile("two.txt", "fix: PROJ-1 PROJ-30 a neighbour outside")
	if out, code := d.graft("", "deploy", "acc"); code == 0 || !strings.Contains(out, "PROJ-30 not in Product v1.2.10") {
		t.Errorf("exit %d\n%s", code, out)
	}
}
