package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/lock"
)

// parentTransitionsConfig adds the target up, before the last: its transition
// hands an item over to qa in Testing, and its sweep, done at Testing, moves
// a parent whose work is all in its subtasks by that transition. upw is the
// same with approved_release, whose withheld commits the target does not run.
func parentTransitionsConfig(transition string) func(state string, q func(string) string) string {
	return func(state string, q func(string) string) string {
		return strings.Replace(deployConfig(state, q), "    gate:\n", `    up:
      env: local
      run: [sh, deploy.sh]
      deployed_sha: {path: `+q("up-sha")+`}
      release_notes: {transitions: [`+transition+`], sweep: {done: Testing, apply: true}}
    upw:
      env: local
      run: [sh, deploy.sh]
      deployed_sha: {path: `+q("upw-sha")+`}
      approved_release: {statuses: [Ready for release, Done]}
      release_notes: {transitions: [`+transition+`], sweep: {done: Testing, apply: true}}
    gate:
`, 1)
	}
}

const upTransition = `{from: [In Progress], to: Testing, assign: qa}`

// parentRepo is a repository with the target up, the parent PROJ-20 In
// Progress and its subtasks PROJ-21 and PROJ-22 in the statuses given.
func parentRepo(t *testing.T, transition string, sub21, sub22 string) *deployFixture {
	t.Helper()
	d := newDeployFixture(t, parentTransitionsConfig(transition))
	d.env = append(d.env, "XDG_CACHE_HOME="+t.TempDir())
	d.git("remote", "set-head", "origin", d.git("branch", "--show-current"))
	d.statuses(map[string]string{"PROJ-20": "In Progress", "PROJ-21": sub21, "PROJ-22": sub22},
		map[string]string{"PROJ-21": "PROJ-20", "PROJ-22": "PROJ-20"})
	return d
}

func (d *deployFixture) statusOf(key string) string {
	d.jira.mu.Lock()
	defer d.jira.mu.Unlock()
	return d.jira.statuses[key]
}

func (d *deployFixture) assignedTo() []string {
	d.jira.mu.Lock()
	defer d.jira.mu.Unlock()
	return slices.Clone(d.jira.assigned)
}

// A parent whose subtasks are all done, their work on the target, goes on
// by the target's transition after the deploy, handed over as a deploy
// hands an item over.
func TestSweepParentMovesByTheTargetTransition(t *testing.T) {
	d := parentRepo(t, upTransition, "Done", "Done")
	d.pushCommits("feat: PROJ-21 a", "feat: PROJ-22 b")
	out := d.mustGraft("deploy", "up")
	if got := d.statusOf("PROJ-20"); got != "Testing" {
		t.Fatalf("PROJ-20 is %s, want Testing\n%s", got, out)
	}
	if !slices.Contains(d.assignedTo(), "PROJ-20 to qa") {
		t.Errorf("PROJ-20 not handed over to qa: %v\n%s", d.assignedTo(), out)
	}
	wantLines(t, out, []string{"sweep up: PROJ-20 moved to Testing: its subtasks are all done (2) and their work is on up"}, nil)
}

// A parent past the target's transition, Ready for release on a target
// before the last, is not taken back to the sweep's done status there.
func TestSweepParentPastTheTargetStays(t *testing.T) {
	d := parentRepo(t, upTransition, "Done", "Done")
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-20"] = "Ready for release"
	d.jira.mu.Unlock()
	d.pushCommits("feat: PROJ-21 a", "feat: PROJ-22 b")
	out := d.mustGraft("deploy", "up")
	if got := d.statusOf("PROJ-20"); got != "Ready for release" {
		t.Fatalf("PROJ-20 is %s, want Ready for release\n%s", got, out)
	}
}

// A subtask not done keeps its parent where it is.
func TestSweepParentStaysForASubtaskNotDone(t *testing.T) {
	d := parentRepo(t, upTransition, "Done", "Testing")
	d.pushCommits("feat: PROJ-21 a", "feat: PROJ-22 b")
	out := d.mustGraft("deploy", "up")
	if got := d.statusOf("PROJ-20"); got != "In Progress" {
		t.Fatalf("PROJ-20 is %s, want In Progress\n%s", got, out)
	}
	wantLines(t, out, []string{"PROJ-20 stays In Progress: its subtask PROJ-22 is Testing"}, nil)
}

