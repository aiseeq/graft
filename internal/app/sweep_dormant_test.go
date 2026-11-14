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
	mover := &noteMover{app: app, client: jira.New(jira.Credentials{BaseURL: server.URL}), conf: &config.TargetNotes{Transitions: []config.NoteTransition{{From: []string{"Testing"}, To: "Done"}}}, dormant: []config.DormantReleaseItem{{Key: "PROJ-123", Reason: "test stand"}}, me: "owner", glint: &glintCheck{}}
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
