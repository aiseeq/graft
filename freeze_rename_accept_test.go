package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func (d *deployFixture) renameVersion(id, name string) {
	d.t.Helper()
	d.jira.mu.Lock()
	defer d.jira.mu.Unlock()
	for _, v := range d.jira.versions {
		if v["id"] == id {
			v["name"] = name
			return
		}
	}
	d.t.Fatalf("no version %s", id)
}

// TestFreezeFollowsARenamedVersion: a freeze holds a Jira version, not its
// name. Renamed under the freeze, the version still takes its own items, still
// refuses others and late ones, and refusals say the name Jira has now. The
// branch and the tag keep the name they were cut with, and prod ends the freeze.
func TestFreezeFollowsARenamedVersion(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("one.txt", "feat: PROJ-1 accepted work")
	d.freeze()
	d.renameVersion("10010", "Product v1.2.77")

	d.commitFile("two.txt", "feat: PROJ-1 more accepted work")
	d.commitFile("bug.txt", "fix: PROJ-8 a bug found in acceptance")
	if out, code := d.graft("", "deploy", "acc"); code != 0 {
		t.Fatalf("the renamed version refused its own items: exit %d\n%s", code, out)
	}

	for _, c := range []struct{ file, msg, want string }{
		{"other.txt", "feat: PROJ-6 another product", "PROJ-6 not in Product v1.2.77"},
		{"keyless.txt", "chore: tidy up", "names no item in Product v1.2.77"},
		{"late.txt", "feat: PROJ-3 joined late", "PROJ-3 joined Product v1.2.77 after the freeze"},
	} {
		sha := d.commitFile(c.file, c.msg)
		out, code := d.graft("", "deploy", "acc")
		if code == 0 || !strings.Contains(out, c.want) {
			t.Errorf("%s: exit %d, want %q\n%s", c.msg, code, c.want, out)
		}
		d.commitFile("revert-"+c.file, "revert: "+c.msg+"\n\nThis reverts commit "+sha+".")
	}
	d.mustGraft("deploy", "acc")

	if out := d.mustGraft("deploy", "freeze", "prod"); !strings.Contains(out, "Product v1.2.10") {
		t.Errorf("the shown freeze lost the name it was cut with:\n%s", out)
	}
	if got := strings.Join(d.remoteTags("freeze/prod/"), " "); got == "" {
		t.Errorf("the freeze tag is gone after a rename")
	}
	d.mustGraft("deploy", "prod")
	if got := strings.Join(d.remoteTags("freeze/prod/"), " "); got != "" {
		t.Errorf("prod did not end the freeze: %s", got)
	}
}

