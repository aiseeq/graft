package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/jira"
)

func subtaskCommitJira(t *testing.T, parent string, moved *bool) (*jira.Client, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/rest/api/3/search/jql":
			if got := r.URL.Query().Get("jql"); got != `parent in ("PROJ-1")` {
				t.Errorf("parent search JQL = %q; want only named parent PROJ-1", got)
			}
			fmt.Fprintf(w, `{"issues":[{"key":"PROJ-2","fields":{"status":{"name":"In Progress"},"issuetype":{"subtask":true},"parent":{"key":%q}}}],"isLast":true}`, parent)
		case r.URL.Path == "/rest/api/3/issue/PROJ-1" && r.Method == http.MethodGet:
			fmt.Fprint(w, `{"key":"PROJ-1","fields":{"status":{"name":"In Progress","statusCategory":{"key":"indeterminate"}},"assignee":{"accountId":"owner","displayName":"Owner"},"issuetype":{"name":"Task","subtask":false},"subtasks":[{"key":"PROJ-2"}]},"transitions":[{"id":"31","to":{"name":"Testing"}}]}`)
		case r.URL.Path == "/rest/api/3/issue/PROJ-2" && r.Method == http.MethodGet:
			fmt.Fprint(w, `{"key":"PROJ-2","fields":{"status":{"name":"In Progress","statusCategory":{"key":"indeterminate"}},"assignee":{"accountId":"owner","displayName":"Owner"},"issuetype":{"name":"Sub-task","subtask":true},"parent":{"key":"PROJ-1"}},"transitions":[{"id":"31","to":{"name":"Testing"}}]}`)
		case r.URL.Path == "/rest/api/3/myself":
			fmt.Fprint(w, `{"accountId":"owner"}`)
		case strings.HasSuffix(r.URL.Path, "/comment") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
		case strings.HasSuffix(r.URL.Path, "/transitions") && r.Method == http.MethodPost:
			*moved = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Jira request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return jira.New(jira.Credentials{BaseURL: srv.URL}), srv.URL
}

