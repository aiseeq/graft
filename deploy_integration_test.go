package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeJira records comments and transitions.
type fakeJira struct {
	mu       sync.Mutex
	comments map[string][]string
	moved    []string
	statuses map[string]string
	failAll  bool
}

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
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/rest/api/3/issue/"), "/")
	key := parts[0]
	switch {
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
	case r.Method == http.MethodGet && len(parts) == 1:
		_ = json.NewEncoder(w).Encode(map[string]any{
			"fields":      map[string]any{"status": map[string]string{"name": j.statuses[key]}},
			"transitions": []any{map[string]any{"id": "31", "to": map[string]string{"name": "Testing"}}},
		})
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "transitions":
		j.moved = append(j.moved, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unexpected", http.StatusNotFound)
	}
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
	if runtime.GOOS == "windows" {
		t.Skip("deploy scripts use sh")
	}
	state := t.TempDir()
	jira := &fakeJira{comments: map[string][]string{}, statuses: map[string]string{"PROJ-1": "In Progress", "PROJ-2": "Done"}}
	srv := httptest.NewServer(jira)
	t.Cleanup(srv.Close)

	cfgDir := t.TempDir()
	envFile := filepath.Join(cfgDir, "jira.env")
	writeFile(t, envFile, "JIRA_BASE_URL="+srv.URL+"\nJIRA_EMAIL=me@example.com\nJIRA_API_TOKEN=secret-token\n")
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "jira:\n  env_file: "+envFile+"\n")

	q := func(p string) string { return yamlQuote(filepath.Join(state, p)) }
	f := newRepo(t, map[string]string{
		".graft.yaml": `schema: 1
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
      release_notes: {transition: Testing}
      status: 'echo test runs; cat ` + filepath.Join(state, "test-version") + `'
      logs: "printf 'a\nerror one\nb\nerror two\n' | tail -n {lines}"
    stage:
      env: local
      run: [sh, deploy.sh]
      requires: test
    prod:
      env: local
      run: [sh, deploy.sh]
      requires: test
      confirm: sudo
release_notes:
  jira:
    project_keys: [PROJ]
    skip_statuses: [Done]
`,
		"VERSION": "1.0.0\n",
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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
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
	d.mustGraft("commit", "-m", "fix: second\n\nRefs PROJ-2 and OTHER-5")
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
	if !slices.Equal(keys, []string{"PROJ-1", "PROJ-2"}) {
		t.Errorf("commented keys = %v\n%s", keys, out)
	}
	if c := comments["PROJ-1"]; len(c) != 1 || !strings.HasPrefix(c[0], "Deployed to test, version 1.0.3, commit ") {
		t.Errorf("comment = %v", c)
	}
	if !slices.Equal(moved, []string{"PROJ-1"}) {
		t.Errorf("moved = %v (PROJ-2 is Done and stays)", moved)
	}
}

func TestDeployJiraFailureDoesNotFailTheDeploy(t *testing.T) {
	d := deployRepo(t)
	d.mustGraft("deploy", "test")
	d.write("a.txt", "a\n")
	d.mustGraft("commit", "-m", "feat: PROJ-1 x")
	d.jira.setFailing()
	out, code := d.graft("", "deploy", "test")
	if code != 0 || !strings.Contains(out, "release note for PROJ-1 not posted") || !strings.Contains(out, "http 500") {
		t.Errorf("exit %d:\n%s", code, out)
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

func TestDeployConfirmNeedsTerminal(t *testing.T) {
	d := deployRepo(t)
	d.mustGraft("deploy", "test")
	out, code := d.graft("", "deploy", "prod")
	if code == 0 || !strings.Contains(out, "foreground of a terminal") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if d.stateFile("prod-run") != "" {
		t.Error("prod deploy ran without confirmation")
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