// The work of a subtask on a branch the target does not run, or of the
// parent itself, keeps the parent where it is.
func TestSweepParentStaysForWorkOffTheTarget(t *testing.T) {
	for _, tc := range []struct{ name, off, want string }{
		{"subtask", "feat: PROJ-22 on a branch", "PROJ-20 stays In Progress: its subtask PROJ-22 has work not on up"},
		{"parent", "feat: PROJ-20 on a branch", "PROJ-20 stays In Progress: its own work is not on up"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := parentRepo(t, upTransition, "Done", "Done")
			d.pushCommits("feat: PROJ-21 a", "feat: PROJ-22 b")
			trunk := d.git("branch", "--show-current")
			d.git("checkout", "-q", "-b", "side")
			d.write("side.txt", "x\n")
			d.mustPlainCommit(tc.off)
			d.git("push", "-q", "-u", "origin", "side")
			d.git("checkout", "-q", trunk)
			out := d.mustGraft("deploy", "up")
			if got := d.statusOf("PROJ-20"); got != "In Progress" {
				t.Fatalf("PROJ-20 is %s, want In Progress\n%s", got, out)
			}
			wantLines(t, out, []string{tc.want}, nil)
		})
	}
}

// A subtask commit withheld from the target is not on it, though the
// target's history has it: the release took its change back out.
func TestSweepParentStaysForWithheldSubtaskWork(t *testing.T) {
	d := parentRepo(t, upTransition, "Done", "Done")
	d.pushCommits("feat: PROJ-21 a")
	d.write("withheld.txt", "the change\n")
	d.mustPlainCommit("feat: PROJ-22 b")
	withheld := d.git("rev-parse", "HEAD")
	d.git("push", "-q", "origin", d.git("branch", "--show-current"))
	d.git("checkout", "-q", "-b", "release")
	d.write("withheld.txt", "")
	d.mustPlainCommit("chore: hold a change back from the release")
	d.git("push", "-q", "-u", "origin", "release")
	writeFile(t, filepath.Join(d.state, "upw-sha"), d.git("rev-parse", "HEAD")+"\n")
	writeFile(t, filepath.Join(d.state, "upw-sha.withheld"), withheld+"\n")
	out := d.mustGraft("deploy", "sweep", "upw", "--apply")
	if got := d.statusOf("PROJ-20"); got != "In Progress" {
		t.Fatalf("PROJ-20 is %s, want In Progress\n%s", got, out)
	}
	wantLines(t, out, []string{"PROJ-20 stays In Progress: its subtask PROJ-22 has work not on upw: " + withheld[:12]}, nil)
}

