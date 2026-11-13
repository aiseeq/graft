package app

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
