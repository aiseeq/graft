package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/jira"
)

func TestDeploySummaryConfiguredEffects(t *testing.T) {
	var out bytes.Buffer
	a := &App{Stdout: &out, Stderr: io.Discard}
	enabled := true
	target := &config.DeployTarget{
		Deps: []string{"build", "test"}, Confirm: "sudo", MergeIntoTrunk: true,
		ReleaseVersion: &config.ReleaseVersion{Enabled: &enabled, Name: "Product {dd}.{mm}.{yy}"},
		ReleasePage:    &config.ReleasePage{Parent: "500"},
		ReleaseNotes: &config.TargetNotes{Transitions: []config.NoteTransition{{From: []string{"In Progress"}, To: "Testing", Assign: "reviewer"}},
			FixVersion: &config.FixVersion{Prefix: "Product"}, Subtasks: true, Sweep: &config.Sweep{Done: "Done"}},
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	r := &deployRun{name: "prod", target: target, repo: &gitx.Repo{CommonDir: t.TempDir()}, cfg: &config.Config{ReleaseNotes: &config.ReleaseNotes{Jira: &config.JiraNotes{}}}}
	a.printDeploySummary(t.Context(), r)
	for _, want := range []string{"build, test", "sudo", "In Progress → Testing", "assign reviewer", "keys of the new commits not checked:", "fix_version:", "subtasks:", "sweep:", "merge_into_trunk:", "release_version:", "release_page:", "command: graft deploy prod"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in %s", want, out.String())
		}
	}
}