// Where the sweep cannot tell whether the work is on the target, the parent
// stays and says so: a subtask without a commit of the window, one whose
// work a revert took out, one with work older than the window.
func TestSweepParentStaysWhereTheWorkIsUnknown(t *testing.T) {
	old := time.Now().Add(-40 * 24 * time.Hour).Format(time.RFC3339)
	for _, tc := range []struct {
		name string
		work func(d *deployFixture)
		want string
	}{
		{"no commit", func(d *deployFixture) { d.pushCommits("feat: PROJ-21 a") },
			"PROJ-20 stays In Progress: its subtask PROJ-22 has no commit of the last 30 days that up or a remote branch has, so whether its work is on up is unknown"},
		{"reverted", func(d *deployFixture) {
			d.pushCommits("feat: PROJ-21 a", "feat: PROJ-22 b")
			d.git("-c", "core.hooksPath=/dev/null", "revert", "--no-edit", "HEAD")
			d.git("push", "-q", "origin", d.git("branch", "--show-current"))
		}, "PROJ-20 stays In Progress: a revert of the last 30 days took work of PROJ-22 out, so what of it up runs is unknown"},
		{"partly reverted", func(d *deployFixture) {
			d.pushCommits("feat: PROJ-21 a", "feat: PROJ-22 b", "feat: PROJ-22 c")
			d.git("-c", "core.hooksPath=/dev/null", "revert", "--no-edit", "HEAD")
			d.git("push", "-q", "origin", d.git("branch", "--show-current"))
		}, "PROJ-20 stays In Progress: a revert of the last 30 days took work of PROJ-22 out, so what of it up runs is unknown"},
		{"parent reverted", func(d *deployFixture) {
			d.pushCommits("feat: PROJ-21 a", "feat: PROJ-22 b", "feat: PROJ-20 p")
			d.git("-c", "core.hooksPath=/dev/null", "revert", "--no-edit", "HEAD")
			d.git("push", "-q", "origin", d.git("branch", "--show-current"))
		}, "PROJ-20 stays In Progress: a revert of the last 30 days took work of PROJ-20 out, so what of it up runs is unknown"},
		{"all reverted", func(d *deployFixture) {
			d.pushCommits("feat: PROJ-21 a", "feat: PROJ-22 b")
			d.git("-c", "core.hooksPath=/dev/null", "revert", "--no-edit", "HEAD")
			d.git("-c", "core.hooksPath=/dev/null", "revert", "--no-edit", "HEAD~2")
			d.git("push", "-q", "origin", d.git("branch", "--show-current"))
		}, "PROJ-20 stays In Progress: a revert of the last 30 days took work of PROJ-21 out, so what of it up runs is unknown"},
		{"revert without a key", func(d *deployFixture) {
			d.pushCommits("feat: PROJ-21 a", "feat: PROJ-22 b")
			sha := d.git("rev-parse", "HEAD")
			d.git("revert", "--no-commit", "HEAD")
			d.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "revert: undo the last change", "-m", "This reverts commit "+sha+".")
			d.git("push", "-q", "origin", d.git("branch", "--show-current"))
		}, "PROJ-20 stays In Progress: a revert of the last 30 days took work of PROJ-22 out, so what of it up runs is unknown"},
		{"local branch", func(d *deployFixture) {
			d.pushCommits("feat: PROJ-21 a", "feat: PROJ-22 b")
			trunk := d.git("branch", "--show-current")
			d.git("checkout", "-q", "-b", "wip")
			d.write("wip.txt", "x\n")
			d.mustPlainCommit("feat: PROJ-22 not pushed")
			d.git("checkout", "-q", trunk)
		}, "PROJ-20 stays In Progress: PROJ-22 has work only on a local branch, so whether up runs it is unknown"},
		{"older", func(d *deployFixture) {
			d.write("old.txt", "x\n")
			d.git("add", "-A")
			if out, code := d.exec(d.work, "", "env", "GIT_COMMITTER_DATE="+old, "GIT_AUTHOR_DATE="+old,
				"git", "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "feat: PROJ-22 long ago"); code != 0 {
				t.Fatalf("old commit: exit %d\n%s", code, out)
			}
			d.pushCommits("feat: PROJ-21 a", "feat: PROJ-22 b")
		}, "PROJ-20 stays In Progress: PROJ-22 has work older than the last 30 days, so whether up runs it is unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := parentRepo(t, upTransition, "Done", "Done")
			tc.work(d)
			out := d.mustGraft("deploy", "up")
			if got := d.statusOf("PROJ-20"); got != "In Progress" {
				t.Fatalf("PROJ-20 is %s, want In Progress\n%s", got, out)
			}
			wantLines(t, out, []string{tc.want}, nil)
		})
	}
}

