package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// freezeConfig: prod releases what acc, the acceptance target, runs; a
// freeze of a Product v version holds acc on its release branch, and lint
// is advisory while it lasts.
func freezeConfig(state string, q func(string) string) string {
	return `schema: 1
version: {mode: file, build_branches: ['hotfix/*', 'release/*']}
envs:
  local: {}
gate:
  - task: lint
tasks:
  lint:
    run: [[sh, -c, '[ ! -e ` + filepath.Join(state, "lint-fails") + ` ]']]
deploy:
  remote: origin
  hotfix: {branches: ['hotfix/*'], label: hotfix, base: prod}
  targets:
    acc:
      env: local
      run: [sh, deploy.sh]
      deps: [lint]
      version: 'cat ` + filepath.Join(state, "acc-version") + `'
      deployed_sha: {path: ` + q("acc-sha") + `}
    prod:
      env: local
      run: [sh, deploy.sh]
      deps: [lint]
      requires: acc
      version: 'cat ` + filepath.Join(state, "prod-version") + `'
      deployed_sha: {path: ` + q("prod-sha") + `}
      approved_release: {prefix: "Product v", ignore_paths: ["docs/**"]}
      release_page: {parent: "500"}
      freeze: {types: [Bug], advisory: [lint]}
release_notes:
  jira:
    project_keys: [PROJ]
`
}

// freezeRepo is a freezeConfig repository whose acc and prod run its first
// commit, with Product v1.2.10 (10010) approved: PROJ-1 joined it before the
// freeze on 2026-10-20 10:00, PROJ-3 (a Task) and PROJ-8 (a Bug) after it;
// PROJ-6 is in another version.
func freezeRepo(t *testing.T) *deployFixture {
	d := newDeployFixture(t, freezeConfig)
	d.git("remote", "set-head", "origin", d.git("branch", "--show-current"))
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-1": {"10010"}, "PROJ-3": {"10010"}, "PROJ-8": {"10010"}, "PROJ-6": {"10001"}})
	d.jira.setApprovers(map[string][]string{"10010": {"APPROVED", "APPROVED"}})
	before := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	after := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	d.jira.mu.Lock()
	d.jira.types = map[string]string{"PROJ-8": "Bug"}
	d.jira.fixChanges = map[string][]fakeFixChange{
		"PROJ-1": {{at: before, id: "10010", added: true}},
		// PROJ-3 was in the version, left it before the freeze and came back after.
		"PROJ-3": {{at: before, id: "10010", added: true}, {at: before.Add(time.Hour), id: "10010"}, {at: after, id: "10010", added: true}},
		"PROJ-8": {{at: after, id: "10010", added: true}},
	}
	d.jira.mu.Unlock()
	d.at("2026-10-19 10:00")
	d.mustGraft("deploy", "acc")
	// prod runs it too, released before the freeze.
	writeFile(t, filepath.Join(d.state, "prod-version"), d.stateFile("acc-version"))
	writeFile(t, filepath.Join(d.state, "prod-sha"), d.stateFile("acc-sha"))
	return d
}

// remoteTags lists the tags on origin under prefix.
func (d *deployFixture) remoteTags(prefix string) []string {
	d.t.Helper()
	var tags []string
	for line := range strings.SplitSeq(d.git("ls-remote", "--tags", "origin", "refs/tags/"+prefix+"*"), "\n") {
		if _, ref, ok := strings.Cut(line, "\t"); ok && !strings.HasSuffix(ref, "^{}") {
			tags = append(tags, strings.TrimPrefix(ref, "refs/tags/"))
		}
	}
	return tags
}

// freeze freezes prod for Product v1.2.10 at 2026-10-20 10:00 from the trunk
// and checks out the release branch.
func (d *deployFixture) freeze() (trunk string) {
	d.t.Helper()
	trunk = d.git("branch", "--show-current")
	d.at("2026-10-20 10:00")
	d.mustGraft("deploy", "freeze", "prod", "Product v1.2.10")
	d.git("fetch", "-q", "origin")
	d.git("switch", "-q", "release/1.2.10")
	d.at("2026-10-20 14:00")
	return trunk
}

