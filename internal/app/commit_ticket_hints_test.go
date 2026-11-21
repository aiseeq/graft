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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gittest"
	"github.com/aiseeq/graft/v2/internal/message"
	"github.com/aiseeq/graft/v2/internal/tasks"
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
			(&App{Stderr: &stderr}).commitTicketHints(t.Context(), cfg, "feat: work PROJ-123", nil)
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

func commitTicketNonReleaseHint(t *testing.T, parent string, configure bool, paths []string) string {
	t.Helper()
	setupCommitHintJira(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/api/3/issue/PROJ-2":
			fmt.Fprintf(w, `{"fields":{"status":{"name":"Open","statusCategory":{"key":"new"}},"issuetype":{"name":"Sub-task","subtask":true},"parent":{"key":%q}}}`, parent)
		case "/rest/api/3/myself":
			fmt.Fprint(w, `{"accountId":"token-user"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	cfg := commitHintConfig()
	target := &config.DeployTarget{ApprovedRelease: &config.ApprovedRelease{NonReleaseParents: []string{"PROJ-1"}}}
	if configure {
		target.ApprovedRelease.Ignore = []*regexp.Regexp{regexp.MustCompile(`^ignored/`)}
		cfg.Deploy.Targets["prod"] = target
	}
	var stderr bytes.Buffer
	(&App{Stderr: &stderr}).commitTicketHints(t.Context(), cfg, "PROJ-2", paths)
	return stderr.String()
}

func TestCommitTicketHintsNonReleaseParentWarnsForCode(t *testing.T) {
	out := commitTicketNonReleaseHint(t, "PROJ-1", true, []string{"src/app.go"})
	want := "graft: PROJ-2 is a subtask of PROJ-1, which does not go to release (approved_release.non_release_parents of prod); code for the release goes under a subtask of the release task"
	if !strings.Contains(out, want) {
		t.Fatalf("output %q does not contain %q", out, want)
	}
}

func TestCommitTicketHintsCombineTargetsForOneSubtask(t *testing.T) {
	setupCommitHintJira(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/api/3/issue/PROJ-2":
			fmt.Fprint(w, `{"fields":{"status":{"name":"Open","statusCategory":{"key":"new"}},"issuetype":{"name":"Sub-task","subtask":true},"parent":{"key":"PROJ-1"}}}`)
		case "/rest/api/3/myself":
			fmt.Fprint(w, `{"accountId":"token-user"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	cfg := commitHintConfig()
	for _, name := range []string{"prod", "stage"} {
		cfg.Deploy.Targets[name] = &config.DeployTarget{ApprovedRelease: &config.ApprovedRelease{NonReleaseParents: []string{"PROJ-1"}}}
	}
	var stderr bytes.Buffer
	(&App{Stderr: &stderr}).commitTicketHints(t.Context(), cfg, "PROJ-2", []string{"src/app.go"})
	const warning = "PROJ-2 is a subtask of PROJ-1, which does not go to release"
	if strings.Count(stderr.String(), warning) != 1 || !strings.Contains(stderr.String(), "approved_release.non_release_parents of prod, stage)") {
		t.Fatalf("expected one warning listing both targets, got %q", stderr.String())
	}
}

func TestCommitTicketHintsNonReleaseParentIgnoresIgnoredPaths(t *testing.T) {
	out := commitTicketNonReleaseHint(t, "PROJ-1", true, []string{"ignored/notes.md"})
	if strings.Contains(out, "does not go to release") {
		t.Fatalf("ignored-only commit warned: %q", out)
	}
}

func TestCommitTicketHintsNonReleaseParentRequiresConfiguredParent(t *testing.T) {
	out := commitTicketNonReleaseHint(t, "PROJ-3", true, []string{"src/app.go"})
	if strings.Contains(out, "does not go to release") {
		t.Fatalf("different parent warned: %q", out)
	}
}

func TestCommitTicketHintsNonReleaseParentWithoutSettingStaysQuiet(t *testing.T) {
	out := commitTicketNonReleaseHint(t, "PROJ-1", false, []string{"src/app.go"})
	if strings.Contains(out, "does not go to release") {
		t.Fatalf("unconfigured target warned: %q", out)
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
			(&App{Stderr: &stderr}).commitTicketHints(t.Context(), commitHintConfig(), "feat: work PROJ-123", nil)
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
	(&App{Stderr: &stderr}).commitTicketHints(t.Context(), commitHintConfig(), "PROJ-123 PROJ-124", nil)
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
	(&App{Stderr: &stderr}).commitTicketHints(t.Context(), commitHintConfig(), "PROJ-123 PROJ-124 PROJ-125 PROJ-126 PROJ-127 PROJ-128", nil)
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

func TestGraftCommitNonReleaseParentHintsUseFinalIndexPaths(t *testing.T) {
	for _, test := range []struct {
		name        string
		branch      string
		files       map[string]string
		remove      []string
		renameFrom  string
		renameTo    string
		message     string
		wantKeys    []string
		wantWarning bool
		versionBump bool
	}{
		{
			name:        "deleted code path",
			files:       map[string]string{"src/gone.go": "package gone\n"},
			remove:      []string{"src/gone.go"},
			message:     "feat: remove work PROJ-2",
			wantKeys:    []string{"PROJ-2"},
			wantWarning: true,
		},
		{
			name:        "rename ignored path outward",
			files:       map[string]string{"ignored/guide.md": "guide\n"},
			renameFrom:  "ignored/guide.md",
			renameTo:    "src/guide.go",
			message:     "feat: move work PROJ-2",
			wantKeys:    []string{"PROJ-2"},
			wantWarning: true,
		},
		{
			name:        "rename code path into ignored paths",
			files:       map[string]string{"src/guide.go": "guide\n"},
			renameFrom:  "src/guide.go",
			renameTo:    "ignored/guide.md",
			message:     "feat: hide code PROJ-2",
			wantKeys:    []string{"PROJ-2"},
			wantWarning: true,
		},
		{
			name:        "key from branch",
			branch:      "PROJ-2-work",
			files:       map[string]string{"src/branch.go": "package branch\n"},
			message:     "feat: branch work",
			wantKeys:    []string{"PROJ-2"},
			wantWarning: true,
		},
		{
			name:        "second of two message keys is subtask",
			files:       map[string]string{"src/two.go": "package two\n"},
			message:     "feat: PROJ-123 and PROJ-2",
			wantKeys:    []string{"PROJ-123", "PROJ-2"},
			wantWarning: true,
		},
		{
			name:        "only ignored paths",
			files:       map[string]string{"ignored/only.md": "notes\n"},
			message:     "feat: docs PROJ-2",
			wantKeys:    []string{"PROJ-2"},
			wantWarning: false,
		},
		{
			name: "gate removes code path from final index",
			files: map[string]string{
				"src/gate-remove.go": "package gate\n",
				"ignored/side.md":    "notes\n",
			},
			message:     "feat: gated paths PROJ-2",
			wantKeys:    []string{"PROJ-2"},
			wantWarning: false,
		},
		{
			name:        "version bump is included in final paths",
			files:       map[string]string{"ignored/only.md": "before\n"},
			message:     "feat: ignored work PROJ-2",
			wantKeys:    []string{"PROJ-2"},
			wantWarning: true,
			versionBump: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("GRAFT_PROC", t.TempDir())
			t.Setenv(tasks.AdmittedEnv, "1")
			var mu sync.Mutex
			seen := map[string]int{}
			setupCommitHintJira(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/"):
					key := strings.TrimPrefix(r.URL.Path, "/rest/api/3/issue/")
					mu.Lock()
					seen[key]++
					mu.Unlock()
					if key == "PROJ-2" {
						fmt.Fprint(w, `{"fields":{"status":{"name":"Open","statusCategory":{"key":"new"}},"issuetype":{"name":"Sub-task","subtask":true},"parent":{"key":"PROJ-1"}}}`)
					} else {
						fmt.Fprint(w, `{"fields":{"status":{"name":"Open","statusCategory":{"key":"new"}},"issuetype":{"name":"Task","subtask":false}}}`)
					}
				case r.URL.Path == "/rest/api/3/myself":
					fmt.Fprint(w, `{"accountId":"token-user"}`)
				default:
					http.NotFound(w, r)
				}
			}))
			root := t.TempDir()
			git := func(args ...string) string { return gittest.Run(t, root, args...) }
			git("init", "-q", "-b", "main")
			git("config", "user.name", "Commit Hint Test")
			git("config", "user.email", "commit-hint@example.invalid")
			for name, contents := range test.files {
				path := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			versionConfig := "version: {mode: none}\n"
			if test.versionBump {
				versionConfig = "version: {mode: file, file: VERSION}\n"
				if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte("1.0.0\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			projectConfig := `schema: 1
` + versionConfig + `
commit: {conventional: false}
ticket: {pattern: 'PROJ-[0-9]+'}
envs: {local: {}}
release_notes:
  jira: {project_keys: [PROJ]}
deploy:
  remote: origin
  targets:
    prod:
      env: local
      run: [sh, deploy.sh]
      deployed_sha: {path: /tmp/graft-commit-hints-deployed-sha}
      approved_release:
        prefix: Product
        ignore_paths: ["ignored/**"]
        non_release_parents: [PROJ-1]
gate: [{task: mark}]
tasks:
  mark:
    desc: marker for commit hint ordering
    run: [[sh, gate-marker.sh]]
`
			if err := os.WriteFile(filepath.Join(root, ".graft.yaml"), []byte(projectConfig), 0o600); err != nil {
				t.Fatal(err)
			}
			gateScript := "#!/bin/sh\nif [ -f src/gate-remove.go ]; then git show HEAD:src/gate-remove.go > src/gate-remove.go; fi\necho gate-marker >&2\n"
			if err := os.WriteFile(filepath.Join(root, "gate-marker.sh"), []byte(gateScript), 0o600); err != nil {
				t.Fatal(err)
			}
			git("add", ".")
			git("commit", "-q", "-m", "base")
			if test.branch != "" {
				git("checkout", "-q", "-b", test.branch)
			}
			for _, name := range test.remove {
				if err := os.Remove(filepath.Join(root, name)); err != nil {
					t.Fatal(err)
				}
			}
			if test.renameFrom != "" {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(root, test.renameTo)), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(root, test.renameFrom), filepath.Join(root, test.renameTo)); err != nil {
					t.Fatal(err)
				}
			}
			for name, contents := range test.files {
				if slices.Contains(test.remove, name) || name == test.renameFrom {
					continue
				}
				if err := os.WriteFile(filepath.Join(root, name), []byte(contents+"updated\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if test.renameTo != "" {
				if err := os.WriteFile(filepath.Join(root, test.renameTo), []byte(test.files[test.renameFrom]), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			git("add", "-A")
			var output bytes.Buffer
			err := (&App{Dir: root, Stdout: &output, Stderr: &output}).Commit(context.Background(), CommitOptions{
				Message: message.Source{Parts: []string{test.message}},
			})
			if err != nil {
				t.Fatalf("Commit: %v (output: %s)", err, output.String())
			}
			warning := strings.Contains(output.String(), "graft: PROJ-2 is a subtask of PROJ-1, which does not go to release")
			if warning != test.wantWarning {
				t.Fatalf("release parent warning = %t, want %t; output: %q", warning, test.wantWarning, output.String())
			}
			legacyHintAt := strings.Index(output.String(), "PROJ-2 has no assignee")
			gateAt := strings.Index(output.String(), "gate-marker")
			if legacyHintAt < 0 || gateAt < 0 || legacyHintAt > gateAt {
				t.Fatalf("legacy Jira hint must precede gate output: %q", output.String())
			}
			mu.Lock()
			defer mu.Unlock()
			for _, key := range test.wantKeys {
				if seen[key] != 1 {
					t.Errorf("Jira read count for %s = %d, want one", key, seen[key])
				}
			}
		})
	}
}
