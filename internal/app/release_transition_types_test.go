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

func TestTransitionTypesKeepNamedAndFollowingSubtasks(t *testing.T) {
	var moved []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/transitions") {
			key := strings.Split(r.URL.Path, "/")[5]
			moved = append(moved, key)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/") {
			fmt.Fprintf(w, `{"fields":{"summary":"synthetic","status":{"name":"In Progress","statusCategory":{"key":"indeterminate"}},"assignee":{"accountId":"token-owner"},"issuetype":{"name":"Sub-task","subtask":true},"parent":{"key":"PROJ-1"},"labels":[]},"transitions":[{"id":"1","to":{"name":"Testing"}}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	client := jira.New(jira.Credentials{BaseURL: srv.URL})
	var output, stderr bytes.Buffer
	app := &App{Stdout: &output, Stderr: &stderr}
	conf := &config.TargetNotes{Transitions: []config.NoteTransition{{From: []string{"In Progress"}, To: "Testing", Types: []string{"Story", "Task", "Bug"}}}, Subtasks: true}
	mover := &noteMover{app: app, client: client, conf: conf, glint: &glintCheck{}}

	// A subtask named by its own key reaches the same central type check.
	if got := mover.delivered(context.Background(), jira.Issue{Key: "PROJ-2", Type: "Sub-task", Status: "In Progress"}); got != "In Progress" {
		t.Fatalf("named subtask reached %q", got)
	}

	// The allowed parent moves, then its following subtask is filtered there too.
	parent := jira.Issue{Key: "PROJ-1", Type: "task", Status: "In Progress", Transitions: []jira.Transition{{ID: "1", To: "Testing"}}, Subtasks: []string{"PROJ-3"}}
	if got := mover.delivered(context.Background(), parent); got != "Testing" {
		t.Fatalf("parent reached %q", got)
	}
	sub := &subtaskRun{mover: mover, notes: &config.JiraNotes{}, me: "token-owner", reached: map[string]string{"PROJ-1": "Testing"}, delivered: []jira.Issue{parent}}
	sub.follow(context.Background())

	if len(moved) != 1 || moved[0] != "PROJ-1" {
		t.Errorf("transition requests = %v, want only parent PROJ-1", moved)
	}
	for _, want := range []string{
		"PROJ-2 stays In Progress: the transition to Testing moves only Story, Task, Bug",
		"PROJ-3 stays In Progress after its parent PROJ-1: the transition to Testing moves only Story, Task, Bug",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q does not contain %q", stderr.String(), want)
		}
	}
}

func TestSweepTransitionTypesKeepOtherTypesInPlace(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/transitions") {
			requests++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/myself") {
			fmt.Fprint(w, `{"accountId":"token-owner"}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	client := jira.New(jira.Credentials{BaseURL: srv.URL})
	transition := config.NoteTransition{From: []string{"Ready for Release"}, To: "Done", Types: []string{"Task"}}
	notes := &config.TargetNotes{Transitions: []config.NoteTransition{transition}, Sweep: &config.Sweep{Done: "Done"}}
	target := &config.DeployTarget{ReleaseNotes: notes}
	var output strings.Builder
	run := &sweepRun{
		app:    &App{Stdout: &output, Stderr: &strings.Builder{}},
		r:      &deployRun{name: "prod", target: target},
		client: client, conf: notes.Sweep, notes: &config.JiraNotes{},
	}
	issue := jira.Issue{Key: "PROJ-2", Type: "Sub-task", Status: "Ready for Release"}
	transitionToDone, allowed := run.finishing(issue)
	if transitionToDone == nil || allowed {
		t.Fatalf("finishing = (%v, %t), want transition denied for %s", transitionToDone, allowed, issue.Type)
	}
	run.item(t.Context(), issue, []sweptCommit{{sha: "abc", when: time.Now(), onTarget: true}})
	if requests != 0 {
		t.Fatalf("transition requests = %d, want 0", requests)
	}
	want := "PROJ-2 stays Ready for Release: the transition to Done moves only Task"
	if !slices.Contains(run.lines, want) {
		t.Fatalf("sweep lines = %q, want %q", run.lines, want)
	}
	run.move(t.Context(), jira.Issue{
		Key: "PROJ-3", Type: "Sub-task", Status: "Testing", Parent: "PROJ-1",
	}, "accepted with PROJ-1", true)
	reviewerWant := "PROJ-3 stays Testing: the transition to Done moves only Task"
	if !slices.Contains(run.lines, reviewerWant) {
		t.Fatalf("sweep lines = %q, want reviewer transition message %q", run.lines, reviewerWant)
	}
	if requests != 0 {
		t.Fatalf("reviewer transition requests = %d, want 0", requests)
	}
}

// A subtask with the reviewer reaches done through the from of the done
// transition: the transition into that from must move its type too.
func TestSweepTransitionTypesGuardTheHopBeforeDone(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/transitions") {
			requests++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/myself") {
			fmt.Fprint(w, `{"accountId":"token-owner"}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	notes := &config.TargetNotes{Transitions: []config.NoteTransition{
		{From: []string{"Testing"}, To: "Ready for Release", Types: []string{"Task"}},
		{From: []string{"Ready for Release"}, To: "Done", Types: []string{"Sub-task", "Task"}},
	}, Sweep: &config.Sweep{Done: "Done"}}
	run := &sweepRun{
		app:    &App{Stdout: &strings.Builder{}, Stderr: &strings.Builder{}},
		r:      &deployRun{name: "prod", target: &config.DeployTarget{ReleaseNotes: notes}},
		client: jira.New(jira.Credentials{BaseURL: srv.URL}), conf: notes.Sweep, notes: &config.JiraNotes{},
		review: []string{"Testing"}, judged: map[string]bool{},
	}
	run.move(t.Context(), jira.Issue{Key: "PROJ-4", Type: "Sub-task", Status: "Testing", Parent: "PROJ-1"}, "accepted with PROJ-1", true)
	want := "PROJ-4 stays Testing: the transition to Ready for Release moves only Task"
	if requests != 0 || !slices.Contains(run.lines, want) {
		t.Fatalf("transition requests = %d, lines = %q, want 0 and %q", requests, run.lines, want)
	}
}

func TestSweepInFlowUsesIssueLabels(t *testing.T) {
	target := &config.DeployTarget{ApprovedRelease: &config.ApprovedRelease{
		Statuses:      []string{"Ready for Release"},
		LabelStatuses: map[string][]string{"internal": {"Internal Review"}},
	}}
	run := &sweepRun{r: &deployRun{cfg: &config.Config{Deploy: &config.Deploy{Targets: map[string]*config.DeployTarget{"prod": target}}}}}
	if run.inFlow(jira.Issue{Key: "PROJ-1", Status: "Internal Review"}) {
		t.Fatal("unlabelled issue in a status belonging only to another label is in flow")
	}
	if !run.inFlow(jira.Issue{Key: "PROJ-1", Status: "Internal Review", Labels: []string{"internal"}}) {
		t.Fatal("labelled issue's approved status is not in flow")
	}
}

func TestVersionStatusOutputNamesLabelStatuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/rest/api/3/search/jql") {
			fmt.Fprint(w, `{"issues":[{"key":"PROJ-1"}],"isLast":true}`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/PROJ-1") {
			fmt.Fprint(w, `{"fields":{"summary":"synthetic","status":{"name":"In Progress","statusCategory":{"key":"indeterminate"}},"issuetype":{"name":"Task"},"labels":["internal"]}}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	conf := &config.ApprovedRelease{
		Statuses:      []string{"Ready for Release"},
		LabelStatuses: map[string][]string{"internal": {"Internal Review"}},
		Version:       &config.ApprovedReleaseVersion{Refs: map[string]string{}},
	}
	app := &App{Stdout: &strings.Builder{}, Stderr: &strings.Builder{}}
	gate := &approvalGate{client: jira.New(jira.Credentials{BaseURL: srv.URL}), conf: conf, issues: map[string]jira.Issue{}}
	check := &releaseVersionCheck{
		gate: &gateRun{app: app, conf: conf, gate: gate}, collect: true,
		selected: jira.Version{ID: "v1", Name: "Synthetic"},
	}
	check.checkItems(t.Context())
	want := "PROJ-1 (In Progress); move to Ready for Release or Internal Review"
	if len(check.failures) != 1 || !strings.Contains(check.failures[0].Error(), want) || strings.Contains(check.failures[0].Error(), "an approved status") {
		t.Fatalf("version status failures = %v, want per-item statuses %q", check.failures, want)
	}
}

func TestVersionStatusOutputWithoutLabelStatuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/rest/api/3/search/jql") {
			fmt.Fprint(w, `{"issues":[{"key":"PROJ-1"}],"isLast":true}`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/PROJ-1") {
			fmt.Fprint(w, `{"fields":{"summary":"synthetic","status":{"name":"In Progress","statusCategory":{"key":"indeterminate"}},"issuetype":{"name":"Task"},"labels":[]}}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	conf := &config.ApprovedRelease{
		Statuses: []string{"Ready for Release"},
		Version:  &config.ApprovedReleaseVersion{Refs: map[string]string{}},
	}
	app := &App{Stdout: &strings.Builder{}, Stderr: &strings.Builder{}}
	client := jira.New(jira.Credentials{BaseURL: srv.URL})
	gate := &approvalGate{client: client, conf: conf, issues: map[string]jira.Issue{}}
	check := &releaseVersionCheck{
		gate: &gateRun{app: app, conf: conf, gate: gate}, collect: true,
		selected: jira.Version{ID: "v1", Name: "Synthetic"},
	}
	check.checkItems(t.Context())
	want := "Synthetic: PROJ-1 (In Progress); move to Ready for Release"
	if len(check.failures) != 1 || !strings.Contains(check.failures[0].Error(), want) {
		t.Fatalf("version status failures = %v, want %q", check.failures, want)
	}
}
