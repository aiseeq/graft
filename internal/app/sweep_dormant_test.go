package app

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/jira"
)

func TestSweepLeavesDormantItemAndMovesOrdinaryItem(t *testing.T) {
	var moved []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/transitions") {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		moved = append(moved, strings.Split(r.URL.Path, "/")[5])
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	var output bytes.Buffer
	app := &App{Stdout: &output, Stderr: &output}
	transition := config.NoteTransition{From: []string{"Ready for release"}, To: "Done"}
	target := &config.DeployTarget{ApprovedRelease: &config.ApprovedRelease{Dormant: []config.DormantReleaseItem{{Key: "PROJ-123", Reason: "test stand"}}}, ReleaseNotes: &config.TargetNotes{Transitions: []config.NoteTransition{transition}}}
	run := &deployRun{name: "prod", target: target, cfg: &config.Config{}}
	client := jira.New(jira.Credentials{BaseURL: server.URL})
	s := &sweepRun{app: app, r: run, conf: &config.Sweep{Days: 30, StaleDays: 3, Done: "Done"}, notes: &config.JiraNotes{}, client: client, me: "owner", now: time.Now(), apply: true,
		issues: map[string]jira.Issue{
			"PROJ-123": {Key: "PROJ-123", Status: "Testing", AssigneeID: "owner"},
			"PROJ-124": {Key: "PROJ-124", Status: "Ready for release", AssigneeID: "owner", Transitions: []jira.Transition{{ID: "1", To: "Done"}}},
		}, judged: map[string]bool{}, glint: &glintCheck{},
	}
	now := time.Now()
	s.judge(context.Background(), map[string][]sweptCommit{
		"PROJ-123": {{sha: strings.Repeat("a", 40), when: now, onTarget: true}},
		"PROJ-124": {{sha: strings.Repeat("b", 40), when: now, onTarget: true}},
	})
	if !strings.Contains(output.String(), "PROJ-123 ships dormant (test stand): left in Testing, not moved") {
		t.Fatalf("missing dormant sweep line:\n%s", output.String())
	}
	if !slices.Equal(moved, []string{"PROJ-124"}) {
		t.Fatalf("transition requests = %v, want only ordinary PROJ-124\n%s", moved, output.String())
	}
	if !s.issues["PROJ-124"].Done {
		t.Errorf("ordinary item was not marked Done: %+v", s.issues["PROJ-124"])
	}
	if s.issues["PROJ-123"].Status != "Testing" {
		t.Errorf("dormant status changed: %+v", s.issues["PROJ-123"])
	}
	if !strings.Contains(output.String(), fmt.Sprintf("%s moved to Done", "PROJ-124")) {
		t.Errorf("ordinary move not reported:\n%s", output.String())
	}
}

func TestDormantNoteMoverLeavesTaskInPlace(t *testing.T) {
	var moved []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			moved = append(moved, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	var output bytes.Buffer
	app := &App{Stdout: &output, Stderr: &output}
	mover := &noteMover{app: app, client: jira.New(jira.Credentials{BaseURL: server.URL}), conf: &config.TargetNotes{Transitions: []config.NoteTransition{{From: []string{"Testing"}, To: "Done"}}}, approved: &config.ApprovedRelease{Dormant: []config.DormantReleaseItem{{Key: "PROJ-123", Reason: "test stand"}}}, me: "owner", glint: &glintCheck{}}
	issue := jira.Issue{Key: "PROJ-123", Status: "Testing", AssigneeID: "owner", Transitions: []jira.Transition{{ID: "1", To: "Done"}}}
	if got := mover.delivered(context.Background(), issue); got != "Testing" {
		t.Fatalf("dormant item status = %q", got)
	}
	if len(moved) != 0 {
		t.Fatalf("Jira transition requests = %v", moved)
	}
	if !strings.Contains(output.String(), "PROJ-123 ships dormant (test stand): left in Testing, not moved") {
		t.Fatalf("missing dormant note-mover line:\n%s", output.String())
	}
}

func TestDormantSubtaskDoesNotFollowOrdinaryParent(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			requests = append(requests, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		fmt.Fprint(w, `{"key":"PROJ-124","fields":{"summary":"synthetic subtask","status":{"name":"In Progress","statusCategory":{"key":"indeterminate"}},"assignee":{"accountId":"owner"},"issuetype":{"name":"Sub-task","subtask":true},"parent":{"key":"PROJ-123"},"labels":[]},"transitions":[{"id":"1","to":{"name":"Testing"}}]}`)
	}))
	t.Cleanup(server.Close)
	var output bytes.Buffer
	app := &App{Stdout: &output, Stderr: &output}
	conf := &config.TargetNotes{Transitions: []config.NoteTransition{{From: []string{"In Progress"}, To: "Testing"}}, Subtasks: true}
	mover := &noteMover{app: app, client: jira.New(jira.Credentials{BaseURL: server.URL}), conf: conf, approved: &config.ApprovedRelease{Dormant: []config.DormantReleaseItem{{Key: "PROJ-124", Reason: "test stand"}}}, me: "owner", glint: &glintCheck{}}
	sub := &subtaskRun{mover: mover, notes: &config.JiraNotes{}, me: "owner", reached: map[string]string{"PROJ-123": "Testing"}, delivered: []jira.Issue{{Key: "PROJ-123", Status: "In Progress", Subtasks: []string{"PROJ-124"}}}}
	sub.follow(context.Background())
	if len(requests) != 0 {
		t.Fatalf("subtask followed parent: %v", requests)
	}
	if !strings.Contains(output.String(), "PROJ-124 ships dormant (test stand): left in In Progress, not moved") {
		t.Fatalf("missing dormant subtask line:\n%s", output.String())
	}
}

