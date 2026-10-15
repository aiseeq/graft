package main

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeJira records comments, transitions and fix version edits. The token
// belongs to the account "me"; assignees maps keys to other accounts ("" for
// nobody), and a key missing there is assigned to "me". handedOver lists keys
// whose history shows them handed over from "me". versions are the versions of
// project PROJ, fixVersions the version ids each item has; failEdit lists keys
// whose edit Jira refuses.
type fakeJira struct {
	mu              sync.Mutex
	comments        map[string][]string
	moved           []string
	statuses        map[string]string
	assignees       map[string]string
	handedOver      []string
	failAll         bool
	versions        []map[string]any
	versionRequests int
	fixVersions     map[string][]string
	edits           []string
	failEdit        []string
	// approvers maps a version id to the statuses of its approvers.
	approvers map[string][]string
	// parents maps a subtask's key to its parent's.
	parents map[string]string
	// assigned records "KEY to account" for every assignment, failAssign
	// lists keys whose assignment Jira refuses.
	assigned   []string
	failAssign []string
	// labels maps a key to its labels.
	labels map[string][]string
}

// fakeTransitions are the moves every item offers, by id.
var fakeTransitions = map[string]string{"31": "Testing", "41": "Ready for release", "51": "Done"}

func (j *fakeJira) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failAll {
		http.Error(w, "down for maintenance", http.StatusInternalServerError)
		return
	}
	if user, pass, ok := r.BasicAuth(); !ok || user != "me@example.com" || pass != "secret-token" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/rest/api/3/myself" {
		_ = json.NewEncoder(w).Encode(map[string]string{"accountId": "me", "displayName": "Me"})
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/rest/api/3/project/PROJ/versions" {
		j.versionRequests++
		_ = json.NewEncoder(w).Encode(j.versions)
		return
	}
	if id, ok := strings.CutPrefix(r.URL.Path, "/rest/api/3/version/"); ok && r.Method == http.MethodGet && r.URL.Query().Get("expand") == "approvers" {
		approvers := []any{}
		for i, status := range j.approvers[id] {
			approvers = append(approvers, map[string]string{"accountId": "approver" + strconv.Itoa(i), "status": status})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "approvers": approvers})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/rest/api/3/issue/"), "/")
	key := parts[0]
	switch {
	case r.Method == http.MethodPut && len(parts) == 1:
		j.edit(w, r, key)
	case r.Method == http.MethodPut && len(parts) == 2 && parts[1] == "assignee":
		var body struct {
			AccountID string `json:"accountId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.AccountID == "" {
			http.Error(w, "unexpected assignee body", http.StatusBadRequest)
			return
		}
		if slices.Contains(j.failAssign, key) {
			http.Error(w, `{"errorMessages":["User cannot be assigned"]}`, http.StatusBadRequest)
			return
		}
		j.assigned = append(j.assigned, key+" to "+body.AccountID)
		if a, ok := j.assignees[key]; !ok || a == "me" {
			j.handedOver = append(j.handedOver, key)
		}
		j.assignees[key] = body.AccountID
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "comment":
		body, _ := io.ReadAll(r.Body)
		var doc struct {
			Body struct {
				Content []struct {
					Content []struct{ Text string } `json:"content"`
				} `json:"content"`
			} `json:"body"`
		}
		_ = json.Unmarshal(body, &doc)
		j.comments[key] = append(j.comments[key], doc.Body.Content[0].Content[0].Text)
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "changelog":
		var values []any
		if slices.Contains(j.handedOver, key) {
			values = append(values, map[string]any{"items": []any{map[string]any{"fieldId": "assignee", "from": "me", "to": j.assignees[key]}}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"isLast": true, "values": values})
	case r.Method == http.MethodGet && len(parts) == 1:
		var assignee any = map[string]string{"accountId": "me", "displayName": "Me"}
		if a, ok := j.assignees[key]; ok {
			assignee = nil
			if a != "" {
				assignee = map[string]string{"accountId": a, "displayName": "Colleague " + a}
			}
		}
		category := "indeterminate"
		switch j.statuses[key] {
		case "Done":
			category = "done"
		case "To Do":
			category = "new"
		}
		fixVersions := []any{}
		for _, id := range j.fixVersions[key] {
			fixVersions = append(fixVersions, map[string]any{"id": id, "name": j.versionName(id)})
		}
		fields := map[string]any{
			"status":      map[string]any{"name": j.statuses[key], "statusCategory": map[string]string{"key": category}},
			"assignee":    assignee,
			"fixVersions": fixVersions,
			"issuetype":   map[string]bool{"subtask": j.parents[key] != ""},
			"labels":      append([]string{}, j.labels[key]...),
		}
		if p := j.parents[key]; p != "" {
			fields["parent"] = map[string]string{"key": p}
		}
		subtasks := []any{}
		for _, st := range slices.Sorted(maps.Keys(j.parents)) {
			if j.parents[st] == key {
				subtasks = append(subtasks, map[string]string{"key": st})
			}
		}
		fields["subtasks"] = subtasks
		transitions := []any{}
		for _, id := range slices.Sorted(maps.Keys(fakeTransitions)) {
			transitions = append(transitions, map[string]any{"id": id, "to": map[string]string{"name": fakeTransitions[id]}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"fields": fields, "transitions": transitions})
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "transitions":
		var body struct {
			Transition struct {
				ID string `json:"id"`
			} `json:"transition"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "unexpected transition body", http.StatusBadRequest)
			return
		}
		to, ok := fakeTransitions[body.Transition.ID]
		if !ok {
			http.Error(w, "unknown transition", http.StatusBadRequest)
			return
		}
		j.moved = append(j.moved, key)
		j.statuses[key] = to
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unexpected", http.StatusNotFound)
	}
}

