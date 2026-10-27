package main

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestDeployRequireComment: an item handed over to the reviewer moves only
// with a comment of the token owner that has require_comment in it, written
// since the work came into In Progress; a subtask following its parent
// counts the parent's, an internal item needs none. graft deploy handover
// and the next deploy move a held item once the comment is there, and let go
// of one moved by hand.
func TestDeployRequireComment(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return strings.Replace(deployConfig(state, q), "keep_assignee_labels: [internal]}", "keep_assignee_labels: [internal], require_comment: How to check}", 1)
	})
	d.env = append(d.env, "XDG_CACHE_HOME="+t.TempDir())
	began := time.Now().Add(-time.Hour)
	d.jira.mu.Lock()
	maps.Copy(d.jira.statuses, map[string]string{"PROJ-80": "In Progress", "PROJ-81": "In Progress", "PROJ-82": "In Progress", "PROJ-83": "In Progress", "PROJ-84": "In Progress", "PROJ-85": "In Progress"})
	d.jira.entered = map[string]time.Time{"PROJ-82": began}
	d.jira.written = map[string][]fakeComment{
		"PROJ-81": {{author: "me", created: began, text: "The form saves now.\nHOW TO CHECK on test: press Save"}},
		"PROJ-82": {{author: "me", created: began.Add(-time.Minute), text: "How to check: the round before review"}},
		"PROJ-83": {{author: "colleague", created: began, text: "How to check this?"}},
	}
	d.jira.labels = map[string][]string{"PROJ-84": {"internal"}}
	d.jira.parents = map[string]string{"PROJ-85": "PROJ-81"}
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "sub")
	d.write("a.txt", "a\n")
	d.mustGraft("commit", "-m", "feat: PROJ-80 the form\n\nAlso PROJ-81, PROJ-82, PROJ-83 and PROJ-84")
	out := d.mustGraft("deploy", "sub")
	_, moved := d.jira.snapshot()
	d.jira.mu.Lock()
	assigned := slices.Sorted(slices.Values(d.jira.assigned))
	d.jira.mu.Unlock()
	if slices.Sort(moved); !slices.Equal(moved, []string{"PROJ-81", "PROJ-84", "PROJ-85"}) || !slices.Equal(assigned, []string{"PROJ-81 to qa", "PROJ-85 to qa"}) {
		t.Errorf("moved = %v, assigned = %v\n%s", moved, assigned, out)
	}
	for _, want := range []string{
		`graft: PROJ-80: NOT MOVED to Testing: no comment with "How to check" by the owner of the Jira token on it since the work came into In Progress; once it is written, graft deploy handover sub moves it`,
		"graft: PROJ-82: NOT MOVED to Testing",
		"graft: PROJ-83: NOT MOVED to Testing",
		"PROJ-84 stays with its assignee: label internal (keep_assignee_labels)",
		"PROJ-85 moved to Testing after its parent PROJ-81\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}

	d.jira.mu.Lock()
	d.jira.written["PROJ-80"] = []fakeComment{{author: "me", created: time.Now(), text: "How to check: open the form"}}
	d.jira.mu.Unlock()
	out, code := d.graft("", "deploy", "handover", "sub")
	if code == 0 || !strings.Contains(out, "PROJ-80 moved to Testing\n") || !strings.Contains(out, "PROJ-80 assigned to qa") || !strings.Contains(out, "2 of sub are still held") {
		t.Errorf("handover: %d\n%s", code, out)
	}

	d.jira.mu.Lock()
	d.jira.written["PROJ-82"] = append(d.jira.written["PROJ-82"], fakeComment{author: "me", created: time.Now(), text: "how to check: the new round"})
	d.jira.statuses["PROJ-83"] = "Testing"
	d.jira.mu.Unlock()
	d.write("b.txt", "b\n")
	d.mustGraft("commit", "-m", "chore: PROJ-86 tidy")
	out = d.mustGraft("deploy", "sub")
	for _, want := range []string{"PROJ-82 moved to Testing\n", "PROJ-82 assigned to qa", "PROJ-83 is held no more: it is Testing"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}
	if out := d.mustGraft("deploy", "handover", "sub"); !strings.Contains(out, "no item of sub is held") {
		t.Errorf("held after all moved:\n%s", out)
	}
}
