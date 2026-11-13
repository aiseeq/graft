package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/jira"
)

func TestApprovedLabelStatusesPassVersionGates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels []string
		want   bool
	}{
		{name: "internal", labels: []string{"internal"}, want: true},
		{name: "unlabelled", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/rest/api/3/search/jql") {
					fmt.Fprint(w, `{"issues":[{"key":"PROJ-1"}],"isLast":true}`)
					return
				}
				if strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/PROJ-1") {
					fmt.Fprintf(w, `{"fields":{"summary":"synthetic","status":{"name":"Done","statusCategory":{"key":"done"}},"issuetype":{"name":"Task"},"labels":%q}}`, tc.labels)
					return
				}
				http.NotFound(w, r)
			}))
			t.Cleanup(srv.Close)
			client := jira.New(jira.Credentials{BaseURL: srv.URL})
			approved := &config.ApprovedRelease{
				Statuses:      []string{"Ready for Release"},
				LabelStatuses: map[string][]string{"internal": {"Done"}},
				Version:       &config.ApprovedReleaseVersion{Refs: map[string]string{}},
			}
			app := &App{Stdout: &strings.Builder{}, Stderr: &strings.Builder{}}
			gate := &gateRun{app: app, conf: approved, gate: &approvalGate{client: client, conf: approved, issues: map[string]jira.Issue{}}}
			check := &releaseVersionCheck{
				gate: gate, collect: true,
				tops:     map[string]jira.Issue{"PROJ-1": {Key: "PROJ-1", Status: "Done", Labels: tc.labels, FixVersions: []jira.Version{{ID: "v1"}}}},
				selected: jira.Version{ID: "v1", Name: "Synthetic 1"},
			}
			check.checkTops()
			check.checkItems(t.Context())
			if got := len(check.failures) == 0; got != tc.want {
				t.Errorf("version checks passed = %t, failures = %v", got, check.failures)
			}
		})
	}
}

func TestApprovedStatusesForSubtasksUseTheirLabels(t *testing.T) {
	approved := &config.ApprovedRelease{
		Statuses:      []string{"Ready for Release"},
		LabelStatuses: map[string][]string{"internal": {"Internal Review"}},
	}
	parent := jira.Issue{Key: "PROJ-1", Status: "Ready for Release", Subtasks: []string{"PROJ-2"}}
	subtask := jira.Issue{Key: "PROJ-2", Status: "Internal Review"}
	gate := &approvalGate{
		conf:   approved,
		target: &config.DeployTarget{ApprovedRelease: approved},
		issues: map[string]jira.Issue{"PROJ-2": subtask},
	}
	why, err := gate.openParent(t.Context(), parent)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(why, "PROJ-2 (Internal Review)") {
		t.Fatalf("open parent reason = %q, want unlabelled subtask held", why)
	}
}

// The hint after a refusal names a label's statuses for its items only: an
// item without the label is not told to move to them.
func TestApprovedRemedyNamesLabelStatusesPerLabel(t *testing.T) {
	g := &approvalGate{conf: &config.ApprovedRelease{
		Statuses:      []string{"Ready for Release"},
		LabelStatuses: map[string][]string{"internal": {"Done"}},
	}}
	want := "move their items (a subtask's parent for a subtask) to Ready for Release, items labelled internal to Done and finish any subtasks called open"
	if got := g.remedy(); got != want {
		t.Fatalf("remedy = %q, want %q", got, want)
	}
	g.conf.LabelStatuses = nil
	if got, want := g.remedy(), "move their items (a subtask's parent for a subtask) to Ready for Release and finish any subtasks called open"; got != want {
		t.Fatalf("remedy without labels = %q, want %q", got, want)
	}
}
