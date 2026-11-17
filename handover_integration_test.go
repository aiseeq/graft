package main

import (
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDeployHandoverNamedDeployedItem(t *testing.T) {
	t.Run("accepts an ordinary configured transition without require_comment", func(t *testing.T) {
		d := newDeployFixture(t, deployConfig)
		d.env = append(d.env, "XDG_CACHE_HOME="+t.TempDir())
		d.jira.mu.Lock()
		d.jira.statuses["PROJ-80"] = "In Progress"
		d.jira.mu.Unlock()
		d.write("a.txt", "work\n")
		d.mustGraft("commit", "-m", "feat: PROJ-80 deployed work")
		sha := d.git("rev-parse", "HEAD")
		writeFile(t, filepath.Join(d.state, "sub-sha"), sha+"\n")
		out := d.mustGraft("deploy", "handover", "sub", "PROJ-80")
		d.jira.mu.Lock()
		status := d.jira.statuses["PROJ-80"]
		assigned := slices.Clone(d.jira.assigned)
		d.jira.mu.Unlock()
		if status != "Testing" || !slices.Contains(assigned, "PROJ-80 to qa") || !strings.Contains(out, "PROJ-80 moved to Testing") {
			t.Errorf("ordinary named handover status=%q assigned=%v\n%s", status, assigned, out)
		}
	})

	t.Run("merge and revert messages do not prove work", func(t *testing.T) {
		for _, kind := range []string{"merge", "revert"} {
			t.Run(kind, func(t *testing.T) {
				d := newDeployFixture(t, deployConfig)
				d.env = append(d.env, "XDG_CACHE_HOME="+t.TempDir())
				head := d.git("rev-parse", "HEAD")
				tree := d.git("rev-parse", "HEAD^{tree}")
				var deployed string
				if kind == "merge" {
					side := d.git("commit-tree", tree, "-p", head, "-m", "feat: unrelated side")
					deployed = d.git("commit-tree", tree, "-p", head, "-p", side, "-m", "Merge PROJ-80")
				} else {
					deployed = d.git("commit-tree", tree, "-p", head, "-m", "revert: PROJ-80 remove change")
				}
				writeFile(t, filepath.Join(d.state, "sub-sha"), deployed+"\n")
				d.jira.mu.Lock()
				d.jira.requests = 0
				d.jira.mu.Unlock()
				out, code := d.graft("", "deploy", "handover", "sub", "PROJ-80")
				d.jira.mu.Lock()
				requests := d.jira.requests
				d.jira.mu.Unlock()
				if code == 0 || !strings.Contains(out, "PROJ-80 has no work commit in the deployed history of sub") || requests != 0 {
					t.Errorf("exit=%d Jira requests=%d\n%s", code, requests, out)
				}
			})
		}
	})

	t.Run("double revert restores work evidence", func(t *testing.T) {
		d := newDeployFixture(t, deployConfig)
		d.env = append(d.env, "XDG_CACHE_HOME="+t.TempDir())
		head := d.git("rev-parse", "HEAD")
		tree := d.git("rev-parse", "HEAD^{tree}")
		deployed := d.git("commit-tree", tree, "-p", head, "-m", `Revert "Revert "feat: PROJ-80 work""`)
		writeFile(t, filepath.Join(d.state, "sub-sha"), deployed+"\n")
		d.jira.mu.Lock()
		d.jira.statuses["PROJ-80"] = "In Progress"
		d.jira.mu.Unlock()
		out, code := d.graft("", "deploy", "handover", "sub", "PROJ-80")
		if code != 0 || !strings.Contains(out, "PROJ-80 moved to Testing") || d.status("PROJ-80") != "Testing" {
			t.Errorf("double revert did not restore work evidence; exit=%d status=%s\n%s", code, d.status("PROJ-80"), out)
		}
	})

	t.Run("a body-only mention does not count as work", func(t *testing.T) {
		d := newDeployFixture(t, deployConfig)
		d.env = append(d.env, "XDG_CACHE_HOME="+t.TempDir())
		d.mustFixtureCommit("fix: unrelated\n\nAfter this, PROJ-80 is mentioned.")
		sha := d.git("rev-parse", "HEAD")
		writeFile(t, filepath.Join(d.state, "sub-sha"), sha+"\n")
		out, code := d.graft("", "deploy", "handover", "sub", "PROJ-80")
		if code == 0 || !strings.Contains(out, "PROJ-80 has no work commit in the deployed history of sub") {
			t.Errorf("body mention was treated as work; exit=%d:\n%s", code, out)
		}
	})

	t.Run("a work commit after the deployed head does not count", func(t *testing.T) {
		d := newDeployFixture(t, deployConfig)
		d.env = append(d.env, "XDG_CACHE_HOME="+t.TempDir())
		deployed := d.git("rev-parse", "HEAD")
		writeFile(t, filepath.Join(d.state, "sub-sha"), deployed+"\n")
		d.write("a.txt", "newer work\n")
		d.mustFixtureCommit("feat: PROJ-80 after deployment")
		out, code := d.graft("", "deploy", "handover", "sub", "PROJ-80")
		if code == 0 || !strings.Contains(out, "PROJ-80 has no work commit in the deployed history of sub") {
			t.Errorf("post-deploy work was treated as deployed; exit=%d:\n%s", code, out)
		}
	})

	t.Run("does not process a key twice when it is already held", func(t *testing.T) {
		d := newDeployFixture(t, func(state string, q func(string) string) string {
			return strings.Replace(deployConfig(state, q), "keep_assignee_labels: [internal]}", `keep_assignee_labels: [internal], require_comment: "How to check"}`, 1)
		})
		d.env = append(d.env, "XDG_CACHE_HOME="+t.TempDir())
		d.jira.mu.Lock()
		d.jira.statuses["PROJ-80"] = "In Progress"
		d.jira.entered = map[string]time.Time{"PROJ-80": time.Now().Add(-time.Hour)}
		d.jira.mu.Unlock()
		d.write("a.txt", "work\n")
		d.mustGraft("commit", "-m", "feat: PROJ-80 deployed work")
		d.mustGraft("deploy", "--unapproved", "sub")
		d.jira.mu.Lock()
		d.jira.written = map[string][]fakeComment{"PROJ-80": {{author: "me", created: time.Now(), text: "How to check: run it"}}}
		d.jira.issueReads = map[string]int{}
		d.jira.mu.Unlock()
		d.mustGraft("deploy", "handover", "sub", "PROJ-80")
		d.jira.mu.Lock()
		reads := d.jira.issueReads["PROJ-80"]
		d.jira.mu.Unlock()
		if reads != 1 {
			t.Errorf("Issue reads for already-held named key = %d, want 1", reads)
		}
	})

	t.Run("moves and assigns a named deployed item with its comment", func(t *testing.T) {
		d := newDeployFixture(t, deployConfig)
		d.env = append(d.env, "XDG_CACHE_HOME="+t.TempDir())
		d.jira.mu.Lock()
		d.jira.statuses["PROJ-80"] = "In Progress"
		d.jira.entered = map[string]time.Time{}
		d.jira.mu.Unlock()
		d.write("a.txt", "work\n")
		d.mustGraft("commit", "-m", "feat: PROJ-80 deployed work")
		d.mustGraft("deploy", "--unapproved", "sub")
		config := strings.Replace(d.read(".graft.yaml"), "keep_assignee_labels: [internal]}", `keep_assignee_labels: [internal], require_comment: "How to check"}`, 1)
		d.write(".graft.yaml", config)
		d.jira.mu.Lock()
		d.jira.statuses["PROJ-80"] = "In Progress"
		d.jira.entered["PROJ-80"] = time.Now().Add(-time.Hour)
		d.jira.written = map[string][]fakeComment{"PROJ-80": {{author: "me", created: time.Now(), text: "How to check: run it"}}}
		d.jira.mu.Unlock()
		out := d.mustGraft("deploy", "handover", "sub", "PROJ-80")
		d.jira.mu.Lock()
		status := d.jira.statuses["PROJ-80"]
		assigned := slices.Clone(d.jira.assigned)
		d.jira.mu.Unlock()
		if status != "Testing" || !slices.Contains(assigned, "PROJ-80 to qa") || !strings.Contains(out, "PROJ-80 moved to Testing") {
			t.Errorf("named handover status=%q assigned=%v\n%s", status, assigned, out)
		}
	})

	t.Run("Jira transition failure leaves the named item held and fails", func(t *testing.T) {
		d := newDeployFixture(t, func(state string, q func(string) string) string {
			return strings.Replace(deployConfig(state, q), "keep_assignee_labels: [internal]}", `keep_assignee_labels: [internal], require_comment: "How to check"}`, 1)
		})
		d.env = append(d.env, "XDG_CACHE_HOME="+t.TempDir())
		d.jira.mu.Lock()
		d.jira.statuses["PROJ-80"] = "In Progress"
		d.jira.entered = map[string]time.Time{"PROJ-80": time.Now().Add(-time.Hour)}
		d.jira.written = map[string][]fakeComment{"PROJ-80": {{author: "me", created: time.Now(), text: "How to check: run it"}}}
		d.jira.failTransitions = []string{"PROJ-80"}
		d.jira.mu.Unlock()
		d.write("a.txt", "work\n")
		d.mustGraft("commit", "-m", "feat: PROJ-80 deployed work")
		sha := d.git("rev-parse", "HEAD")
		writeFile(t, filepath.Join(d.state, "sub-sha"), sha+"\n")
		out, code := d.graft("", "deploy", "handover", "sub", "PROJ-80")
		if code == 0 || !strings.Contains(out, "http 500") {
			t.Fatalf("transition failure exit=%d:\n%s", code, out)
		}
		d.jira.mu.Lock()
		d.jira.failTransitions = nil
		d.jira.mu.Unlock()
		retry := d.mustGraft("deploy", "handover", "sub")
		if !strings.Contains(retry, "PROJ-80 moved to Testing") {
			t.Errorf("failed named item was not saved for handover retry:\n%s", retry)
		}
	})

	t.Run("failed named assignment fails without holding", func(t *testing.T) {
		d := newDeployFixture(t, func(state string, q func(string) string) string {
			return strings.Replace(deployConfig(state, q), "keep_assignee_labels: [internal]}", `keep_assignee_labels: [internal], require_comment: "How to check"}`, 1)
		})
		d.env = append(d.env, "XDG_CACHE_HOME="+t.TempDir())
		d.jira.mu.Lock()
		d.jira.statuses["PROJ-80"] = "In Progress"
		d.jira.entered = map[string]time.Time{"PROJ-80": time.Now().Add(-time.Hour)}
		d.jira.written = map[string][]fakeComment{"PROJ-80": {{author: "me", created: time.Now(), text: "How to check: run it"}}}
		d.jira.failAssign = []string{"PROJ-80"}
		d.jira.mu.Unlock()
		d.write("a.txt", "work\n")
		d.mustGraft("commit", "-m", "feat: PROJ-80 deployed work")
		sha := d.git("rev-parse", "HEAD")
		writeFile(t, filepath.Join(d.state, "sub-sha"), sha+"\n")
		out, code := d.graft("", "deploy", "handover", "sub", "PROJ-80")
		d.jira.mu.Lock()
		status := d.jira.statuses["PROJ-80"]
		d.jira.mu.Unlock()
		if code == 0 || status != "Testing" || !strings.Contains(out, "PROJ-80: assignee NOT SET (") || !strings.Contains(out, "400") || strings.Contains(out, "still held") || !strings.Contains(out, "the assignee is not set on PROJ-80") {
			t.Errorf("failed assign did not fail without holding: exit=%d status=%q\n%s", code, status, out)
		}
	})

	t.Run("rejects a key absent from deployed work before Jira access", func(t *testing.T) {
		d := newDeployFixture(t, func(state string, q func(string) string) string {
			return strings.Replace(deployConfig(state, q), "keep_assignee_labels: [internal]}", `keep_assignee_labels: [internal], require_comment: "How to check"}`, 1)
		})
		d.env = append(d.env, "XDG_CACHE_HOME="+t.TempDir())
		d.jira.mu.Lock()
		d.jira.requests = 0
		d.jira.nonGET = nil
		d.jira.mu.Unlock()
		out, code := d.graft("", "deploy", "handover", "sub", "PROJ-80")
		d.jira.mu.Lock()
		requests := d.jira.requests
		nonGET := slices.Clone(d.jira.nonGET)
		d.jira.mu.Unlock()
		if code == 0 || !strings.Contains(out, "PROJ-80 has no work commit in the deployed history of sub") || requests != 0 || len(nonGET) != 0 {
			t.Errorf("exit=%d requests=%d writes=%v\n%s", code, requests, nonGET, out)
		}
	})

	t.Run("holds a named item that still needs its comment", func(t *testing.T) {
		d := newDeployFixture(t, deployConfig)
		d.env = append(d.env, "XDG_CACHE_HOME="+t.TempDir())
		d.jira.mu.Lock()
		d.jira.statuses["PROJ-80"] = "In Progress"
		d.jira.entered = map[string]time.Time{}
		d.jira.mu.Unlock()
		d.write("a.txt", "work\n")
		d.mustGraft("commit", "-m", "feat: PROJ-80 deployed work")
		d.mustGraft("deploy", "--unapproved", "sub")
		config := strings.Replace(d.read(".graft.yaml"), "keep_assignee_labels: [internal]}", `keep_assignee_labels: [internal], require_comment: "How to check"}`, 1)
		d.write(".graft.yaml", config)
		d.jira.mu.Lock()
		d.jira.statuses["PROJ-80"] = "In Progress"
		d.jira.entered["PROJ-80"] = time.Now().Add(-time.Hour)
		d.jira.written = map[string][]fakeComment{}
		d.jira.mu.Unlock()
		out, code := d.graft("", "deploy", "handover", "sub", "PROJ-80")
		if code == 0 || !strings.Contains(out, "PROJ-80: NOT MOVED to Testing") {
			t.Errorf("exit=%d\n%s", code, out)
		}
		retry, retryCode := d.graft("", "deploy", "handover", "sub")
		if retryCode == 0 || !strings.Contains(retry, "PROJ-80: NOT MOVED to Testing") || !strings.Contains(retry, "1 of sub are still held") {
			t.Errorf("named item was not persisted as held: exit=%d\n%s", retryCode, retry)
		}
	})

	t.Run("keeps the no-key no-held behavior", func(t *testing.T) {
		d := newDeployFixture(t, func(state string, q func(string) string) string {
			return strings.Replace(deployConfig(state, q), "keep_assignee_labels: [internal]}", `keep_assignee_labels: [internal], require_comment: "How to check"}`, 1)
		})
		d.env = append(d.env, "XDG_CACHE_HOME="+t.TempDir())
		if out := d.mustGraft("deploy", "handover", "sub"); !strings.Contains(out, "no item of sub is held") {
			t.Errorf("unexpected no-key result:\n%s", out)
		}
	})
}

// TestDeployHandoverSubtasksWaitForTheirOwnCommits checks that handover
// follows a parent only when each worked subtask's commits are on the target.
func TestDeployHandoverSubtasksWaitForTheirOwnCommits(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		c := deployConfig(state, q)
		return strings.Replace(c, "keep_assignee_labels: [internal]}", "keep_assignee_labels: [internal], require_comment: How to check}", 1)
	})
	d.env = append(d.env, "XDG_CACHE_HOME="+t.TempDir())
	began := time.Now().Add(-time.Hour)
	d.jira.mu.Lock()
	d.jira.entered = map[string]time.Time{"PROJ-10": began}
	d.jira.mu.Unlock()
	subtaskWork(d)
	if out := d.mustGraft("deploy", "sub"); !strings.Contains(out, "PROJ-10: NOT MOVED to Testing") {
		t.Errorf("parent was not held for its comment:\n%s", out)
	}

	d.jira.mu.Lock()
	d.jira.written = map[string][]fakeComment{"PROJ-10": {{author: "me", created: time.Now(), text: "How to check: run the feature"}}}
	d.jira.mu.Unlock()
	checkSubtasksFollowed(t, d, d.mustGraft("deploy", "handover", "sub"))
}

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
	d.mustGraft("commit", "-m", "feat: the form\n\nPROJ-80\nPROJ-81\nPROJ-82\nPROJ-83\nPROJ-84")
	out := d.mustGraft("deploy", "sub")
	_, moved := d.jira.snapshot()
	d.jira.mu.Lock()
	assigned := slices.Sorted(slices.Values(d.jira.assigned))
	d.jira.mu.Unlock()
	if slices.Sort(moved); !slices.Equal(moved, []string{"PROJ-81", "PROJ-84", "PROJ-85"}) || !slices.Equal(assigned, []string{"PROJ-81 to qa"}) {
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
	d.mustGraft("commit", "-m", "chore: tidy\n\nPROJ-82\nPROJ-86")
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

// TestDeployCheckNamesHeldItems: graft deploy check names an item an earlier
// deploy held, which the deploy moves once its comment is written though no
// new commit names it, and leaves it held: the check only reads.
func TestDeployCheckNamesHeldItems(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return strings.Replace(deployConfig(state, q), "keep_assignee_labels: [internal]}", "keep_assignee_labels: [internal], require_comment: How to check}", 1)
	})
	d.env = append(d.env, "XDG_CACHE_HOME="+t.TempDir())
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-80"] = "In Progress"
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "sub")
	d.write("a.txt", "a\n")
	d.mustGraft("commit", "-m", "feat: the form\n\nPROJ-80")
	if out := d.mustGraft("deploy", "sub"); !strings.Contains(out, "graft: PROJ-80: NOT MOVED to Testing") {
		t.Fatalf("PROJ-80 not held:\n%s", out)
	}
	d.jira.mu.Lock()
	d.jira.written = map[string][]fakeComment{"PROJ-80": {{author: "me", created: time.Now(), text: "How to check: open the form"}}}
	d.jira.nonGET = nil
	d.jira.mu.Unlock()
	d.write("b.txt", "b\n")
	d.mustGraft("commit", "-m", "chore: tidy")
	out, _ := d.graft("", "deploy", "check", "sub")
	if !strings.Contains(out, "- PROJ-80: In Progress → Testing; assign qa") {
		t.Errorf("check does not name the held PROJ-80:\n%s", out)
	}
	d.jira.mu.Lock()
	nonGET := slices.Clone(d.jira.nonGET)
	d.jira.mu.Unlock()
	if len(nonGET) != 0 {
		t.Errorf("check sent %v", nonGET)
	}
	if out, _ := d.graft("", "deploy", "handover", "sub"); !strings.Contains(out, "PROJ-80 moved to Testing") {
		t.Errorf("check let go of the held PROJ-80:\n%s", out)
	}
}