// require_comment holds the parent as it holds any item, and an item the
// deploy held is not moved past its comment by the sweep. The handover of
// the held parent judges its work again: a subtask reopened keeps it held.
func TestSweepParentHeldForItsCommentIsJudgedAgain(t *testing.T) {
	d := parentRepo(t, `{from: [In Progress], to: Testing, assign: qa, require_comment: "How to check"}`, "Done", "Done")
	d.pushCommits("feat: PROJ-21 a", "feat: PROJ-22 b", "feat: PROJ-1 ordinary")
	out := d.mustGraft("deploy", "up")
	for _, key := range []string{"PROJ-20", "PROJ-1"} {
		if got := d.statusOf(key); got != "In Progress" {
			t.Fatalf("%s is %s without its comment, want In Progress\n%s", key, got, out)
		}
	}
	wantLines(t, out, []string{"graft: PROJ-20: NOT MOVED to Testing", "PROJ-20 stays In Progress though its subtasks are all done (2) and their work is on up: its move to Testing is held"}, nil)
	held, err := filepath.Glob(filepath.Join(d.envValue("XDG_CACHE_HOME"), "graft", "held", "*"))
	if err != nil || len(held) != 1 {
		t.Fatalf("held records = %v, %v", held, err)
	}
	if data, err := os.ReadFile(held[0]); err != nil || !strings.Contains(string(data), "PROJ-20 +subtasks\n") {
		t.Fatalf("held record = %q, %v; want PROJ-20 marked as a parent", data, err)
	}

	d.jira.mu.Lock()
	d.jira.statuses["PROJ-22"] = "In Progress"
	d.jira.written = map[string][]fakeComment{
		"PROJ-20": {{author: "me", text: "How to check: open it", created: time.Now()}},
		"PROJ-1":  {{author: "me", text: "How to check: run it", created: time.Now()}},
	}
	d.jira.mu.Unlock()
	out, code := d.graft("", "deploy", "handover", "up")
	if code == 0 {
		t.Errorf("handover passed with the parent still held:\n%s", out)
	}
	if got := d.statusOf("PROJ-20"); got != "In Progress" {
		t.Fatalf("PROJ-20 is %s with its subtask reopened, want In Progress\n%s", got, out)
	}
	wantLines(t, out, []string{"PROJ-20 is still held: PROJ-20 stays In Progress: its subtask PROJ-22 is In Progress"}, nil)

	d.jira.mu.Lock()
	d.jira.statuses["PROJ-22"] = "Done"
	d.jira.mu.Unlock()
	out = d.mustGraft("deploy", "handover", "up")
	if got := d.statusOf("PROJ-20"); got != "Testing" {
		t.Fatalf("PROJ-20 is %s after its handover, want Testing\n%s", got, out)
	}
	if !slices.Contains(d.assignedTo(), "PROJ-20 to qa") {
		t.Errorf("PROJ-20 not handed over to qa: %v\n%s", d.assignedTo(), out)
	}
}

// Jira failing to read a subtask does not keep the deploy's release notes
// back: the parent stays, with a warning.
func TestSweepParentSubtaskReadFailureLeavesTheNotes(t *testing.T) {
	d := parentRepo(t, upTransition, "Done", "Done")
	d.pushCommits("feat: PROJ-22 b")
	d.mustGraft("deploy", "up")
	d.jira.mu.Lock()
	d.jira.failIssues = map[string]int{"PROJ-22": 500}
	d.jira.mu.Unlock()
	d.pushCommits("feat: PROJ-21 a", "feat: PROJ-1 ordinary")
	out := d.mustGraft("deploy", "up")
	comments, _ := d.jira.snapshot()
	for _, key := range []string{"PROJ-21", "PROJ-1"} {
		if len(comments[key]) == 0 {
			t.Errorf("no release note on %s:\n%s", key, out)
		}
	}
	if got := d.statusOf("PROJ-1"); got != "Testing" {
		t.Errorf("PROJ-1 is %s, want Testing\n%s", got, out)
	}
	if got := d.statusOf("PROJ-20"); got != "In Progress" {
		t.Fatalf("PROJ-20 is %s, want In Progress\n%s", got, out)
	}
	wantLines(t, out, []string{"graft: warning: sweep up: PROJ-22 not read", "PROJ-20 stays In Progress: its subtask PROJ-22 status is unknown"}, nil)
}

// envValue is the value the fixture gives an environment variable.
func (d *deployFixture) envValue(name string) string {
	for _, kv := range slices.Backward(d.env) {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v
		}
	}
	return ""
}

// With the target running the trunk's own commit, a commit withheld from it
// is still not on it: neither the parent of its subtask nor its item moves.
func TestSweepWithheldWhereTheTargetRunsTheTrunk(t *testing.T) {
	d := parentRepo(t, upTransition, "Done", "Done")
	d.pushCommits("feat: PROJ-21 a")
	d.write("withheld-22.txt", "the change\n")
	d.mustPlainCommit("feat: PROJ-22 b")
	sub := d.git("rev-parse", "HEAD")
	d.write("withheld-1.txt", "the change\n")
	d.mustPlainCommit("feat: PROJ-1 c")
	item := d.git("rev-parse", "HEAD")
	d.write("withheld-22.txt", "")
	d.write("withheld-1.txt", "")
	d.mustPlainCommit("chore: hold changes back from the release")
	d.git("push", "-q", "origin", d.git("branch", "--show-current"))
	writeFile(t, filepath.Join(d.state, "upw-sha"), d.git("rev-parse", "HEAD")+"\n")
	writeFile(t, filepath.Join(d.state, "upw-sha.withheld"), sub+"\n"+item+"\n")
	out := d.mustGraft("deploy", "sweep", "upw", "--apply")
	for _, key := range []string{"PROJ-20", "PROJ-1"} {
		if got := d.statusOf(key); got != "In Progress" {
			t.Errorf("%s is %s, want In Progress\n%s", key, got, out)
		}
	}
	wantLines(t, out, []string{"PROJ-20 stays In Progress: its subtask PROJ-22 has work not on upw: " + sub[:12]}, nil)
}

