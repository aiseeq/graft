package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/envs"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/jira"
	"github.com/aiseeq/graft/v2/internal/tasks"
)

func TestFreezeAdvisoryFailureStopsDeployFromOtherBranch(t *testing.T) {
	setFreezeGitConfig(t)
	run := func(withFreeze bool) (*deployRun, error, string, string) {
		t.Helper()
		root := t.TempDir()
		advisoryRan := filepath.Join(root, "advisory-ran")
		deployRan := filepath.Join(root, "deploy-ran")
		repo, cfg, _ := freezeTrunkFixture(t)
		target := &config.DeployTarget{
			Env: "local", Deps: []string{"advice"},
			Run:    config.Command{Argv: []string{"sh", "-c", "touch \"$1\"", "deploy", deployRan}, Source: "synthetic deploy marker"},
			Freeze: &config.Freeze{Branch: "release/{version}", Advisory: []string{"advice"}},
		}
		cfg.Tasks = map[string]*config.Task{
			"advice": {Run: []config.Step{{Argv: []string{"sh", "-c", "touch \"$1\"; exit 1", "advice", advisoryRan}}}},
		}
		cfg.Version = config.Version{Mode: config.ModeNone}
		cfg.Envs = map[string]*config.Env{"local": {}}
		cfg.Deploy.Targets = map[string]*config.DeployTarget{"prod": target}
		if withFreeze {
			tag := "freeze/prod/1.2.3"
			freezeGit(t, repo.Root, "tag", "-a", tag, "-m", "version: Product 1.2.3\nbranch: release/1.2.3\ntime: 2026-11-01T12:00:00Z")
			freezeGit(t, repo.Root, "push", "origin", "refs/tags/"+tag)
		}
		env, err := envs.Get(cfg, repo.Root, "local")
		if err != nil {
			t.Fatal(err)
		}
		r := &deployRun{name: "prod", cfg: cfg, repo: repo, target: target, env: env, tasks: tasks.NewRunner(repo.Root, cfg, io.Discard, io.Discard)}
		err = (&App{Stdout: io.Discard, Stderr: io.Discard}).deploy(context.Background(), r, DeployOptions{Target: "prod"})
		return r, err, advisoryRan, deployRan
	}
	r, err, advisoryRan, deployRan := run(true)
	if err == nil || !strings.Contains(err.Error(), "advice") {
		t.Fatalf("deploy result = %v, want the failed advisory task's error", err)
	}
	if r.freeze == nil || r.freeze.target != "prod" || r.freeze.config != r.target || !slices.Contains(r.freeze.config.Freeze.Advisory, "advice") || r.freeze.branch == r.branch {
		t.Fatalf("active freeze = %+v; deploy branch = %q; want prod freeze with advice on a different branch", r.freeze, r.branch)
	}
	if _, err := os.Stat(advisoryRan); err != nil {
		t.Fatalf("advisory task did not leave its run marker: %v", err)
	}
	if _, err := os.Stat(deployRan); !os.IsNotExist(err) {
		t.Fatalf("deploy script ran after the advisory failure: stat error = %v", err)
	}
	control, controlErr, controlAdvisoryRan, controlDeployRan := run(false)
	if control.freeze != nil || controlErr == nil || !strings.Contains(controlErr.Error(), "advice") {
		t.Fatalf("without a freeze tag: freeze=%+v error=%v; want ordinary dependency failure", control.freeze, controlErr)
	}
	if controlErr.Error() != err.Error() {
		t.Fatalf("failure with freeze = %q, without freeze = %q; branch-mismatched advisory should follow ordinary dependency behavior", err, controlErr)
	}
	if _, err := os.Stat(controlAdvisoryRan); err != nil {
		t.Fatalf("control dependency did not run: %v", err)
	}
	if _, err := os.Stat(controlDeployRan); !os.IsNotExist(err) {
		t.Fatalf("control deploy script ran after its failed dependency: stat error = %v", err)
	}
}

