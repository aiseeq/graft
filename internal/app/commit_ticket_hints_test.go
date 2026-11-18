package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gittest"
	"github.com/aiseeq/graft/v2/internal/message"
)

func commitHintConfig() *config.Config {
	return &config.Config{
		Ticket:       &config.Ticket{Regexp: regexp.MustCompile(`[A-Z]+-[0-9]+`)},
		ReleaseNotes: &config.ReleaseNotes{Jira: &config.JiraNotes{ProjectKeys: []string{"PROJ"}}},
		Deploy: &config.Deploy{Targets: map[string]*config.DeployTarget{
			"test": {ReleaseNotes: &config.TargetNotes{Transitions: []config.NoteTransition{{From: []string{"Open"}, To: "In Progress", Types: []string{"Task"}}}}},
		}},
	}
}

func setupCommitHintJira(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	configHome := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "jira.env")
	if err := os.WriteFile(envFile, []byte("JIRA_BASE_URL="+server.URL+"\nJIRA_EMAIL=synthetic@example.invalid\nJIRA_API_TOKEN=synthetic-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(configHome, "graft"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "graft", "config.yaml"), []byte("jira:\n  env_file: "+envFile+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Cleanup(server.Close)
	return server
}

func commitHintJiraHandler(assigneeID, assigneeName, issueType, issueStatus string, subtask bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/PROJ-"):
			assignee := "null"
			if assigneeID != "" {
				assignee = fmt.Sprintf(`{"accountId":%q,"displayName":%q}`, assigneeID, assigneeName)
			}
			fmt.Fprintf(w, `{"fields":{"status":{"name":%q,"statusCategory":{"key":"new"}},"assignee":%s,"issuetype":{"name":%q,"subtask":%t}}}`, issueStatus, assignee, issueType, subtask)
		case r.URL.Path == "/rest/api/3/myself":
			fmt.Fprint(w, `{"accountId":"token-user"}`)
		default:
			http.NotFound(w, r)
		}
	}
}

func TestCommitTicketHintsAssigneeAndTransitions(t *testing.T) {
	for _, test := range []struct {
		name        string
		assigneeID  string
		assignee    string
		typeName    string
		status      string
		subtask     bool
		transitions bool
		want        []string
		absent      []string
	}{
		{name: "foreign task", assigneeID: "other", assignee: "Alex Example", typeName: "Task", status: "Open", want: []string{"PROJ-123 is Alex Example 's task", "deploying to test moves PROJ-123 to In Progress"}},
		{name: "own task", assigneeID: "token-user", assignee: "Me", typeName: "Task", status: "Open", absent: []string{"is Me 's task"}, want: []string{"deploying to test moves PROJ-123"}},
		{name: "unassigned task", typeName: "Task", status: "Open", want: []string{"PROJ-123 has no assignee; the commit lands in its Development", "deploying to test moves PROJ-123"}},
		{name: "subtask", assigneeID: "other", assignee: "Alex", typeName: "Sub-task", status: "Open", subtask: true, absent: []string{"deploying to test"}, want: []string{"is Alex 's task"}},
		{name: "no matching target transitions", typeName: "Task", status: "Open", transitions: true, absent: []string{"deploying to test"}},
		{name: "case-insensitive type", typeName: "task", status: "Open", want: []string{"deploying to test moves PROJ-123 to In Progress"}},
		{name: "already at earlier target", typeName: "Task", status: "Testing", transitions: true, absent: []string{"deploying to test"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			setupCommitHintJira(t, commitHintJiraHandler(test.assigneeID, test.assignee, test.typeName, test.status, test.subtask))
			cfg := commitHintConfig()
			if test.transitions {
				cfg.Deploy.Targets["test"].ReleaseNotes.Transitions = nil
			}
			if test.name == "already at earlier target" {
				cfg.Deploy.Targets["test"].ReleaseNotes.Transitions = []config.NoteTransition{
					{From: []string{"Open"}, To: "Testing", Types: []string{"Task"}},
					{From: []string{"Testing"}, To: "Done", Types: []string{"Task"}},
				}
			}
			var stderr bytes.Buffer
			(&App{Stderr: &stderr}).commitTicketHints(t.Context(), cfg, "feat: work PROJ-123")
			for _, want := range test.want {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("hint output %q does not contain %q", stderr.String(), want)
				}
			}
			for _, absent := range test.absent {
				if strings.Contains(stderr.String(), absent) {
					t.Errorf("hint output %q unexpectedly contains %q", stderr.String(), absent)
				}
			}
		})
	}
}

func TestCommitTicketHintsJiraFailuresAreBounded(t *testing.T) {
	for _, test := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{name: "server error", handler: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "private-data-token-xyz", http.StatusInternalServerError)
		}},
		{name: "timeout", handler: func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			setupCommitHintJira(t, test.handler)
			var stderr bytes.Buffer
			start := time.Now()
			(&App{Stderr: &stderr}).commitTicketHints(t.Context(), commitHintConfig(), "feat: work PROJ-123")
			elapsed := time.Since(start)
			if !strings.Contains(stderr.String(), "graft: PROJ-123 not checked in Jira:") {
				t.Fatalf("failure hint = %q", stderr.String())
			}
			if test.name == "server error" && (!strings.Contains(stderr.String(), "HTTP 500") || strings.Contains(stderr.String(), "private-data-token-xyz")) {
				t.Fatalf("failure hint should expose only HTTP class: %q", stderr.String())
			}
			if test.name == "timeout" && elapsed > commitTicketHintTimeout+500*time.Millisecond {
				t.Fatalf("hint took %s, want at most %s plus 500ms", elapsed, commitTicketHintTimeout)
			}
		})
	}
}