func setupSubtaskCommitJiraEnv(t *testing.T, baseURL string) {
	t.Helper()
	root := t.TempDir()
	envFile := filepath.Join(root, "jira.env")
	if err := os.WriteFile(envFile, []byte("JIRA_BASE_URL="+baseURL+"\nJIRA_EMAIL=test@example.invalid\nJIRA_API_TOKEN=synthetic-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configRoot := filepath.Join(root, "config")
	if err := os.MkdirAll(filepath.Join(configRoot, "graft"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configRoot, "graft", "config.yaml"), []byte("jira:\n  env_file: "+envFile+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", configRoot)
}

func subtaskCommitRun(t *testing.T) (*deployRun, string) {
	return subtaskCommitRunWithCommit(t, "feat: PROJ-2 work")
}

func subtaskCommitRunWithCommit(t *testing.T, subject string) (*deployRun, string) {
	t.Helper()
	setFreezeGitConfig(t)
	repo, _, _ := freezeTrunkFixture(t)
	writeFreezeCommit(t, repo.Root, subject)
	head := freezeGit(t, repo.Root, "rev-parse", "HEAD")
	cfg := &config.Config{ReleaseNotes: &config.ReleaseNotes{Jira: &config.JiraNotes{ProjectKeys: []string{"PROJ"}}}}
	target := &config.DeployTarget{ApprovedRelease: &config.ApprovedRelease{}}
	r := &deployRun{repo: repo, cfg: cfg, target: target}
	return r, head
}

func parentSearchJira(t *testing.T, status int, body string, searches *atomic.Int32) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/3/search/jql" {
			t.Errorf("unexpected Jira request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
			return
		}
		searches.Add(1)
		if got := r.URL.Query().Get("jql"); got != `parent in ("PROJ-1")` {
			t.Errorf("parent search JQL = %q; want only named parent PROJ-1", got)
		}
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestHandoverIgnoresReturnedSubtaskWithoutDeployedCommit(t *testing.T) {
	var searches atomic.Int32
	baseURL := parentSearchJira(t, http.StatusOK, `{"issues":[{"key":"PROJ-3","fields":{"issuetype":{"subtask":true},"parent":{"key":"PROJ-1"}}}],"isLast":true}`, &searches)
	setupSubtaskCommitJiraEnv(t, baseURL)
	r, head := subtaskCommitRunWithCommit(t, "feat: PROJ-5 work")
	_, err := namedHandoverItems(context.Background(), r, "test", []string{"PROJ-1"}, func(context.Context) (string, error) { return head, nil }, nil)
	if err == nil || !strings.Contains(err.Error(), "PROJ-1 has no work commit in the deployed history of test") {
		t.Fatalf("named handover error = %v; want no-work-commit refusal", err)
	}
	if searches.Load() != 1 {
		t.Fatalf("parent searches = %d; want 1", searches.Load())
	}
}

func TestHandoverIgnoresNonSubtaskWithParentField(t *testing.T) {
	var searches atomic.Int32
	baseURL := parentSearchJira(t, http.StatusOK, `{"issues":[{"key":"PROJ-3","fields":{"issuetype":{"subtask":false},"parent":{"key":"PROJ-1"}}}],"isLast":true}`, &searches)
	setupSubtaskCommitJiraEnv(t, baseURL)
	r, head := subtaskCommitRunWithCommit(t, "feat: PROJ-3 work")
	_, err := namedHandoverItems(context.Background(), r, "test", []string{"PROJ-1"}, func(context.Context) (string, error) { return head, nil }, nil)
	if err == nil || !strings.Contains(err.Error(), "PROJ-1 has no work commit in the deployed history of test") {
		t.Fatalf("named handover error = %v; want no-work-commit refusal", err)
	}
}

func TestHandoverParentSearchFailureIsReported(t *testing.T) {
	var searches atomic.Int32
	baseURL := parentSearchJira(t, http.StatusInternalServerError, `synthetic Jira search failure`, &searches)
	setupSubtaskCommitJiraEnv(t, baseURL)
	r, head := subtaskCommitRunWithCommit(t, "feat: PROJ-2 work")
	_, err := namedHandoverItems(context.Background(), r, "test", []string{"PROJ-1"}, func(context.Context) (string, error) { return head, nil }, nil)
	if err == nil || !strings.Contains(err.Error(), "reading parents of deployed work items") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("named handover error = %v; want parent-search context and HTTP 500", err)
	}
}

func TestHandoverWithOwnCommitDoesNotSearchParents(t *testing.T) {
	var searches atomic.Int32
	baseURL := parentSearchJira(t, http.StatusOK, `{"issues":[],"isLast":true}`, &searches)
	setupSubtaskCommitJiraEnv(t, baseURL)
	r, head := subtaskCommitRunWithCommit(t, "feat: PROJ-1 work")
	held, err := namedHandoverItems(context.Background(), r, "test", []string{"PROJ-1"}, func(context.Context) (string, error) { return head, nil }, nil)
	if err != nil || len(held) != 1 || held[0].key != "PROJ-1" {
		t.Fatalf("named handover = %+v, %v; want PROJ-1 accepted by own commit", held, err)
	}
	if searches.Load() != 0 {
		t.Fatalf("parent searches = %d; want 0 when the named key has its own commit", searches.Load())
	}
}

func TestHandoverParentFromSubtaskCommitMoves(t *testing.T) {
	var moved bool
	client, baseURL := subtaskCommitJira(t, "PROJ-1", &moved)
	setupSubtaskCommitJiraEnv(t, baseURL)
	r, head := subtaskCommitRun(t)
	held, err := namedHandoverItems(context.Background(), r, "test", []string{"PROJ-1"}, func(context.Context) (string, error) { return head, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || held[0].key != "PROJ-1" {
		t.Fatalf("held = %+v; want parent PROJ-1", held)
	}
	app := &App{Stdout: io.Discard, Stderr: io.Discard}
	mover := &noteMover{app: app, client: client, conf: &config.TargetNotes{Transitions: []config.NoteTransition{{From: []string{"In Progress"}, To: "Testing"}}}, glint: &glintCheck{}}
	sub := &subtaskRun{mover: mover, notes: &config.JiraNotes{}, reached: map[string]string{}}
	app.retryHeldNamed(context.Background(), sub, held, nil, map[string]bool{"PROJ-1": true})
	if !moved {
		t.Fatal("handover did not transition parent PROJ-1")
	}
}

func TestHandoverSubtaskOfAnotherParentDoesNotQualify(t *testing.T) {
	var moved bool
	_, baseURL := subtaskCommitJira(t, "PROJ-9", &moved)
	setupSubtaskCommitJiraEnv(t, baseURL)
	r, head := subtaskCommitRun(t)
	_, err := namedHandoverItems(context.Background(), r, "test", []string{"PROJ-1"}, func(context.Context) (string, error) { return head, nil }, nil)
	if err == nil || !strings.Contains(err.Error(), "PROJ-1 has no work commit in the deployed history of test") {
		t.Fatalf("named handover error = %v; want previous no-work-commit refusal", err)
	}
}

func TestHandoverLongHistorySearchesOnlyNamedParents(t *testing.T) {
	var moved bool
	_, baseURL := subtaskCommitJira(t, "PROJ-1", &moved)
	setupSubtaskCommitJiraEnv(t, baseURL)
	r, _ := subtaskCommitRun(t)
	var keys []string
	for n := 2; n < 302; n++ {
		keys = append(keys, "PROJ-"+strconv.Itoa(n))
	}
	writeFreezeCommit(t, r.repo.Root, "feat: "+strings.Join(keys, " "))
	head := freezeGit(t, r.repo.Root, "rev-parse", "HEAD")
	if len(keys) < 300 {
		t.Fatalf("fixture has %d committed keys; want at least 300", len(keys))
	}
	held, err := namedHandoverItems(context.Background(), r, "test", []string{"PROJ-1"}, func(context.Context) (string, error) { return head, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || held[0].key != "PROJ-1" {
		t.Fatalf("held = %+v; want named parent PROJ-1", held)
	}
}