// edit takes only an update that adds one fix version by id.
func (j *fakeJira) edit(w http.ResponseWriter, r *http.Request, key string) {
	var body struct {
		Fields map[string]any                            `json:"fields"`
		Update map[string][]map[string]map[string]string `json:"update"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Fields) > 0 || len(body.Update) != 1 || len(body.Update["fixVersions"]) != 1 {
		http.Error(w, "unexpected edit", http.StatusBadRequest)
		return
	}
	id := body.Update["fixVersions"][0]["add"]["id"]
	if id == "" {
		http.Error(w, "no add id", http.StatusBadRequest)
		return
	}
	if slices.Contains(j.failEdit, key) {
		http.Error(w, `{"errors":{"fixVersions":"Field 'fixVersions' cannot be set."}}`, http.StatusBadRequest)
		return
	}
	j.edits = append(j.edits, key+" add "+id)
	j.fixVersions[key] = append(j.fixVersions[key], id)
	w.WriteHeader(http.StatusNoContent)
}

func (j *fakeJira) versionName(id string) string {
	for _, v := range j.versions {
		if v["id"] == id {
			return v["name"].(string)
		}
	}
	return "version " + id
}

func (j *fakeJira) setFailing() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.failAll = true
}

// snapshot copies what the server recorded.
func (j *fakeJira) snapshot() (map[string][]string, []string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	comments := map[string][]string{}
	for k, v := range j.comments {
		comments[k] = slices.Clone(v)
	}
	return comments, slices.Clone(j.moved)
}

type deployFixture struct {
	*fixture
	state string
	jira  *fakeJira
}

func deployRepo(t *testing.T) *deployFixture {
	return newDeployFixture(t, deployConfig)
}

// newDeployFixture builds a repository whose .graft.yaml is config(state, q),
// q quoting a path in the state directory, beside a fake Jira and a stand-in
// deploy script.
func newDeployFixture(t *testing.T, config func(state string, q func(string) string) string) *deployFixture {
	if runtime.GOOS == "windows" {
		t.Skip("deploy scripts use sh")
	}
	state := t.TempDir()
	jira := &fakeJira{
		comments:    map[string][]string{},
		fixVersions: map[string][]string{},
		statuses:    map[string]string{"PROJ-1": "In Progress", "PROJ-2": "Done", "PROJ-3": "In Progress", "PROJ-4": "To Do", "PROJ-5": "Testing", "PROJ-6": "Paused", "PROJ-7": "Blocked"},
		assignees:   map[string]string{"PROJ-3": "colleague", "PROJ-4": "", "PROJ-5": "reviewer"},
		handedOver:  []string{"PROJ-5"},
	}
	srv := httptest.NewServer(jira)
	t.Cleanup(srv.Close)

	cfgDir := t.TempDir()
	envFile := filepath.Join(cfgDir, "jira.env")
	writeFile(t, envFile, "JIRA_BASE_URL="+srv.URL+"\nJIRA_EMAIL=me@example.com\nJIRA_API_TOKEN=secret-token\n")
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "jira:\n  env_file: "+envFile+"\n")

	q := func(p string) string { return yamlQuote(filepath.Join(state, p)) }
	f := newRepo(t, map[string]string{
		".graft.yaml": config(state, q),
		"VERSION":     "1.0.0\n",
		// The stand-in deploy script records what it was given.
		"deploy.sh": `set -e
STATE=` + state + `
{ echo "target=$GRAFT_DEPLOY_TARGET env=$GRAFT_DEPLOY_ENV version=$GRAFT_DEPLOY_VERSION"; echo "sha=$GRAFT_DEPLOY_SHA"; echo "prev=$GRAFT_DEPLOY_PREVIOUS_SHA"; echo "args=$*"; } > "$STATE/$GRAFT_DEPLOY_TARGET-run"
graft deploy check-head
if [ -n "$MOVE_HEAD" ]; then git -c core.hooksPath=/dev/null commit -q --allow-empty -m moved; fi
echo "$GRAFT_DEPLOY_VERSION" > "$STATE/$GRAFT_DEPLOY_TARGET-version"
exit "${DEPLOY_EXIT:-0}"
`,
	}, "origin")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir)
	f.mustGraft("commit", "-m", "feat: base")
	return &deployFixture{fixture: f, state: state, jira: jira}
}

func deployConfig(state string, q func(string) string) string {
	return `schema: 1
version: {mode: file}
envs:
  local: {}
deploy:
  remote: origin
  targets:
    test:
      env: local
      run: [sh, deploy.sh]
      version: 'cat ` + filepath.Join(state, "test-version") + `'
      deployed_sha: {path: ` + q("test-sha") + `}
      release_notes: {transitions: [{from: [In Progress], to: Testing}]}
      status: 'echo test runs; cat ` + filepath.Join(state, "test-version") + `'
      logs: "printf 'a\nerror one\nb\nerror two\n' | tail -n {lines}"
    uat:
      env: local
      run: [sh, deploy.sh]
      deployed_sha: {path: ` + q("uat-sha") + `}
      release_notes: {transitions: [{from: [In Progress], to: Testing}], fix_version: {prefix: "Product v"}}
    sub:
      env: local
      run: [sh, deploy.sh]
      deployed_sha: {path: ` + q("sub-sha") + `}
      release_notes: {transitions: [{from: [In Progress], to: Testing, assign: qa}, {from: [Ready for release], to: Done}], subtasks: true}
    stage:
      env: local
      run: [sh, deploy.sh]
      requires: test
    prod:
      env: local
      run: [sh, deploy.sh]
      requires: test
      confirm: sudo
    live:
      env: local
      run: [sh, deploy.sh]
      deployed_sha: {path: ` + q("live-sha") + `}
      approved_release: {prefix: "Product v", ignore_paths: ["docs/**", "*.md"]}
    gate:
      env: local
      run: [sh, deploy.sh]
      deployed_sha: {path: ` + q("gate-sha") + `}
      approved_release: {statuses: [Ready for release, Done]}
release_notes:
  jira:
    project_keys: [PROJ]
    skip_statuses: [Blocked]
`
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (d *deployFixture) stateFile(name string) string {
	data, err := os.ReadFile(filepath.Join(d.state, name))
	if err != nil {
		return ""
	}
	return string(data)
}

func TestDeployRefusesUnpublishedWork(t *testing.T) {
	d := deployRepo(t)
	d.write("extra.txt", "x\n")
	if out, code := d.graft("", "deploy", "test"); code == 0 || !strings.Contains(out, "not clean") {
		t.Errorf("dirty tree: %d\n%s", code, out)
	}
	d.git("add", "-A")
	d.run(d.work, "git", "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "local only")
	if out, code := d.graft("", "deploy", "test"); code == 0 || !strings.Contains(out, "is not origin/feature/PROJ-42-thing") {
		t.Errorf("unpushed commit: %d\n%s", code, out)
	}
	if d.stateFile("test-run") != "" {
		t.Error("the deploy script ran")
	}
}

func TestDeployRunsScriptAndPostsReleaseNotes(t *testing.T) {
	d := deployRepo(t)
	out := d.mustGraft("deploy", "test", "--", "--skip-tests", "two words")
	head := d.git("rev-parse", "HEAD")
	run := d.stateFile("test-run")
	for _, want := range []string{"target=test env=local version=1.0.1", "sha=" + head, "prev=\n", "args=--skip-tests two words"} {
		if !strings.Contains(run, want) {
			t.Errorf("script did not get %q:\n%s", want, run)
		}
	}
	if !strings.Contains(out, "HEAD is still") {
		t.Errorf("check-head from the script:\n%s", out)
	}
	if got := strings.TrimSpace(d.stateFile("test-sha")); got != head {
		t.Errorf("deployed sha = %q, want %q", got, head)
	}
	if !strings.Contains(out, "no previously deployed commit recorded") {
		t.Errorf("first deploy has no range:\n%s", out)
	}

	d.write("a.txt", "a\n")
	d.mustGraft("commit", "-m", "feat: PROJ-1 first thing, hashes with SHA-256")
	d.write("b.txt", "b\n")
	d.mustGraft("commit", "-m", "fix: second\n\nRefs PROJ-2 and OTHER-5, see also PROJ-3, PROJ-4, PROJ-5, PROJ-6 and PROJ-7")
	out = d.mustGraft("deploy", "test")
	if !strings.Contains(d.stateFile("test-run"), "prev="+head) {
		t.Errorf("previous sha not passed:\n%s", d.stateFile("test-run"))
	}
	comments, moved := d.jira.snapshot()
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range comments {
			if !yield(k) {
				return
			}
		}
	})
	if !slices.Equal(keys, []string{"PROJ-1", "PROJ-5", "PROJ-6"}) {
		t.Errorf("commented keys = %v\n%s", keys, out)
	}
	if c := comments["PROJ-1"]; len(c) != 1 || !strings.HasPrefix(c[0], "Deployed to test, version 1.0.3, commit ") {
		t.Errorf("comment = %v", c)
	}
	if !slices.Equal(moved, []string{"PROJ-1"}) {
		t.Errorf("moved = %v (PROJ-5 is already in Testing, PROJ-6 is paused)", moved)
	}
	for _, want := range []string{
		"PROJ-2 left alone: it is Done",
		"PROJ-6 stays Paused: release_notes.transitions move only from In Progress",
		"PROJ-7 left alone: it is Blocked",
		"PROJ-3 left alone: assigned to Colleague colleague and never assigned to the owner",
		"PROJ-4 left alone: it is To Do",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}
}

func TestDeployJiraFailureDoesNotFailTheDeploy(t *testing.T) {
	d := deployRepo(t)
	d.mustGraft("deploy", "test")
	d.write("a.txt", "a\n")
	d.mustGraft("commit", "-m", "feat: PROJ-1 x")
	d.jira.setFailing()
	out, code := d.graft("", "deploy", "test")
	if code != 0 || !strings.Contains(out, "release notes skipped") || !strings.Contains(out, "http 500") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

// productVersions are the versions of PROJ: the nearest unreleased
// "Product v" one is Product v1.2.10 (id 10010).
func productVersions() []map[string]any {
	return []map[string]any{
		{"id": "10009", "name": "Product v1.2.9", "released": true, "archived": false, "releaseDate": "2026-01-01"},
		{"id": "10030", "name": "Product v1.3.0", "released": false, "archived": false, "releaseDate": "2026-12-01"},
		{"id": "10200", "name": "Product v2.0.0", "released": false, "archived": false},
		{"id": "10010", "name": "Product v1.2.10", "released": false, "archived": false, "releaseDate": "2026-11-01"},
		{"id": "10001", "name": "Other v1.0.0", "released": false, "archived": false, "releaseDate": "2026-10-01"},
		{"id": "10008", "name": "Product v1.2.8", "released": false, "archived": true, "releaseDate": "2025-12-01"},
	}
}

// deliverAllKeys deploys to target once to record a commit, then commits a
// change that mentions PROJ-1 to PROJ-7.
func (d *deployFixture) deliverAllKeys(target string) {
	d.mustGraft("deploy", target)
	d.write("b.txt", "b\n")
	d.mustGraft("commit", "-m", "fix: second\n\nRefs PROJ-1, PROJ-2, PROJ-3, PROJ-4, PROJ-5, PROJ-6 and PROJ-7")
}

func (j *fakeJira) setVersions(versions []map[string]any, fixVersions map[string][]string, failEdit ...string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.versions, j.fixVersions, j.failEdit = versions, fixVersions, failEdit
}

func (j *fakeJira) editsSnapshot() ([]string, int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.edits), j.versionRequests
}

func TestDeployAddsFixVersion(t *testing.T) {
	d := deployRepo(t)
	// PROJ-5 has the version already, PROJ-6 has a version of its own.
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-5": {"10010"}, "PROJ-6": {"10001"}})
	d.deliverAllKeys("uat")
	out := d.mustGraft("deploy", "uat")
	edits, requests := d.jira.editsSnapshot()
	if !slices.Equal(edits, []string{"PROJ-1 add 10010", "PROJ-6 add 10010"}) {
		t.Errorf("edits = %v\n%s", edits, out)
	}
	if requests != 1 {
		t.Errorf("versions read %d times, want once per deploy", requests)
	}
	for _, want := range []string{"fix version Product v1.2.10 added to PROJ-1", "PROJ-5 already has fix version Product v1.2.10", "PROJ-1 moved to Testing"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}
	d.jira.mu.Lock()
	kept := slices.Clone(d.jira.fixVersions["PROJ-6"])
	d.jira.mu.Unlock()
	if !slices.Equal(kept, []string{"10001", "10010"}) {
		t.Errorf("PROJ-6 fix versions = %v, its own one must stay", kept)
	}

	// The next deploy finds the version on PROJ-1 and asks for no edit.
	d.write("c.txt", "c\n")
	d.mustGraft("commit", "-m", "fix: PROJ-1 third")
	out = d.mustGraft("deploy", "uat")
	if edits, _ := d.jira.editsSnapshot(); len(edits) != 2 {
		t.Errorf("edits after the second deploy = %v\n%s", edits, out)
	}
	if !strings.Contains(out, "PROJ-1 already has fix version Product v1.2.10") {
		t.Errorf("second deploy:\n%s", out)
	}
}

func TestDeployFixVersionFailureFailsTheDeploy(t *testing.T) {
	tie := append(productVersions(), map[string]any{"id": "10011", "name": "Product v1.2.11", "released": false, "archived": false, "releaseDate": "2026-11-01"})
	cases := []struct {
		name     string
		versions []map[string]any
		failEdit []string
		outage   bool
		unset    string
		want     []string
	}{
		{
			name:     "no version",
			versions: []map[string]any{productVersions()[0], productVersions()[4]},
			unset:    "PROJ-1, PROJ-5, PROJ-6",
			want:     []string{"PROJ-1: fix version NOT SET: ", `no unreleased version of PROJ starts with "Product v" (unreleased: Other v1.0.0)`, "PROJ-6: fix version NOT SET: "},
		},
		{
			name:     "tie",
			versions: tie,
			unset:    "PROJ-1, PROJ-5, PROJ-6",
			want:     []string{"PROJ-5: fix version NOT SET: ", "share the nearest release date 2026-11-01: Product v1.2.10, Product v1.2.11"},
		},
		{
			name:     "edit refused",
			versions: productVersions(),
			failEdit: []string{"PROJ-6"},
			unset:    "PROJ-6",
			want:     []string{"PROJ-6: fix version NOT SET: ", "http 400", "fix version Product v1.2.10 added to PROJ-1"},
		},
		{
			name:     "outage",
			versions: productVersions(),
			outage:   true,
			unset:    "PROJ-1, PROJ-2, PROJ-3, PROJ-4, PROJ-5, PROJ-6, PROJ-7",
			want:     []string{"PROJ-7: fix version NOT SET: ", "http 500"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := deployRepo(t)
			d.jira.setVersions(c.versions, map[string][]string{}, c.failEdit...)
			d.deliverAllKeys("uat")
			if c.outage {
				d.jira.setFailing()
			}
			out, code := d.graft("", "deploy", "uat")
			if code == 0 {
				t.Errorf("exit 0:\n%s", out)
			}
			want := append(slices.Clone(c.want), "but the fix version is not set on "+c.unset+" (see above)")
			for _, w := range want {
				if !strings.Contains(out, w) {
					t.Errorf("no %q:\n%s", w, out)
				}
			}
			// The release is live all the same: recorded and noted.
			if got := strings.TrimSpace(d.stateFile("uat-sha")); got != d.git("rev-parse", "HEAD") {
				t.Errorf("deployed sha = %q", got)
			}
			if comments, _ := d.jira.snapshot(); !c.outage && len(comments["PROJ-1"]) != 1 {
				t.Errorf("PROJ-1 comments = %v", comments["PROJ-1"])
			}
		})
	}
}

func (j *fakeJira) setApprovers(approvers map[string][]string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.approvers = approvers
}

// TestDeployFixVersionPassesOverApproved: once anybody approves the nearest
// version, its items are fixed and a later deploy notes into the next one.
func TestDeployFixVersionPassesOverApproved(t *testing.T) {
	d := deployRepo(t)
	d.jira.setVersions(productVersions(), map[string][]string{})
	d.jira.setApprovers(map[string][]string{"10010": {"APPROVED", "PENDING"}, "10030": {"PENDING"}})
	d.deliverAllKeys("uat")
	out := d.mustGraft("deploy", "uat")
	edits, _ := d.jira.editsSnapshot()
	if !slices.Equal(edits, []string{"PROJ-1 add 10030", "PROJ-5 add 10030", "PROJ-6 add 10030"}) {
		t.Errorf("edits = %v\n%s", edits, out)
	}
	if !strings.Contains(out, "Product v1.2.10 is being approved: no item joins it") {
		t.Errorf("no word about the approved version:\n%s", out)
	}

	// Every dated version approved, the undated one too: nothing to pick.
	d.jira.setApprovers(map[string][]string{"10010": {"APPROVED"}, "10030": {"APPROVED"}, "10200": {"APPROVED"}})
	d.write("c.txt", "c\n")
	d.mustGraft("commit", "-m", "fix: PROJ-1 third")
	out, code := d.graft("", "deploy", "uat")
	if code == 0 || !strings.Contains(out, `every unreleased version of PROJ starting with "Product v" is being approved`) {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

// TestDeployApprovedRelease: a target with approved_release lets out only
// commits whose items are in a version every approver approved.
func TestDeployApprovedRelease(t *testing.T) {
	d := deployRepo(t)
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-1": {"10010"}, "PROJ-5": {"10030"}, "PROJ-6": {"10001"}})
	d.jira.setApprovers(map[string][]string{"10010": {"APPROVED", "APPROVED"}, "10030": {"APPROVED", "PENDING"}, "10001": {"APPROVED"}})

	out, code := d.graft("", "deploy", "live")
	if code == 0 || !strings.Contains(out, "no deployed commit recorded, what goes out is unknown") {
		t.Errorf("first deploy without a recorded commit: exit %d\n%s", code, out)
	}
	out = d.mustGraft("deploy", "--unapproved", "live")
	if !strings.Contains(out, "approved release not checked (--unapproved)") {
		t.Errorf("--unapproved:\n%s", out)
	}
	deployed := d.git("rev-parse", "HEAD")

	d.write("a.txt", "a\n")
	d.mustGraft("commit", "-m", "feat: PROJ-1 approved thing")
	d.write("b.txt", "b\n")
	d.mustGraft("commit", "-m", "fix: PROJ-5 half approved")
	d.write("c.txt", "c\n")
	d.mustGraft("commit", "-m", "fix: PROJ-6 in another product")
	d.write("d.txt", "d\n")
	d.mustGraft("commit", "-m", "chore: tidy up")
	d.write("e.txt", "e\n")
	d.mustGraft("commit", "-m", "revert: PROJ-5 half approved")
	d.write("docs/guide with space.md", "g\n")
	d.write("NOTES.md", "n\n")
	d.mustGraft("commit", "-m", "docs: the guide")
	d.write("docs/more.md", "m\n")
	d.write("code.txt", "c\n")
	d.mustGraft("commit", "-m", "docs: PROJ-5 the guide and some code")
	out, code = d.graft("", "deploy", "live")
	if code == 0 {
		t.Fatalf("exit 0:\n%s", out)
	}
	for _, want := range []string{
		"nothing was deployed: approved release: commits outside an approved release",
		`fix: PROJ-5 half approved: PROJ-5 not in an approved "Product v" version`,
		`fix: PROJ-6 in another product: PROJ-6 not in an approved "Product v" version`,
		"chore: tidy up: names no item",
		"revert: PROJ-5 half approved: a revert, passes",
		"docs: the guide: changes only ignore_paths, passes",
		`docs: PROJ-5 the guide and some code: PROJ-5 not in an approved "Product v" version`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "feat: PROJ-1 approved thing:") {
		t.Errorf("the approved commit is listed:\n%s", out)
	}
	if got := strings.TrimSpace(d.stateFile("live-sha")); got != deployed {
		t.Errorf("deployed sha = %q, want the earlier %q", got, deployed)
	}

	// PROJ-5's version gets its last approval, PROJ-6 moves into it, the
	// chore names PROJ-1: everything goes out.
	d.jira.setApprovers(map[string][]string{"10010": {"APPROVED", "APPROVED"}, "10030": {"APPROVED", "APPROVED"}})
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-1": {"10010"}, "PROJ-5": {"10030"}, "PROJ-6": {"10001", "10010"}})
	d.write("f.txt", "f\n")
	d.mustGraft("commit", "-m", "chore: PROJ-1 tidy up again")
	out, code = d.graft("", "deploy", "live")
	if code == 0 || !strings.Contains(out, "chore: tidy up: names no item") || strings.Contains(out, "PROJ-5 not in") || strings.Contains(out, "PROJ-6 not in") {
		t.Errorf("only the keyless commit stays outside: exit %d\n%s", code, out)
	}
}

// TestDeployApprovedReleaseByStatus: with statuses, a commit goes out when an
// item it names, or the parent of a subtask it names, is in one of them. A
// revert of a revert brings the work back and is checked like any commit.
func TestDeployApprovedReleaseByStatus(t *testing.T) {
	d := deployRepo(t)
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-8"] = "Ready for release"
	d.jira.statuses["PROJ-9"] = "In Progress"
	d.jira.parents = map[string]string{"PROJ-9": "PROJ-8"}
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "--unapproved", "gate")
	for i, msg := range []string{
		"feat: PROJ-8 checked work",
		"fix: PROJ-9 subtask of checked work",
		"fix: PROJ-2 follow-up of done work",
		"feat: PROJ-5 still in testing",
		"feat: PROJ-1 in progress",
		"chore: no item",
		"Revert \"feat: PROJ-5 still in testing\"",
		"Revert \"Revert \"feat: PROJ-5 still in testing\"\"",
		"Reapply \"feat: PROJ-5 still in testing\"",
	} {
		d.write("f"+strconv.Itoa(i)+".txt", msg+"\n")
		d.mustGraft("commit", "-m", msg)
	}
	out, code := d.graft("", "deploy", "gate")
	if code == 0 {
		t.Fatalf("exit 0:\n%s", out)
	}
	for _, want := range []string{
		"commits outside an approved release",
		"feat: PROJ-5 still in testing: PROJ-5 is Testing, not Ready for release or Done",
		"feat: PROJ-1 in progress: PROJ-1 is In Progress, not Ready for release or Done",
		"chore: no item: names no item",
		`Revert "Revert "feat: PROJ-5 still in testing"": PROJ-5 is Testing`,
		`Reapply "feat: PROJ-5 still in testing": PROJ-5 is Testing`,
		`Revert "feat: PROJ-5 still in testing": a revert, passes`,
		"fix: PROJ-2 follow-up of done work: PROJ-2 is Done, passes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"PROJ-8 checked work:", "PROJ-9 subtask of checked work:"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("%q is listed:\n%s", unwanted, out)
		}
	}
}

func TestDeployApprovedReleaseGoesOut(t *testing.T) {
	d := deployRepo(t)
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-1": {"10010"}})
	d.jira.setApprovers(map[string][]string{"10010": {"APPROVED", "APPROVED"}})
	d.mustGraft("deploy", "--unapproved", "live")
	prev := d.git("rev-parse", "HEAD")[:12]
	d.write("a.txt", "a\n")
	d.mustGraft("commit", "-m", "feat: PROJ-1 approved thing")
	out := d.mustGraft("deploy", "live")
	if !strings.Contains(out, "approved release: 1 commits since "+prev+", all in Product v1.2.10") {
		t.Errorf("no summary:\n%s", out)
	}
	if got := strings.TrimSpace(d.stateFile("live-sha")); got != d.git("rev-parse", "HEAD") {
		t.Errorf("deployed sha = %q", got)
	}
}

// TestDeploySubtasksFollowParents: with subtasks, a parent that reaches a
// transition's status takes its open subtasks along by that transition, the
// reviewer included, and a delivered subtask follows its parent into a status
// a transition leads to whatever from says.
func TestDeploySubtasksFollowParents(t *testing.T) {
	d := deployRepo(t)
	d.jira.mu.Lock()
	for k, st := range map[string]string{"PROJ-20": "In Progress", "PROJ-21": "In Progress", "PROJ-22": "Done", "PROJ-23": "In Progress", "PROJ-24": "In Progress", "PROJ-30": "Done", "PROJ-31": "Paused", "PROJ-40": "In Progress", "PROJ-41": "In Progress"} {
		d.jira.statuses[k] = st
	}
	d.jira.assignees["PROJ-23"] = "colleague"
	d.jira.assignees["PROJ-24"] = "qa"
	d.jira.handedOver = append(d.jira.handedOver, "PROJ-24")
	d.jira.parents = map[string]string{"PROJ-21": "PROJ-20", "PROJ-22": "PROJ-20", "PROJ-23": "PROJ-20", "PROJ-24": "PROJ-20", "PROJ-31": "PROJ-30", "PROJ-41": "PROJ-40"}
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "sub")
	d.write("a.txt", "a\n")
	d.mustGraft("commit", "-m", "feat: PROJ-20 parent work\n\nAlso PROJ-31")
	out := d.mustGraft("deploy", "sub")
	_, moved := d.jira.snapshot()
	slices.Sort(moved)
	if !slices.Equal(moved, []string{"PROJ-20", "PROJ-21", "PROJ-24", "PROJ-31"}) {
		t.Errorf("moved = %v\n%s", moved, out)
	}
	d.jira.mu.Lock()
	assigned := slices.Sorted(slices.Values(d.jira.assigned))
	statuses := map[string]string{"PROJ-20": d.jira.statuses["PROJ-20"], "PROJ-21": d.jira.statuses["PROJ-21"], "PROJ-24": d.jira.statuses["PROJ-24"], "PROJ-31": d.jira.statuses["PROJ-31"]}
	d.jira.mu.Unlock()
	if !slices.Equal(assigned, []string{"PROJ-20 to qa", "PROJ-21 to qa"}) {
		t.Errorf("assigned = %v (PROJ-24 is on qa already)\n%s", assigned, out)
	}
	if want := map[string]string{"PROJ-20": "Testing", "PROJ-21": "Testing", "PROJ-24": "Testing", "PROJ-31": "Done"}; !maps.Equal(statuses, want) {
		t.Errorf("statuses = %v, want %v", statuses, want)
	}
	for _, want := range []string{
		"PROJ-20 moved to Testing\n",
		"PROJ-20 assigned to qa",
		"PROJ-21 moved to Testing after its parent PROJ-20\n",
		"PROJ-21 assigned to qa",
		"PROJ-24 moved to Testing after its parent PROJ-20\n",
		"PROJ-31 moved to Done after its parent PROJ-30",
		"PROJ-23 left alone: assigned to Colleague colleague",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}
}

// TestDeployAssignFailureFailsTheDeploy: an item moved but left on its
// developer is reported, and the deploy exits non-zero.
func TestDeployAssignFailureFailsTheDeploy(t *testing.T) {
	d := deployRepo(t)
	d.jira.mu.Lock()
	d.jira.failAssign = []string{"PROJ-1"}
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "sub")
	d.write("a.txt", "a\n")
	d.mustGraft("commit", "-m", "feat: PROJ-1 work")
	out, code := d.graft("", "deploy", "sub")
	if code == 0 || !strings.Contains(out, "PROJ-1: assignee NOT SET: ") || !strings.Contains(out, "the assignee is not set on PROJ-1") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if got := strings.TrimSpace(d.stateFile("sub-sha")); got != d.git("rev-parse", "HEAD") {
		t.Errorf("the deployed commit is not recorded: %q", got)
	}
}

func TestDeployScriptFailurePassesExitCode(t *testing.T) {
	d := deployRepo(t)
	d.env = append(d.env, "DEPLOY_EXIT=3")
	out, code := d.graft("", "deploy", "test")
	if code != 3 || !strings.Contains(out, "exit status 3") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if d.stateFile("test-sha") != "" {
		t.Error("a failed deploy was recorded")
	}
}

func TestDeployRefusesWhenHeadMoves(t *testing.T) {
	d := deployRepo(t)
	d.env = append(d.env, "MOVE_HEAD=1")
	out, code := d.graft("", "deploy", "test")
	if code == 0 || !strings.Contains(out, "HEAD moved during the deploy") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if d.stateFile("test-sha") != "" {
		t.Error("recorded a commit that was not the one deployed")
	}
}

func TestDeployRequiresVersionOnRequiredTarget(t *testing.T) {
	d := deployRepo(t)
	writeFile(t, filepath.Join(d.state, "test-version"), "0.9.0\n")
	out, code := d.graft("", "deploy", "stage")
	if code == 0 || !strings.Contains(out, "test runs version 0.9.0, not 1.0.1") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	d.mustGraft("deploy", "test")
	if out := d.mustGraft("deploy", "stage"); !strings.Contains(out, "test already runs version 1.0.1") {
		t.Errorf("stage after test:\n%s", out)
	}
}

// fakeSudo puts a sudo stand-in first on PATH: it records every call and
// exits with exit, as a fingerprint accepted or refused would.
func (d *deployFixture) fakeSudo(t *testing.T, exit int) string {
	t.Helper()
	return d.fakeSudoScript(t, "exit "+strconv.Itoa(exit))
}

// fakeSudoScript is fakeSudo whose body after recording the call is script;
// $n there is the number of the call.
func (d *deployFixture) fakeSudoScript(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	writeFile(t, filepath.Join(dir, "sudo"), "#!/bin/sh\necho \"$*\" >> "+yamlQuote(calls)+"\nn=$(wc -l < "+yamlQuote(calls)+")\n"+script+"\n")
	if err := os.Chmod(filepath.Join(dir, "sudo"), 0o755); err != nil {
		t.Fatal(err)
	}
	d.env = append(d.env, "PATH="+dir+string(os.PathListSeparator)+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

// Without a terminal sudo can still confirm without typing (a fingerprint,
// cached credentials): the deploy goes on.
func TestDeployConfirmWithoutTerminalBySudo(t *testing.T) {
	d := deployRepo(t)
	d.mustGraft("deploy", "test")
	calls := d.fakeSudo(t, 0)
	out := d.mustGraft("deploy", "prod")
	if !strings.Contains(out, "no terminal, touch the fingerprint reader to confirm the deploy to prod") {
		t.Errorf("no hint about the confirmation:\n%s", out)
	}
	if d.stateFile("prod-run") == "" {
		t.Error("prod deploy did not run after sudo confirmed it")
	}
	if got := readFile(t, calls); !strings.HasPrefix(got, "-v -p ") {
		t.Errorf("sudo calls: %q", got)
	}
}

// Without a terminal a fingerprint reader that refused (no finger in time) is
// asked again, and the deploy goes on once it accepts.
func TestDeployConfirmWithoutTerminalAsksTheReaderAgain(t *testing.T) {
	d := deployRepo(t)
	d.mustGraft("deploy", "test")
	calls := d.fakeSudoScript(t, `if [ "$n" -lt 3 ]; then echo "Verification timed out"; exit 1; fi`)
	out := d.mustGraft("deploy", "prod")
	for _, want := range []string{"fingerprint attempt 1 refused, asking again", "fingerprint attempt 2 refused, asking again"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}
	if d.stateFile("prod-run") == "" {
		t.Error("prod deploy did not run after the reader accepted")
	}
	if got := strings.Count(readFile(t, calls), "\n"); got != 3 {
		t.Errorf("sudo asked %d times, want 3", got)
	}
}

// A sudo refusal without a terminal that did not come from the fingerprint
// reader ends the deploy after one attempt: asking again where nobody can type
// would wait forever.
func TestDeployConfirmWithoutTerminalRefused(t *testing.T) {
	d := deployRepo(t)
	d.mustGraft("deploy", "test")
	calls := d.fakeSudo(t, 1)
	out, code := d.graft("", "deploy", "prod")
	if code == 0 || !strings.Contains(out, "deploying to prod was not confirmed") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if d.stateFile("prod-run") != "" {
		t.Error("prod deploy ran without confirmation")
	}
	if got := strings.Count(readFile(t, calls), "\n"); got != 1 {
		t.Errorf("sudo asked %d times, want 1", got)
	}
}

func TestDeployStatusAndLogs(t *testing.T) {
	d := deployRepo(t)
	d.mustGraft("deploy", "test")
	out := d.mustGraft("deploy", "status", "test")
	subject := d.git("log", "-1", "--format=%h %s (%cs)")
	if !strings.Contains(out, "graft: deployed "+subject+"\ntest runs\n1.0.1") {
		t.Errorf("status:\n%s", out)
	}
	// A recorded commit the local repository does not have.
	if err := os.WriteFile(filepath.Join(d.state, "test-sha"), []byte(strings.Repeat("ab", 20)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := d.mustGraft("deploy", "status", "test"); !strings.Contains(out, "deployed abababababab, not in the local repository") {
		t.Errorf("status with an unknown commit:\n%s", out)
	}
	out = d.mustGraft("deploy", "logs", "test", "--lines", "3", "--grep", "error")
	if out != "error one\nerror two\n" {
		t.Errorf("logs: %q", out)
	}
	if out := d.mustGraft("deploy", "logs", "test", "--grep", "nothing-like-this"); !strings.Contains(out, "no lines match") {
		t.Errorf("no match:\n%s", out)
	}
	if out, code := d.graft("", "deploy", "status", "stage"); code == 0 || !strings.Contains(out, "neither status nor deployed_sha is configured") {
		t.Errorf("unconfigured status: %d\n%s", code, out)
	}
	if out, code := d.graft("", "deploy", "nowhere"); code == 0 || !strings.Contains(out, "unknown deploy target") {
		t.Errorf("unknown target: %d\n%s", code, out)
	}
}

func TestDeployKeysAndCommandArguments(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("deploy scripts use sh")
	}
	state := t.TempDir()
	f := newRepo(t, map[string]string{
		".graft.yaml": `schema: 1
version: {mode: none}
envs:
  local: {}
dotenv_sets:
  ship: [SHIP_HOST, 'SHIP_PORT?']
deploy:
  remote: origin
  targets:
    box:
      env: local
      run: [sh, deploy.sh]
      dotenv_sets: [ship]
      dotenv: [SHIP_TOKEN]
      status: [sh, -c, 'echo "status $*"', sh, '{args}']
      logs: echo logs {lines} app-{args}
`,
		".gitignore": ".env\n",
		"deploy.sh":  `echo "host=$SHIP_HOST port=${SHIP_PORT-unset} token=$SHIP_TOKEN other=${OTHER-unset}" > ` + filepath.Join(state, "out") + "\n",
	}, "origin")
	f.mustGraft("commit", "-m", "feat: base")

	f.write(".env", "SHIP_HOST=h\nOTHER=o\n")
	out, code := f.graft("", "deploy", "box")
	if code != 1 || !strings.Contains(out, "deploy box: nothing was done, missing:") || !strings.Contains(out, "SHIP_TOKEN is not set") {
		t.Errorf("missing key: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(state, "out")); !os.IsNotExist(err) {
		t.Errorf("the script ran: %v", err)
	}

	f.write(".env", "SHIP_HOST=h\nSHIP_TOKEN=t\nOTHER=o\n")
	f.mustGraft("deploy", "box")
	if got, _ := os.ReadFile(filepath.Join(state, "out")); string(got) != "host=h port=unset token=t other=unset\n" {
		t.Errorf("script env: %q", got)
	}

	if out := f.mustGraft("deploy", "status", "box", "--", "a b", "c$d"); out != "status a b c$d\n" {
		t.Errorf("status: %q", out)
	}
	if out := f.mustGraft("deploy", "logs", "box", "--lines", "7", "--", "blue"); out != "logs 7 app-blue\n" {
		t.Errorf("logs: %q", out)
	}
	if out, code := f.graft("", "deploy", "logs", "box", "--", "blue", "green"); code != 1 || !strings.Contains(out, "takes exactly one argument, got 2") {
		t.Errorf("two args in a word: exit %d\n%s", code, out)
	}
	if out, code := f.graft("", "deploy", "check-head", "--", "x"); code != 2 {
		t.Errorf("check-head with args: exit %d\n%s", code, out)
	}
}

func TestDeployRunsDepsFirst(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("deploy scripts use sh")
	}
	state := t.TempDir()
	out := filepath.Join(state, "out")
	f := newRepo(t, map[string]string{
		".graft.yaml": `schema: 1
version: {mode: none}
envs:
  local: {}
test_db:
  image: postgres:18-alpine
  container: graft-never-created
  port: 1
  database: app_test
  user: app
  password: app
tasks:
  lint:
    run: [` + helperStep("append", out, "lint") + `]
  dbsmoke:
    deps: [lint]
    test_db: true
    run: [` + helperStep("env", out, "TEST_DB_DSN") + `]
  broken:
    run: [` + helperStep("fail") + `]
  keyed:
    dotenv: [NEED_KEY]
    run: [` + helperStep("append", out, "keyed") + `]
deploy:
  remote: origin
  targets:
    staging:
      env: local
      deps: [lint, dbsmoke]
      run: [sh, deploy.sh]
    production:
      env: local
      deps: [lint, broken]
      run: [sh, deploy.sh]
    keyed:
      env: local
      deps: [lint, keyed]
      run: [sh, deploy.sh]
`,
		"deploy.sh": `echo "deploy $GRAFT_DEPLOY_TARGET" >> ` + out + "\n",
	}, "origin")
	f.mustGraft("commit", "-m", "feat: base")
	// A DSN already in the environment (a CI service container) spares the
	// docker container; the dep must still receive it.
	dsn := "postgres://app:app@127.0.0.1:5432/app_test"
	f.env = append(f.env, "TEST_DB_DSN="+dsn)

	f.mustGraft("deploy", "staging")
	if got := readFile(t, out); got != "lint\nTEST_DB_DSN="+dsn+"\ndeploy staging\n" {
		t.Errorf("staging ran:\n%s", got)
	}

	if err := os.Remove(out); err != nil {
		t.Fatal(err)
	}
	got, code := f.graft("", "deploy", "production")
	if code == 0 || !strings.Contains(got, "task broken failed") {
		t.Errorf("failing dep: exit %d\n%s", code, got)
	}
	if ran := readFile(t, out); ran != "lint\n" {
		t.Errorf("after a failing dep ran:\n%s", ran)
	}

	if err := os.Remove(out); err != nil {
		t.Fatal(err)
	}
	got, code = f.graft("", "deploy", "keyed")
	if code == 0 || !strings.Contains(got, "nothing was done, missing:") || !strings.Contains(got, "NEED_KEY is not set") {
		t.Errorf("missing dep key: exit %d\n%s", code, got)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("something ran before the missing key was reported: %v", err)
	}
}

func TestDeployRequiresBeforeDeps(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("deploy scripts use sh")
	}
	state := t.TempDir()
	out := filepath.Join(state, "out")
	f := newRepo(t, map[string]string{
		".graft.yaml": `schema: 1
version: {mode: file}
envs:
  local: {}
tasks:
  lint:
    run: [` + helperStep("append", out, "lint") + `]
deploy:
  remote: origin
  targets:
    staging:
      env: local
      run: [sh, deploy.sh]
      version: 'cat ` + filepath.Join(state, "staging-version") + `'
    production:
      env: local
      requires: staging
      deps: [lint]
      run: [sh, deploy.sh]
`,
		"VERSION":   "1.0.0\n",
		"deploy.sh": `echo "deploy $GRAFT_DEPLOY_TARGET" >> ` + out + "\n",
	}, "origin")
	f.mustGraft("commit", "-m", "feat: base")
	writeFile(t, filepath.Join(state, "staging-version"), "0.9.0\n")

	got, code := f.graft("", "deploy", "production")
	if code == 0 || !strings.Contains(got, "staging runs version 0.9.0, not 1.0.1") {
		t.Errorf("requires: exit %d\n%s", code, got)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("a dep ran before the requires check failed: %v", err)
	}
}

// A deploy of the commit the target already runs (deployed_sha) is refused
// before deps, sudo and the script, unless --redeploy asks for it.
func TestDeployRefusesSameCommit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("deploy scripts use sh")
	}
	state := t.TempDir()
	out := filepath.Join(state, "out")
	sudoDir := t.TempDir()
	calls := filepath.Join(sudoDir, "calls")
	writeFile(t, filepath.Join(sudoDir, "sudo"), "#!/bin/sh\necho \"$*\" >> "+yamlQuote(calls)+"\n")
	if err := os.Chmod(filepath.Join(sudoDir, "sudo"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := newRepo(t, map[string]string{
		".graft.yaml": `schema: 1
version: {mode: none}
envs:
  local: {}
tasks:
  lint:
    run: [` + helperStep("append", out, "lint") + `]
deploy:
  remote: origin
  targets:
    box:
      env: local
      deps: [lint]
      confirm: sudo
      deployed_sha: {path: ` + yamlQuote(filepath.Join(state, "sha")) + `}
      run: [sh, deploy.sh]
    plain:
      env: local
      run: [sh, deploy.sh]
`,
		"deploy.sh": `echo "deploy $GRAFT_DEPLOY_TARGET" >> ` + out + "\n",
	}, "origin")
	f.mustGraft("commit", "-m", "feat: base")
	f.env = append(f.env, "PATH="+sudoDir+string(os.PathListSeparator)+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	reset := func() {
		t.Helper()
		for _, p := range []string{out, calls} {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
	}

	f.mustGraft("deploy", "box")
	if got := readFile(t, out); got != "lint\ndeploy box\n" {
		t.Errorf("first deploy ran:\n%s", got)
	}

	reset()
	short := f.git("rev-parse", "--short=12", "HEAD")
	got, code := f.graft("", "deploy", "box")
	if code != 1 || !strings.Contains(got, "box already runs "+short+" feat: base; pass --redeploy to deploy it again") {
		t.Errorf("same commit: exit %d\n%s", code, got)
	}
	for _, p := range []string{out, calls} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s exists: deps, sudo or the script ran for the same commit", filepath.Base(p))
		}
	}

	reset()
	f.mustGraft("deploy", "--redeploy", "box")
	if got := readFile(t, out); got != "lint\ndeploy box\n" {
		t.Errorf("--redeploy ran:\n%s", got)
	}

	reset()
	f.write("a.txt", "a\n")
	f.mustGraft("commit", "-m", "fix: next")
	f.mustGraft("deploy", "box")
	if got := readFile(t, out); got != "lint\ndeploy box\n" {
		t.Errorf("a new commit ran:\n%s", got)
	}

	// Without deployed_sha nothing is known about the target: deploys repeat.
	reset()
	f.mustGraft("deploy", "plain")
	f.mustGraft("deploy", "plain")
	if got := readFile(t, out); got != "deploy plain\ndeploy plain\n" {
		t.Errorf("plain ran:\n%s", got)
	}
}
