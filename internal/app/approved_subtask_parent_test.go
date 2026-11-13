package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/jira"
)

func TestApprovedDoneParentWithOpenSubtaskIsHeld(t *testing.T) {
	parent := jira.Issue{Key: "PROJ-1", Status: "Done", Done: true, Subtasks: []string{"PROJ-2"}}
	gate := &approvalGate{
		conf:   &config.ApprovedRelease{Statuses: []string{"Ready for release", "Done"}},
		target: &config.DeployTarget{ApprovedRelease: &config.ApprovedRelease{Statuses: []string{"Ready for release", "Done"}}},
		issues: map[string]jira.Issue{"PROJ-1": parent, "PROJ-2": {Key: "PROJ-2", Status: "Testing", Parent: "PROJ-1"}},
	}
	pass, err := gate.item(t.Context(), "PROJ-1")
	if err != nil || pass.ok || !strings.Contains(pass.why, "open subtasks PROJ-2 (Testing)") {
		t.Fatalf("parent gate = %+v, %v; want open subtask refusal", pass, err)
	}
}

func TestApprovedPrefixHoldsOpenSubtasksRegardlessOfParentStatus(t *testing.T) {
	gate := &approvalGate{
		conf: &config.ApprovedRelease{Prefix: "Product v", Version: &config.ApprovedReleaseVersion{}},
		issues: map[string]jira.Issue{
			"PROJ-1": {Key: "PROJ-1", Status: "In Progress", Subtasks: []string{"PROJ-2"}},
			"PROJ-2": {Key: "PROJ-2", Status: "Testing", Parent: "PROJ-1"},
		},
	}
	pass, err := gate.item(t.Context(), "PROJ-1")
	if err != nil || pass.ok || !strings.Contains(pass.why, "open subtasks PROJ-2 (Testing)") {
		t.Fatalf("prefix parent gate = %+v, %v; want open subtask refusal", pass, err)
	}
	pass, err = gate.item(t.Context(), "PROJ-2")
	if err != nil || pass.ok || !strings.Contains(pass.why, "open subtasks PROJ-2 (Testing)") {
		t.Fatalf("prefix subtask gate = %+v, %v; want its parent to hold it", pass, err)
	}
}

func TestSkippedUnreadableSubtaskIsNotRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer server.Close()
	gate := &approvalGate{client: jira.New(jira.Credentials{BaseURL: server.URL}), skipKeys: []string{"PROJ-2"}, issues: map[string]jira.Issue{}}
	got, err := gate.openSubtasks(t.Context(), jira.Issue{Key: "PROJ-1", Subtasks: []string{"PROJ-2"}})
	if err != nil || got != "" {
		t.Fatalf("openSubtasks = %q, %v; want no refusal and no Jira read", got, err)
	}
}

func TestApprovedVersionReadTopsAddsNamedParentToUmbrellas(t *testing.T) {
	conf := &config.ApprovedRelease{Version: &config.ApprovedReleaseVersion{}}
	gate := &approvalGate{conf: conf, issues: map[string]jira.Issue{
		"PROJ-1": {Key: "PROJ-1", Subtasks: []string{"PROJ-2"}},
	}}
	v := &releaseVersionCheck{gate: &gateRun{conf: conf, gate: gate, activeKeys: map[string]bool{"PROJ-1": true}}, tops: map[string]jira.Issue{}, umbrellas: map[string]bool{}}
	if v.readTops(t.Context()) || !v.umbrellas["PROJ-1"] {
		t.Fatalf("umbrellas = %v; want directly named parent", v.umbrellas)
	}
}

