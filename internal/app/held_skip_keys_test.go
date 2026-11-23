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

func TestRetryHeldSkipKeyDropsRecordAndOrdinaryKeyStillMoves(t *testing.T) {
	for _, tc := range []struct {
		name     string
		skip     bool
		wantMove bool
	}{
		{name: "added to skip_keys after held", skip: true},
		{name: "ordinary held key still moves", wantMove: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			moves := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/transitions") {
					moves++
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issue/PROJ-123") {
					fmt.Fprint(w, `{"key":"PROJ-123","fields":{"status":{"name":"In Progress","statusCategory":{"key":"indeterminate"}},"assignee":{"accountId":"owner"},"issuetype":{"name":"Task","subtask":false},"labels":[]},"transitions":[{"id":"1","to":{"name":"Testing"}}]}`)
					return
				}
				http.Error(w, "unexpected synthetic Jira request", http.StatusNotFound)
			}))
			t.Cleanup(server.Close)
			repo := newEmptyWithheldRepo(t)
			run := &deployRun{repo: repo.repo, name: "test"}
			held := []heldItem{{key: "PROJ-123"}}
			if err := writeHeld(run, held); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			app := &App{Stdout: &output, Stderr: &output}
			var skipKeys []string
			if tc.skip {
				skipKeys = []string{"PROJ-123"}
			}
			mover := &noteMover{app: app, client: jira.New(jira.Credentials{BaseURL: server.URL}), conf: &config.TargetNotes{Transitions: []config.NoteTransition{{From: []string{"In Progress"}, To: "Testing"}}}, standing: skipKeys, glint: &glintCheck{}}
			sub := &subtaskRun{mover: mover, reached: map[string]string{}}
			left := app.retryHeld(context.Background(), sub, held, nil)
			if err := writeHeld(run, left); err != nil {
				t.Fatal(err)
			}
			recorded, err := readHeld(run)
			if err != nil {
				t.Fatal(err)
			}
			if tc.skip {
				if moves != 0 || len(recorded) != 0 {
					t.Fatalf("skip key: transitions=%d held=%+v", moves, recorded)
				}
				if !strings.Contains(output.String(), "PROJ-123 left alone: release_notes.jira.skip_keys") {
					t.Fatalf("missing skip_keys line: %q", output.String())
				}
			} else if moves != 1 || len(recorded) != 0 || !tc.wantMove {
				t.Fatalf("ordinary key: transitions=%d held=%+v", moves, recorded)
			}
		})
	}
}

func TestDeployCheckRetryHeldSkipKeySaysLeftAlone(t *testing.T) {
	var output bytes.Buffer
	app := &App{Stdout: &output, Stderr: &output}
	mover := &noteMover{app: app, standing: []string{"PROJ-123"}, readOnly: true}
	app.retryHeld(context.Background(), &subtaskRun{mover: mover}, []heldItem{{key: "PROJ-123"}}, nil)
	if !strings.Contains(output.String(), "- PROJ-123: left alone") || strings.Contains(output.String(), "NOT MOVED") {
		t.Fatalf("deploy check held output = %q", output.String())
	}
}

func TestHandoverMovesDropsHeldSkipKeyFromConfig(t *testing.T) {
	moves := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/rest/api/3/myself":
			fmt.Fprint(w, `{"accountId":"owner"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issue/PROJ-123"):
			fmt.Fprint(w, `{"key":"PROJ-123","fields":{"status":{"name":"In Progress","statusCategory":{"key":"indeterminate"}},"assignee":{"accountId":"owner"},"issuetype":{"name":"Task","subtask":false},"labels":[]},"transitions":[{"id":"1","to":{"name":"Testing"}}]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/transitions"):
			moves++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected synthetic Jira request", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	setupSubtaskCommitJiraEnv(t, server.URL)
	repo := newEmptyWithheldRepo(t)
	target := &config.DeployTarget{ReleaseNotes: &config.TargetNotes{Transitions: []config.NoteTransition{{From: []string{"In Progress"}, To: "Testing"}}}}
	cfg := &config.Config{ReleaseNotes: &config.ReleaseNotes{Jira: &config.JiraNotes{SkipKeys: []string{"PROJ-123"}}}}
	run := &deployRun{repo: repo.repo, name: "test", target: target, cfg: cfg}
	if err := writeHeld(run, []heldItem{{key: "PROJ-123"}}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	app := &App{Stdout: &output, Stderr: &output}
	if err := app.handoverMoves(context.Background(), run, "test", nil, func(context.Context) (string, error) { return "", nil }); err != nil {
		t.Fatal(err)
	}
	held, err := readHeld(run)
	if err != nil {
		t.Fatal(err)
	}
	if moves != 0 || len(held) != 0 {
		t.Fatalf("handover transitions=%d held=%+v", moves, held)
	}
	if !strings.Contains(output.String(), "PROJ-123 left alone: release_notes.jira.skip_keys") || strings.Contains(output.String(), "release note for") {
		t.Fatalf("handover output = %q", output.String())
	}
}