// TestFreezeCutsTheBranchAndTagsIt: graft deploy freeze cuts the release
// branch from the trunk, pushes it and a tag of the freeze every clone sees,
// shows the freeze, and refuses a second one and an unknown version.
func TestFreezeCutsTheBranchAndTagsIt(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("one.txt", "feat: PROJ-1 accepted work")
	head := d.git("rev-parse", "HEAD")
	if out, code := d.graft("", "deploy", "freeze", "prod", "Product v9.9.9"); code == 0 || !strings.Contains(out, "Product v9.9.9") {
		t.Errorf("a version Jira does not have: exit %d\n%s", code, out)
	}
	d.at("2026-10-20 10:00")
	out := d.mustGraft("deploy", "freeze", "prod", "Product v1.2.10")
	if !strings.Contains(out, "release/1.2.10") {
		t.Errorf("the freeze does not name its branch:\n%s", out)
	}
	if got := d.git("ls-remote", "origin", "refs/heads/release/1.2.10"); !strings.HasPrefix(got, head) {
		t.Errorf("release/1.2.10 on origin = %q, want at %s", got, head)
	}
	if tags := d.remoteTags("freeze/"); len(tags) != 1 || tags[0] != "freeze/prod/1.2.10" {
		t.Fatalf("freeze tags on origin = %q", tags)
	}
	d.git("fetch", "-q", "origin", "refs/tags/freeze/prod/1.2.10:refs/tags/freeze/prod/1.2.10")
	if msg := d.git("tag", "-l", "--format=%(contents)", "freeze/prod/1.2.10"); !strings.Contains(msg, "Product v1.2.10") || !strings.Contains(msg, "2026-10-20T10:00:00+03:00") {
		t.Errorf("the tag does not hold the version and the time:\n%s", msg)
	}
	if out := d.mustGraft("deploy", "freeze", "prod"); !strings.Contains(out, "Product v1.2.10") || !strings.Contains(out, "release/1.2.10") {
		t.Errorf("graft deploy freeze prod does not show the freeze:\n%s", out)
	}
	if out, code := d.graft("", "deploy", "freeze", "prod", "Product v1.3.0"); code == 0 || !strings.Contains(out, "Product v1.2.10") {
		t.Errorf("a second freeze of prod: exit %d\n%s", code, out)
	}
}

// TestFrozenTargetTakesOnlyTheReleaseBranch: while prod is frozen, acc takes
// deploys from the release branch and from hotfix branches, the hotfix
// journaled; the trunk is refused unless --past-freeze gives a reason, which
// is journaled too.
func TestFrozenTargetTakesOnlyTheReleaseBranch(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("one.txt", "feat: PROJ-1 accepted work")
	trunk := d.freeze()
	d.mustGraft("deploy", "acc")
	accepted := strings.TrimSpace(d.stateFile("acc-sha"))

	d.git("switch", "-q", trunk)
	d.commitFile("two.txt", "feat: PROJ-1 more after the freeze")
	out, code := d.graft("", "deploy", "acc")
	if code == 0 || !strings.Contains(out, "release/1.2.10") || !strings.Contains(out, "Product v1.2.10") {
		t.Errorf("acc took the trunk while frozen: exit %d\n%s", code, out)
	}
	if got := strings.TrimSpace(d.stateFile("acc-sha")); got != accepted {
		t.Errorf("acc runs %s, want the accepted %s", got, accepted)
	}
	if out, code := d.graft("", "deploy", "check", "acc"); code == 0 || !strings.Contains(out, "release/1.2.10") {
		t.Errorf("graft deploy check acc does not show the freeze: exit %d\n%s", code, out)
	}

	out = d.mustGraft("deploy", "--past-freeze", "the customer asked for the trunk", "acc")
	if !strings.Contains(out, "the customer asked for the trunk") {
		t.Errorf("the bypass is not said:\n%s", out)
	}
	logs := d.remoteTags("freeze-log/prod/1.2.10/")
	if len(logs) != 1 || !strings.HasSuffix(logs[0], "-bypass") {
		t.Fatalf("freeze-log after the bypass = %q", logs)
	}
	d.git("fetch", "-q", "origin", "refs/tags/freeze-log/*:refs/tags/freeze-log/*")
	if msg := d.git("tag", "-l", "--format=%(contents)", logs[0]); !strings.Contains(msg, "the customer asked for the trunk") || !strings.Contains(msg, "acc") {
		t.Errorf("the bypass record:\n%s", msg)
	}

	// A hotfix from what prod runs goes to acc, journaled by its branch.
	d.git("switch", "-q", "-c", "hotfix/urgent", strings.TrimSpace(d.stateFile("prod-sha")))
	d.commitFile("urgent.txt", "fix: PROJ-9 urgent")
	d.jira.mu.Lock()
	d.jira.labels = map[string][]string{"PROJ-9": {"hotfix"}}
	d.jira.mu.Unlock()
	d.git("push", "-q", "-u", "origin", "HEAD")
	d.mustGraft("deploy", "acc")
	if logs := d.remoteTags("freeze-log/prod/1.2.10/"); len(logs) != 2 {
		t.Errorf("freeze-log after the hotfix = %q, want the bypass and the hotfix", logs)
	}
	d.git("fetch", "-q", "origin", "refs/tags/freeze-log/*:refs/tags/freeze-log/*")
	if msgs := d.git("tag", "-l", "--format=%(contents)", "freeze-log/*"); !strings.Contains(msgs, "hotfix/urgent") {
		t.Errorf("the hotfix is not journaled by its branch:\n%s", msgs)
	}
}

