package app

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/envs"
)

// Review finding 4: with version.match and no prefix, and no dormant entry,
// the selected Jira version does not become one of the approved versions that
// name the release; only a prefix does that, as before.
func TestVersionOnlyGateLeavesApprovedVersionsAsBefore(t *testing.T) {
	setFreezeGitConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/rest/api/3/issue/PROJ-11":
			fmt.Fprint(w, `{"key":"PROJ-11","fields":{"summary":"synthetic","issuetype":{"name":"Task"},"status":{"name":"Ready for release","statusCategory":{"key":"indeterminate"}},"fixVersions":[{"id":"1","name":"Product 05.11.26"}],"subtasks":[]}}`)
		case r.URL.Path == "/rest/api/3/search/jql":
			fmt.Fprint(w, `{"issues":[{"key":"PROJ-11"}],"isLast":true}`)
		case r.URL.Path == "/rest/api/3/version/1":
			fmt.Fprint(w, `{"id":"1","name":"Product 05.11.26","approvers":[{"accountId":"a","status":"APPROVED"}]}`)
		case r.URL.Path == "/rest/api/3/user":
			fmt.Fprint(w, `{"accountId":"a","displayName":"Approver"}`)
		default:
			t.Errorf("unexpected Jira request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	configRoot := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "jira.env")
	if err := os.WriteFile(envFile, []byte("JIRA_BASE_URL="+server.URL+"\nJIRA_EMAIL=synthetic@example.invalid\nJIRA_API_TOKEN="+"synthetic"+"-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(configRoot, "graft"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configRoot, "graft", "config.yaml"), []byte("jira:\n  env_file: "+envFile+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", configRoot)

	repo, cfg, _ := freezeTrunkFixture(t)
	prev := strings.TrimSpace(freezeGit(t, repo.Root, "rev-parse", "HEAD"))
	writeFreezeCommit(t, repo.Root, "feat: PROJ-11 work")
	head := strings.TrimSpace(freezeGit(t, repo.Root, "rev-parse", "HEAD"))
	cfg.Version = config.Version{Mode: config.ModeNone}
	cfg.Envs = map[string]*config.Env{"local": {}}
	cfg.ReleaseNotes = &config.ReleaseNotes{Jira: &config.JiraNotes{ProjectKeys: []string{"PROJ"}}}
	target := &config.DeployTarget{Env: "local", DeployedSHA: &config.DeployedSHA{Path: filepath.Join(t.TempDir(), "sha")},
		ApprovedRelease: &config.ApprovedRelease{Statuses: []string{"Ready for release", "Done"}, Version: &config.ApprovedReleaseVersion{Match: "^Product"}}}
	cfg.Deploy.Targets["prod"] = target
	env, err := envs.Get(cfg, repo.Root, "local")
	if err != nil {
		t.Fatal(err)
	}
	r := &deployRun{name: "prod", cfg: cfg, repo: repo, target: target, env: env, prev: prev, head: head, dryRun: true}
	if err := (&App{Stdout: io.Discard, Stderr: io.Discard}).checkApproved(t.Context(), r, false); err != nil {
		t.Fatal(err)
	}
	if !r.gated || len(r.approvedVersions) != 0 {
		t.Fatalf("gated=%v approvedVersions=%v; want the gate passed and no approved versions without a prefix", r.gated, r.approvedVersions)
	}
}