func TestFreezeSubtaskFollowsParentVersion(t *testing.T) {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	gate := &approvalGate{issues: map[string]jira.Issue{
		"PROJ-124": {Key: "PROJ-124", Parent: "PROJ-123", Type: "Sub-task"},
		"PROJ-123": {Key: "PROJ-123", Type: "Bug", Created: at.Add(-time.Hour), FixVersions: []jira.Version{{ID: "version-123"}}},
	}, history: map[string]fixVersionHistoryResult{"PROJ-123\x00version-123\x00" + at.Format(time.RFC3339Nano): {hasAddition: true}}}
	var matched []string
	if err := checkFreezeIssue(t.Context(), gate, &activeFreeze{versionIDs: []string{"version-123"}, time: at}, "PROJ-124", &matched); err != nil {
		t.Fatal(err)
	}
	if len(matched) != 1 || matched[0] != "PROJ-124" {
		t.Fatalf("matched issues = %v, want the subtask to follow its parent's version", matched)
	}
}

func TestFreezeRejectsLateBugOnVersionedParent(t *testing.T) {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	gate := freezeParentGate(at, "Bug")
	var matched []string
	err := checkFreezeIssue(t.Context(), gate, &activeFreeze{versionIDs: []string{"version-123"}, time: at, config: &config.DeployTarget{Freeze: &config.Freeze{}}}, "PROJ-124", &matched)
	if err == nil || !strings.Contains(err.Error(), "PROJ-123 joined") {
		t.Fatalf("late Bug result = %v, want parent freeze refusal", err)
	}
}

func TestFreezeAllowsLateTaskOnVersionedParent(t *testing.T) {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	gate := freezeParentGate(at, "Task")
	f := &activeFreeze{versionIDs: []string{"version-123"}, time: at, config: &config.DeployTarget{Freeze: &config.Freeze{Types: []string{"Task"}}}}
	var matched []string
	if err := checkFreezeIssue(t.Context(), gate, f, "PROJ-124", &matched); err != nil {
		t.Fatalf("late Task result = %v, want configured type exception", err)
	}
	if len(matched) != 1 || matched[0] != "PROJ-124" {
		t.Fatalf("matched issues = %v, want subtask matched by parent", matched)
	}
}

func freezeParentGate(at time.Time, typ string) *approvalGate {
	return &approvalGate{issues: map[string]jira.Issue{
		"PROJ-124": {Key: "PROJ-124", Parent: "PROJ-123", Type: "Sub-task"},
		"PROJ-123": {Key: "PROJ-123", Type: typ, Created: at.Add(time.Hour), FixVersions: []jira.Version{{ID: "version-123"}}},
	}, history: map[string]fixVersionHistoryResult{"PROJ-123\x00version-123\x00" + at.Format(time.RFC3339Nano): {added: true, hasAddition: true}}}
}

func TestDeployFreezeTrunkRequiresTrunkBranch(t *testing.T) {
	setFreezeGitConfig(t)
	repo, cfg, _ := freezeTrunkFixture(t)
	freezeGit(t, repo.Root, "switch", "-c", "feature/PROJ-123")
	err := deployFreezeTrunk(repo, cfg)
	if err == nil || !strings.Contains(err.Error(), "trunk branch") {
		t.Fatalf("freeze from feature branch = %v, want refusal", err)
	}
}

func TestDeployFreezeTrunkRequiresCurrentRemoteHead(t *testing.T) {
	setFreezeGitConfig(t)
	repo, cfg, remote := freezeTrunkFixture(t)
	other := filepath.Join(filepath.Dir(repo.Root), "other")
	freezeGit(t, filepath.Dir(repo.Root), "clone", "-q", remote, other)
	freezeGit(t, other, "config", "user.name", "Test User")
	freezeGit(t, other, "config", "user.email", "test@example.invalid")
	writeFreezeCommit(t, other, "remote advancement")
	freezeGit(t, other, "push", "origin", "trunk")
	err := deployFreezeTrunk(repo, cfg)
	if err == nil || !strings.Contains(err.Error(), "HEAD differs from remote trunk") {
		t.Fatalf("freeze at stale HEAD = %v, want remote HEAD refusal", err)
	}
}