// A held parent that gets a commit of its own moves at that deploy only as
// the sweep would move it: with a subtask reopened it stays held.
func TestSweepHeldParentWithItsOwnCommitIsJudgedAgain(t *testing.T) {
	d := parentRepo(t, `{from: [In Progress], to: Testing, assign: qa, require_comment: "How to check"}`, "Done", "Done")
	d.pushCommits("feat: PROJ-21 a", "feat: PROJ-22 b")
	out := d.mustGraft("deploy", "up")
	wantLines(t, out, []string{"graft: PROJ-20: NOT MOVED to Testing"}, nil)
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-22"] = "In Progress"
	d.jira.written = map[string][]fakeComment{"PROJ-20": {{author: "me", text: "How to check: open it", created: time.Now()}}}
	d.jira.mu.Unlock()
	d.pushCommits("feat: PROJ-20 a fix of its own")
	out = d.mustGraft("deploy", "up")
	if got := d.statusOf("PROJ-20"); got != "In Progress" {
		t.Fatalf("PROJ-20 is %s with its subtask reopened, want In Progress\n%s", got, out)
	}
	wantLines(t, out, []string{"PROJ-20 is still held: PROJ-20 stays In Progress: its subtask PROJ-22 is In Progress"}, nil)
}

// Where Jira does not say whether the move to done ends the work, the
// parent stays and the sweep says so.
func TestSweepParentStaysWithoutTheCategoryOfDone(t *testing.T) {
	d := sweepRepo(t)
	d.env = append(d.env, "XDG_CACHE_HOME="+t.TempDir())
	d.statuses(map[string]string{"PROJ-20": "In Progress", "PROJ-21": "Done", "PROJ-22": "Done"},
		map[string]string{"PROJ-21": "PROJ-20", "PROJ-22": "PROJ-20"})
	d.jira.mu.Lock()
	d.jira.noTransitionCategory = true
	d.jira.mu.Unlock()
	d.pushCommits("feat: PROJ-21 a", "feat: PROJ-22 b")
	out := d.mustGraft("deploy", "swept")
	if got := d.statusOf("PROJ-20"); got != "In Progress" {
		t.Fatalf("PROJ-20 is %s, want In Progress\n%s", got, out)
	}
	wantLines(t, out, []string{"PROJ-20 stays In Progress though its subtasks are all done (2) and their work is on swept: Jira does not say whether its move to Done is of the done category"}, nil)
}

// The sweep moves an item as a deploy does: one handed over by its
// transition without the comment require_comment asks for stays held until
// graft deploy handover finds it.
func TestSweepItemHeldForItsComment(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return strings.Replace(sweepConfig(state, q), "release_notes: {transitions: [{from: [Ready for release], to: Done}], sweep: {}}",
			`release_notes: {transitions: [{from: [Ready for release], to: Done, assign: qa, require_comment: "How to check"}], sweep: {apply: true}}`, 1)
	})
	d.env = append(d.env, "XDG_CACHE_HOME="+t.TempDir())
	d.git("remote", "set-head", "origin", d.git("branch", "--show-current"))
	d.statuses(map[string]string{"PROJ-10": "Ready for release"}, nil)
	d.pushCommits("feat: PROJ-10 a")
	out := d.mustGraft("deploy", "swept")
	if got := d.statusOf("PROJ-10"); got != "Ready for release" {
		t.Fatalf("PROJ-10 is %s without its comment, want Ready for release\n%s", got, out)
	}
	wantLines(t, out, []string{"graft: PROJ-10: NOT MOVED to Done", "PROJ-10 stays Ready for release though its work of the last 30 days is on swept (commits: 1): its move to Done is held"}, nil)
	d.jira.mu.Lock()
	d.jira.written = map[string][]fakeComment{"PROJ-10": {{author: "me", text: "How to check: open it", created: time.Now()}}}
	d.jira.mu.Unlock()
	out = d.mustGraft("deploy", "handover", "swept")
	if got := d.statusOf("PROJ-10"); got != "Done" {
		t.Fatalf("PROJ-10 is %s after its handover, want Done\n%s", got, out)
	}
	if !slices.Contains(d.assignedTo(), "PROJ-10 to qa") {
		t.Errorf("PROJ-10 not handed over to qa: %v\n%s", d.assignedTo(), out)
	}
}