// TestFreezeHoldsTheVersionOfEveryProject: Jira versions belong to one project,
// and a release across project_keys is one version name in each, with its own
// id. The freeze holds all of them: items of the second project pass and are
// refused like those of the first, a rename of one leaves the other in force,
// and only when every one is gone is the version gone.
func TestFreezeHoldsTheVersionOfEveryProject(t *testing.T) {
	d := freezeRepoWith(t, func(state string, q func(string) string) string {
		return strings.Replace(freezeConfig(state, q), "project_keys: [PROJ]", "project_keys: [PROJ, OTHER]", 1)
	})
	before := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	after := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	d.jira.mu.Lock()
	d.jira.otherVersions = []map[string]any{{"id": "20010", "name": "Product v1.2.10"}}
	d.jira.fixVersions["OTHER-20"] = []string{"20010"}
	d.jira.fixVersions["OTHER-21"] = []string{"20010"}
	d.jira.fixChanges["OTHER-20"] = []fakeFixChange{{at: before, id: "20010", added: true}}
	d.jira.fixChanges["OTHER-21"] = []fakeFixChange{{at: after, id: "20010", added: true}}
	d.jira.mu.Unlock()

	d.commitFile("one.txt", "feat: PROJ-1 accepted work")
	d.commitFile("two.txt", "feat: OTHER-20 accepted work of the second project")
	d.freeze()
	if out, code := d.graft("", "deploy", "acc"); code != 0 {
		t.Fatalf("the second project's item in the same-named version was refused: exit %d\n%s", code, out)
	}

	sha := d.commitFile("late.txt", "feat: OTHER-21 joined late")
	if out, code := d.graft("", "deploy", "acc"); code == 0 || !strings.Contains(out, "OTHER-21 joined") {
		t.Errorf("a late item of the second project: exit %d\n%s", code, out)
	}
	d.commitFile("revert-late.txt", "revert: feat: OTHER-21 joined late\n\nThis reverts commit "+sha+".")

	d.renameVersion("10010", "Product v1.2.77")
	d.commitFile("three.txt", "feat: OTHER-20 more work after one project renamed")
	if out, code := d.graft("", "deploy", "acc"); code != 0 {
		t.Errorf("a rename in one project refused the other's item: exit %d\n%s", code, out)
	}

	// One of the two deleted: the freeze still holds the other.
	d.jira.mu.Lock()
	d.jira.versions = slices.DeleteFunc(d.jira.versions, func(v map[string]any) bool { return v["id"] == "10010" })
	d.jira.mu.Unlock()
	d.commitFile("four.txt", "feat: OTHER-20 more work after one project deleted it")
	if out, code := d.graft("", "deploy", "acc"); code != 0 {
		t.Errorf("a version left in one project read as gone: exit %d\n%s", code, out)
	}
}

// TestFreezeRefusesWhenItsVersionIsGone: a version deleted from Jira under the
// freeze is an error naming it, not a refusal of every item as "not in".
func TestFreezeRefusesWhenItsVersionIsGone(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("one.txt", "feat: PROJ-1 accepted work")
	d.freeze()
	d.jira.mu.Lock()
	kept := d.jira.versions[:0]
	for _, v := range d.jira.versions {
		if v["id"] != "10010" {
			kept = append(kept, v)
		}
	}
	d.jira.versions = kept
	d.jira.mu.Unlock()

	d.commitFile("two.txt", "feat: PROJ-1 more accepted work")
	out, code := d.graft("", "deploy", "acc")
	if code == 0 || !strings.Contains(out, "10010") || !strings.Contains(out, "Product v1.2.10") {
		t.Errorf("a deleted version: exit %d, want an error naming id 10010 and its frozen name\n%s", code, out)
	}
	if strings.Contains(out, "not in Product") {
		t.Errorf("a deleted version reads as items outside it:\n%s", out)
	}
}

// TestFreezeTagWithoutVersionIDNamesTheWayOut: a tag cut before the id was kept
// cannot say which version it holds. The deploy says so and how to freeze again;
// thaw still ends it.
func TestFreezeTagWithoutVersionIDNamesTheWayOut(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("one.txt", "feat: PROJ-1 accepted work")
	d.freeze()
	tag := d.remoteTags("freeze/prod/")[0]
	body := d.git("tag", "-l", "--format=%(contents)", tag)
	var kept []string
	for line := range strings.SplitSeq(body, "\n") {
		if !strings.Contains(strings.ToLower(line), "id: ") {
			kept = append(kept, line)
		}
	}
	d.git("tag", "-f", "-a", tag, "-m", strings.Join(kept, "\n"))
	d.git("push", "-q", "-f", "origin", "refs/tags/"+tag)

	d.commitFile("two.txt", "feat: PROJ-1 more accepted work")
	out, code := d.graft("", "deploy", "acc")
	if code == 0 || !strings.Contains(out, "version id") || !strings.Contains(out, "thaw") {
		t.Errorf("an old tag: exit %d, want a refusal naming the version id and thaw\n%s", code, out)
	}
	d.mustGraft("deploy", "thaw", "prod", "--reason", "cut again with the id")
	if got := d.remoteTags("freeze/prod/"); len(got) != 0 {
		t.Errorf("thaw left %v", got)
	}
}