func TestDormantParentSweepAndDormantChildKeepParentsOpen(t *testing.T) {
	var moved []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			moved = append(moved, strings.Split(r.URL.Path, "/")[5])
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	var output bytes.Buffer
	app := &App{Stdout: &output, Stderr: &output}
	ar := &config.ApprovedRelease{Dormant: []config.DormantReleaseItem{{Key: "PROJ-123", Reason: "parent disabled"}, {Key: "PROJ-126", Reason: "subtask disabled"}}}
	target := &config.DeployTarget{ApprovedRelease: ar, ReleaseNotes: &config.TargetNotes{Transitions: []config.NoteTransition{{From: []string{"Ready for release"}, To: "Done"}}}}
	cfg := &config.Config{Deploy: &config.Deploy{Targets: map[string]*config.DeployTarget{"prod": target}}}
	s := &sweepRun{app: app, r: &deployRun{name: "prod", target: target, cfg: cfg}, conf: &config.Sweep{Days: 30, StaleDays: 3, Done: "Done"}, notes: &config.JiraNotes{}, client: jira.New(jira.Credentials{BaseURL: server.URL}), me: "owner", now: time.Now(), apply: true,
		issues: map[string]jira.Issue{
			"PROJ-123": {Key: "PROJ-123", Status: "Ready for release", AssigneeID: "owner", Subtasks: []string{"PROJ-124"}},
			"PROJ-124": {Key: "PROJ-124", Status: "Done", Done: true, AssigneeID: "owner", Parent: "PROJ-123"},
			"PROJ-125": {Key: "PROJ-125", Status: "Ready for release", AssigneeID: "owner", Subtasks: []string{"PROJ-126"}},
			"PROJ-126": {Key: "PROJ-126", Status: "Done", Done: true, AssigneeID: "owner", Parent: "PROJ-125"},
		}, judged: map[string]bool{}, glint: &glintCheck{},
	}
	allOn := func(string) bool { return true }
	s.finishParent(context.Background(), s.issues["PROJ-123"], allOn)
	s.finishParent(context.Background(), s.issues["PROJ-125"], allOn)
	if len(moved) != 0 {
		t.Fatalf("a parent with dormant work moved: %v", moved)
	}
	s.judge(context.Background(), map[string][]sweptCommit{"PROJ-123": {{when: time.Now(), onTarget: true}}, "PROJ-126": {{when: time.Now(), onTarget: true}}})
	if len(moved) != 0 {
		t.Fatalf("dormant parent or child got transitioned: %v", moved)
	}
	for _, want := range []string{"PROJ-125 stays Ready for release: its subtask PROJ-126 ships dormant", "PROJ-123 ships dormant (parent disabled): left in Ready for release, not moved", "PROJ-126 ships dormant (subtask disabled): left in Done, not moved"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("missing %q:\n%s", want, output.String())
		}
	}
}

func TestDormantHeldSubtaskStaysDuringHandoverRetry(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			requests = append(requests, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		fmt.Fprint(w, `{"key":"PROJ-124","fields":{"summary":"synthetic subtask","status":{"name":"In Progress","statusCategory":{"key":"indeterminate"}},"assignee":{"accountId":"owner"},"issuetype":{"name":"Sub-task","subtask":true},"parent":{"key":"PROJ-123"},"labels":[]},"transitions":[{"id":"1","to":{"name":"Testing"}}]}`)
	}))
	t.Cleanup(server.Close)
	var output bytes.Buffer
	app := &App{Stdout: &output, Stderr: &output}
	mover := &noteMover{app: app, client: jira.New(jira.Credentials{BaseURL: server.URL}), conf: &config.TargetNotes{Transitions: []config.NoteTransition{{From: []string{"In Progress"}, To: "Testing"}}}, approved: &config.ApprovedRelease{Dormant: []config.DormantReleaseItem{{Key: "PROJ-123", Reason: "disabled parent"}}}, me: "owner", glint: &glintCheck{}}
	sub := &subtaskRun{mover: mover, notes: &config.JiraNotes{}, reached: map[string]string{}}
	if unread := app.retryHeld(context.Background(), sub, []heldItem{{key: "PROJ-124", after: "PROJ-123"}}, nil); len(unread) != 0 {
		t.Fatalf("unread held tasks: %v", unread)
	}
	if len(requests) != 0 {
		t.Fatalf("handover retry transitioned dormant subtask: %v", requests)
	}
	if !strings.Contains(output.String(), "PROJ-123 ships dormant (disabled parent): left in In Progress, not moved") {
		t.Fatalf("missing dormant handover line:\n%s", output.String())
	}
}