// Two runs that move a target's items take turns: the second waits for the
// lock of the moves the first holds, then sees what the first moved and
// held, and neither moves an item twice nor drops what the other held.
func TestSweepWaitsForTheMovesOfAnotherRun(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return strings.Replace(sweepConfig(state, q), "release_notes: {transitions: [{from: [Ready for release], to: Done}], sweep: {}}",
			`release_notes: {transitions: [{from: [Ready for release], to: Done, assign: qa, require_comment: "How to check"}], sweep: {}}`, 1)
	})
	cache := t.TempDir()
	d.env = append(d.env, "XDG_CACHE_HOME="+cache)
	d.git("remote", "set-head", "origin", d.git("branch", "--show-current"))
	d.statuses(map[string]string{"PROJ-10": "Ready for release", "PROJ-12": "Ready for release"}, nil)
	d.pushCommits("feat: PROJ-10 a", "feat: PROJ-12 b")
	writeFile(t, filepath.Join(d.state, "swept-sha"), d.git("rev-parse", "HEAD")+"\n")
	common := d.git("rev-parse", "--path-format=absolute", "--git-common-dir")
	// The first run is moving: it holds the target's lock of the moves.
	release := holdMoves(t, d, "swept")
	second, out := d.start(d.work, "deploy", "sweep", "swept", "--apply")
	waitFor(t, func() bool { return strings.Contains(out.String(), "lock moves-swept.lock") })
	// Meanwhile the first moves PROJ-10 and records PROJ-11 held.
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-10"] = "Done"
	d.jira.mu.Unlock()
	sum := sha256.Sum256([]byte(common + "\x00swept"))
	held := filepath.Join(cache, "graft", "held", hex.EncodeToString(sum[:12]))
	writeFile(t, held, "PROJ-11\n")
	release()
	if err := second.Wait(); err != nil {
		t.Fatalf("the second run: %v\n%s", err, out.String())
	}
	if _, moved := d.jira.snapshot(); slices.Contains(moved, "PROJ-10") {
		t.Errorf("PROJ-10 moved again: %v\n%s", moved, out.String())
	}
	data, err := os.ReadFile(held)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(data)); !slices.Equal(got, []string{"PROJ-11", "PROJ-12"}) {
		t.Errorf("held = %q, want PROJ-11 of the first run and PROJ-12 of the second\n%s", got, out.String())
	}
}