func TestTransitionTypesKeepNamedAndFollowingSubtasks(t *testing.T) {
	var moved []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/transitions") {
			key := strings.Split(r.URL.Path, "/")[5]
			moved = append(moved, key)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/") {
			fmt.Fprintf(w, `{"fields":{"summary":"synthetic","status":{"name":"In Progress","statusCategory":{"key":"indeterminate"}},"assignee":{"accountId":"token-owner"},"issuetype":{"name":"Sub-task","subtask":true},"parent":{"key":"PROJ-1"},"labels":[]},"transitions":[{"id":"1","to":{"name":"Testing"}}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	client := jira.New(jira.Credentials{BaseURL: srv.URL})
	var output, stderr bytes.Buffer
	app := &App{Stdout: &output, Stderr: &stderr}
	conf := &config.TargetNotes{Transitions: []config.NoteTransition{{From: []string{"In Progress"}, To: "Testing", Types: []string{"Story", "Task", "Bug"}}}, Subtasks: true}
	mover := &noteMover{app: app, client: client, conf: conf, glint: &glintCheck{}}

	// A subtask named by its own key reaches the same central type check.
	if got := mover.delivered(context.Background(), jira.Issue{Key: "PROJ-2", Type: "Sub-task", Status: "In Progress"}); got != "In Progress" {
		t.Fatalf("named subtask reached %q", got)
	}

	// The allowed parent moves, then its following subtask is filtered there too.
	parent := jira.Issue{Key: "PROJ-1", Type: "task", Status: "In Progress", Transitions: []jira.Transition{{ID: "1", To: "Testing"}}, Subtasks: []string{"PROJ-3"}}
	if got := mover.delivered(context.Background(), parent); got != "Testing" {
		t.Fatalf("parent reached %q", got)
	}
	sub := &subtaskRun{mover: mover, notes: &config.JiraNotes{}, me: "token-owner", reached: map[string]string{"PROJ-1": "Testing"}, delivered: []jira.Issue{parent}}
	sub.follow(context.Background())

	if len(moved) != 1 || moved[0] != "PROJ-1" {
		t.Errorf("transition requests = %v, want only parent PROJ-1", moved)
	}
	for _, want := range []string{
		"PROJ-2 stays In Progress: the transition to Testing moves only Story, Task, Bug",
		"PROJ-3 stays In Progress after its parent PROJ-1: the transition to Testing moves only Story, Task, Bug",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q does not contain %q", stderr.String(), want)
		}
	}
}

func TestSweepTransitionTypesKeepOtherTypesInPlace(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/transitions") {
			requests++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/myself") {
			fmt.Fprint(w, `{"accountId":"token-owner"}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	client := jira.New(jira.Credentials{BaseURL: srv.URL})
	transition := config.NoteTransition{From: []string{"Ready for Release"}, To: "Done", Types: []string{"Task"}}
	notes := &config.TargetNotes{Transitions: []config.NoteTransition{transition}, Sweep: &config.Sweep{Done: "Done"}}
	target := &config.DeployTarget{ReleaseNotes: notes}
	var output strings.Builder
	run := &sweepRun{
		app:    &App{Stdout: &output, Stderr: &strings.Builder{}},
		r:      &deployRun{name: "prod", target: target},
		client: client, conf: notes.Sweep, notes: &config.JiraNotes{},
	}
	issue := jira.Issue{Key: "PROJ-2", Type: "Sub-task", Status: "Ready for Release"}
	run.item(t.Context(), issue, []sweptCommit{{sha: "abc", when: time.Now(), onTarget: true}})
	if requests != 0 {
		t.Fatalf("transition requests = %d, want 0", requests)
	}
	want := "PROJ-2 stays Ready for Release: the transition to Done moves only Task"
	if !slices.Contains(run.lines, want) {
		t.Fatalf("sweep lines = %q, want %q", run.lines, want)
	}
	run.move(t.Context(), jira.Issue{
		Key: "PROJ-3", Type: "Sub-task", Status: "Testing", Parent: "PROJ-1",
	}, "accepted with PROJ-1", true)
	reviewerWant := "PROJ-3 stays Testing: the transition to Done moves only Task"
	if !slices.Contains(run.lines, reviewerWant) {
		t.Fatalf("sweep lines = %q, want reviewer transition message %q", run.lines, reviewerWant)
	}
	if requests != 0 {
		t.Fatalf("reviewer transition requests = %d, want 0", requests)
	}
}

// Of several transitions to done, the one from the item's status decides
// whether its type moves.
func TestSweepTransitionTypesUseTheTransitionFromTheStatus(t *testing.T) {
	notes := &config.TargetNotes{Transitions: []config.NoteTransition{
		{From: []string{"Other"}, To: "Done", Types: []string{"Sub-task"}},
		{From: []string{"Ready for Release"}, To: "Done", Types: []string{"Task"}},
	}, Sweep: &config.Sweep{Done: "Done"}}
	run := &sweepRun{r: &deployRun{target: &config.DeployTarget{ReleaseNotes: notes}}, conf: notes.Sweep, review: []string{"Testing"}}
	if got := run.typeStop(jira.Issue{Key: "PROJ-2", Type: "Sub-task", Status: "Ready for Release"}); got != "PROJ-2 stays Ready for Release: the transition to Done moves only Task" {
		t.Errorf("done transition: typeStop = %q", got)
	}
	// Of several transitions into the from a reviewer's subtask goes through,
	// the one from its status decides.
	notes = &config.TargetNotes{Transitions: []config.NoteTransition{
		{From: []string{"Ready for Release"}, To: "Done", Types: []string{"Sub-task", "Task"}},
		{From: []string{"Elsewhere"}, To: "Ready for Release", Types: []string{"Sub-task"}},
		{From: []string{"Testing"}, To: "Ready for Release", Types: []string{"Task"}},
	}, Sweep: &config.Sweep{Done: "Done"}}
	run.r.target.ReleaseNotes, run.conf = notes, notes.Sweep
	if got := run.typeStop(jira.Issue{Key: "PROJ-3", Type: "Sub-task", Status: "Testing", Parent: "PROJ-1"}); got != "PROJ-3 stays Testing: the transition to Ready for Release moves only Task" {
		t.Errorf("hop transition: typeStop = %q", got)
	}
}

// The route sweep checks is the route it takes, the one of origin/main: a
// reviewer's subtask goes through the first done transition's from even with
// a transition of its own to done, so that transition's types decide.
func TestSweepTransitionDoneTakesTheCheckedRoute(t *testing.T) {
	var moves []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/transitions") {
			body, _ := io.ReadAll(r.Body)
			moves = append(moves, string(body))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	notes := &config.TargetNotes{Transitions: []config.NoteTransition{
		{From: []string{"Ready for Release"}, To: "Done", Types: []string{"Task"}},
		{From: []string{"Testing"}, To: "Done", Types: []string{"Sub-task"}},
	}, Sweep: &config.Sweep{Done: "Done"}}
	run := &sweepRun{
		app:    &App{Stdout: &strings.Builder{}, Stderr: &strings.Builder{}},
		r:      &deployRun{name: "prod", target: &config.DeployTarget{ReleaseNotes: notes}},
		client: jira.New(jira.Credentials{BaseURL: srv.URL}), conf: notes.Sweep, review: []string{"Testing"},
	}
	issue := jira.Issue{Key: "PROJ-2", Type: "Sub-task", Status: "Testing", Parent: "PROJ-1",
		Transitions: []jira.Transition{{ID: "1", To: "Done"}, {ID: "2", To: "Ready for Release"}}}
	want := "PROJ-2 stays Testing: the transition to Done moves only Task"
	if stop := run.typeStop(issue); stop != want {
		t.Fatalf("typeStop = %q, want %q", stop, want)
	}
	if err := run.transitionDone(t.Context(), issue); !errors.Is(err, errTypeNotAllowed) || len(moves) != 0 {
		t.Fatalf("transitionDone = %v, requests %q, want the type refusal and none", err, moves)
	}
}

// After the hop sweep reads the item again: Jira having put it elsewhere, the
// move to done from there must take its type.
func TestSweepTransitionDoneChecksTheStatusAfterTheHop(t *testing.T) {
	var moves []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/transitions"):
			body, _ := io.ReadAll(r.Body)
			moves = append(moves, string(body))
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/PROJ-2"):
			fmt.Fprint(w, `{"fields":{"summary":"synthetic","status":{"name":"Other","statusCategory":{"key":"indeterminate"}},"issuetype":{"name":"Sub-task","subtask":true},"parent":{"key":"PROJ-1"},"labels":[]},"transitions":[{"id":"3","name":"Done","to":{"name":"Done"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	notes := &config.TargetNotes{Transitions: []config.NoteTransition{
		{From: []string{"Ready for Release"}, To: "Done", Types: []string{"Sub-task"}},
		{From: []string{"Other"}, To: "Done", Types: []string{"Task"}},
	}, Sweep: &config.Sweep{Done: "Done"}}
	run := &sweepRun{
		app:    &App{Stdout: &strings.Builder{}, Stderr: &strings.Builder{}},
		r:      &deployRun{name: "prod", target: &config.DeployTarget{ReleaseNotes: notes}},
		client: jira.New(jira.Credentials{BaseURL: srv.URL}), conf: notes.Sweep, review: []string{"Testing"},
		notes: &config.JiraNotes{}, glint: &glintCheck{}, judged: map[string]bool{}, issues: map[string]jira.Issue{}, apply: true,
	}
	issue := jira.Issue{Key: "PROJ-2", Type: "Sub-task", Status: "Testing", Parent: "PROJ-1",
		Transitions: []jira.Transition{{ID: "2", To: "Ready for Release"}}}
	if err := run.transitionDone(t.Context(), issue); !errors.Is(err, errTypeNotAllowed) || len(moves) != 1 {
		t.Fatalf("transitionDone = %v, requests %q, want the hop only and the type refusal", err, moves)
	}
	// The pass lists where it stays, as a refusal before the hop is listed.
	run.move(t.Context(), issue, "synthetic", true)
	if want := "PROJ-2 stays Other: the transition to Done moves only Task"; !slices.Contains(run.lines, want) {
		t.Fatalf("sweep lines %q, want %q", run.lines, want)
	}
}

// Jira offering a reviewer's subtask no direct move to done, sweep takes it
// through the done transition's from, as before types.
func TestSweepTransitionDoneHopsWhenJiraOffersNoDirectMove(t *testing.T) {
	var moves []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/transitions"):
			body, _ := io.ReadAll(r.Body)
			moves = append(moves, string(body))
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/PROJ-2"):
			fmt.Fprint(w, `{"fields":{"summary":"synthetic","status":{"name":"Ready for Release","statusCategory":{"key":"indeterminate"}},"issuetype":{"name":"Sub-task","subtask":true},"parent":{"key":"PROJ-1"},"labels":[]},"transitions":[{"id":"3","name":"Done","to":{"name":"Done"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	notes := &config.TargetNotes{Transitions: []config.NoteTransition{
		{From: []string{"Ready for Release"}, To: "Done"},
		{From: []string{"Testing"}, To: "Done"},
	}, Sweep: &config.Sweep{Done: "Done"}}
	run := &sweepRun{
		app:    &App{Stdout: &strings.Builder{}, Stderr: &strings.Builder{}},
		r:      &deployRun{name: "prod", target: &config.DeployTarget{ReleaseNotes: notes}},
		client: jira.New(jira.Credentials{BaseURL: srv.URL}), conf: notes.Sweep, review: []string{"Testing"},
	}
	issue := jira.Issue{Key: "PROJ-2", Type: "Sub-task", Status: "Testing", Parent: "PROJ-1",
		Transitions: []jira.Transition{{ID: "2", To: "Ready for Release"}}}
	if err := run.transitionDone(t.Context(), issue); err != nil {
		t.Fatal(err)
	}
	if len(moves) != 2 || !strings.Contains(moves[0], `"2"`) || !strings.Contains(moves[1], `"3"`) {
		t.Fatalf("transition requests = %q, want Ready for Release, then Done", moves)
	}
}

// A subtask with the reviewer reaches done through the from of the done
// transition: the transition into that from must move its type too.
func TestSweepTransitionTypesGuardTheHopBeforeDone(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/transitions") {
			requests++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/myself") {
			fmt.Fprint(w, `{"accountId":"token-owner"}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	notes := &config.TargetNotes{Transitions: []config.NoteTransition{
		{From: []string{"Testing"}, To: "Ready for Release", Types: []string{"Task"}},
		{From: []string{"Ready for Release"}, To: "Done", Types: []string{"Sub-task", "Task"}},
	}, Sweep: &config.Sweep{Done: "Done"}}
	run := &sweepRun{
		app:    &App{Stdout: &strings.Builder{}, Stderr: &strings.Builder{}},
		r:      &deployRun{name: "prod", target: &config.DeployTarget{ReleaseNotes: notes}},
		client: jira.New(jira.Credentials{BaseURL: srv.URL}), conf: notes.Sweep, notes: &config.JiraNotes{},
		review: []string{"Testing"}, judged: map[string]bool{},
	}
	run.move(t.Context(), jira.Issue{Key: "PROJ-4", Type: "Sub-task", Status: "Testing", Parent: "PROJ-1"}, "accepted with PROJ-1", true)
	want := "PROJ-4 stays Testing: the transition to Ready for Release moves only Task"
	if requests != 0 || !slices.Contains(run.lines, want) {
		t.Fatalf("transition requests = %d, lines = %q, want 0 and %q", requests, run.lines, want)
	}
}

func TestSweepInFlowUsesIssueLabels(t *testing.T) {
	target := &config.DeployTarget{ApprovedRelease: &config.ApprovedRelease{
		Statuses:      []string{"Ready for Release"},
		LabelStatuses: map[string][]string{"internal": {"Internal Review"}},
	}}
	run := &sweepRun{r: &deployRun{cfg: &config.Config{Deploy: &config.Deploy{Targets: map[string]*config.DeployTarget{"prod": target}}}}}
	if run.inFlow(jira.Issue{Key: "PROJ-1", Status: "Internal Review"}) {
		t.Fatal("unlabelled issue in a status belonging only to another label is in flow")
	}
	if !run.inFlow(jira.Issue{Key: "PROJ-1", Status: "Internal Review", Labels: []string{"internal"}}) {
		t.Fatal("labelled issue's approved status is not in flow")
	}
}

func TestVersionStatusOutputNamesLabelStatuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/rest/api/3/search/jql") {
			fmt.Fprint(w, `{"issues":[{"key":"PROJ-1"}],"isLast":true}`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/PROJ-1") {
			fmt.Fprint(w, `{"fields":{"summary":"synthetic","status":{"name":"In Progress","statusCategory":{"key":"indeterminate"}},"issuetype":{"name":"Task"},"labels":["internal"]}}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	conf := &config.ApprovedRelease{
		Statuses:      []string{"Ready for Release"},
		LabelStatuses: map[string][]string{"internal": {"Internal Review"}},
		Version:       &config.ApprovedReleaseVersion{Refs: map[string]string{}},
	}
	app := &App{Stdout: &strings.Builder{}, Stderr: &strings.Builder{}}
	gate := &approvalGate{client: jira.New(jira.Credentials{BaseURL: srv.URL}), conf: conf, issues: map[string]jira.Issue{}}
	check := &releaseVersionCheck{
		gate: &gateRun{app: app, conf: conf, gate: gate}, collect: true,
		selected: jira.Version{ID: "v1", Name: "Synthetic"},
	}
	check.checkItems(t.Context())
	want := "PROJ-1 (In Progress); move to Ready for Release or Internal Review"
	if len(check.failures) != 1 || !strings.Contains(check.failures[0].Error(), want) || strings.Contains(check.failures[0].Error(), "an approved status") {
		t.Fatalf("version status failures = %v, want per-item statuses %q", check.failures, want)
	}
}

func TestVersionStatusOutputWithoutLabelStatuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/rest/api/3/search/jql") {
			fmt.Fprint(w, `{"issues":[{"key":"PROJ-1"}],"isLast":true}`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/PROJ-1") {
			fmt.Fprint(w, `{"fields":{"summary":"synthetic","status":{"name":"In Progress","statusCategory":{"key":"indeterminate"}},"issuetype":{"name":"Task"},"labels":[]}}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	conf := &config.ApprovedRelease{
		Statuses: []string{"Ready for Release"},
		Version:  &config.ApprovedReleaseVersion{Refs: map[string]string{}},
	}
	app := &App{Stdout: &strings.Builder{}, Stderr: &strings.Builder{}}
	client := jira.New(jira.Credentials{BaseURL: srv.URL})
	gate := &approvalGate{client: client, conf: conf, issues: map[string]jira.Issue{}}
	check := &releaseVersionCheck{
		gate: &gateRun{app: app, conf: conf, gate: gate}, collect: true,
		selected: jira.Version{ID: "v1", Name: "Synthetic"},
	}
	check.checkItems(t.Context())
	want := "Synthetic: PROJ-1 (In Progress); move to Ready for Release"
	if len(check.failures) != 1 || !strings.Contains(check.failures[0].Error(), want) {
		t.Fatalf("version status failures = %v, want %q", check.failures, want)
	}
}

func TestQuietReleaseNotesSkipCommentsButStillMoveItems(t *testing.T) {
	for _, tc := range []struct {
		name  string
		quiet *config.QuietNotes
		want  int
	}{
		{name: "quiet", quiet: &config.QuietNotes{Subtasks: true, Labels: []string{"internal"}}, want: 1},
		{name: "without quiet", want: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			comments, transitions := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/comment"):
					comments++
					w.WriteHeader(http.StatusCreated)
				case strings.HasSuffix(r.URL.Path, "/transitions"):
					transitions++
					w.WriteHeader(http.StatusNoContent)
				case strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/"):
					key := strings.TrimPrefix(r.URL.Path, "/rest/api/3/issue/")
					subtask, label := key == "PROJ-1", []string{}
					if key == "PROJ-2" {
						label = []string{"internal"}
					}
					fmt.Fprintf(w, `{"fields":{"summary":"synthetic","status":{"name":"In Progress","statusCategory":{"key":"indeterminate"}},"assignee":{"accountId":"token-owner","displayName":"Synthetic"},"issuetype":{"name":"Task","subtask":%t},"parent":{"key":"PROJ-10"},"labels":%s},"transitions":[{"id":"1","to":{"name":"Done"}}]}`, subtask, mustJSONLabels(label))
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			cfg := &config.Config{ReleaseNotes: &config.ReleaseNotes{Jira: &config.JiraNotes{ProjectKeys: []string{"PROJ"}, Quiet: tc.quiet}}}
			notes := &config.JiraNotes{Quiet: tc.quiet}
			transition := config.NoteTransition{From: []string{"In Progress"}, To: "Done"}
			out := &bytes.Buffer{}
			app := &App{Stdout: out, Stderr: &bytes.Buffer{}}
			client := jira.New(jira.Credentials{BaseURL: srv.URL})
			mover := &noteMover{app: app, client: client, conf: &config.TargetNotes{Transitions: []config.NoteTransition{transition}}, me: "token-owner", glint: &glintCheck{}}
			sub := &subtaskRun{mover: mover, notes: notes, reached: map[string]string{}}
			items := &noteItems{app: app, run: &deployRun{cfg: cfg}, client: client, me: "token-owner", text: "synthetic release note", fix: &fixVersionRun{}, mover: mover, sub: sub}
			for _, key := range []string{"PROJ-1", "PROJ-2", "PROJ-3"} {
				items.post(t.Context(), key)
			}
			if comments != tc.want || transitions != 3 {
				t.Errorf("comments/transitions = %d/%d, want %d/3", comments, transitions, tc.want)
			}
			if tc.quiet != nil {
				for _, message := range []string{
					"release note for PROJ-1 not posted: release_notes.jira.quiet (subtask)",
					"release note for PROJ-2 not posted: release_notes.jira.quiet (label internal)",
				} {
					if strings.Count(out.String(), message) != 1 {
						t.Errorf("output has %d instances of %q", strings.Count(out.String(), message), message)
					}
				}
			} else if strings.Contains(out.String(), "release_notes.jira.quiet") {
				t.Errorf("unexpected quiet output: %s", out.String())
			}
		})
	}
}

func mustJSONLabels(labels []string) string {
	if len(labels) == 0 {
		return "[]"
	}
	return `["` + strings.Join(labels, `","`) + `"]`
}