func TestCommitTicketHintsTimeoutKeepsCompletedKeyResults(t *testing.T) {
	setupCommitHintJira(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/api/3/issue/PROJ-123":
			fmt.Fprint(w, `{"fields":{"status":{"name":"Open","statusCategory":{"key":"new"}},"assignee":{"accountId":"other","displayName":"Alex"},"issuetype":{"name":"Task","subtask":false}}}`)
		case "/rest/api/3/issue/PROJ-124":
			<-r.Context().Done()
		case "/rest/api/3/myself":
			fmt.Fprint(w, `{"accountId":"token-user"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	var stderr bytes.Buffer
	start := time.Now()
	(&App{Stderr: &stderr}).commitTicketHints(t.Context(), commitHintConfig(), "PROJ-123 PROJ-124")
	if elapsed := time.Since(start); elapsed > commitTicketHintTimeout+500*time.Millisecond {
		t.Fatalf("hint read took %s, want at most timeout plus 500ms", elapsed)
	}
	for _, want := range []string{
		"PROJ-123 is Alex 's task",
		"deploying to test moves PROJ-123 to In Progress",
		"PROJ-124 not checked in Jira: timeout",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("output %q does not contain %q", stderr.String(), want)
		}
	}
	if strings.Contains(stderr.String(), "PROJ-123 not checked in Jira") {
		t.Fatalf("successful key was reported unchecked: %q", stderr.String())
	}
}

func TestCommitTicketHintsCheckAllKeysInParallelAndLimitFive(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	setupCommitHintJira(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/"):
			mu.Lock()
			seen[strings.TrimPrefix(r.URL.Path, "/rest/api/3/issue/")] = true
			mu.Unlock()
			fmt.Fprint(w, `{"fields":{"status":{"name":"Open","statusCategory":{"key":"new"}},"assignee":{"accountId":"other","displayName":"Alex"},"issuetype":{"name":"Task","subtask":false}}}`)
		case r.URL.Path == "/rest/api/3/myself":
			fmt.Fprint(w, `{"accountId":"token-user"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	var stderr bytes.Buffer
	(&App{Stderr: &stderr}).commitTicketHints(t.Context(), commitHintConfig(), "PROJ-123 PROJ-124 PROJ-125 PROJ-126 PROJ-127 PROJ-128")
	if len(seen) != commitTicketHintMaxKeys {
		t.Fatalf("read keys %v, want exactly %d", seen, commitTicketHintMaxKeys)
	}
	for _, key := range []string{"PROJ-123", "PROJ-124", "PROJ-125", "PROJ-126", "PROJ-127"} {
		if !seen[key] || !strings.Contains(stderr.String(), key+" is Alex 's task") {
			t.Errorf("key %s was not checked and hinted; requests=%v, output=%q", key, seen, stderr.String())
		}
	}
	if seen["PROJ-128"] || strings.Contains(stderr.String(), "PROJ-128") {
		t.Fatalf("more than five keys were checked: %v; output=%q", seen, stderr.String())
	}
}

func TestCommitContinuesWhenJiraIsUnavailable(t *testing.T) {
	for _, test := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{name: "server error", handler: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", http.StatusInternalServerError)
		}},
		{name: "timeout", handler: func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			setupCommitHintJira(t, test.handler)
			root := t.TempDir()
			git := func(args ...string) string { return gittest.Run(t, root, args...) }
			git("init", "-q", "-b", "main")
			git("config", "user.name", "Commit Hint Test")
			git("config", "user.email", "commit-hint@example.invalid")
			projectConfig := "schema: 1\nversion: {mode: none}\ncommit: {conventional: false}\nticket:\n  pattern: 'PROJ-[0-9]+'\nrelease_notes:\n  jira:\n    project_keys: [PROJ]\n"
			if err := os.WriteFile(filepath.Join(root, ".graft.yaml"), []byte(projectConfig), 0o600); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(root, "tracked.txt")
			if err := os.WriteFile(file, []byte("before\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			git("add", ".")
			git("commit", "-q", "-m", "base")
			if err := os.WriteFile(file, []byte("after\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			start := time.Now()
			err := (&App{Dir: root, Stdout: io.Discard, Stderr: &stderr}).Commit(context.Background(), CommitOptions{
				Message: message.Source{Parts: []string{"feat: PROJ-123 change"}},
			})
			if err != nil {
				t.Fatalf("Commit: %v (stderr: %s)", err, stderr.String())
			}
			if !strings.Contains(stderr.String(), "graft: PROJ-123 not checked in Jira:") {
				t.Fatalf("missing Jira advisory: %q", stderr.String())
			}
			if test.name == "timeout" && time.Since(start) > commitTicketHintTimeout+1500*time.Millisecond {
				t.Fatalf("commit took %s, want at most timeout plus 1.5s", time.Since(start))
			}
		})
	}
}
