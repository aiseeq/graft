package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/jira"
)

func conventionalReturn(t *testing.T, r *withheldRepo) ([]deliveredCommit, deliveredCommit) {
	t.Helper()
	w := r.commitText("work.txt", "work\n", "fix: PROJ-1 work")
	r.revert(w)
	r1, err := oneCommit(r.repo, r.head())
	if err != nil {
		t.Fatal(err)
	}
	content := "returned\n"
	r.writePath("work.txt", &content)
	r.git("add", "work.txt")
	r.git("commit", "-m", "revert: restore PROJ-1", "-m", fmt.Sprintf("This reverts commit %s.\n\nPROJ-1\n", r1.sha))
	r2, err := oneCommit(r.repo, r.head())
	if err != nil {
		t.Fatal(err)
	}
	return []deliveredCommit{{sha: w, subject: "fix: PROJ-1 work"}, r1, r2}, r2
}

func TestUnknownRevertTargetKeepsMainExemptions(t *testing.T) {
	r := newWithheldRepo(t, "base\n")
	base := r.head()
	change := "staged\n"
	r.writePath("extra.txt", &change)
	r.git("add", "extra.txt")
	const unknown = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	commit := deliveredCommit{sha: r.head(), subject: "revert: restore", body: "This reverts commit " + unknown + "."}
	cfg := &config.Config{
		Version: config.Version{Mode: config.ModeNone},
		Ticket:  &config.Ticket{Required: true, Pattern: `PROJ-[0-9]+`, Regexp: regexp.MustCompile(`PROJ-[0-9]+`)},
	}
	if _, err := prepareCommit(r.repo, cfg, commit.subject+"\n\n"+commit.body, 0, false); err != nil {
		t.Fatalf("prepareCommit with unknown revert target = %v, want main's exemption", err)
	}
	if err := requireTicket(r.repo, cfg, commit.subject+"\n\n"+commit.body, ""); err != nil {
		t.Fatalf("requireTicket with unknown revert target = %v, want main's exemption", err)
	}
	freeze := &activeFreeze{config: &config.DeployTarget{ApprovedRelease: &config.ApprovedRelease{}}, name: "projectA"}
	if err := checkFreezeCommit(t.Context(), nil, &deployRun{repo: r.repo, cfg: cfg}, freeze, nil, cfg.Ticket.Regexp, commit, ""); err != nil {
		t.Fatalf("freeze with unknown revert target = %v, want main's exemption", err)
	}
	removed, err := revertedIn(&deployRun{repo: r.repo}, []loggedCommit{{deliveredCommit: commit}})
	if err != nil || len(removed) != 0 {
		t.Fatalf("sweep with unknown revert target = (%v, %v), want pass with no resolved SHA", removed, err)
	}
	r.git("commit", "-m", commit.subject, "-m", commit.body)
	outside, err := (&App{}).outsideHold(&deployRun{repo: r.repo, head: r.head()}, &config.DeployTarget{}, base, "")
	if err != nil || len(outside) != 0 {
		t.Fatalf("release with unknown revert target = (%v, %v), want main's exemption", outside, err)
	}
}

func TestFreezeAnchoredCountsConventionalReturnAsLiveWork(t *testing.T) {
	r := newWithheldRepo(t, "base\n")
	w := r.commitText("work.txt", "work\n", "fix: PROJ-1 work")
	r.revert(w)
	r1, err := oneCommit(r.repo, r.head())
	if err != nil {
		t.Fatal(err)
	}
	content := "returned\n"
	r.writePath("work.txt", &content)
	r.git("add", "work.txt")
	r.git("commit", "-m", "revert: restore PROJ-1", "-m", "This reverts commit "+r1.sha+".\n\nPROJ-1\n")
	returned, err := oneCommit(r.repo, r.head())
	if err != nil {
		t.Fatal(err)
	}
	conf := &config.ApprovedRelease{Dormant: []config.DormantReleaseItem{{Key: "PROJ-2", Reason: "disabled", OutsideVersion: "projectA"}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/rest/api/3/project/PROJ/versions" {
			_, _ = w.Write([]byte(`[{"id":"v1","name":"projectA"}]`))
			return
		}
		http.NotFound(w, req)
	}))
	t.Cleanup(server.Close)
	client := jira.New(jira.Credentials{BaseURL: server.URL})
	gate := &approvalGate{conf: conf, client: client, versions: map[string]bool{"v1": true}, issues: map[string]jira.Issue{
		"PROJ-1": {Key: "PROJ-1", FixVersions: []jira.Version{{ID: "v1", Name: "projectA"}}},
	}}
	cfg := &config.Config{ReleaseNotes: &config.ReleaseNotes{Jira: &config.JiraNotes{ProjectKeys: []string{"PROJ"}}}}
	f := &activeFreeze{config: &config.DeployTarget{ApprovedRelease: conf}, name: "projectA", versionIDs: []string{"v1"}}
	anchored, err := freezeAnchored(t.Context(), client, &deployRun{repo: r.repo, cfg: cfg}, f, gate, regexp.MustCompile(`PROJ-[0-9]+`), []deliveredCommit{returned}, nil)
	if err != nil || !anchored {
		t.Fatalf("freezeAnchored = (%t, %v), want returned work to anchor the version", anchored, err)
	}
}