// TestFrozenTargetTakesOnlyTheFrozenVersion: on the release branch acc
// refuses a commit naming an item outside the version, a commit naming
// none, and an item that joined the version after the freeze unless it is a
// Bug; reverts and ignore_paths pass, as in approved_release.
func TestFrozenTargetTakesOnlyTheFrozenVersion(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("one.txt", "feat: PROJ-1 accepted work")
	d.freeze()
	d.commitFile("bug.txt", "fix: PROJ-8 a bug found in acceptance")
	d.commitFile("docs/notes.md", "docs: release notes draft")
	d.mustGraft("deploy", "acc")

	for _, c := range []struct{ file, msg, want string }{
		{"other.txt", "feat: PROJ-6 another product", "PROJ-6 not in Product v1.2.10"},
		{"keyless.txt", "chore: tidy up", "names no item"},
		{"late.txt", "feat: PROJ-3 joined late", "PROJ-3 joined Product v1.2.10 after the freeze"},
	} {
		sha := d.commitFile(c.file, c.msg)
		out, code := d.graft("", "deploy", "acc")
		if code == 0 || !strings.Contains(out, c.want) {
			t.Errorf("%s: exit %d, want %q\n%s", c.msg, code, c.want, out)
		}
		d.commitFile("revert-"+c.file, "revert: "+c.msg+"\n\nThis reverts commit "+sha+".")
	}
	// Each refused commit is reverted after it.
	if out := d.mustGraft("deploy", "acc"); strings.Contains(out, "joined") {
		t.Errorf("the reverted commits still count:\n%s", out)
	}

	// --past-freeze lets an item refusal through too, journaled.
	d.commitFile("late2.txt", "feat: PROJ-3 the customer wants it after all")
	if out, code := d.graft("", "deploy", "acc"); code == 0 {
		t.Errorf("PROJ-3 again: exit %d\n%s", code, out)
	}
	d.mustGraft("deploy", "--past-freeze", "agreed with the customer", "acc")
	if logs := d.remoteTags("freeze-log/prod/1.2.10/"); len(logs) != 1 || !strings.HasSuffix(logs[0], "-bypass") {
		t.Errorf("freeze-log after the bypass of an item = %q", logs)
	}
}

// TestAdvisoryTaskWarnsOnTheFrozenBranch: while prod is frozen, a failing
// advisory task does not stop a deploy of the release branch to acc or to
// prod, nor a commit on it, and is journaled; on the trunk it stops both.
func TestAdvisoryTaskWarnsOnTheFrozenBranch(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("one.txt", "feat: PROJ-1 accepted work")
	trunk := d.freeze()
	writeFile(t, filepath.Join(d.state, "lint-fails"), "")

	d.write("fix.txt", "fix\n")
	if out := d.mustGraft("commit", "-m", "fix: PROJ-8 committed through the gate"); !strings.Contains(out, "advisory while Product v1.2.10 is frozen") {
		t.Errorf("the gate on the release branch does not say lint is advisory:\n%s", out)
	}
	if logs := d.remoteTags("freeze-log/prod/1.2.10/"); len(logs) != 1 || !strings.HasSuffix(logs[0], "-advisory") {
		t.Errorf("freeze-log after the advisory failure on the gate = %q", logs)
	}
	if out := d.mustGraft("deploy", "acc"); !strings.Contains(out, "lint failed, advisory while Product v1.2.10 is frozen") {
		t.Errorf("the acc deploy does not say lint is advisory:\n%s", out)
	}
	if logs := d.remoteTags("freeze-log/prod/1.2.10/"); len(logs) == 0 || !strings.HasSuffix(logs[len(logs)-1], "-advisory") {
		t.Errorf("freeze-log after the advisory failure = %q", logs)
	}

	d.git("switch", "-q", trunk)
	d.write("trunk.txt", "trunk\n")
	if out, code := d.graft("", "commit", "-m", "feat: PROJ-1 on the trunk"); code == 0 {
		t.Errorf("the gate on the trunk passed a failing lint:\n%s", out)
	}
}

