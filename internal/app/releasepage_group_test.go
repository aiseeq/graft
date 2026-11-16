package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/jira"
)

// A parent Jira refuses to give keeps its one technical row, without a type,
// and a warning names it: the page is built all the same.
func TestGroupSubtasksKeepsAnUnreadableParent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	client := jira.New(jira.Credentials{BaseURL: srv.URL, Email: "me@example.com", Token: "token"})
	p := &config.ReleasePage{Subtasks: "parent"}
	issues := []jira.Issue{
		{Key: "PROJ-20", Type: "Task"},
		{Key: "PROJ-11", Type: "Sub-task", Subtask: true, Parent: "PROJ-10"},
		{Key: "PROJ-12", Type: "Sub-task", Subtask: true, Parent: "PROJ-10"},
	}
	var warnings []string
	warn := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	grouped, technical := groupSubtasks(t.Context(), client, p, issues, warn)
	if len(grouped) != 1 || grouped[0].Key != "PROJ-20" {
		t.Errorf("grouped = %+v, want PROJ-20 alone", grouped)
	}
	if len(technical) != 1 || technical[0].Key != "PROJ-10" || technical[0].Summary != p.Label("technical") || technical[0].Type != "" {
		t.Errorf("technical = %+v, want one PROJ-10 row with the label and no type", technical)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "PROJ-10, the parent of PROJ-11, is listed without its type") || !strings.Contains(warnings[0], "403") {
		t.Errorf("warnings = %q", warnings)
	}
}

func TestReleasePageListsDormantWork(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/") {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"key":"PROJ-123","fields":{"summary":"disabled synthetic feature","status":{"name":"Testing","statusCategory":{"key":"indeterminate"}},"issuetype":{"name":"Task"},"labels":[]}}`)
	}))
	t.Cleanup(server.Close)
	client := jira.New(jira.Credentials{BaseURL: server.URL})
	r := &deployRun{version: "1.2.3", name: "prod", target: &config.DeployTarget{ApprovedRelease: &config.ApprovedRelease{Dormant: []config.DormantReleaseItem{{Key: "PROJ-123", Reason: "feature flag stays off"}, {Key: "PROJ-124", Reason: "not part of this release"}}}}}
	section, err := buildSection(context.Background(), client, &config.ReleasePage{}, r, []string{"PROJ-123"}, nil, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<h3>ships dormant</h3>", "PROJ-123 — feature flag stays off"} {
		if !strings.Contains(section, want) {
			t.Errorf("missing %q in release page section:\n%s", want, section)
		}
	}
	if strings.Contains(section, "not part of this release") {
		t.Errorf("listed dormant work outside the release:\n%s", section)
	}
}

func TestReleasePageKeepsLinkedOutsideWorkOnlyInDormantSection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"key":"PROJ-124","fields":{"summary":"disabled checkout","status":{"name":"Testing","statusCategory":{"key":"indeterminate"}},"issuetype":{"name":"Task"},"labels":[]}}`)
	}))
	t.Cleanup(server.Close)
	item := config.DormantReleaseItem{Key: "PROJ-124", Reason: "disabled checkout", DisabledBy: "checkout.v2", OutsideVersion: "Product X"}
	page := func(dormant []config.DormantReleaseItem) string {
		t.Helper()
		r := &deployRun{version: "1.2.3", approvedVersions: []string{"Product X"}, dormantItems: dormant, target: &config.DeployTarget{ApprovedRelease: &config.ApprovedRelease{Dormant: []config.DormantReleaseItem{item}}}}
		section, err := buildSection(context.Background(), jira.New(jira.Credentials{BaseURL: server.URL}), &config.ReleasePage{}, r, []string{item.Key}, nil, func(string, ...any) {})
		if err != nil {
			t.Fatal(err)
		}
		return section
	}
	// Linked by the gate: only in the dormant section, with its version.
	section := page([]config.DormantReleaseItem{item})
	if strings.Contains(section, "<h3>changes</h3>") || strings.Contains(section, "<h3>fixes</h3>") {
		t.Fatalf("outside work appeared in changes or fixes:\n%s", section)
	}
	if !strings.Contains(section, item.Line()) {
		t.Fatalf("outside dormant line missing:\n%s", section)
	}
	// Not linked (review finding 8): the old dormant line, never "outside".
	section = page(nil)
	if strings.Contains(section, "outside Product X") || !strings.Contains(section, "ships dormant: PROJ-124 — disabled checkout; disabled by checkout.v2") {
		t.Fatalf("unlinked entry shown as linked:\n%s", section)
	}
}