func TestHotfixUnlabelledChecksConventionalReturn(t *testing.T) {
	r := newWithheldRepo(t, "base\n")
	commits, returned := conventionalReturn(t, r)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/rest/api/3/issue/PROJ-1" {
			_, _ = w.Write([]byte(`{"key":"PROJ-1","fields":{"summary":"work","status":{"name":"In Progress","statusCategory":{"key":"indeterminate"}},"issuetype":{"name":"Task"},"labels":[]}}`))
			return
		}
		http.NotFound(w, req)
	}))
	t.Cleanup(server.Close)
	cfg := &config.Config{
		ReleaseNotes: &config.ReleaseNotes{Jira: &config.JiraNotes{ProjectKeys: []string{"PROJ"}}},
		Deploy:       &config.Deploy{Hotfix: &config.Hotfix{Base: "prod", Label: "hotfix"}, Targets: map[string]*config.DeployTarget{"prod": {}}},
	}
	run := &deployRun{repo: r.repo, cfg: cfg}
	outside, _, err := unlabelledWithClient(t.Context(), run, commits[2:], jira.New(jira.Credentials{BaseURL: server.URL}))
	if err != nil || len(outside) != 1 || !strings.Contains(outside[0], returned.sha[:7]) {
		t.Fatalf("unlabelled return = (%v, %v), want returned commit refused for missing label", outside, err)
	}
}

func TestSweepItemsIncludesConventionalReturn(t *testing.T) {
	r := newWithheldRepo(t, "base\n")
	commits, returned := conventionalReturn(t, r)
	base := r.git("rev-parse", commits[0].sha+"^")
	target := &config.DeployTarget{ApprovedRelease: &config.ApprovedRelease{}}
	cfg := &config.Config{
		ReleaseNotes: &config.ReleaseNotes{Jira: &config.JiraNotes{ProjectKeys: []string{"PROJ"}}},
		Deploy:       &config.Deploy{Targets: map[string]*config.DeployTarget{"projectA": target}},
	}
	items, _, err := sweepItems(&deployRun{repo: r.repo, cfg: cfg, target: target}, returned.sha, base, nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(items["PROJ-1"], func(c sweptCommit) bool { return c.sha == returned.sha }) {
		t.Fatalf("sweep items for PROJ-1 = %+v, want returned commit %s", items["PROJ-1"], returned.sha)
	}
}

func TestRevertReturningWorkIsCheckedByFreeze(t *testing.T) {
	r := newWithheldRepo(t, "base\n")
	_, returned := conventionalReturn(t, r)
	gate := &approvalGate{issues: map[string]jira.Issue{"PROJ-1": {Key: "PROJ-1", Status: "In Progress"}}}
	err := checkFreezeCommit(t.Context(), nil, &deployRun{repo: r.repo, cfg: &config.Config{}}, &activeFreeze{
		config: &config.DeployTarget{ApprovedRelease: &config.ApprovedRelease{}}, name: "projectA",
	}, gate, regexp.MustCompile(`PROJ-[0-9]+`), returned, "")
	if err == nil || !strings.Contains(err.Error(), "PROJ-1") {
		t.Fatalf("freeze check = %v, want refusal to check returned work", err)
	}
}

func TestRevertReturningWorkIsOutsideReleaseHold(t *testing.T) {
	r := newWithheldRepo(t, "base\n")
	_, returned := conventionalReturn(t, r)
	commits, _ := logCommits(r.repo, "--reverse", "HEAD~3..HEAD")
	base := r.git("rev-parse", commits[0].sha+"^")
	run := &deployRun{repo: r.repo, head: r.head()}
	bad, err := (&App{}).outsideHold(run, &config.DeployTarget{}, base, commits[0].sha)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 1 || !strings.Contains(bad[0], returned.sha[:7]) {
		t.Fatalf("outside hold = %v, want returned commit %s", bad, returned.sha)
	}
}

func TestSweepDoesNotTreatReturningRevertAsRemoval(t *testing.T) {
	r := newWithheldRepo(t, "base\n")
	commits, _ := conventionalReturn(t, r)
	logged := make([]loggedCommit, len(commits))
	for i, c := range commits {
		logged[i] = loggedCommit{deliveredCommit: c}
	}
	removed, err := revertedIn(&deployRun{repo: r.repo}, logged)
	if err != nil {
		t.Fatal(err)
	}
	if !removed[commits[0].sha] || removed[commits[1].sha] {
		t.Fatalf("reverted commits = %v, want original work removed and the reverted revert retained", removed)
	}
}

func TestPrepareCommitChecksSmartCommitOnRevertReturningWork(t *testing.T) {
	r := newWithheldRepo(t, "base\n")
	commits, _ := conventionalReturn(t, r)
	change := "change\n"
	r.writePath("extra.txt", &change)
	r.git("add", "extra.txt")
	cfg := &config.Config{Version: config.Version{Mode: config.ModeNone}, Ticket: &config.Ticket{Pattern: `PROJ-[0-9]+`, Regexp: regexp.MustCompile(`PROJ-[0-9]+`)}}
	_, err := prepareCommit(r.repo, cfg, "revert: restore PROJ-1 #done\n\nThis reverts commit "+commits[1].sha+".", 0, false)
	if err == nil || !strings.Contains(err.Error(), "Smart Commit token") {
		t.Fatalf("prepareCommit error = %v, want Smart Commit refusal", err)
	}
}

func TestRequireTicketChecksRevertReturningWork(t *testing.T) {
	r := newWithheldRepo(t, "base\n")
	commits, returned := conventionalReturn(t, r)
	change := "change\n"
	r.writePath("extra.txt", &change)
	r.git("add", "extra.txt")
	cfg := &config.Config{Ticket: &config.Ticket{Required: true, Pattern: `PROJ-[0-9]+`, Regexp: regexp.MustCompile(`PROJ-[0-9]+`)}}
	msg := "revert: restore\n\nThis reverts commit " + commits[1].sha + "."
	if err := requireTicket(r.repo, cfg, msg, ""); err == nil || !strings.Contains(err.Error(), "ticket.required") {
		t.Fatalf("requireTicket(%s) = %v, want ticket.required refusal", returned.sha, err)
	}
}
