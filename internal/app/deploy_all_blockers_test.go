package app

import (
	"context"
	"errors"
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
	"github.com/aiseeq/graft/v2/internal/gittest"
)

// A live deploy of the commit a confirm target already runs refuses it
// without --redeploy, and names in the same refusal what --redeploy would
// refuse next: here an item of a commit withheld from an earlier release.
func TestDeploySameCommitRefusalNamesRedeployBlockers(t *testing.T) {
	setFreezeGitConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/api/3/issue/PROJ-12" {
			fmt.Fprint(w, `{"key":"PROJ-12","fields":{"summary":"synthetic","issuetype":{"name":"Task"},"status":{"name":"In Progress","statusCategory":{"key":"indeterminate"}},"fixVersions":[],"subtasks":[]}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	setJiraTestEnv(t, server.URL)

	repo, cfg, _ := freezeTrunkFixture(t)
	head := strings.TrimSpace(freezeGit(t, repo.Root, "rev-parse", "HEAD"))
	freezeGit(t, repo.Root, "switch", "-q", "-c", "withheld")
	writeFreezeCommit(t, repo.Root, "feat: PROJ-12 withheld work")
	withheld := strings.TrimSpace(freezeGit(t, repo.Root, "rev-parse", "HEAD"))
	freezeGit(t, repo.Root, "switch", "-q", "trunk")

	cfg.Version = config.Version{Mode: config.ModeNone}
	cfg.Envs = map[string]*config.Env{"local": {}}
	cfg.ReleaseNotes = &config.ReleaseNotes{Jira: &config.JiraNotes{ProjectKeys: []string{"PROJ"}}}
	sha := filepath.Join(t.TempDir(), "sha")
	for path, content := range map[string]string{sha: head + "\n", sha + withheldSuffix: withheld + "\n"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	target := &config.DeployTarget{Env: "local", Confirm: "sudo", DeployedSHA: &config.DeployedSHA{Path: sha},
		ApprovedRelease: &config.ApprovedRelease{Statuses: []string{"Ready for release", "Done"}}}
	cfg.Deploy.Targets["prod"] = target
	env, err := envs.Get(cfg, repo.Root, "local")
	if err != nil {
		t.Fatal(err)
	}
	r := &deployRun{name: "prod", cfg: cfg, repo: repo, target: target, env: env, head: head, branch: "trunk"}
	err = (&App{Stdout: io.Discard, Stderr: io.Discard}).checkTargetRuns(t.Context(), r, DeployOptions{})
	if err == nil || !strings.Contains(err.Error(), "prod already runs "+head[:12]) || !strings.Contains(err.Error(), "feat: PROJ-12 withheld work: PROJ-12") {
		t.Fatalf("checkTargetRuns = %v; want the same-commit refusal and the withheld PROJ-12", err)
	}
}

// A deploy that went live while only its merge into the trunk failed says
// so in the ticket journal: the release is live, the merge is not done.
func TestDeployJournalNamesLiveReleaseWithFailedTrunkMerge(t *testing.T) {
	repo := ticketJournalRepo(t)
	gittest.Run(t, repo.Root, "commit", "--allow-empty", "-q", "-m", "fix: proj-123-fix deployed")
	head := gittest.Run(t, repo.Root, "rev-parse", "HEAD")
	log := ticketJournalStub(t, `[{"path":"docs/tasks/ticket.md","slug":"proj-123-fix","status":"в работе"}]`, false)
	r := &deployRun{repo: repo, name: "prod", head: head, version: "1.2.3", trunkMergeFailed: true}
	(&App{Stderr: io.Discard}).noteDeployJournal(context.Background(), r, &ExitCodeError{Code: ExitDeployTrunkMerge, Err: errors.New("synthetic merge failure")})
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "deploy prod 1.2.3: live, the merge into the trunk failed") {
		t.Fatalf("the journal does not say the release went live: %q", got)
	}
	// A script that exits 3 itself fails the deploy.
	r.trunkMergeFailed = false
	(&App{Stderr: io.Discard}).noteDeployJournal(context.Background(), r, &ExitCodeError{Code: 3, Err: errors.New("the deploy script failed with exit status 3")})
	if got, err = os.ReadFile(log); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "deploy prod 1.2.3: failed") {
		t.Fatalf("the journal does not say the deploy failed: %q", got)
	}
}