func TestDeployFreezeTrunkRequiresCleanTree(t *testing.T) {
	setFreezeGitConfig(t)
	repo, cfg, _ := freezeTrunkFixture(t)
	if err := os.WriteFile(filepath.Join(repo.Root, "untracked"), []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := deployFreezeTrunk(repo, cfg)
	if err == nil || !strings.Contains(err.Error(), "untracked") {
		t.Fatalf("freeze with dirty tree = %v, want tree check refusal", err)
	}
}

func TestHotfixReleasePageListsHotfixItem(t *testing.T) {
	var pageBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/rest/api/3/issue/PROJ-123":
			fmt.Fprint(w, `{"fields":{"summary":"synthetic hotfix item","issuetype":{"name":"Bug"},"status":{"name":"Done","statusCategory":{"key":"done"}},"fixVersions":[]}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/rest/api/3/project/PROJ/versions":
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == "/wiki/api/v2/pages/500":
			fmt.Fprint(w, `{"id":"500","spaceId":"space-1"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/wiki/api/v2/pages":
			fmt.Fprint(w, `{"results":[]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/wiki/api/v2/pages":
			var body struct {
				Body struct {
					Value string `json:"value"`
				} `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode create page: %v", err)
			}
			pageBody = body.Body.Value
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"501","title":"Release 1.2.3 hotfix","spaceId":"space-1","version":{"number":1},"body":{"storage":{"value":%q}}}`, body.Body.Value)
		default:
			t.Errorf("unexpected Jira request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	configRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(configRoot, "graft"), 0o700); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(t.TempDir(), "jira.env")
	if err := os.WriteFile(envFile, []byte("JIRA_BASE_URL="+server.URL+"\nJIRA_EMAIL=synthetic@example.invalid\nJIRA_API_TOKEN="+"synthetic"+"-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configRoot, "graft", "config.yaml"), []byte("jira:\n  env_file: "+envFile+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	r := &deployRun{name: "prod", branch: "hotfix/PROJ-123", version: "1.2.3", hotfix: true, now: time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), hotfixItems: []string{"PROJ-123"}, target: &config.DeployTarget{Window: &config.Window{Location: time.UTC}, ReleasePage: &config.ReleasePage{Parent: "500", Title: "Release {release}"}, ReleaseVersion: &config.ReleaseVersion{HotfixTemplate: &config.VersionName{Template: "Release {dd}.{mm}.{yy} hotfix"}}}, cfg: &config.Config{ReleaseNotes: &config.ReleaseNotes{Jira: &config.JiraNotes{ProjectKeys: []string{"PROJ"}}}}}
	if err := (&App{Stdout: os.Stdout, Stderr: os.Stderr}).releasePage(t.Context(), r, r.target.ReleasePage); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pageBody, "PROJ-123") || !strings.Contains(pageBody, "synthetic hotfix item") {
		t.Fatalf("created release page omitted hotfix item: %s", pageBody)
	}
}

func freezeTrunkFixture(t *testing.T) (*gitx.Repo, *config.Config, string) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	freezeGit(t, root, "init", "--bare", "-q", remote)
	freezeGit(t, root, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/trunk")
	freezeGit(t, work, "init", "-q", "-b", "trunk")
	freezeGit(t, work, "config", "user.name", "Test User")
	freezeGit(t, work, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(work, "file"), []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	freezeGit(t, work, "add", "file")
	freezeGit(t, work, "commit", "-q", "-m", "base")
	freezeGit(t, work, "remote", "add", "origin", remote)
	freezeGit(t, work, "push", "-u", "origin", "trunk")
	freezeGit(t, work, "remote", "set-head", "origin", "trunk")
	repo, err := gitx.Open(work)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Deploy: &config.Deploy{Remote: "origin", Targets: map[string]*config.DeployTarget{}}}
	return repo, cfg, remote
}

func writeFreezeCommit(t *testing.T, work, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(work, "file"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	freezeGit(t, work, "add", "file")
	freezeGit(t, work, "commit", "-q", "-m", content)
}

func freezeGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = freezeGitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func freezeGitEnv() []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if key == "GIT_CONFIG_GLOBAL" || key == "GIT_CONFIG_NOSYSTEM" || key == "GIT_CONFIG_PARAMETERS" {
			continue
		}
		env = append(env, value)
	}
	return append(env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
}

func setFreezeGitConfig(t *testing.T) {
	t.Helper()
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
}
