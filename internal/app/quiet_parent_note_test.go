package app

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/jira"
)

type quietParentFixture struct {
	parents       map[string]string
	parentLabels  map[string][]string
	doneIssues    map[string]bool
	assigneeIDs   map[string]string
	issueGets     map[string]int
	comments      map[string]int
	transitions   map[string]int
	fixUpdates    map[string]int
	versionReads  int
	failParentGet int
	failChangelog bool
	commentFail   string
	server        *httptest.Server
}

func newQuietParentFixture(t *testing.T) *quietParentFixture {
	t.Helper()
	f := &quietParentFixture{
		parents:      map[string]string{"PROJ-123": "PROJ-200", "PROJ-124": "PROJ-200", "PROJ-125": "projectA-300"},
		parentLabels: map[string][]string{}, doneIssues: map[string]bool{}, assigneeIDs: map[string]string{}, issueGets: map[string]int{}, comments: map[string]int{},
		transitions: map[string]int{}, fixUpdates: map[string]int{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/comment"):
			key := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/rest/api/3/issue/"), "/comment")
			if key == f.commentFail {
				http.Error(w, "synthetic comment failure", http.StatusInternalServerError)
				return
			}
			f.comments[key]++
			w.WriteHeader(http.StatusCreated)
		case strings.HasSuffix(r.URL.Path, "/transitions"):
			key := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/rest/api/3/issue/"), "/transitions")
			f.transitions[key]++
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/versions") && strings.Contains(r.URL.Path, "/project/"):
			f.versionReads++
			fmt.Fprint(w, `[{"id":"v1","name":"Release 2"}]`)
		case strings.HasPrefix(r.URL.Path, "/rest/api/3/version/"):
			fmt.Fprint(w, `{"id":"v1","approvers":[]}`)
		case strings.Contains(r.URL.Path, "/changelog"):
			if f.failChangelog {
				http.Error(w, "synthetic changelog failure", http.StatusInternalServerError)
				return
			}
			fmt.Fprint(w, `{"startAt":0,"total":0,"isLast":true,"values":[]}`)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/"):
			key := strings.TrimPrefix(r.URL.Path, "/rest/api/3/issue/")
			f.fixUpdates[key]++
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/"):
			key := strings.TrimPrefix(r.URL.Path, "/rest/api/3/issue/")
			key = strings.Split(key, "?")[0]
			f.issueGets[key]++
			if key == "PROJ-200" && f.failParentGet > 0 {
				f.failParentGet--
				http.Error(w, "synthetic issue failure", http.StatusInternalServerError)
				return
			}
			parent, subtask := f.parents[key], false
			if parent != "" {
				subtask = true
			}
			parentJSON := ""
			if parent != "" {
				parentJSON = fmt.Sprintf(`,"parent":{"key":%q}`, parent)
			}
			labels := "[]"
			if values := f.parentLabels[key]; len(values) > 0 {
				labels = `[` + `"` + strings.Join(values, `","`) + `"` + `]`
			}
			status, category := "In Progress", "indeterminate"
			if f.doneIssues[key] {
				status, category = "Done", "done"
			}
			assignee := f.assigneeIDs[key]
			if assignee == "" {
				assignee = "owner"
			}
			fmt.Fprintf(w, `{"fields":{"status":{"name":%q,"statusCategory":{"key":%q}},"assignee":{"accountId":%q},"issuetype":{"name":"Task","subtask":%t}%s,"labels":%s},"transitions":[{"id":"1","to":{"name":"Done"}}]}`, status, category, assignee, subtask, parentJSON, labels)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func quietParentItems(t *testing.T, f *quietParentFixture, projectKeys, skipKeys []string, fixVersion bool) (*noteItems, *bytes.Buffer) {
	t.Helper()
	notes := &config.JiraNotes{ProjectKeys: projectKeys, SkipKeys: skipKeys, Quiet: &config.QuietNotes{Subtasks: true, Labels: []string{"internal"}}}
	cfg := &config.Config{ReleaseNotes: &config.ReleaseNotes{Jira: notes}}
	targetNotes := &config.TargetNotes{Transitions: []config.NoteTransition{{From: []string{"In Progress"}, To: "Done"}}}
	if fixVersion {
		targetNotes.FixVersion = &config.FixVersion{Prefix: "Release"}
	}
	run := &deployRun{cfg: cfg, target: &config.DeployTarget{ReleaseNotes: targetNotes}}
	var output bytes.Buffer
	app := &App{Stdout: &output, Stderr: &output}
	client := jira.New(jira.Credentials{BaseURL: f.server.URL})
	mover := &noteMover{app: app, client: client, conf: targetNotes, me: "owner", glint: &glintCheck{}}
	sub := &subtaskRun{mover: mover, notes: notes, reached: map[string]string{}}
	items := &noteItems{app: app, run: run, client: client, me: "owner", text: "synthetic release note", fix: &fixVersionRun{app: app, conf: targetNotes.FixVersion, client: client}, mover: mover, sub: sub}
	return items, &output
}

func TestQuietSubtasksPostOneParentNote(t *testing.T) {
	f := newQuietParentFixture(t)
	items, _ := quietParentItems(t, f, []string{"PROJ", "projectA"}, nil, false)
	for _, key := range []string{"PROJ-123", "PROJ-124", "PROJ-125"} {
		items.post(t.Context(), key)
	}
	if f.comments["PROJ-200"] != 1 || f.comments["projectA-300"] != 1 {
		t.Fatalf("parent comments = %v, want one for each parent", f.comments)
	}
	for _, key := range []string{"PROJ-123", "PROJ-124", "PROJ-125"} {
		if f.comments[key] != 0 {
			t.Errorf("quiet subtask %s got %d comments", key, f.comments[key])
		}
	}
	if f.issueGets["PROJ-200"] != 1 {
		t.Errorf("parent Jira reads = %d, want one despite sibling subtasks", f.issueGets["PROJ-200"])
	}
	if f.transitions["PROJ-200"] != 0 || f.fixUpdates["PROJ-200"] != 0 {
		t.Errorf("uncommitted parent was modified: transitions=%v fix updates=%v", f.transitions, f.fixUpdates)
	}
}

func TestQuietSubtaskAndCommittedParentShareOneComment(t *testing.T) {
	f := newQuietParentFixture(t)
	items, _ := quietParentItems(t, f, []string{"PROJ"}, nil, false)
	for _, key := range []string{"PROJ-123", "PROJ-200"} {
		items.post(t.Context(), key)
	}
	if f.comments["PROJ-200"] != 1 {
		t.Fatalf("parent comments = %v, want one", f.comments)
	}
}

func TestQuietSubtaskParentIssueFailureDoesNotSuppressCommittedParent(t *testing.T) {
	f := newQuietParentFixture(t)
	f.failParentGet = 1
	items, output := quietParentItems(t, f, []string{"PROJ"}, nil, false)
	items.post(t.Context(), "PROJ-123")
	items.post(t.Context(), "PROJ-200")
	if f.issueGets["PROJ-200"] != 2 || f.comments["PROJ-200"] != 1 {
		t.Fatalf("parent gets/comments = %d/%d, want retry and one comment", f.issueGets["PROJ-200"], f.comments["PROJ-200"])
	}
	if slices.Contains(items.unsure, "PROJ-200") {
		t.Fatalf("successfully noted parent remains unsure: %v", items.unsure)
	}
	if !strings.Contains(output.String(), "release note for PROJ-200 not posted:") || !strings.Contains(output.String(), "synthetic issue failure") {
		t.Fatalf("missing parent issue warning: %q", output.String())
	}
}

func TestQuietSubtaskParentCommentFailureIsWarnedAndUnsure(t *testing.T) {
	f := newQuietParentFixture(t)
	f.commentFail = "PROJ-200"
	items, output := quietParentItems(t, f, []string{"PROJ"}, nil, false)
	items.post(t.Context(), "PROJ-123")
	if !slices.Contains(items.unsure, "PROJ-200") {
		t.Fatalf("unsure keys = %v, want PROJ-200", items.unsure)
	}
	if !strings.Contains(output.String(), "release note for PROJ-200 not posted:") {
		t.Fatalf("missing parent comment warning: %q", output.String())
	}
}

func TestQuietSubtaskParentHonorsProjectSkipQuietAndMoveOut(t *testing.T) {
	tests := []struct {
		name     string
		projects []string
		skip     []string
		labels   []string
		moveOut  bool
		wantLog  string
	}{
		{name: "outside project keys", projects: []string{"PROJ"}},
		{name: "skip key", projects: []string{"PROJ"}, skip: []string{"PROJ-200"}, wantLog: "release_notes.jira.skip_keys"},
		{name: "quiet label", projects: []string{"PROJ"}, labels: []string{"internal"}, wantLog: "release_notes.jira.quiet (label internal)"},
		{name: "move out", projects: []string{"PROJ"}, moveOut: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newQuietParentFixture(t)
			f.parentLabels["PROJ-200"] = tc.labels
			items, output := quietParentItems(t, f, tc.projects, tc.skip, false)
			childKey, parentKey := "PROJ-123", "PROJ-200"
			if tc.name == "outside project keys" {
				childKey, parentKey = "PROJ-125", "projectA-300"
			}
			if tc.moveOut {
				items.run.moveOut = &versionMove{keys: []string{parentKey}}
			}
			if tc.name == "quiet label" {
				items.post(t.Context(), parentKey)
			}
			items.post(t.Context(), childKey)
			if tc.name == "skip key" {
				items.post(t.Context(), parentKey)
			}
			if f.comments[parentKey] != 0 {
				t.Fatalf("parent comments = %v, want none", f.comments)
			}
			if tc.wantLog != "" && !strings.Contains(output.String(), tc.wantLog) {
				t.Fatalf("output %q does not contain %q", output.String(), tc.wantLog)
			}
			if tc.name == "quiet label" && strings.Count(output.String(), tc.wantLog) != 2 {
				t.Fatalf("committed quiet parent and quiet subtask output = %q, want the same quiet decision on both paths", output.String())
			}
			if tc.name == "skip key" && strings.Count(output.String(), tc.wantLog) != 1 {
				t.Fatalf("skip parent output = %q, want one left-alone line", output.String())
			}
			if tc.name == "outside project keys" && f.issueGets["PROJ-200"] != 0 {
				t.Fatalf("out-of-project parent was queried: %v", f.issueGets)
			}
		})
	}
}

func TestQuietSubtaskParentDoesNotGetFixVersion(t *testing.T) {
	f := newQuietParentFixture(t)
	items, _ := quietParentItems(t, f, []string{"PROJ"}, nil, true)
	items.post(t.Context(), "PROJ-123")
	if f.fixUpdates["PROJ-200"] != 0 {
		t.Fatalf("parent fix version updates = %v", f.fixUpdates)
	}
	if f.fixUpdates["PROJ-123"] != 1 {
		t.Fatalf("subtask fix version updates = %v; output=%q version reads=%d", f.fixUpdates, f.server.URL, f.versionReads)
	}
}

func TestDoneIssueNoteableFailureDoesNotFailFixVersion(t *testing.T) {
	f := newQuietParentFixture(t)
	f.doneIssues["PROJ-123"] = true
	f.assigneeIDs["PROJ-123"] = "someone-else"
	f.failChangelog = true
	items, output := quietParentItems(t, f, []string{"PROJ"}, nil, true)
	items.post(t.Context(), "PROJ-123")
	if slices.Contains(items.fix.unset, "PROJ-123") {
		t.Fatalf("Done issue was marked fix-version unset: %v", items.fix.unset)
	}
	if !strings.Contains(output.String(), "synthetic changelog failure") {
		t.Fatalf("missing noteable warning: %q", output.String())
	}
}

func TestQuietSubtaskDormantParentMessagePrintedOnce(t *testing.T) {
	f := newQuietParentFixture(t)
	items, output := quietParentItems(t, f, []string{"PROJ"}, nil, false)
	approved := &config.ApprovedRelease{Dormant: []config.DormantReleaseItem{{Key: "PROJ-200", Reason: "disabled parent", OutsideVersion: "2.0"}}}
	items.run.target.ApprovedRelease = approved
	items.mover.approved = approved
	items.post(t.Context(), "PROJ-123")
	items.post(t.Context(), "PROJ-200")
	want := "PROJ-200 ships dormant (disabled parent): left in In Progress, not moved"
	if count := strings.Count(output.String(), want); count != 1 {
		t.Fatalf("dormant parent message count = %d, output %q", count, output.String())
	}
}

func TestDeployCheckNamesParentNoteFromQuietSubtask(t *testing.T) {
	f := newQuietParentFixture(t)
	runItems, output := quietParentItems(t, f, []string{"PROJ"}, nil, false)
	m := runItems.mover
	m.readOnly = true
	m.previewed = map[string]bool{}
	a := runItems.app
	a.previewNoteKey(t.Context(), runItems.run, m, "PROJ-123")
	if !strings.Contains(output.String(), "- PROJ-200: note only (from quiet subtask)") {
		t.Fatalf("deploy check output = %q", output.String())
	}
}
