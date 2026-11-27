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

func TestReleasePageGroupsSubtasksByConfiguredParentLabels(t *testing.T) {
	issues := map[string]string{
		"PROJ-11": `{"key":"PROJ-11","fields":{"summary":"technical child one","status":{"statusCategory":{"key":"indeterminate"}},"issuetype":{"name":"Sub-task","subtask":true},"parent":{"key":"PROJ-10"},"labels":[]}}`,
		"PROJ-12": `{"key":"PROJ-12","fields":{"summary":"technical child two","status":{"statusCategory":{"key":"indeterminate"}},"issuetype":{"name":"Sub-task","subtask":true},"parent":{"key":"PROJ-10"},"labels":[]}}`,
		"PROJ-10": `{"key":"PROJ-10","fields":{"summary":"technical parent","status":{"statusCategory":{"key":"indeterminate"}},"issuetype":{"name":"Task"},"labels":["release-technical"]}}`,
		"PROJ-21": `{"key":"PROJ-21","fields":{"summary":"bug child one","status":{"statusCategory":{"key":"indeterminate"}},"issuetype":{"name":"Sub-task","subtask":true},"parent":{"key":"PROJ-20"},"labels":[]}}`,
		"PROJ-22": `{"key":"PROJ-22","fields":{"summary":"bug child two","status":{"statusCategory":{"key":"indeterminate"}},"issuetype":{"name":"Sub-task","subtask":true},"parent":{"key":"PROJ-20"},"labels":[]}}`,
		"PROJ-20": `{"key":"PROJ-20","fields":{"summary":"synthetic bug summary","status":{"statusCategory":{"key":"indeterminate"}},"issuetype":{"name":"Bug"},"labels":[]}}`,
		"PROJ-30": `{"key":"PROJ-30","fields":{"summary":"listed parent summary","status":{"statusCategory":{"key":"indeterminate"}},"issuetype":{"name":"Story"},"labels":[]}}`,
		"PROJ-31": `{"key":"PROJ-31","fields":{"summary":"listed parent child","status":{"statusCategory":{"key":"indeterminate"}},"issuetype":{"name":"Sub-task","subtask":true},"parent":{"key":"PROJ-30"},"labels":[]}}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/rest/api/3/issue/")
		if r.Method != http.MethodGet || issues[key] == "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, issues[key])
	}))
	t.Cleanup(server.Close)
	client := jira.New(jira.Credentials{BaseURL: server.URL})
	p := &config.ReleasePage{Subtasks: "parent", FixTypes: []string{"Bug"}, TechnicalLabels: []string{"release-technical"}}
	r := &deployRun{version: "1.2.3", name: "prod", target: &config.DeployTarget{}}
	section, err := buildSection(t.Context(), client, p, r, []string{"PROJ-11", "PROJ-12", "PROJ-21", "PROJ-22", "PROJ-30", "PROJ-31"}, nil, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	changes := strings.SplitN(strings.SplitN(section, "<h3>Changes</h3>", 2)[1], "<h3>Fixes</h3>", 2)[0]
	fixes := strings.SplitN(section, "<h3>Fixes</h3>", 2)[1]
	for _, want := range []string{"PROJ-10", "Technical changes", "Task", "PROJ-30", "listed parent summary", "Story"} {
		if !strings.Contains(changes, want) {
			t.Errorf("changes section missing %q:\n%s", want, changes)
		}
	}
	if strings.Count(changes, ">PROJ-10</a>") != 1 || strings.Count(changes, ">PROJ-30</a>") != 1 {
		t.Errorf("parent duplicated in changes:\n%s", changes)
	}
	if !strings.Contains(fixes, "PROJ-20") || !strings.Contains(fixes, "synthetic bug summary") || !strings.Contains(fixes, "Bug") {
		t.Errorf("ordinary parent missing from fixes:\n%s", fixes)
	}
	if strings.Count(fixes, ">PROJ-20</a>") != 1 || strings.Contains(section, "technical child") || strings.Contains(section, "bug child") {
		t.Errorf("subtasks were not hidden or parent was duplicated:\n%s", section)
	}
}

func TestReleasePageParentSubtasksWithoutTechnicalLabelsUseParentRows(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"key":"PROJ-123","fields":{"summary":"synthetic parent summary","status":{"statusCategory":{"key":"indeterminate"}},"issuetype":{"name":"Task"},"labels":["release-technical"]}}`)
	}))
	t.Cleanup(server.Close)
	issues := []jira.Issue{{Key: "PROJ-124", Type: "Sub-task", Subtask: true, Parent: "PROJ-123"}}
	grouped, technical := groupSubtasks(t.Context(), jira.New(jira.Credentials{BaseURL: server.URL}), &config.ReleasePage{Subtasks: "parent"}, issues, func(string, ...any) {})
	if len(grouped) != 1 || grouped[0].Key != "PROJ-123" || grouped[0].Summary != "synthetic parent summary" || grouped[0].Type != "Task" || len(technical) != 0 {
		t.Fatalf("grouped = %+v, technical = %+v", grouped, technical)
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