func TestApprovedVersionStatusAdviceDoesNotTargetSubtask(t *testing.T) {
	conf := &config.ApprovedRelease{Statuses: []string{"Ready for release", "Done"}, Version: &config.ApprovedReleaseVersion{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"issues":[{"key":"PROJ-2"}],"isLast":true}`))
	}))
	defer server.Close()
	v := &releaseVersionCheck{
		gate:     &gateRun{conf: conf, gate: &approvalGate{conf: conf, client: jira.New(jira.Credentials{BaseURL: server.URL}), issues: map[string]jira.Issue{"PROJ-2": {Key: "PROJ-2", Parent: "PROJ-1", Status: "Testing"}}}},
		collect:  true,
		tops:     map[string]jira.Issue{"PROJ-2": {Key: "PROJ-2", Parent: "PROJ-1", Status: "Testing"}},
		selected: jira.Version{ID: "v1", Name: "Product 1"},
	}
	if v.checkItems(t.Context()) || len(v.failures) != 1 || !strings.Contains(v.failures[0].Error(), "version-subtasks") || strings.Contains(v.failures[0].Error(), "version-status") {
		t.Fatalf("checkItems failures = %v; want only version-subtasks", v.failures)
	}
}

func TestFreezeIgnoresSubtaskOwnFixVersion(t *testing.T) {
	at := time.Time{}
	gate := &approvalGate{
		issues: map[string]jira.Issue{
			"PROJ-2": {Key: "PROJ-2", Parent: "PROJ-1", FixVersions: []jira.Version{{ID: "v1"}}},
			"PROJ-1": {Key: "PROJ-1"},
		},
		history: map[string]fixVersionHistoryResult{
			"PROJ-2\x00v1\x00" + at.Format(time.RFC3339Nano): {hasAddition: true},
		},
	}
	var matched []string
	if err := checkFreezeIssue(t.Context(), gate, &activeFreeze{versionIDs: []string{"v1"}}, "PROJ-2", &matched); err != nil {
		t.Fatal(err)
	}
	if len(matched) != 0 {
		t.Fatalf("freeze matched %v by the subtask's own version", matched)
	}
}

func TestApprovedVersionUmbrellaAddsNamedTopLevelItem(t *testing.T) {
	conf := &config.ApprovedRelease{Version: &config.ApprovedReleaseVersion{}}
	v := &releaseVersionCheck{
		gate:      &gateRun{conf: conf, gate: &approvalGate{conf: conf, issues: map[string]jira.Issue{"PROJ-2": {Key: "PROJ-2", Status: "Testing"}}}},
		tops:      map[string]jira.Issue{"PROJ-1": {Key: "PROJ-1", Subtasks: []string{"PROJ-2"}}},
		umbrellas: map[string]bool{"PROJ-1": true},
	}
	if !v.checkUmbrellas(t.Context()) || len(v.failures) != 1 || !strings.Contains(v.failures[0].Error(), "PROJ-2") {
		t.Fatalf("umbrella failures = %v; want open subtask refusal", v.failures)
	}
}

func TestApprovedVersionUmbrellaSkipsKeyBeforeJiraRead(t *testing.T) {
	conf := &config.ApprovedRelease{Version: &config.ApprovedReleaseVersion{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer server.Close()
	v := &releaseVersionCheck{
		gate:      &gateRun{conf: conf, gate: &approvalGate{conf: conf, client: jira.New(jira.Credentials{BaseURL: server.URL}), skipKeys: []string{"PROJ-2"}}},
		tops:      map[string]jira.Issue{"PROJ-1": {Key: "PROJ-1", Subtasks: []string{"PROJ-2"}}},
		umbrellas: map[string]bool{"PROJ-1": true},
	}
	if v.checkUmbrellas(t.Context()) || len(v.failures) != 0 {
		t.Fatalf("umbrella failures = %v; want skipped unreadable subtask", v.failures)
	}
}

// A subtask is done once its code is in the trunk, before the release: it
// goes out by its parent's status and labels alone, never its own.
func TestApprovedSubtaskPassesByItsParentAlone(t *testing.T) {
	for _, tc := range []struct {
		name            string
		subtask, parent jira.Issue
		statuses, skip  []string
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
		{name: "done subtask, parent ready with another subtask open but in skip_keys",
			subtask: jira.Issue{Key: "PROJ-2", Status: "Done", Done: true, Parent: "PROJ-1"},
			parent:  jira.Issue{Key: "PROJ-1", Status: "Ready for release", Subtasks: []string{"PROJ-2", "PROJ-3"}},
			skip:    []string{"PROJ-3"},
			want:    true},
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
				conf:     approved,
				skipKeys: tc.skip,
				target:   &config.DeployTarget{ApprovedRelease: approved},
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

// An approved subtask version cannot replace its parent's version.
func TestApprovedSubtaskIgnoresOwnVersion(t *testing.T) {
	gate := &approvalGate{
		conf:     &config.ApprovedRelease{Prefix: "Product v", Version: &config.ApprovedReleaseVersion{}},
		versions: map[string]bool{"2": true},
		issues: map[string]jira.Issue{
			"PROJ-1": {Key: "PROJ-1", Status: "In Progress"},
			"PROJ-2": {Key: "PROJ-2", Status: "Ready for release", Parent: "PROJ-1", FixVersions: []jira.Version{{ID: "2", Name: "Product v1"}}},
		},
	}
	pass, err := gate.item(t.Context(), "PROJ-2")
	if err != nil {
		t.Fatal(err)
	}
	if pass.ok {
		t.Fatalf("subtask passed by its own approved version: %+v", pass)
	}
}
