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