// TestFreezeEndsWhenTheBranchReachesProd: prod running the release branch
// ends the freeze and its journal, the release page lists the journal, and
// acc takes the trunk again; graft deploy thaw ends a freeze by hand.
func TestFreezeEndsWhenTheBranchReachesProd(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("one.txt", "feat: PROJ-1 accepted work")
	trunk := d.freeze()
	d.mustGraft("deploy", "acc")
	d.git("switch", "-q", trunk)
	d.commitFile("two.txt", "feat: PROJ-1 trunk work after the freeze")
	d.mustGraft("deploy", "--past-freeze", "a look at the trunk", "acc")
	d.git("switch", "-q", "release/1.2.10")
	d.mustGraft("deploy", "--redeploy", "acc")
	writeFile(t, filepath.Join(d.state, "lint-fails"), "")

	out := d.mustGraft("deploy", "prod")
	if !strings.Contains(out, "lint failed, advisory while Product v1.2.10 is frozen") {
		t.Errorf("the prod deploy does not say lint is advisory:\n%s", out)
	}
	if tags := d.remoteTags("freeze"); len(tags) != 0 {
		t.Errorf("tags left after the release = %q", tags)
	}
	body := d.jira.page("Product v1.2.10: release notes")
	for _, want := range []string{"a look at the trunk", "lint"} {
		if !strings.Contains(body, want) {
			t.Errorf("the release page lacks %q:\n%s\n%s", want, body, out)
		}
	}

	if err := os.Remove(filepath.Join(d.state, "lint-fails")); err != nil {
		t.Fatal(err)
	}
	d.git("switch", "-q", trunk)
	d.mustGraft("deploy", "acc")

	// By hand.
	d.at("2026-10-21 10:00")
	d.mustGraft("deploy", "freeze", "prod", "Product v1.3.0")
	if out, code := d.graft("", "deploy", "thaw", "prod"); code == 0 || !strings.Contains(out, "--reason") {
		t.Errorf("thaw without a reason: exit %d\n%s", code, out)
	}
	d.mustGraft("deploy", "thaw", "prod", "--reason", "the release was called off")
	if tags := d.remoteTags("freeze"); len(tags) != 0 {
		t.Errorf("tags left after thaw = %q", tags)
	}
	d.commitFile("three.txt", "feat: PROJ-1 after the thaw")
	d.mustGraft("deploy", "acc")
	if out, code := d.graft("", "deploy", "acc"); code == 0 || !strings.Contains(out, "already runs") {
		t.Errorf("acc took the commit it runs again without --redeploy: exit %d\n%s", code, out)
	}
}

// TestNoFreezeKeepsDeploysAsBefore: without freeze in the config acc takes
// the trunk and any item, and a failing lint stops acc and prod as before.
func TestNoFreezeKeepsDeploysAsBefore(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return strings.Replace(freezeConfig(state, q), "      freeze: {types: [Bug], advisory: [lint]}\n", "", 1)
	})
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-1": {"10010"}, "PROJ-6": {"10001"}})
	d.jira.setApprovers(map[string][]string{"10010": {"APPROVED", "APPROVED"}})
	d.at("2026-10-20 10:00")
	if out, code := d.graft("", "deploy", "freeze", "prod", "Product v1.2.10"); code == 0 || !strings.Contains(out, "freeze") {
		t.Errorf("a freeze of a target without freeze in the config: exit %d\n%s", code, out)
	}
	d.commitFile("other.txt", "feat: PROJ-6 another product")
	d.mustGraft("deploy", "acc")
	writeFile(t, filepath.Join(d.state, "lint-fails"), "")
	d.commitFile("one.txt", "feat: PROJ-1 accepted work")
	if out, code := d.graft("", "deploy", "acc"); code == 0 || strings.Contains(out, "advisory") {
		t.Errorf("acc with a failing lint: exit %d\n%s", code, out)
	}
	if out, code := d.graft("", "deploy", "prod"); code == 0 || strings.Contains(out, "advisory") {
		t.Errorf("prod with a failing lint: exit %d\n%s", code, out)
	}
}
