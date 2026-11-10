package main

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// workKeysRepo is a deploy fixture for commit work attribution.
func workKeysRepo(t *testing.T) *deployFixture {
	t.Helper()
	d := deployRepo(t)
	d.write(".graft.yaml", strings.Replace(d.read(".graft.yaml"), "    skip_keys: [PROJ-40]\n", `    skip_keys: [PROJ-40]
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
		return freezeConfig(state, q)
	})
	d.commitFile("one.txt", "feat: PROJ-1 accepted work")
	d.freeze()
	d.commitFile("two.txt", "fix: PROJ-1 PROJ-30 a neighbour outside")
	if out, code := d.graft("", "deploy", "acc"); code == 0 || !strings.Contains(out, "PROJ-30 not in Product v1.2.10") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// A sentence of its own is work: its key goes through the gate.
func TestDeployWorkKeysSentence(t *testing.T) {
	d := workKeysRepo(t)
	d.statuses(map[string]string{"PROJ-8": "Ready for release"}, nil)
	d.mustGraft("deploy", "--unapproved", "gate")
	d.commits("fix: PROJ-8 one part. PROJ-5 the other part")
	out, code := d.graft("", "deploy", "gate")
	if code == 0 {
		t.Fatalf("exit 0:\n%s", out)
	}
	wantLines(t, out, []string{"PROJ-5 is Testing"}, nil)
}

// Skipping the done status would take the note from a done item whose work
// ships: a config error, not a silence.
func TestDeployWorkKeysSkipStatusesNamesDone(t *testing.T) {
	d := workKeysRepo(t)
	d.write(".graft.yaml", strings.NewReplacer(
		"skip_statuses: [Blocked]", `skip_statuses: [Done, "Won't do"]`,
		"subtasks: true}", "subtasks: true, sweep: {}}",
	).Replace(d.read(".graft.yaml")))
	out, code := d.graft("", "deploy", "check", "sub")
	if code == 0 || !strings.Contains(out, "skip_statuses names Done") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// A done parent only learns what shipped: its subtasks have their own commits
// to go by.
func TestDeployDoneParentSubtasks(t *testing.T) {
	d := workKeysRepo(t)
	d.statuses(map[string]string{"PROJ-70": "Ready for release"}, map[string]string{"PROJ-70": "PROJ-2"})
	d.mustGraft("deploy", "--unapproved", "sub")
	d.commits("fix: PROJ-2 the work of a done parent")
	d.mustGraft("deploy", "sub")
	d.jira.mu.Lock()
	defer d.jira.mu.Unlock()
	if got := d.jira.statuses["PROJ-70"]; got != "Ready for release" {
		t.Errorf("the subtask is %s, want Ready for release", got)
	}
}

// A target that goes by the version alone takes nothing from a done parent, so
// a done parent keeps nothing out either.
func TestDeployWorkKeysDoneParentOnVersionTarget(t *testing.T) {
	d := workKeysRepo(t)
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-44": {"10010"}})
	d.jira.setApprovers(map[string][]string{"10010": {"APPROVED"}})
	d.statuses(map[string]string{"PROJ-43": "Done", "PROJ-44": "In Progress"}, map[string]string{"PROJ-44": "PROJ-43"})
	d.mustGraft("deploy", "--unapproved", "live")
	d.commits("feat: PROJ-44 in the approved version")
	d.mustGraft("deploy", "live")

	// By the version of the done parent alone it would go out unfinished.
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-43": {"10010"}})
	d.commits("feat: PROJ-44 only the parent is in the approved version")
	out, code := d.graft("", "deploy", "live")
	if code == 0 {
		t.Fatalf("exit 0:\n%s", out)
	}
	wantLines(t, out, []string{"PROJ-43 is Done with open subtasks PROJ-44 (In Progress)"}, nil)
}

// The refusal of an open parent says what gets the commit out.
func TestDeployWorkKeysOpenParentWords(t *testing.T) {
	d := workKeysRepo(t)
	d.statuses(map[string]string{"PROJ-8": "Ready for release", "PROJ-9": "In Progress"}, map[string]string{"PROJ-9": "PROJ-8"})
	d.mustGraft("deploy", "--unapproved", "gate")
	d.commits("feat: PROJ-8 the parent")
	out, code := d.graft("", "deploy", "gate")
	if code == 0 {
		t.Fatalf("exit 0:\n%s", out)
	}
	wantLines(t, out, []string{"finish them or name PROJ-8 in release_notes.jira.skip_keys", "finish any subtasks called open"}, []string{"take PROJ-8 out of"})
}

// A subtask in the release by its parent is not named as outside it.
func TestFreezeWorkKeysNamesTheItemOutside(t *testing.T) {
	d := freezeRepoWith(t, func(state string, q func(string) string) string {
		return freezeConfig(state, q)
	})
	d.jira.mu.Lock()
	d.jira.parents = map[string]string{"PROJ-31": "PROJ-1"}
	d.jira.mu.Unlock()
	d.commitFile("one.txt", "feat: PROJ-1 accepted work")
	d.freeze()
	d.commitFile("two.txt", "fix: PROJ-30 PROJ-31 a neighbour and a subtask")
	out, code := d.graft("", "deploy", "acc")
	if code == 0 {
		t.Fatalf("exit 0:\n%s", out)
	}
	wantLines(t, out, []string{"PROJ-30 not in Product v1.2.10"}, []string{"PROJ-31 not in"})
}

// A subtask moves but is not handed over, named in a commit or
// following its parent, and needs no comment for it; the parent is handed over
// as before.
func TestDeploySubtaskKeepsAssignee(t *testing.T) {
	d := workKeysRepo(t)
	d.jira.mu.Lock()
	maps.Copy(d.jira.statuses, map[string]string{"PROJ-70": "In Progress", "PROJ-71": "In Progress", "PROJ-72": "In Progress", "PROJ-73": "In Progress"})
	d.jira.parents = map[string]string{"PROJ-71": "PROJ-70", "PROJ-73": "PROJ-72"}
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "--unapproved", "sub")
	d.commits("feat: PROJ-70 the item", "feat: PROJ-73 a subtask named, its parent in work")
	out := d.mustGraft("deploy", "sub")
	d.jira.mu.Lock()
	defer d.jira.mu.Unlock()
	assigned := slices.Sorted(slices.Values(d.jira.assigned))
	want := []string{"PROJ-70 to qa"}
	if !slices.Equal(assigned, want) {
		t.Errorf("assigned = %v, want %v\n%s", assigned, want, out)
	}
	for key, want := range map[string]string{"PROJ-70": "Testing", "PROJ-71": "Testing", "PROJ-73": "Testing", "PROJ-72": "In Progress"} {
		if got := d.jira.statuses[key]; got != want {
			t.Errorf("%s is %s, want %s\n%s", key, got, want, out)
		}
	}
}

// require_comment holds the handover, and a subtask is handed over no more.
func TestDeploySubtaskNeedsNoComment(t *testing.T) {
	d := workKeysRepo(t)
	d.write(".graft.yaml", strings.Replace(d.read(".graft.yaml"), "assign: qa,", `assign: qa, require_comment: "How to check",`, 1))
	d.mustFixtureCommit("docs: PROJ-2 ask for the comment")
	d.jira.mu.Lock()
	maps.Copy(d.jira.statuses, map[string]string{"PROJ-72": "In Progress", "PROJ-73": "In Progress"})
	d.jira.parents = map[string]string{"PROJ-73": "PROJ-72"}
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "--unapproved", "sub")
	d.commits("feat: PROJ-72 the item without a comment", "feat: PROJ-73 its subtask")
	out := d.mustGraft("deploy", "sub")
	wantLines(t, out, []string{"PROJ-72: NOT MOVED to Testing", "PROJ-73 moved to Testing", "PROJ-73 stays with its assignee: a subtask of PROJ-72"}, nil)
}

// a day is how long ago, in days, a move happened in a history.
func daysAgo(n int) time.Time { return time.Now().Add(-time.Duration(n) * 24 * time.Hour) }

// into is a history that shows the item come into the status n days ago.
func into(status string, n int) []fakeStatusMove {
	return []fakeStatusMove{{at: daysAgo(n), from: "In Progress", to: status}}
}

// Accepting an item takes along the subtasks that waited for the reviewer when
// it was accepted; work unfinished, or come to the reviewer's status later,
// keeps it out.
func TestDeployWorkKeysSubtaskAcceptedWithParent(t *testing.T) {
	d := workKeysRepo(t)
	d.statuses(map[string]string{
		"PROJ-8": "Ready for release", "PROJ-9": "Testing",
		"PROJ-12": "Ready for release", "PROJ-13": "Testing",
		"PROJ-14": "Ready for release", "PROJ-15": "In Progress",
		"PROJ-16": "Ready for release", "PROJ-17": "Testing",
		"PROJ-18": "In Progress", "PROJ-19": "Testing",
	}, map[string]string{"PROJ-9": "PROJ-8", "PROJ-13": "PROJ-12", "PROJ-15": "PROJ-14", "PROJ-17": "PROJ-16", "PROJ-19": "PROJ-18"})
	d.jira.mu.Lock()
	d.jira.statusMoves = map[string][]fakeStatusMove{
		"PROJ-8": into("Ready for release", 2), "PROJ-9": into("Testing", 5),
		"PROJ-12": into("Ready for release", 5), "PROJ-13": into("Testing", 2),
		"PROJ-14": into("Ready for release", 2),
		"PROJ-19": into("Testing", 5),
	}
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "--unapproved", "gate")
	d.commits(
		"feat: PROJ-9 accepted with its parent",
		"feat: PROJ-13 came to testing after the parent was accepted",
		"feat: PROJ-15 unfinished",
		"feat: PROJ-17 its history does not tell",
		"feat: PROJ-19 under a parent in work",
	)
	out, code := d.graft("", "deploy", "gate")
	if code == 0 {
		t.Fatalf("exit 0:\n%s", out)
	}
	wantLines(t, out, []string{
		"PROJ-12 is Ready for release with open subtasks PROJ-13 (Testing, not accepted with PROJ-12)",
		"PROJ-14 is Ready for release with open subtasks PROJ-15 (In Progress)",
		"PROJ-16 is Ready for release with open subtasks PROJ-17 (Testing, not accepted with PROJ-16)",
		"feat: PROJ-19 under a parent in work: PROJ-19 is Testing",
	}, []string{"PROJ-9 accepted with its parent:"})
}

// The sweep takes a subtask accepted with its parent through ready to done,
// with or without commits of its own, and the parent closes in the same pass.
func TestDeploySweepSubtaskAcceptedWithParent(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return sweepConfig(state, q)
	})
	trunk := d.git("branch", "--show-current")
	d.git("remote", "set-head", "origin", trunk)
	d.statuses(map[string]string{
		"PROJ-80": "Ready for release", "PROJ-81": "Testing", "PROJ-82": "Testing",
		"PROJ-85": "Ready for release", "PROJ-86": "Testing",
		"PROJ-88": "In Progress", "PROJ-89": "Testing",
	}, map[string]string{"PROJ-81": "PROJ-80", "PROJ-82": "PROJ-80", "PROJ-86": "PROJ-85", "PROJ-89": "PROJ-88"})
	d.jira.mu.Lock()
	d.jira.statusMoves = map[string][]fakeStatusMove{
		"PROJ-80": into("Ready for release", 2), "PROJ-81": into("Testing", 5), "PROJ-82": into("Testing", 5),
		"PROJ-85": into("Ready for release", 5), "PROJ-86": into("Testing", 2),
		"PROJ-89": into("Testing", 5),
	}
	d.jira.mu.Unlock()
	for i, msg := range []string{"feat: PROJ-81 a", "feat: PROJ-86 b", "feat: PROJ-89 c"} {
		d.write("s"+strconv.Itoa(i)+".txt", "x\n")
		d.mustPlainCommit(msg)
	}
	d.git("push", "-q", "origin", trunk)
	d.mustGraft("deploy", "swept")
	out := d.mustGraft("deploy", "sweep", "swept")
	wantLines(t, out, []string{"PROJ-81 would move to Done: it was accepted with PROJ-80", "PROJ-80 would move to Done: its subtasks are all done (2)"}, []string{"PROJ-86 would", "PROJ-89 would", "PROJ-85 would"})
	d.jira.mu.Lock()
	if d.jira.statuses["PROJ-81"] != "Testing" {
		t.Errorf("moved without apply: %v", d.jira.statuses["PROJ-81"])
	}
	d.jira.mu.Unlock()

	out = d.mustGraft("deploy", "sweep", "swept", "--apply")
	_, moved := d.jira.snapshot()
	d.jira.mu.Lock()
	defer d.jira.mu.Unlock()
	for key, want := range map[string]string{
		"PROJ-80": "Done", "PROJ-81": "Done", "PROJ-82": "Done",
		"PROJ-85": "Ready for release", "PROJ-86": "Testing", "PROJ-88": "In Progress", "PROJ-89": "Testing",
	} {
		if got := d.jira.statuses[key]; got != want {
			t.Errorf("%s is %s, want %s\n%s", key, got, want, out)
		}
	}
	if steps := slices.DeleteFunc(slices.Clone(moved), func(k string) bool { return k != "PROJ-81" }); len(steps) != 2 {
		t.Errorf("PROJ-81 moved %d times, want through ready to done: %v", len(steps), moved)
	}
}

// sweepRepo is a repo with the sweep target.
func sweepRepo(t *testing.T) *deployFixture {
	t.Helper()
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return sweepConfig(state, q)
	})
	d.git("remote", "set-head", "origin", d.git("branch", "--show-current"))
	return d
}

// pushCommits commits one file per message and pushes the branch.
func (d *deployFixture) pushCommits(msgs ...string) {
	d.t.Helper()
	for _, msg := range msgs {
		d.write("c"+d.git("rev-list", "--count", "HEAD")+".txt", "x\n")
		d.mustPlainCommit(msg)
	}
	d.git("push", "-q", "origin", d.git("branch", "--show-current"))
}

// A parent stays open while the work of a done subtask is off the target.
func TestDeploySweepParentWaitsForSubtaskWork(t *testing.T) {
	d := sweepRepo(t)
	d.statuses(map[string]string{"PROJ-20": "In Progress", "PROJ-21": "Done", "PROJ-22": "Done"},
		map[string]string{"PROJ-21": "PROJ-20", "PROJ-22": "PROJ-20"})
	d.pushCommits("feat: PROJ-21 a")
	d.mustGraft("deploy", "swept")
	d.git("checkout", "-q", "-b", "side")
	d.pushCommits("feat: PROJ-22 on a branch")
	d.git("checkout", "-q", "-")

	out := d.mustGraft("deploy", "sweep", "swept", "--apply")
	d.jira.mu.Lock()
	defer d.jira.mu.Unlock()
	if got := d.jira.statuses["PROJ-20"]; got != "In Progress" {
		t.Errorf("PROJ-20 is %s with the work of PROJ-22 off the target\n%s", got, out)
	}
}

// A done parent still takes along a subtask that waited for the reviewer when
// it was accepted.
func TestDeploySweepDoneParentTakesAcceptedSubtask(t *testing.T) {
	d := sweepRepo(t)
	d.statuses(map[string]string{"PROJ-80": "Done", "PROJ-81": "Testing"}, map[string]string{"PROJ-81": "PROJ-80"})
	d.jira.mu.Lock()
	d.jira.statusMoves = map[string][]fakeStatusMove{"PROJ-80": into("Done", 2), "PROJ-81": into("Testing", 5)}
	d.jira.mu.Unlock()
	d.pushCommits("feat: PROJ-81 a")
	d.mustGraft("deploy", "swept")

	out := d.mustGraft("deploy", "sweep", "swept", "--apply")
	d.jira.mu.Lock()
	defer d.jira.mu.Unlock()
	if got := d.jira.statuses["PROJ-81"]; got != "Done" {
		t.Errorf("PROJ-81 is %s\n%s", got, out)
	}
}

// A parent put aside or not started that has a commit of its own is told about
// once: judged by its subtasks, it is not judged again by the commit.
func TestDeploySweepParentAsideListedOnce(t *testing.T) {
	for _, status := range []string{"Paused", "To Do"} {
		t.Run(status, func(t *testing.T) {
			d := sweepRepo(t)
			d.statuses(map[string]string{"PROJ-60": status, "PROJ-61": "Done"}, map[string]string{"PROJ-61": "PROJ-60"})
			d.pushCommits("feat: PROJ-61 k", "feat: PROJ-60 own")
			d.mustGraft("deploy", "swept")

			out := d.mustGraft("deploy", "sweep", "swept", "--apply")
			wantLines(t, out, []string{"PROJ-60 is " + status + ", but its subtasks are all done (1)"}, nil)
			if n := strings.Count(out, "PROJ-60 is "); n != 1 {
				t.Errorf("PROJ-60 told about %d times\n%s", n, out)
			}
			d.jira.mu.Lock()
			defer d.jira.mu.Unlock()
			if got := d.jira.statuses["PROJ-60"]; got != status {
				t.Errorf("PROJ-60 moved to %s\n%s", got, out)
			}
		})
	}
}

// A done item gets no version, so failing to tell whose it is leaves none
// unset: the note is what is lost.
func TestDeployWorkKeysDoneItemUnreadHistory(t *testing.T) {
	d := workKeysRepo(t)
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-2": {"10030"}})
	d.mustGraft("deploy", "--unapproved", "uat")
	d.jira.mu.Lock()
	d.jira.assignees = map[string]string{"PROJ-2": "colleague"}
	d.jira.changelogDown = true
	d.jira.mu.Unlock()
	d.commits("fix: PROJ-2 the part of a done item")
	out, _ := d.graft("", "deploy", "uat")
	wantLines(t, out, []string{"release note for PROJ-2 not posted"}, []string{"PROJ-2: fix version NOT SET"})
}

// Nobody reviews a standing item, so its subtask still goes to the reviewer.
func TestDeploySubtaskOfStandingItemHandedOver(t *testing.T) {
	d := workKeysRepo(t)
	d.jira.mu.Lock()
	maps.Copy(d.jira.statuses, map[string]string{"PROJ-40": "In Progress", "PROJ-41": "In Progress"})
	d.jira.parents = map[string]string{"PROJ-41": "PROJ-40"}
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "--unapproved", "sub")
	d.commits("feat: PROJ-41 a subtask of the standing item")
	out := d.mustGraft("deploy", "sub")
	d.jira.mu.Lock()
	defer d.jira.mu.Unlock()
	if !slices.Equal(d.jira.assigned, []string{"PROJ-41 to qa"}) {
		t.Errorf("assigned = %v\n%s", d.jira.assigned, out)
	}
}

// The sweep takes an accepted subtask through the review status before done.
func TestDeploySweepAcceptedSubtaskTakesReviewStep(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return sweepConfig(state, q)
	})
	trunk := d.git("branch", "--show-current")
	d.git("remote", "set-head", "origin", trunk)
	d.statuses(map[string]string{"PROJ-90": "Ready for release", "PROJ-91": "Testing"}, map[string]string{"PROJ-91": "PROJ-90"})
	d.jira.mu.Lock()
	d.jira.statusMoves = map[string][]fakeStatusMove{"PROJ-90": into("Ready for release", 2), "PROJ-91": into("Testing", 5)}
	d.jira.mu.Unlock()
	for i, msg := range []string{"feat: PROJ-90 parent", "feat: PROJ-91 a"} {
		d.write("s"+strconv.Itoa(i)+".txt", "x\n")
		d.mustPlainCommit(msg)
	}
	d.git("push", "-q", "origin", trunk)
	d.mustGraft("deploy", "swept")
	out := d.mustGraft("deploy", "sweep", "swept", "--apply")
	_, moved := d.jira.snapshot()
	if steps := slices.DeleteFunc(slices.Clone(moved), func(k string) bool { return k != "PROJ-91" }); len(steps) != 2 {
		t.Errorf("PROJ-91 moved %d times, want through review to done: %v\n%s", len(steps), moved, out)
	}
}
