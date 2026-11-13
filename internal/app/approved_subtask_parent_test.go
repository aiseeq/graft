package app

import (
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/jira"
)

// A subtask is done once its code is in the trunk, before the release: it
// goes out by its parent's status and labels alone, never its own.
func TestApprovedSubtaskPassesByItsParentAlone(t *testing.T) {
	for _, tc := range []struct {
		name            string
		subtask, parent jira.Issue
		statuses        []string
		want            bool
	}{
		{name: "done subtask with done approved, parent in progress",
			subtask:  jira.Issue{Key: "PROJ-2", Status: "Done", Done: true, Parent: "PROJ-1"},
			parent:   jira.Issue{Key: "PROJ-1", Status: "In Progress", Subtasks: []string{"PROJ-2"}},
			statuses: []string{"Ready for release", "Done"}},
		{name: "done subtask, parent in progress",
			subtask: jira.Issue{Key: "PROJ-2", Status: "Done", Done: true, Parent: "PROJ-1"},
			parent:  jira.Issue{Key: "PROJ-1", Status: "In Progress", Subtasks: []string{"PROJ-2"}}},
		{name: "labelled subtask in its label's status, parent in progress",
			subtask: jira.Issue{Key: "PROJ-2", Status: "Done", Done: true, Parent: "PROJ-1", Labels: []string{"internal"}},
			parent:  jira.Issue{Key: "PROJ-1", Status: "In Progress", Subtasks: []string{"PROJ-2"}}},
		{name: "done subtask, parent ready",
			subtask: jira.Issue{Key: "PROJ-2", Status: "Done", Done: true, Parent: "PROJ-1"},
			parent:  jira.Issue{Key: "PROJ-1", Status: "Ready for release", Subtasks: []string{"PROJ-2"}},
			want:    true},
		{name: "done subtask, parent ready with another subtask open",
			subtask: jira.Issue{Key: "PROJ-2", Status: "Done", Done: true, Parent: "PROJ-1"},
			parent:  jira.Issue{Key: "PROJ-1", Status: "Ready for release", Subtasks: []string{"PROJ-2", "PROJ-3"}}},
		{name: "subtask in progress, parent ready",
			subtask: jira.Issue{Key: "PROJ-2", Status: "In Progress", Parent: "PROJ-1"},
			parent:  jira.Issue{Key: "PROJ-1", Status: "Ready for release", Subtasks: []string{"PROJ-2"}}},
		{name: "subtask of a labelled parent in the label's status",
			subtask: jira.Issue{Key: "PROJ-2", Status: "Done", Done: true, Parent: "PROJ-1"},
			parent:  jira.Issue{Key: "PROJ-1", Status: "Done", Done: true, Labels: []string{"internal"}, Subtasks: []string{"PROJ-2"}},
			want:    true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			approved := &config.ApprovedRelease{
				Statuses:      []string{"Ready for release"},
				LabelStatuses: map[string][]string{"internal": {"Done"}},
			}
			if tc.statuses != nil {
				approved.Statuses = tc.statuses
			}
			gate := &approvalGate{
				conf:   approved,
				target: &config.DeployTarget{ApprovedRelease: approved},
				issues: map[string]jira.Issue{"PROJ-1": tc.parent, "PROJ-2": tc.subtask,
					"PROJ-3": {Key: "PROJ-3", Status: "In Progress", Parent: "PROJ-1"}},
			}
			pass, err := gate.item(t.Context(), "PROJ-2")
			if err != nil {
				t.Fatal(err)
			}
			if pass.ok != tc.want {
				t.Fatalf("subtask passes = %t (%q), want %t", pass.ok, pass.why, tc.want)
			}
		})
	}
}