// holdMoves takes the target's lock of its moves as another run moving
// there would; release gives it up, once.
func holdMoves(t *testing.T, d *deployFixture, target string) (release func()) {
	t.Helper()
	common := d.git("rev-parse", "--path-format=absolute", "--git-common-dir")
	held, err := lock.Acquire(t.Context(), filepath.Join(common, "graft-locks", "moves-"+target+".lock"), time.Minute, "the first run", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			if err := held.Release(); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(release)
	return release
}

// A sweep that waited for the lock of the moves sweeps what the target runs
// once it holds the lock: the deploy it waited for has put more on it.
func TestSweepReadsTheTargetUnderTheLockOfTheMoves(t *testing.T) {
	d := sweepRepo(t)
	d.env = append(d.env, "XDG_CACHE_HOME="+t.TempDir())
	d.statuses(map[string]string{"PROJ-10": "Ready for release", "PROJ-11": "Ready for release"}, nil)
	d.pushCommits("feat: PROJ-10 a")
	writeFile(t, filepath.Join(d.state, "swept-sha"), d.git("rev-parse", "HEAD")+"\n")
	d.pushCommits("feat: PROJ-11 b")
	release := holdMoves(t, d, "swept")
	second, out := d.start(d.work, "deploy", "sweep", "swept", "--apply")
	waitFor(t, func() bool { return strings.Contains(out.String(), "lock moves-swept.lock") })
	// Meanwhile a deploy puts PROJ-11 on the target.
	writeFile(t, filepath.Join(d.state, "swept-sha"), d.git("rev-parse", "HEAD")+"\n")
	release()
	if err := second.Wait(); err != nil {
		t.Fatalf("the sweep: %v\n%s", err, out.String())
	}
	if _, moved := d.jira.snapshot(); !slices.Equal(slices.Sorted(slices.Values(moved)), []string{"PROJ-10", "PROJ-11"}) {
		t.Errorf("moved = %v, want PROJ-10 and PROJ-11 the target runs now\n%s", moved, out.String())
	}
}

// A daily sweep that cannot take the lock of the moves in time says so and
// stays due: the next commit sweeps again.
func TestDailySweepWithoutTheLockStaysDue(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return strings.Replace(sweepConfig(state, q), "sweep: {}}", "sweep: {apply: true}}", 1) + "\nlock:\n  timeout: 300ms\n"
	})
	cache := t.TempDir()
	d.env = append(d.env, "XDG_CACHE_HOME="+cache)
	d.git("remote", "set-head", "origin", d.git("branch", "--show-current"))
	d.statuses(map[string]string{"PROJ-10": "Ready for release"}, nil)
	d.pushCommits("feat: PROJ-10 a")
	d.mustGraft("deploy", "swept")
	stamps, err := filepath.Glob(filepath.Join(cache, "graft", "sweep", "*"))
	if err != nil || len(stamps) != 1 {
		t.Fatalf("stamps = %v, %v", stamps, err)
	}
	day := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(stamps[0], day, day); err != nil {
		t.Fatal(err)
	}
	release := holdMoves(t, d, "swept")
	d.write("k.txt", "x\n")
	out := d.mustGraft("commit", "-m", "feat: PROJ-10 more")
	release()
	wantLines(t, out, []string{"lock moves-swept.lock still held after 300ms", "the daily sweep is not done, the next commit tries it again"}, nil)
	fi, err := os.Stat(stamps[0])
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(fi.ModTime()) < 24*time.Hour {
		t.Errorf("the daily sweep is marked done though it could not move\n%s", out)
	}
}

// A daily sweep that cannot record what it held stays due: no handover
// would find the item, so the next commit sweeps again.
func TestDailySweepWithoutTheHeldRecordStaysDue(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return strings.Replace(sweepConfig(state, q), "release_notes: {transitions: [{from: [Ready for release], to: Done}], sweep: {}}",
			`release_notes: {transitions: [{from: [Ready for release], to: Done, assign: qa, require_comment: "How to check"}], sweep: {apply: true}}`, 1)
	})
	cache := t.TempDir()
	d.env = append(d.env, "XDG_CACHE_HOME="+cache)
	d.git("remote", "set-head", "origin", d.git("branch", "--show-current"))
	d.statuses(map[string]string{"PROJ-10": "Ready for release"}, nil)
	d.pushCommits("feat: PROJ-10 a")
	d.mustGraft("deploy", "swept")
	stamps, err := filepath.Glob(filepath.Join(cache, "graft", "sweep", "*"))
	if err != nil || len(stamps) != 1 {
		t.Fatalf("stamps = %v, %v", stamps, err)
	}
	day := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(stamps[0], day, day); err != nil {
		t.Fatal(err)
	}
	// The record of the held items cannot be read or written.
	if err := os.RemoveAll(filepath.Join(cache, "graft", "held")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cache, "graft", "held"), "not a directory\n")
	d.write("k.txt", "x\n")
	out := d.mustGraft("commit", "-m", "chore: tidy up")
	wantLines(t, out, []string{"graft: PROJ-10: NOT MOVED to Done", "the daily sweep is not done, the next commit tries it again"}, nil)
	fi, err := os.Stat(stamps[0])
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(fi.ModTime()) < 24*time.Hour {
		t.Errorf("the daily sweep is marked done though what it held is not recorded\n%s", out)
	}
}
