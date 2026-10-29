package main

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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
	// manageVersions is the token's MANAGE_VERSIONS permission.
	manageVersions bool
	// types maps a key to its issue type, Task by default.
	types map[string]string
	// pages are the Confluence pages by id; page 500 is the parent.
	pages map[string]*fakePage
	// written are the comments people wrote, beside graft's in comments;
	// entered maps a key to when its history shows it come into In
	// Progress.
	written map[string][]fakeComment
	entered map[string]time.Time
}

type fakeComment struct {
	author, text string
	created      time.Time
}

type fakePage struct {
	id, title, parent, body string
	version                 int
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
	if j.versionsAPI(w, r) || j.confluenceAPI(w, r) {
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
		if when, ok := j.entered[key]; ok {
			values = append(values, map[string]any{"created": when.Format(fakeTimeLayout), "items": []any{map[string]any{"fieldId": "status", "fromString": "Testing", "toString": "In Progress"}}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"isLast": true, "values": values})
	case r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "comment":
		j.listComments(w, key)
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
		issueType := "Task"
		if t, ok := j.types[key]; ok {
			issueType = t
		}
		fields := map[string]any{
			"summary":     "Summary of " + key,
			"status":      map[string]any{"name": j.statuses[key], "statusCategory": map[string]string{"key": category}},
			"assignee":    assignee,
			"fixVersions": fixVersions,
			"issuetype":   map[string]any{"name": issueType, "subtask": j.parents[key] != ""},
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
	if id := body.Update["fixVersions"][0]["remove"]["id"]; id != "" {
		j.edits = append(j.edits, key+" remove "+id)
		j.fixVersions[key] = slices.DeleteFunc(j.fixVersions[key], func(v string) bool { return v == id })
		w.WriteHeader(http.StatusNoContent)
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

// versionsAPI serves the project, the permission, version changes and the
// fix version search; it tells whether the request was one of them.
func (j *fakeJira) versionsAPI(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/rest/api/3/project/PROJ":
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "10000", "key": "PROJ"})
	case r.Method == http.MethodGet && r.URL.Path == "/rest/api/3/mypermissions":
		if r.URL.Query().Get("projectKey") != "PROJ" || r.URL.Query().Get("permissions") != "MANAGE_VERSIONS" {
			http.Error(w, "unexpected permission query", http.StatusBadRequest)
			return true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"permissions": map[string]any{"MANAGE_VERSIONS": map[string]bool{"havePermission": j.manageVersions}}})
	case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/version":
		var v map[string]any
		if err := json.NewDecoder(r.Body).Decode(&v); err != nil || v["projectId"] != float64(10000) || v["name"] == "" {
			http.Error(w, "unexpected version", http.StatusBadRequest)
			return true
		}
		delete(v, "projectId")
		v["id"] = strconv.Itoa(30000 + len(j.versions))
		v["archived"] = false
		j.versions = append(j.versions, v)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(v)
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/rest/api/3/version/"):
		id := strings.TrimPrefix(r.URL.Path, "/rest/api/3/version/")
		i := slices.IndexFunc(j.versions, func(v map[string]any) bool { return v["id"] == id })
		var fields map[string]any
		if err := json.NewDecoder(r.Body).Decode(&fields); err != nil || i < 0 {
			http.Error(w, "unexpected version update", http.StatusBadRequest)
			return true
		}
		maps.Copy(j.versions[i], fields)
		_ = json.NewEncoder(w).Encode(j.versions[i])
	case r.Method == http.MethodGet && r.URL.Path == "/rest/api/3/search/jql":
		id, ok := strings.CutPrefix(r.URL.Query().Get("jql"), "fixVersion = ")
		if !ok {
			http.Error(w, "unexpected jql", http.StatusBadRequest)
			return true
		}
		issues := []any{}
		for _, key := range slices.Sorted(maps.Keys(j.fixVersions)) {
			if slices.Contains(j.fixVersions[key], id) {
				issues = append(issues, map[string]string{"key": key})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issues": issues, "isLast": true})
	default:
		return false
	}
	return true
}

// confluenceAPI serves the Confluence pages of one space, S1.
func (j *fakeJira) confluenceAPI(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/wiki/api/v2/pages") {
		return false
	}
	if j.pages == nil {
		j.pages = map[string]*fakePage{"500": {id: "500", title: "Changelog", version: 1}}
	}
	encode := func(p *fakePage) map[string]any {
		return map[string]any{"id": p.id, "title": p.title, "spaceId": "S1", "parentId": p.parent,
			"version": map[string]int{"number": p.version}, "body": map[string]any{"storage": map[string]string{"value": p.body}},
			"_links": map[string]string{"webui": "/spaces/S/pages/" + p.id}}
	}
	type pageBody struct {
		SpaceID, ParentID, Title, Status string
		Body                             struct{ Representation, Value string }
		Version                          struct{ Number int }
	}
	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/wiki/api/v2/pages"), "/")
	switch {
	case r.Method == http.MethodGet && id != "":
		if p, ok := j.pages[id]; ok {
			_ = json.NewEncoder(w).Encode(encode(p))
			return true
		}
		http.Error(w, "no page", http.StatusNotFound)
	case r.Method == http.MethodGet:
		q := r.URL.Query()
		results := []any{}
		for _, p := range j.pages {
			if q.Get("space-id") == "S1" && q.Get("body-format") == "storage" && p.title == q.Get("title") {
				results = append(results, encode(p))
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	case r.Method == http.MethodPost:
		var b pageBody
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.SpaceID != "S1" || j.pages[b.ParentID] == nil || b.Body.Representation != "storage" || b.Status != "current" {
			http.Error(w, "unexpected page", http.StatusBadRequest)
			return true
		}
		for _, p := range j.pages {
			if p.title == b.Title {
				http.Error(w, "a page with this title already exists", http.StatusBadRequest)
				return true
			}
		}
		p := &fakePage{id: strconv.Itoa(600 + len(j.pages)), title: b.Title, parent: b.ParentID, body: b.Body.Value, version: 1}
		j.pages[p.id] = p
		_ = json.NewEncoder(w).Encode(encode(p))
	case r.Method == http.MethodPut:
		var b pageBody
		p := j.pages[id]
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil || p == nil || b.Version.Number != p.version+1 || b.Title != p.title {
			http.Error(w, "unexpected page update", http.StatusConflict)
			return true
		}
		p.body, p.version = b.Body.Value, b.Version.Number
		_ = json.NewEncoder(w).Encode(encode(p))
	default:
		return false
	}
	return true
}

// page returns the body of the page with the title, "" for none.
func (j *fakeJira) page(title string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, p := range j.pages {
		if p.title == title {
			return p.body
		}
	}
	return ""
}

// version returns a copy of the version with the id.
func (j *fakeJira) version(id string) map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, v := range j.versions {
		if v["id"] == id {
			return maps.Clone(v)
		}
	}
	return nil
}

// versionNamed returns a copy of the version with the name.
func (j *fakeJira) versionNamed(name string) map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, v := range j.versions {
		if v["name"] == name {
			return maps.Clone(v)
		}
	}
	return nil
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
	state   string
	jira    *fakeJira
	jiraURL func() string
}

func deployRepo(t *testing.T) *deployFixture {
	return newDeployFixture(t, deployConfig)
}

func (d *deployFixture) mustFixtureCommit(msg string) {
	d.t.Helper()
	d.mustVersionedCommit(d.work, msg, false)
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
{ echo "target=$GRAFT_DEPLOY_TARGET env=$GRAFT_DEPLOY_ENV version=$GRAFT_DEPLOY_VERSION"; echo "sha=$GRAFT_DEPLOY_SHA"; echo "prev=$GRAFT_DEPLOY_PREVIOUS_SHA"; echo "args=$*"; echo "priority=$GRAFT_LOAD_PRIORITY"; echo "budget=$(cat "$GRAFT_LOAD_BUDGET")"; echo "budget file=$GRAFT_LOAD_BUDGET"; } > "$STATE/$GRAFT_DEPLOY_TARGET-run"
graft deploy check-head
if [ -n "$MOVE_HEAD" ]; then git -c core.hooksPath=/dev/null commit -q --allow-empty -m moved; fi
echo "$GRAFT_DEPLOY_VERSION" > "$STATE/$GRAFT_DEPLOY_TARGET-version"
exit "${DEPLOY_EXIT:-0}"
`,
	}, "origin")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir)
	// The fixture needs the version bump and published base for deploy tests.
	f.write("VERSION", "1.0.1\n")
	f.git("add", "-A")
	f.run(f.work, "git", "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "feat: base")
	f.git("push", "-q", "-u", "origin", "HEAD")
	return &deployFixture{fixture: f, state: state, jira: jira, jiraURL: func() string { return srv.URL }}
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
      release_notes: {transitions: [{from: [In Progress], to: Testing, assign: qa, keep_assignee_labels: [internal]}, {from: [Ready for release], to: Done}], subtasks: true}
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
    skip_keys: [PROJ-40]
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
	for _, want := range []string{"target=test env=local version=1.0.1", "sha=" + head, "prev=\n", "args=--skip-tests two words", "priority=graft deploy test",
		"budget=600000000000\n"} {
		if !strings.Contains(run, want) {
			t.Errorf("script did not get %q:\n%s", want, run)
		}
	}
	// The budget for waiting goes with the deploy.
	if _, file, ok := strings.Cut(run, "budget file="); !ok {
		t.Errorf("no budget file:\n%s", run)
	} else if _, err := os.Stat(strings.TrimSpace(file)); !os.IsNotExist(err) {
		t.Errorf("the budget file outlived the deploy: %v", err)
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
	d.mustFixtureCommit("feat: PROJ-1 first thing, hashes with SHA-256")
	d.write("b.txt", "b\n")
	d.mustFixtureCommit("fix: second\n\nRefs PROJ-2 and OTHER-5, see also PROJ-3, PROJ-4, PROJ-5, PROJ-6 and PROJ-7")
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
	d.mustFixtureCommit("feat: PROJ-1 x")
	d.jira.setFailing()
	out, code := d.graft("", "deploy", "test")
	if code != 0 || !strings.Contains(out, "release notes skipped") || !strings.Contains(out, "http 500") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

// TestDeployReleaseNotesSkipReverts: a revert takes its item's work out, so
// neither it nor a commit it reverts in the same deploy posts a release note;
// an item another commit names still gets one.
func TestDeployReleaseNotesSkipReverts(t *testing.T) {
	d := deployRepo(t)
	d.mustGraft("deploy", "test")
	d.write("a.txt", "a\n")
	d.mustFixtureCommit("feat: PROJ-1 first thing")
	d.mustGraft("deploy", "test")
	revert := func(subject string) {
		t.Helper()
		sha := d.git("rev-parse", "HEAD")
		d.git("revert", "--no-commit", "HEAD")
		d.git("revert", "--quit")
		d.mustFixtureCommit("Revert \"" + subject + "\"\n\nThis reverts commit " + sha + ".")
	}
	revert("feat: PROJ-1 first thing")
	d.write("b.txt", "b\n")
	d.mustFixtureCommit("feat: PROJ-5 then taken out")
	revert("feat: PROJ-5 then taken out")
	d.write("c.txt", "c\n")
	d.mustFixtureCommit("feat: PROJ-6 stays")
	out := d.mustGraft("deploy", "test")
	comments, _ := d.jira.snapshot()
	if len(comments["PROJ-1"]) != 1 || len(comments["PROJ-5"]) != 0 || len(comments["PROJ-6"]) != 1 {
		t.Errorf("comments = %v\n%s", comments, out)
	}
	for _, key := range []string{"PROJ-1", "PROJ-5"} {
		if want := "release note for " + key + " not posted: this deploy only takes its work out"; !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}
}

// TestDeployReleaseNotesSkipQuietCommits: an item whose commits in the deploy
// are docs, test, style or ci only changes nothing on the target, so it gets
// no release note and stays where it is; one with any other commit gets both.
func TestDeployReleaseNotesSkipQuietCommits(t *testing.T) {
	d := deployRepo(t)
	d.mustGraft("deploy", "test")
	d.write("a.txt", "a\n")
	d.mustFixtureCommit("docs(api): PROJ-1 comment reworded")
	d.write("b.txt", "b\n")
	d.mustFixtureCommit("test: PROJ-1 one more case")
	d.write("c.txt", "c\n")
	d.mustFixtureCommit("docs: PROJ-6 comment reworded")
	d.write("d.txt", "d\n")
	d.mustFixtureCommit("fix(api)!: PROJ-6 real fix")
	// A merge brings what its branch's commits do, whatever its subject says.
	trunk := d.git("rev-parse", "--abbrev-ref", "HEAD")
	d.git("switch", "-q", "-c", "feature/PROJ-5-docs")
	d.write("e.txt", "e\n")
	d.mustFixtureCommit("docs: PROJ-5 comment reworded")
	d.git("switch", "-q", trunk)
	d.git("-c", "core.hooksPath=/dev/null", "merge", "-q", "--no-ff", "-m", "Merge branch 'feature/PROJ-5-docs'", "feature/PROJ-5-docs")
	// A branch whose name alone carries the key, its commits keyless.
	d.jira.statuses["PROJ-9"] = "In Progress"
	d.git("switch", "-q", "-c", "feature/PROJ-9-docs")
	d.write("f.txt", "f\n")
	d.git("add", "-A")
	d.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "docs: comment reworded")
	d.git("switch", "-q", trunk)
	d.git("-c", "core.hooksPath=/dev/null", "merge", "-q", "--no-ff", "-m", "Merge branch 'feature/PROJ-9-docs'", "feature/PROJ-9-docs")
	d.jira.statuses["PROJ-10"] = "Testing"
	d.git("switch", "-q", "-c", "feature/PROJ-10-fix")
	d.write("g.txt", "g\n")
	d.git("add", "-A")
	d.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "fix: real fix")
	d.git("switch", "-q", trunk)
	d.git("-c", "core.hooksPath=/dev/null", "merge", "-q", "--no-ff", "-m", "Merge branch 'feature/PROJ-10-fix'", "feature/PROJ-10-fix")
	d.git("push", "-q", "origin", trunk)
	out := d.mustGraft("deploy", "test")
	comments, moved := d.jira.snapshot()
	if len(comments["PROJ-1"]) != 0 || len(comments["PROJ-5"]) != 0 || len(comments["PROJ-9"]) != 0 || len(comments["PROJ-6"]) != 1 || len(comments["PROJ-10"]) != 1 || len(moved) != 0 {
		t.Errorf("comments = %v, moved = %v\n%s", comments, moved, out)
	}
	for _, key := range []string{"PROJ-1", "PROJ-5", "PROJ-9"} {
		if want := "release note for " + key + " not posted, " + key + " left alone: its commits in this deploy are docs, test, style or ci only"; !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
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
	d.mustFixtureCommit("fix: second\n\nRefs PROJ-1, PROJ-2, PROJ-3, PROJ-4, PROJ-5, PROJ-6 and PROJ-7")
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
	d.mustFixtureCommit("fix: PROJ-1 third")
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
	d.mustFixtureCommit("fix: PROJ-1 third")
	out, code := d.graft("", "deploy", "uat")
	if code == 0 || !strings.Contains(out, `every unreleased version of PROJ starting with "Product v" is being approved`) {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

// TestDeployApprovedReleaseSubtaskByParentVersion: a subtask with no version
// of its own goes out when its parent is in an approved version, and not when
// the parent's version still waits for an approval.
func TestDeployApprovedReleaseSubtaskByParentVersion(t *testing.T) {
	d := deployRepo(t)
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-30": {"10030"}})
	d.jira.setApprovers(map[string][]string{"10030": {"APPROVED", "PENDING"}})
	d.jira.mu.Lock()
	d.jira.parents = map[string]string{"PROJ-31": "PROJ-30"}
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "--unapproved", "live")
	d.write("a.txt", "a\n")
	d.mustFixtureCommit("feat: PROJ-31 part of the whole")
	if out, code := d.graft("", "deploy", "live"); code == 0 || !strings.Contains(out, `PROJ-31 not in an approved "Product v" version, nor is its parent PROJ-30`) {
		t.Fatalf("the parent's version is not approved yet: exit %d\n%s", code, out)
	}
	d.jira.setApprovers(map[string][]string{"10030": {"APPROVED", "APPROVED"}})
	if out := d.mustGraft("deploy", "live"); !strings.Contains(out, "approved release: 1 commits since") {
		t.Errorf("the subtask goes out by its parent's version:\n%s", out)
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
	d.mustFixtureCommit("feat: PROJ-1 approved thing")
	d.write("b.txt", "b\n")
	d.mustFixtureCommit("fix: PROJ-5 half approved")
	d.write("c.txt", "c\n")
	d.mustFixtureCommit("fix: PROJ-6 in another product")
	d.write("d.txt", "d\n")
	d.mustFixtureCommit("chore: tidy up")
	d.write("e.txt", "e\n")
	d.mustFixtureCommit("revert: PROJ-5 half approved")
	d.write("docs/guide with space.md", "g\n")
	d.write("NOTES.md", "n\n")
	d.mustFixtureCommit("docs: the guide")
	d.write("docs/more.md", "m\n")
	d.write("code.txt", "c\n")
	d.mustFixtureCommit("docs: PROJ-5 the guide and some code")
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
	d.mustFixtureCommit("chore: PROJ-1 tidy up again")
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
		d.mustFixtureCommit(msg)
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
	d.mustFixtureCommit("feat: PROJ-1 approved thing")
	out := d.mustGraft("deploy", "live")
	if !strings.Contains(out, "approved release: 1 commits since "+prev+", all in Product v1.2.10") {
		t.Errorf("no summary:\n%s", out)
	}
	if got := strings.TrimSpace(d.stateFile("live-sha")); got != d.git("rev-parse", "HEAD") {
		t.Errorf("deployed sha = %q", got)
	}
}

// TestDeployApprovedReleaseAttribute: a commit whose message names no item
// goes out as a commit of the item approved_release.attribute ties it to:
// checked by status, a subtask by its parent, by an approved version, and
// noted on the item. An attribution the repository contradicts stops graft
// check and the deploy.
func TestDeployApprovedReleaseAttribute(t *testing.T) {
	d := deployRepo(t)
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-8"] = "Ready for release"
	d.jira.statuses["PROJ-9"] = "In Progress"
	d.jira.mu.Unlock()
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-1": {"10010"}, "PROJ-2": {"10010"}, "PROJ-6": {"10030"}})
	d.jira.setApprovers(map[string][]string{"10010": {"APPROVED", "APPROVED"}, "10030": {"APPROVED", "PENDING"}})
	d.mustGraft("deploy", "--unapproved", "gate")
	d.mustGraft("deploy", "--unapproved", "live")
	d.write("a.txt", "a\n")
	d.mustFixtureCommit("feat: forgot the key")
	keyless := d.git("rev-parse", "HEAD")
	if out, code := d.graft("", "deploy", "gate"); code == 0 || !strings.Contains(out, "feat: forgot the key: names no item") {
		t.Fatalf("the keyless commit: exit %d\n%s", code, out)
	}

	attribute := func(gateItem, liveItem string) {
		cfg := strings.Replace(d.read(".graft.yaml"), "approved_release: {statuses: [Ready for release, Done]}",
			"approved_release: {statuses: [Ready for release, Done], attribute: [{commit: "+keyless+", item: "+gateItem+", reason: key left out}]}\n      release_notes: {}", 1)
		cfg = strings.Replace(cfg, `ignore_paths: ["docs/**", "*.md"]}`,
			`ignore_paths: ["docs/**", "*.md"], attribute: [{commit: `+keyless+`, item: `+liveItem+`, reason: "key left out, see PROJ-1"}]}`, 1)
		d.write(".graft.yaml", cfg)
	}
	attribute("PROJ-9", "PROJ-6")
	d.write("docs/attribute.md", "x\n")
	d.mustFixtureCommit("docs: PROJ-2 attribute the keyless commit")
	out, code := d.graft("", "deploy", "gate")
	if code == 0 || !strings.Contains(out, "feat: forgot the key: attributed to PROJ-9: key left out") ||
		!strings.Contains(out, "feat: forgot the key: PROJ-9 is In Progress, not Ready for release or Done") {
		t.Fatalf("attributed to an unchecked item: exit %d\n%s", code, out)
	}
	if out, code := d.graft("", "deploy", "live"); code == 0 || !strings.Contains(out, `feat: forgot the key: PROJ-6 not in an approved "Product v" version`) {
		t.Fatalf("attributed to an unapproved version: exit %d\n%s", code, out)
	}

	// A subtask goes out by its parent, a version item by its approvals; the
	// release note goes to the item.
	d.jira.mu.Lock()
	d.jira.parents = map[string]string{"PROJ-9": "PROJ-8"}
	d.jira.mu.Unlock()
	d.jira.setApprovers(map[string][]string{"10010": {"APPROVED", "APPROVED"}, "10030": {"APPROVED", "APPROVED"}})
	out = d.mustGraft("deploy", "gate")
	if !strings.Contains(out, "approved release: 2 commits since") {
		t.Errorf("the attributed commit goes out:\n%s", out)
	}
	out = d.mustGraft("deploy", "live")
	if !strings.Contains(out, "all in Product v1.2.10, Product v1.3.0") {
		t.Errorf("the attributed commit goes out by its version:\n%s", out)
	}
	comments, _ := d.jira.snapshot()
	if !slices.ContainsFunc(comments["PROJ-9"], func(c string) bool { return strings.Contains(c, "Deployed to gate") }) {
		t.Errorf("no notes on the attributed items: %v", comments)
	}

	// The repository contradicts an attribution: the commit is unknown, or
	// it names the item already.
	d.write("b.txt", "b\n")
	d.mustFixtureCommit("feat: PROJ-8 named")
	named := d.git("rev-parse", "HEAD")
	unknown := strings.Repeat("ab", 20)
	attributed := d.read(".graft.yaml")
	cfg := strings.Replace(attributed, keyless+", item: PROJ-9", named+", item: PROJ-8", 1)
	d.write(".graft.yaml", strings.Replace(cfg, keyless+", item: PROJ-6", unknown+", item: PROJ-6", 1))
	for _, args := range [][]string{{"check"}, {"deploy", "gate"}} {
		out, code := d.graft("", args...)
		if code == 0 ||
			!strings.Contains(out, "deploy.targets.gate.approved_release.attribute[0]: commit "+named[:12]+` "feat: PROJ-8 named" names PROJ-8 already: the attribution changes nothing`) ||
			!strings.Contains(out, "deploy.targets.live.approved_release.attribute[0]: commit "+unknown+" is not in this repository (git fetch?)") {
			t.Errorf("%v: exit %d\n%s", args, code, out)
		}
	}

	// A commit naming the wrong item goes out as the item it is attributed to.
	d.write(".graft.yaml", strings.Replace(attributed, keyless+", item: PROJ-9", named+", item: PROJ-9", 1))
	d.mustFixtureCommit("docs: PROJ-2 attribute the named commit")
	before, _ := d.jira.snapshot()
	out = d.mustGraft("deploy", "gate")
	if !strings.Contains(out, `feat: PROJ-8 named: attributed to PROJ-9 instead of PROJ-8: key left out`) {
		t.Errorf("the named commit goes out as PROJ-9:\n%s", out)
	}
	after, _ := d.jira.snapshot()
	if len(after["PROJ-9"]) != len(before["PROJ-9"])+1 || len(after["PROJ-8"]) != len(before["PROJ-8"]) {
		t.Errorf("the note goes to PROJ-9 alone: before %v, after %v\n%s", before, after, out)
	}

	// Its revert, quoting the message, takes out the work of PROJ-9 too.
	d.git("rm", "-q", "b.txt")
	d.mustFixtureCommit("Revert \"feat: PROJ-8 named\"\n\nThis reverts commit " + named + ".")
	out = d.mustGraft("deploy", "gate")
	if !strings.Contains(out, "release note for PROJ-9 not posted: this deploy only takes its work out") || strings.Contains(out, "PROJ-8 not posted") {
		t.Errorf("the revert takes out PROJ-9's work:\n%s", out)
	}
}

// TestDeploySubtasksFollowParents: with subtasks, a parent that reaches a
// transition's status takes along its subtasks in that transition's from,
// the reviewer included; a subtask further on, or elsewhere, stays, and an
// item of skip_keys is left alone with its subtasks.
func TestDeploySubtasksFollowParents(t *testing.T) {
	d := deployRepo(t)
	d.jira.mu.Lock()
	maps.Copy(d.jira.statuses, map[string]string{"PROJ-20": "In Progress", "PROJ-21": "In Progress", "PROJ-22": "Done", "PROJ-23": "In Progress", "PROJ-24": "In Progress",
		"PROJ-25": "Ready for release", "PROJ-30": "Ready for release", "PROJ-31": "Paused", "PROJ-32": "Ready for release", "PROJ-33": "Testing",
		"PROJ-40": "In Progress", "PROJ-41": "In Progress"})
	d.jira.assignees["PROJ-23"] = "colleague"
	d.jira.assignees["PROJ-24"] = "qa"
	d.jira.handedOver = append(d.jira.handedOver, "PROJ-24")
	d.jira.labels = map[string][]string{"PROJ-21": {"backend", "internal"}}
	d.jira.parents = map[string]string{"PROJ-21": "PROJ-20", "PROJ-22": "PROJ-20", "PROJ-23": "PROJ-20", "PROJ-24": "PROJ-20", "PROJ-25": "PROJ-20",
		"PROJ-31": "PROJ-30", "PROJ-32": "PROJ-30", "PROJ-33": "PROJ-30", "PROJ-41": "PROJ-40"}
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "sub")
	d.write("a.txt", "a\n")
	d.mustFixtureCommit("feat: PROJ-20 parent work\n\nAlso PROJ-30, PROJ-31 and PROJ-40")
	out := d.mustGraft("deploy", "sub")
	_, moved := d.jira.snapshot()
	slices.Sort(moved)
	if !slices.Equal(moved, []string{"PROJ-20", "PROJ-21", "PROJ-24", "PROJ-30", "PROJ-32"}) {
		t.Errorf("moved = %v\n%s", moved, out)
	}
	d.jira.mu.Lock()
	assigned := slices.Sorted(slices.Values(d.jira.assigned))
	statuses := map[string]string{}
	for _, k := range []string{"PROJ-20", "PROJ-21", "PROJ-24", "PROJ-25", "PROJ-30", "PROJ-31", "PROJ-32", "PROJ-33", "PROJ-40", "PROJ-41"} {
		statuses[k] = d.jira.statuses[k]
	}
	comments := d.jira.comments["PROJ-40"]
	d.jira.mu.Unlock()
	if !slices.Equal(assigned, []string{"PROJ-20 to qa"}) {
		t.Errorf("assigned = %v (PROJ-21 is internal, PROJ-24 is on qa already)\n%s", assigned, out)
	}
	if want := map[string]string{"PROJ-20": "Testing", "PROJ-21": "Testing", "PROJ-24": "Testing", "PROJ-25": "Ready for release",
		"PROJ-30": "Done", "PROJ-31": "Paused", "PROJ-32": "Done", "PROJ-33": "Testing", "PROJ-40": "In Progress", "PROJ-41": "In Progress"}; !maps.Equal(statuses, want) {
		t.Errorf("statuses = %v, want %v", statuses, want)
	}
	if len(comments) > 0 {
		t.Errorf("a note on an item of skip_keys: %v", comments)
	}
	for _, want := range []string{
		"PROJ-20 moved to Testing\n",
		"PROJ-20 assigned to qa",
		"PROJ-21 moved to Testing after its parent PROJ-20\n",
		"PROJ-21 stays with its assignee: label internal (keep_assignee_labels)",
		"PROJ-24 moved to Testing after its parent PROJ-20\n",
		"PROJ-25 stays Ready for release after its parent PROJ-20: the transition to Testing moves only from In Progress",
		"PROJ-32 moved to Done after its parent PROJ-30",
		"PROJ-33 stays Testing after its parent PROJ-30: the transition to Done moves only from Ready for release",
		"PROJ-31 stays Paused",
		"PROJ-23 left alone: assigned to Colleague colleague",
		"release note for PROJ-40 not posted, PROJ-40 left alone: release_notes.jira.skip_keys",
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
	d.mustFixtureCommit("feat: PROJ-1 work")
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

func TestDeployConfirmWithoutTerminalAsksAgainForRefusalOnStderr(t *testing.T) {
	d := deployRepo(t)
	d.mustGraft("deploy", "test")
	calls := d.fakeSudoScript(t, `if [ "$n" -lt 3 ]; then echo "Failed to match fingerprint" >&2; exit 1; fi`)
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
	f.mustPlainCommit("feat: base")
	f.git("push", "-u", "origin", "HEAD")

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
	f.mustPlainCommit("feat: base")
	f.git("push", "-u", "origin", "HEAD")
	// A DSN already in the environment (a CI service container) spares the
	// docker container; the dep must still receive it.
	dsn := "postgres://app:app@127.0.0.1:5432/app_test"
	f.env = append(f.env, "TEST_DB_DSN="+dsn)

	cache := t.TempDir()
	f.env = append(f.env, "XDG_CACHE_HOME="+cache)
	f.mustGraft("deploy", "staging")
	if got := readFile(t, out); got != "lint\nTEST_DB_DSN="+dsn+"\ndeploy staging\n" {
		t.Errorf("staging ran:\n%s", got)
	}
	// The deploy's line holds its deps, whose lines name it; its breakdown
	// shows them beside the script.
	in := map[string]string{}
	for _, l := range readStats(t, filepath.Join(cache, "graft", "stats.jsonl")) {
		if l.Kind == "task" {
			in[l.Task] = l.In
		}
	}
	if in["lint"] != "deploy staging" || in["dbsmoke"] != "deploy staging" || in["deploy staging"] != "" {
		t.Errorf("the tasks ran in: %v", in)
	}
	rows := strings.Join(strings.Fields(f.mustGraft("stats", "--task", "deploy staging")), " ")
	for _, want := range []string{"(task) 1 ", "sh deploy.sh 1 ", "task lint 1 ", "task dbsmoke 1 "} {
		if !strings.Contains(rows, want) {
			t.Errorf("the breakdown lacks %q:\n%s", want, rows)
		}
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
	f.write("VERSION", "1.0.1\n")
	f.mustPlainCommit("feat: base")
	f.git("push", "-u", "origin", "HEAD")
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
	f.mustPlainCommit("feat: base")
	f.git("push", "-u", "origin", "HEAD")
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
	f.mustPlainCommit("fix: next")
	f.git("push", "-q", "origin", "HEAD")
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

// TestDeployCheck: graft deploy check lists every refusal of graft deploy,
// the items of the commits outside the approved release among them, and
// deploys and records nothing; with nothing to refuse it passes.
func TestDeployCheck(t *testing.T) {
	d := deployRepo(t)
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-1": {"10010"}, "PROJ-5": {"10030"}})
	d.jira.setApprovers(map[string][]string{"10010": {"APPROVED", "APPROVED"}, "10030": {"APPROVED", "PENDING"}})
	d.mustGraft("deploy", "--unapproved", "live")
	deployed := d.git("rev-parse", "HEAD")
	d.write("a.txt", "a\n")
	d.mustFixtureCommit("feat: PROJ-1 approved thing")
	if out := d.mustGraft("deploy", "check", "live"); !strings.Contains(out, "approved release: 1 commits since") ||
		!strings.Contains(out, "graft deploy live passes its checks now") {
		t.Errorf("an approved release passes:\n%s", out)
	}
	d.write("b.txt", "b\n")
	d.mustFixtureCommit("fix: PROJ-5 half approved")
	d.write("c.txt", "c\n")
	d.mustFixtureCommit("chore: tidy up")
	d.write("dirty.txt", "uncommitted\n")
	before := readFile(t, filepath.Join(d.state, "live-run"))

	out, code := d.graft("", "deploy", "check", "live")
	if code == 0 {
		t.Fatalf("exit 0:\n%s", out)
	}
	for _, want := range []string{
		"checking graft deploy live for ",
		"graft deploy live would refuse:",
		"- the work tree is not clean",
		`fix: PROJ-5 half approved: PROJ-5 not in an approved "Product v" version`,
		"chore: tidy up: names no item",
		"Their items:",
		"PROJ-5 Summary of PROJ-5: Testing; fix versions Product v1.3.0 (not approved by every approver)",
		"commits naming no item: tie each to its item with approved_release.attribute, or revert it",
		"would refuse, see above",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}
	if got := strings.TrimSpace(d.stateFile("live-sha")); got != deployed {
		t.Errorf("deployed sha = %q, want the earlier %q", got, deployed)
	}
	if after := readFile(t, filepath.Join(d.state, "live-run")); after != before {
		t.Errorf("the deploy script ran:\n%s", after)
	}
}

func TestDeployVersions(t *testing.T) {
	d := deployRepo(t)
	d.mustGraft("deploy", "test")
	subject := d.git("log", "-1", "--format=%h %s (%cs)")
	out := d.mustGraft("deploy", "versions")
	for _, want := range []string{
		"test (local): version 1.0.1; commit " + subject + ", at origin/" + d.git("rev-parse", "--abbrev-ref", "HEAD") + "\n",
		"live (local): version no version command; commit none recorded\n",
		"stage (local): version no version command; commit no deployed_sha\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "stage (") < strings.Index(out, "test (") {
		t.Errorf("stage requires test and comes after it:\n%s", out)
	}
	writeFile(t, filepath.Join(d.state, "test-version"), "")
	if out, code := d.graft("", "deploy", "versions"); code == 0 || !strings.Contains(out, "test (local): version unknown: the version command on local printed nothing") ||
		!strings.Contains(out, "could not read test") {
		t.Errorf("an unreadable version: exit %d\n%s", code, out)
	}
}

// TestDeployShowsWhatTheTargetRuns: before its checks, graft deploy and
// graft deploy check print the version, the commit and the branch the
// target runs, then the commits the deploy brings and those it takes back.
func TestDeployShowsWhatTheTargetRuns(t *testing.T) {
	d := deployRepo(t)
	trunk := d.git("rev-parse", "--abbrev-ref", "HEAD")

	out := d.mustGraft("deploy", "--unapproved", "live")
	if !strings.Contains(out, "live runs version no version command; commit none recorded\n") {
		t.Errorf("a first deploy:\n%s", out)
	}
	deployed := d.git("log", "-1", "--format=%h %s (%cs)")
	deployedVersion := strings.TrimSpace(d.git("show", "HEAD:VERSION"))
	d.write("a.txt", "a\n")
	d.mustFixtureCommit("feat: next")
	d.write("b.txt", "b\n")
	d.mustFixtureCommit("feat: after next")
	// The commits name no item: the check refuses after the lines.
	out, _ = d.graft("", "deploy", "check", "live")
	for _, want := range []string{
		"live runs version " + deployedVersion + " by its commit; commit " + deployed + ", in origin/" + trunk + "\n",
		"deploying " + d.git("rev-parse", "--short=12", "HEAD") + " over it brings 2 commits, takes none back\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}

	// A target with a version command shows its answer, or why there is none.
	out = d.mustGraft("deploy", "test")
	if !strings.Contains(out, "test runs version unknown: reading the deployed version on local: ") {
		t.Errorf("an unreadable version:\n%s", out)
	}
	runs := d.git("log", "-1", "--format=%h %s (%cs)")
	d.git("switch", "-q", "-c", "side", "HEAD~1")
	d.write("c.txt", "c\n")
	d.mustFixtureCommit("feat: side work")
	out = d.mustGraft("deploy", "check", "test")
	for _, want := range []string{
		"test runs version " + strings.TrimSpace(d.stateFile("test-version")) + "; commit " + runs + ", at origin/" + trunk + "\n",
		"over it brings 1 commit and takes back 1 it runs: git log ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}
}

// sweepConfig adds the target swept, whose release notes sweep.
func sweepConfig(state string, q func(string) string) string {
	return strings.Replace(deployConfig(state, q), "    gate:\n", `    swept:
      env: local
      run: [sh, deploy.sh]
      deployed_sha: {path: `+q("swept-sha")+`}
      release_notes: {transitions: [{from: [Ready for release], to: Done}], sweep: {}}
    gate:
`, 1)
}

// TestDeploySweep: the sweep moves to Done an item in Ready for release whose
// commits the target runs, and a parent whose subtasks are done and on it;
// it lists the items git contradicts, and leaves alone an item a commit only
// refers to.
func TestDeploySweep(t *testing.T) {
	d := newDeployFixture(t, sweepConfig)
	cache := t.TempDir()
	d.env = append(d.env, "GRAFT_NOW="+time.Now().Add(5*24*time.Hour+time.Hour).UTC().Format(time.RFC3339), "XDG_CACHE_HOME="+cache)
	trunk := d.git("branch", "--show-current")
	d.git("remote", "set-head", "origin", trunk)
	d.jira.mu.Lock()
	maps.Copy(d.jira.statuses, map[string]string{"PROJ-10": "Ready for release", "PROJ-11": "Ready for release", "PROJ-12": "Done", "PROJ-13": "To Do",
		"PROJ-14": "In Progress", "PROJ-15": "Ready for release", "PROJ-16": "Done", "PROJ-20": "In Progress", "PROJ-21": "Done", "PROJ-22": "Done",
		"PROJ-60": "Paused", "PROJ-61": "Done"})
	d.jira.parents = map[string]string{"PROJ-21": "PROJ-20", "PROJ-22": "PROJ-20", "PROJ-61": "PROJ-60"}
	d.jira.mu.Unlock()
	for i, msg := range []string{"feat: PROJ-10 a", "feat: PROJ-11 b", "fix: PROJ-14 c\n\nafter PROJ-15", "feat: PROJ-13 d", "feat: PROJ-6 e",
		"feat: PROJ-21 f", "chore: g\n\nPROJ-22", "feat: PROJ-61 k"} {
		d.write(fmt.Sprintf("f%d.txt", i), "x\n")
		d.mustPlainCommit(msg)
	}
	d.git("push", "-q", "origin", trunk)
	out := d.mustGraft("deploy", "swept")
	for _, want := range []string{
		"release notes skipped: no previously deployed commit recorded",
		"sweep swept: PROJ-10 would move to Done: its work of the last 30 days is on swept (commits: 1)",
		"sweep swept: PROJ-11 would move to Done",
		"sweep swept: PROJ-20 would move to Done: its subtasks are all done (2) and their work is on swept",
		"PROJ-13 is To Do, but its commits are in origin/" + trunk,
		// Paused is not in skip_statuses: in work, as far as graft knows.
		"PROJ-6 is Paused, but all its work is on swept, the newest commit 5 days old",
		"PROJ-14 is In Progress, but all its work is on swept, the newest commit 5 days old",
		"PROJ-60 is Paused, but its subtasks are all done (1) and their work is on swept",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "PROJ-15") {
		t.Errorf("an item a commit only refers to:\n%s", out)
	}
	if _, moved := d.jira.snapshot(); len(moved) > 0 {
		t.Errorf("moved without apply: %v", moved)
	}

	// PROJ-11 gets a commit the target does not run.
	d.git("switch", "-q", "-c", "feature/x")
	d.write("g1.txt", "x\n")
	d.mustPlainCommit("feat: PROJ-11 more")
	d.write("g2.txt", "x\n")
	d.mustPlainCommit("fix: PROJ-12 x")
	d.git("push", "-q", "-u", "origin", "HEAD")
	branchSHA := d.git("rev-parse", "HEAD")
	d.git("switch", "-q", trunk)
	d.write("h.txt", "x\n")
	d.mustPlainCommit("feat: PROJ-16 later")
	d.git("push", "-q", "origin", trunk)
	later := d.git("rev-parse", "HEAD")
	out = d.mustGraft("deploy", "sweep", "swept", "--apply")
	for _, want := range []string{
		"sweep swept: PROJ-10 moved to Done",
		"sweep swept: PROJ-20 moved to Done",
		"PROJ-12 is Done, but " + branchSHA[:12] + " is not in origin/" + trunk,
		"PROJ-16 is Done, but " + later[:12] + " is not on swept yet",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}
	if _, moved := d.jira.snapshot(); !slices.Equal(slices.Sorted(slices.Values(moved)), []string{"PROJ-10", "PROJ-20"}) {
		t.Errorf("moved = %v\n%s", moved, out)
	}

	// A commit sweeps by the commit the last deploy recorded, once a day.
	d.write("i.txt", "x\n")
	if out := d.mustGraft("commit", "-m", "feat: PROJ-16 still later"); strings.Contains(out, "sweep swept") {
		t.Errorf("swept again within a day of the deploy:\n%s", out)
	}
	stamps, err := filepath.Glob(filepath.Join(cache, "graft", "sweep", "*"))
	if err != nil || len(stamps) != 1 {
		t.Fatalf("stamps = %v, %v", stamps, err)
	}
	day := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(stamps[0], day, day); err != nil {
		t.Fatal(err)
	}
	d.write("j.txt", "x\n")
	if out := d.mustGraft("commit", "-m", "feat: PROJ-16 a day on"); !strings.Contains(out, "PROJ-16 is Done, but "+d.git("rev-parse", "--short=12", "HEAD")+" is not on swept yet") {
		t.Errorf("no daily sweep:\n%s", out)
	}
}

// TestDeploySweepApprovedRelease: with approved_release.prefix, the sweep
// moves an item whose work the target runs only once its version is
// approved: code deployed past the check is not a release.
func TestDeploySweepApprovedRelease(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return strings.Replace(deployConfig(state, q), `approved_release: {prefix: "Product v", ignore_paths: ["docs/**", "*.md"]}`,
			`approved_release: {prefix: "Product v", ignore_paths: ["docs/**", "*.md"]}
      release_notes: {transitions: [{from: [Ready for release], to: Done}], sweep: {}}`, 1)
	})
	d.env = append(d.env, "XDG_CACHE_HOME="+t.TempDir())
	d.git("remote", "set-head", "origin", d.git("branch", "--show-current"))
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-50": {"10030"}})
	d.jira.setApprovers(map[string][]string{"10030": {"APPROVED", "PENDING"}})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-50"] = "Ready for release"
	d.jira.mu.Unlock()
	d.write("a.txt", "a\n")
	d.mustFixtureCommit("feat: PROJ-50 thing")
	out := d.mustGraft("deploy", "--unapproved", "live")
	if !strings.Contains(out, `PROJ-50 stays Ready for release though its work of the last 30 days is on live (commits: 1): PROJ-50 not in an approved "Product v" version`) {
		t.Errorf("moved before its version is approved:\n%s", out)
	}
	d.jira.setApprovers(map[string][]string{"10030": {"APPROVED", "APPROVED"}})
	if out := d.mustGraft("deploy", "sweep", "live"); !strings.Contains(out, "PROJ-50 would move to Done") {
		t.Errorf("not moved once approved:\n%s", out)
	}
}

// TestDeployKeepAssigneeLabelOfParent: a subtask without a label of its own
// stays with its assignee when its parent has one of keep_assignee_labels;
// one under an unlabelled parent goes to the reviewer.
func TestDeployKeepAssigneeLabelOfParent(t *testing.T) {
	d := deployRepo(t)
	d.jira.mu.Lock()
	maps.Copy(d.jira.statuses, map[string]string{"PROJ-70": "In Progress", "PROJ-71": "In Progress", "PROJ-72": "In Progress", "PROJ-73": "In Progress"})
	d.jira.parents = map[string]string{"PROJ-71": "PROJ-70", "PROJ-73": "PROJ-72"}
	d.jira.labels = map[string][]string{"PROJ-70": {"internal"}}
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "sub")
	d.write("a.txt", "a\n")
	d.mustFixtureCommit("chore: PROJ-71 tooling\n\nand PROJ-73 a screen")
	out := d.mustGraft("deploy", "sub")
	_, moved := d.jira.snapshot()
	d.jira.mu.Lock()
	assigned := slices.Clone(d.jira.assigned)
	d.jira.mu.Unlock()
	if slices.Sort(moved); !slices.Equal(moved, []string{"PROJ-71", "PROJ-73"}) || !slices.Equal(assigned, []string{"PROJ-73 to qa"}) {
		t.Errorf("moved = %v, assigned = %v\n%s", moved, assigned, out)
	}
	if !strings.Contains(out, "PROJ-71 stays with its assignee: label internal of its parent PROJ-70 (keep_assignee_labels)") {
		t.Errorf("no word on the inherited label:\n%s", out)
	}
}

// TestDeploySkipKeysSubtaskOwnCommit: skip_keys leave their item alone and
// take no subtask along by it, but a subtask a commit names by its own key
// is an item like any other: the deploy notes and moves it, and the sweep
// lists it done before the target runs its work.
func TestDeploySkipKeysSubtaskOwnCommit(t *testing.T) {
	d := newDeployFixture(t, sweepConfig)
	d.env = append(d.env, "XDG_CACHE_HOME="+t.TempDir())
	d.git("remote", "set-head", "origin", d.git("branch", "--show-current"))
	d.jira.mu.Lock()
	maps.Copy(d.jira.statuses, map[string]string{"PROJ-40": "Ready for release", "PROJ-41": "Ready for release", "PROJ-43": "Done"})
	d.jira.parents = map[string]string{"PROJ-41": "PROJ-40", "PROJ-43": "PROJ-40"}
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "swept")
	d.write("a.txt", "a\n")
	d.mustFixtureCommit("feat: PROJ-41 part of the standing item")
	out := d.mustGraft("deploy", "swept")
	d.jira.mu.Lock()
	statuses := map[string]string{"PROJ-40": d.jira.statuses["PROJ-40"], "PROJ-41": d.jira.statuses["PROJ-41"]}
	notes := len(d.jira.comments["PROJ-41"])
	d.jira.mu.Unlock()
	if want := map[string]string{"PROJ-40": "Ready for release", "PROJ-41": "Done"}; !maps.Equal(statuses, want) || notes != 1 {
		t.Errorf("statuses = %v, want %v; notes on PROJ-41: %d\n%s", statuses, want, notes, out)
	}
	d.write("b.txt", "b\n")
	d.mustFixtureCommit("feat: PROJ-43 another part")
	out = d.mustGraft("deploy", "sweep", "swept")
	if !regexp.MustCompile(`PROJ-43 is Done, but [0-9a-f]+ is not on swept yet`).MatchString(out) {
		t.Errorf("PROJ-43 not listed:\n%s", out)
	}
	if !strings.Contains(out, "it lists only: pass --apply to move\n") {
		t.Errorf("no word on --apply:\n%s", out)
	}
}

// TestDeploySweepByContent: a commit the target does not run by history but
// does by content is on it: one that changes only files approved_release
// ignores, and two commits of the trunk a release branch took in one; the
// commit that took them is in the trunk by content too. A commit whose change
// the target lacks stays listed. An item whose work ships nothing to the
// target moves saying so; one whose commit ships work and docs the release
// left out is on it, and one with both kinds of commit says how many ship
// nothing.
func TestDeploySweepByContent(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return strings.Replace(deployConfig(state, q), `approved_release: {prefix: "Product v", ignore_paths: ["docs/**", "*.md"]}`,
			`approved_release: {prefix: "Product v", ignore_paths: ["docs/**", "*.md"]}
      release_notes: {transitions: [{from: [Ready for release], to: Done}], sweep: {}}`, 1)
	})
	d.env = append(d.env, "XDG_CACHE_HOME="+t.TempDir())
	trunk := d.git("branch", "--show-current")
	d.git("remote", "set-head", "origin", trunk)
	d.jira.mu.Lock()
	maps.Copy(d.jira.statuses, map[string]string{"PROJ-80": "Done", "PROJ-81": "Done", "PROJ-82": "Done", "PROJ-83": "Done", "PROJ-84": "Done", "PROJ-85": "Ready for release",
		"PROJ-86": "Ready for release", "PROJ-87": "Ready for release"})
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "--unapproved", "live")
	base := d.git("rev-parse", "HEAD")
	d.write("docs/handover.md", "notes\n")
	d.mustFixtureCommit("docs: PROJ-80 handover")
	d.write("docs/tools.md", "tools\n")
	d.mustFixtureCommit("docs: PROJ-85 tools")
	d.write("b.txt", "b\n")
	d.mustFixtureCommit("feat: PROJ-81 b")
	d.write("c.txt", "c\n")
	d.mustFixtureCommit("feat: PROJ-82 c")
	d.write("e.txt", "e\n")
	d.mustFixtureCommit("feat: PROJ-83 e")
	d.write("h.txt", "h\n")
	d.write("docs/h.md", "h\n")
	d.mustFixtureCommit("feat: PROJ-86 h with its docs")
	d.write("docs/k.md", "k\n")
	d.mustFixtureCommit("docs: PROJ-87 k")
	d.write("k.txt", "k\n")
	d.mustFixtureCommit("feat: PROJ-87 k")
	d.write("g.txt", "g\n")
	d.mustFixtureCommit("feat: PROJ-84 g")
	picked := d.git("rev-parse", "HEAD")
	d.git("switch", "-q", "-c", "release/1.0", base)
	d.write("b.txt", "b\n")
	d.write("c.txt", "c\n")
	d.mustFixtureCommit("chore: PROJ-81 and PROJ-82 to the release")
	d.write("h.txt", "h\n")
	d.write("k.txt", "k\n")
	d.mustFixtureCommit("chore: h and k to the release, without docs")
	// Taken in another shape the release needs: it names what it took.
	d.write("g.txt", "g for the release\n")
	d.mustFixtureCommit("chore: PROJ-84 to the release\n\n(cherry picked from commit " + picked[:9] + ")")
	d.mustGraft("deploy", "--unapproved", "live")

	out := d.mustGraft("deploy", "sweep", "live")
	for _, key := range []string{"PROJ-80", "PROJ-81", "PROJ-82", "PROJ-84"} {
		if strings.Contains(out, key+" is Done") {
			t.Errorf("%s listed, though live runs its work:\n%s", key, out)
		}
	}
	if !regexp.MustCompile(`PROJ-83 is Done, but [0-9a-f]+ is not on live yet`).MatchString(out) {
		t.Errorf("PROJ-83 not listed:\n%s", out)
	}
	for _, want := range []string{
		"PROJ-85 stays Ready for release though its work of the last 30 days ships nothing to live, changing only approved_release.ignore_paths (commits: 1)",
		"PROJ-86 stays Ready for release though its work of the last 30 days is on live (commits: 1)",
		"PROJ-87 stays Ready for release though its work of the last 30 days is on live, but for 1 commits that ship nothing to it, changing only approved_release.ignore_paths (commits: 2)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}
}

// TestDeploySweepStatusesNoTransitionNames: with no transition from In
// Progress or Testing, an item there is judged as one in work, listed only
// when all its work is on the target and stale_days old; an item in
// skip_statuses, put aside by hand, is listed with commits in the trunk.
func TestDeploySweepStatusesNoTransitionNames(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		c := strings.ReplaceAll(sweepConfig(state, q), "{from: [In Progress], to: Testing, assign: qa, keep_assignee_labels: [internal]}, ", "")
		return strings.ReplaceAll(c, "{transitions: [{from: [In Progress], to: Testing}]", "{transitions: [{from: [Ready for release], to: Done}]")
	})
	if c, _ := os.ReadFile(d.path(".graft.yaml")); strings.Contains(string(c), "In Progress") {
		t.Fatalf("a transition still names In Progress:\n%s", c)
	}
	d.env = append(d.env, "GRAFT_NOW="+time.Now().Add(5*24*time.Hour+time.Hour).UTC().Format(time.RFC3339), "XDG_CACHE_HOME="+t.TempDir())
	d.git("remote", "set-head", "origin", d.git("branch", "--show-current"))
	d.jira.mu.Lock()
	maps.Copy(d.jira.statuses, map[string]string{"PROJ-90": "In Progress", "PROJ-91": "Testing", "PROJ-92": "Blocked"})
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "swept")
	d.write("a.txt", "a\n")
	d.mustFixtureCommit("feat: PROJ-90 a")
	d.write("b.txt", "b\n")
	d.mustFixtureCommit("feat: PROJ-92 b")
	d.mustGraft("deploy", "swept")
	d.write("c.txt", "c\n")
	d.mustFixtureCommit("feat: PROJ-91 c")
	out := d.mustGraft("deploy", "sweep", "swept")
	if !strings.Contains(out, "PROJ-90 is In Progress, but all its work is on swept, the newest commit 5 days old") {
		t.Errorf("PROJ-90 not listed as stale:\n%s", out)
	}
	if strings.Contains(out, "PROJ-91") {
		t.Errorf("PROJ-91 listed, though its work is not on swept:\n%s", out)
	}
	if !strings.Contains(out, "PROJ-92 is Blocked, but its commits are in ") {
		t.Errorf("PROJ-92 not listed:\n%s", out)
	}
}

// fakeTimeLayout is how Jira writes times.
const fakeTimeLayout = "2006-01-02T15:04:05.000-0700"

// listComments serves the comments of an item in one page: those people
// wrote and graft's, written by "me" just now.
func (j *fakeJira) listComments(w http.ResponseWriter, key string) {
	all := slices.Clone(j.written[key])
	for _, text := range j.comments[key] {
		all = append(all, fakeComment{author: "me", text: text, created: time.Now()})
	}
	comments := []any{}
	for _, c := range all {
		comments = append(comments, map[string]any{
			"author":  map[string]string{"accountId": c.author},
			"created": c.created.Format(fakeTimeLayout),
			"body": map[string]any{"type": "doc", "version": 1, "content": []any{map[string]any{
				"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": c.text}},
			}}},
		})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"total": len(comments), "comments": comments})
}

// TestDeployReleaseNotesMergeOfDeployedBranch: a merge brings nothing the
// target has already run, so merging a branch deployed before posts no note.
func TestDeployReleaseNotesMergeOfDeployedBranch(t *testing.T) {
	d := deployRepo(t)
	d.mustGraft("deploy", "test")
	trunk := d.git("rev-parse", "--abbrev-ref", "HEAD")
	d.jira.statuses["PROJ-10"] = "Testing"
	d.git("switch", "-q", "-c", "feature/PROJ-10-fix")
	d.write("g.txt", "g\n")
	d.git("add", "-A")
	d.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "fix: PROJ-10 real fix")
	d.git("push", "-q", "-u", "origin", "feature/PROJ-10-fix")
	d.mustGraft("deploy", "test")
	d.git("switch", "-q", trunk)
	d.git("-c", "core.hooksPath=/dev/null", "merge", "-q", "--no-ff", "-m", "Merge branch 'feature/PROJ-10-fix'", "feature/PROJ-10-fix")
	d.git("push", "-q", "origin", trunk)
	out := d.mustGraft("deploy", "test")
	if comments, _ := d.jira.snapshot(); len(comments["PROJ-10"]) != 1 {
		t.Errorf("comments = %v\n%s", comments, out)
	}
}
