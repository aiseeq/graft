package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"make smoke", []string{"make", "smoke"}},
		{"  go   test  -short ./... ", []string{"go", "test", "-short", "./..."}},
		{`bash scripts/smoke.sh`, []string{"bash", "scripts/smoke.sh"}},
		{`go test -run 'Test[A-Z]*'`, []string{"go", "test", "-run", "Test[A-Z]*"}},
		{`echo "a b" c`, []string{"echo", "a b", "c"}},
		{`echo "say \"hi\" \$x"`, []string{"echo", `say "hi" $x`}},
		{`echo "back\slash"`, []string{"echo", `back\slash`}},
		{`echo a\ b`, []string{"echo", "a b"}},
		{`echo ''`, []string{"echo", ""}},
		{`echo x~y`, []string{"echo", "x~y"}},
	}
	for _, c := range cases {
		got, err := SplitCommand(c.in)
		if err != nil {
			t.Errorf("SplitCommand(%q): %v", c.in, err)
			continue
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("SplitCommand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSplitCommandRejectsShellSyntax(t *testing.T) {
	for _, in := range []string{
		"make smoke && make lint",
		"go test | tee log",
		"go test > log",
		"echo $HOME",
		`echo "$HOME"`,
		"echo `date`",
		"echo $(date)",
		"rm *.tmp",
		"ls ~/bin",
		"CGO_ENABLED=0 go build",
		"a; b",
		`echo "open`,
		`echo 'open`,
		"",
		"   ",
	} {
		if got, err := SplitCommand(in); err == nil {
			t.Errorf("SplitCommand(%q) = %q, want an error", in, got)
		}
	}
}

const minimal = "schema: 1\nversion:\n  mode: none\n"

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse([]byte("schema: 1\nversion:\n  mode: file\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version.File != "VERSION" || cfg.Version.TagPrefix != "v" {
		t.Errorf("version defaults: %+v", cfg.Version)
	}
	if cfg.Hooks.Dir != ".githooks" || cfg.Lock.Timeout != 15*time.Minute {
		t.Errorf("hooks/lock defaults: %+v %+v", cfg.Hooks, cfg.Lock)
	}
	if !cfg.Checks.Secrets.IsEnabled() || !cfg.Checks.LargeFiles.IsEnabled() {
		t.Error("checks must be on by default")
	}
	if cfg.Checks.LargeFiles.MaxBinaryBytes != 1<<20 || cfg.Checks.LargeFiles.MaxTextBytes != 5<<20 {
		t.Errorf("size defaults: %+v", cfg.Checks.LargeFiles)
	}
	if cfg.Ticket != nil {
		t.Error("ticket must stay unset without a ticket section")
	}
}

func TestParseFull(t *testing.T) {
	cfg, err := Parse([]byte(`
schema: 1
gate:
  - make smoke
  - [sh, -c, 'go vet ./... && go test ./...']
version:
  mode: file
  file: meta/VERSION
  tag_on_commit: true
  sync:
    - path: web/package.json
      format: json
      key: [version]
    - path: config.yaml
      format: regex
      pattern: 'version:\s*"([^"]*)"'
    - path: web/VERSION
      format: plain
ticket:
  pattern: '[A-Z][A-Z]+-[0-9]+'
push:
  remotes: [origin]
checks:
  secrets:
    enabled: false
    extra_patterns: ['token_[a-z]{10}']
    exceptions:
      - path: 'testdata/**'
        reason: synthetic keys
  large_files:
    max_binary_bytes: 10
    binary_extensions: [PNG]
hooks:
  dir: tools/hooks
lock:
  timeout: 90s
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Gate[1].Argv; !slices.Equal(got, []string{"sh", "-c", "go vet ./... && go test ./..."}) {
		t.Errorf("list command: %q", got)
	}
	if cfg.Version.Sync[1].Regexp == nil || cfg.Ticket.Regexp == nil {
		t.Error("patterns must be compiled")
	}
	if cfg.Checks.Secrets.IsEnabled() {
		t.Error("secrets: enabled false ignored")
	}
	if !slices.Equal(cfg.Checks.LargeFiles.BinaryExtensions, []string{"png"}) {
		t.Errorf("extensions: %q", cfg.Checks.LargeFiles.BinaryExtensions)
	}
	if cfg.Lock.Timeout != 90*time.Second {
		t.Errorf("timeout: %v", cfg.Lock.Timeout)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"missing schema":         "version:\n  mode: none\n",
		"unknown key":            minimal + "gates: []\n",
		"missing mode":           "schema: 1\nversion: {}\n",
		"bad mode":               "schema: 1\nversion:\n  mode: semver\n",
		"sync outside file mode": "schema: 1\nversion:\n  mode: git-tag\n  sync: [{path: a, format: plain}]\n",
		"regex without group":    "schema: 1\nversion:\n  mode: file\n  sync: [{path: a, format: regex, pattern: 'v.*'}]\n",
		"regex two groups":       "schema: 1\nversion:\n  mode: file\n  sync: [{path: a, format: regex, pattern: '(a)(b)'}]\n",
		"json without key":       "schema: 1\nversion:\n  mode: file\n  sync: [{path: a, format: json}]\n",
		"unknown format":         "schema: 1\nversion:\n  mode: file\n  sync: [{path: a, format: toml}]\n",
		"path escapes tree":      "schema: 1\nversion:\n  mode: file\n  file: ../VERSION\n",
		"absolute path":          "schema: 1\nversion:\n  mode: file\n  file: /etc/VERSION\n",
		"exception w/o reason":   minimal + "checks:\n  secrets:\n    exceptions: [{path: a}]\n",
		"shell in gate":          minimal + "gate: ['make a && make b']\n",
		"empty ticket":           minimal + "ticket: {}\n",
		"duplicate remote":       minimal + "push:\n  remotes: [a, a]\n",
		"dotted extension":       minimal + "checks:\n  large_files:\n    binary_extensions: [.png]\n",
		"negative timeout":       minimal + "lock:\n  timeout: -1s\n",
		"template is dotenv":     minimal + "dotenv_template: .env\n",
		"template escapes tree":  minimal + "dotenv_template: ../.env.example\n",
	}
	for name, data := range cases {
		if _, err := Parse([]byte(data)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestCompileGlob(t *testing.T) {
	cases := []struct {
		glob  string
		match []string
		miss  []string
	}{
		{"docs/big.js", []string{"docs/big.js"}, []string{"docs/big.jsx", "x/docs/big.js"}},
		{"testdata/**", []string{"testdata/a", "testdata/a/b.go"}, []string{"testdata", "x/testdata/a"}},
		{"**/fixtures/*.pem", []string{"fixtures/a.pem", "a/b/fixtures/c.pem"}, []string{"fixtures/a/b.pem"}},
		{"*.min.js", []string{"app.min.js"}, []string{"web/app.min.js"}},
		{"file?.txt", []string{"file1.txt"}, []string{"file10.txt"}},
	}
	for _, c := range cases {
		re, err := CompileGlob(c.glob)
		if err != nil {
			t.Fatalf("%s: %v", c.glob, err)
		}
		for _, p := range c.match {
			if !re.MatchString(p) {
				t.Errorf("%s should match %s", c.glob, p)
			}
		}
		for _, p := range c.miss {
			if re.MatchString(p) {
				t.Errorf("%s should not match %s", c.glob, p)
			}
		}
	}
	if _, err := CompileGlob("../x"); err == nil || !strings.Contains(err.Error(), "relative") {
		t.Errorf("escaping glob: %v", err)
	}
}

const flagsBase = `schema: 1
version:
  mode: none
envs:
  local: {}
  prod: {ssh: {host: 10.0.0.2, user: deploy}}
`

func TestParseFlags(t *testing.T) {
	cfg, err := Parse([]byte(flagsBase + `
flags:
  default_env: prod
  exceptions: tools/flags-exceptions.conf
  sql:
    table: public.app_events
    columns: {id: id, class: kind, subject: subject, status: status, last_seen: last_seen_at, note: note}
    open_statuses: [open]
    resolved_status: resolved
    psql:
      local: [psql, '${DB_DSN}', -X, -q, -t, -A, -v, ON_ERROR_STOP=1]
      prod: "exec psql -X -q -t -A -v ON_ERROR_STOP=1"
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Flags.SQL.Actor != "graft" || cfg.Flags.SQL.Psql["prod"].Shell == "" || len(cfg.Flags.SQL.Psql["local"].Argv) != 8 {
		t.Errorf("flags: %+v", cfg.Flags.SQL)
	}
	if cfg.DotEnv != ".env" || cfg.Envs["prod"].SSH.User != "deploy" {
		t.Errorf("envs: %+v", cfg.Envs)
	}
}

func TestParseFlagsErrors(t *testing.T) {
	sql := func(extra string) string {
		return flagsBase + `
flags:
  default_env: prod
  exceptions: x.conf
  sql:
    table: events
    columns: {id: id, class: kind, subject: subject, status: status, last_seen: seen, note: note}
    open_statuses: [open]
    resolved_status: resolved
    psql: {prod: "psql"}
` + extra
	}
	if _, err := Parse([]byte(sql(""))); err != nil {
		t.Fatalf("base must parse: %v", err)
	}
	cases := map[string]string{
		"unknown default env": strings.Replace(sql(""), "default_env: prod", "default_env: staging", 1),
		"unknown psql env":    strings.Replace(sql(""), "psql: {prod:", "psql: {uat:", 1),
		"injection in column": strings.Replace(sql(""), "class: kind", "class: 'kind; drop'", 1),
		"bad table":           strings.Replace(sql(""), "table: events", "table: 'ev ents'", 1),
		"missing note":        strings.Replace(sql(""), ", note: note", "", 1),
		"no adapter":          flagsBase + "flags: {default_env: prod, exceptions: x.conf}\n",
		"bad env name":        "schema: 1\nversion: {mode: none}\nenvs: {Prod: {}}\n",
		"ssh without host":    "schema: 1\nversion: {mode: none}\nenvs: {prod: {ssh: {user: x}}}\n",
		"http both transports": flagsBase + `
flags:
  default_env: local
  exceptions: x.conf
  http:
    list: {path: /l, items: data, page_size: 10}
    get: {path: '/l/{id}'}
    ack: {method: PUT, path: '/l/{id}'}
    fields: {id: id, class: c, subject: s, status: st, last_seen: ls}
    open_statuses: [new]
    transport: {local: {base_url: 'http://x', command: 'y'}}
`,
	}
	for name, data := range cases {
		if _, err := Parse([]byte(data)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestParseDeployErrors(t *testing.T) {
	base := `schema: 1
version: {mode: file}
envs: {local: {}, prod: {ssh: {host: 10.0.0.2}}}
release_notes: {jira: {project_keys: [PROJ]}}
deploy:
  remote: origin
  targets:
    test: {env: local, run: [sh, deploy.sh], version: 'cat /opt/app/VERSION', deployed_sha: {path: /opt/app/DEPLOYED_SHA}, release_notes: {}}
    prod: {env: prod, run: [sh, deploy.sh], requires: test, confirm: sudo}
`
	cfg, err := Parse([]byte(base))
	if err != nil {
		t.Fatalf("base must parse: %v", err)
	}
	if cfg.ReleaseNotes.Jira.Comment != DefaultNoteComment {
		t.Errorf("default comment: %q", cfg.ReleaseNotes.Jira.Comment)
	}
	cases := map[string]string{
		"no remote":            strings.Replace(base, "remote: origin", "remote: ''", 1),
		"unknown env":          strings.Replace(base, "env: prod,", "env: staging,", 1),
		"unknown requires":     strings.Replace(base, "requires: test", "requires: uat", 1),
		"requires self":        strings.Replace(base, "requires: test", "requires: prod", 1),
		"requires w/o version": strings.Replace(base, "version: 'cat /opt/app/VERSION', ", "", 1),
		"bad confirm":          strings.Replace(base, "confirm: sudo", "confirm: yes", 1),
		"notes w/o sha":        strings.Replace(base, "deployed_sha: {path: /opt/app/DEPLOYED_SHA}, ", "", 1),
		"notes w/o jira":       strings.Replace(base, "release_notes: {jira: {project_keys: [PROJ]}}\n", "", 1),
		"bad project key":      strings.Replace(base, "[PROJ]", "[proj]", 1),
		"no project keys":      strings.Replace(base, "{project_keys: [PROJ]}", "{comment: x}", 1),
		"reserved target name": strings.Replace(base, "    prod:", "    status:", 1),
		"no run":               strings.Replace(base, "run: [sh, deploy.sh], requires", "requires", 1),
		"requires without ver": strings.Replace(base, "mode: file", "mode: none", 1),
		"unknown dotenv set":   strings.Replace(base, "confirm: sudo}", "confirm: sudo, dotenv_sets: [nope]}", 1),
		"placeholder twice":    strings.Replace(base, "requires: test, confirm", "status: [x, '{args}{args}'], requires: test, confirm", 1),
		"unknown dep":          strings.Replace(base, "confirm: sudo}", "confirm: sudo, deps: [nope]}", 1),
	}
	for name, data := range cases {
		if _, err := Parse([]byte(data)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestParseDeployDeps(t *testing.T) {
	base := `schema: 1
version: {mode: none}
envs: {local: {}}
tasks:
  smoke: {run: [go vet ./...]}
deploy:
  remote: origin
  targets:
    staging: {env: local, run: [sh, deploy.sh], deps: [smoke]}
`
	cfg, err := Parse([]byte(base))
	if err != nil {
		t.Fatalf("base must parse: %v", err)
	}
	if got := cfg.Deploy.Targets["staging"].Deps; !slices.Equal(got, []string{"smoke"}) {
		t.Errorf("deps = %v", got)
	}
	_, err = Parse([]byte(strings.Replace(base, "deps: [smoke]", "deps: [smoke, nope]", 1)))
	if err == nil || !strings.Contains(err.Error(), `deploy.targets.staging.deps: unknown task "nope"`) {
		t.Errorf("unknown dep: %v", err)
	}
}

func TestRemoteArgvRefusesKeys(t *testing.T) {
	base := `schema: 1
version: {mode: file}
envs:
  local: {}
  prod: {ssh: {host: 10.0.0.2}}
flags:
  default_env: prod
  exceptions: tools/flags-exceptions.conf
  sql:
    table: notices
    columns: {id: id, class: kind, subject: subject, status: status, last_seen: last_seen_at, note: note}
    open_statuses: [open]
    resolved_status: resolved
    psql:
      prod: PSQL
      local: [psql, '${DB_URL}']
deploy:
  remote: origin
  targets:
    prod: {env: prod, run: [sh, deploy.sh], status: STATUS, version: 'cat /opt/app/VERSION'}
`
	ok := strings.NewReplacer("PSQL", `"psql \"$DB_URL\""`, "STATUS", `"systemctl status app-$APP"`).Replace(base)
	if _, err := Parse([]byte(ok)); err != nil {
		t.Fatalf("strings over ssh and ${KEY} in local argv must parse: %v", err)
	}
	for name, data := range map[string]string{
		"psql argv over ssh":   strings.NewReplacer("PSQL", `[psql, '${DB_URL}']`, "STATUS", "x").Replace(base),
		"status argv over ssh": strings.NewReplacer("PSQL", "psql", "STATUS", `[systemctl, status, 'app-${APP}']`).Replace(base),
	} {
		_, err := Parse([]byte(data))
		if err == nil || !strings.Contains(err.Error(), "in an argv command is not supported in the ssh environment prod: write the command as a string") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
