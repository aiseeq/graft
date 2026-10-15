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
	notes := func(n string) string { return strings.Replace(base, "release_notes: {}}", "release_notes: "+n+"}", 1) }
	cfg, err = Parse([]byte(notes("{transitions: [{from: [In Progress, To Do], to: Testing, assign: qa-account}, {from: [Ready for release], to: Done}]}")))
	if err != nil {
		t.Errorf("transitions: %v", err)
	} else if tr := cfg.Deploy.Targets["test"].ReleaseNotes.Transitions; len(tr) != 2 || tr[0].To != "Testing" || tr[0].Assign != "qa-account" || !slices.Equal(tr[1].From, []string{"Ready for release"}) {
		t.Errorf("transitions = %+v", tr)
	}
	if _, err := Parse([]byte(notes("{transition: Testing, from: [In Progress]}"))); err == nil || !strings.Contains(err.Error(), "release_notes.transition and from are replaced by transitions: [{from: [In Progress], to: Testing}]") {
		t.Errorf("the old transition: %v", err)
	}
	cfg, err = Parse([]byte(notes(`{transitions: [{from: [In Progress], to: Testing}], fix_version: {prefix: "Product v"}}`)))
	if err != nil {
		t.Errorf("fix_version: %v", err)
	} else if fv := cfg.Deploy.Targets["test"].ReleaseNotes.FixVersion; fv == nil || fv.Prefix != "Product v" {
		t.Errorf("fix_version = %+v", fv)
	}
	cfg, err = Parse([]byte(strings.Replace(base, "release_notes: {}}", "release_notes: {}, approved_release: {prefix: Product v}}", 1)))
	if err != nil {
		t.Errorf("approved_release: %v", err)
	} else if ar := cfg.Deploy.Targets["test"].ApprovedRelease; ar == nil || ar.Prefix != "Product v" {
		t.Errorf("approved_release = %+v", ar)
	}
	cases := map[string]string{
		"transition w/o from":  notes("{transitions: [{to: Testing}]}"),
		"transition w/o to":    notes("{transitions: [{from: [In Progress]}]}"),
		"from has transition":  notes("{transitions: [{from: [Testing], to: Testing}]}"),
		"from twice":           notes("{transitions: [{from: [In Progress], to: Testing}, {from: [In Progress], to: Done}]}"),
		"blank assign":         notes("{transitions: [{from: [In Progress], to: Testing, assign: ' '}]}"),
		"old from":             notes("{from: [In Progress]}"),
		"fix_version w/o pfx":  notes("{fix_version: {}}"),
		"subtasks w/o move":    notes("{subtasks: true}"),
		"fix_version blank":    notes("{fix_version: {prefix: '  '}}"),
		"fix_version name":     notes("{fix_version: {name: Product v1}}"),
		"approved w/o sha":     strings.Replace(base, "confirm: sudo}", "confirm: sudo, approved_release: {prefix: Product v}}", 1),
		"approved w/o prefix":  strings.Replace(base, "release_notes: {}}", "release_notes: {}, approved_release: {}}", 1),
		"approved bad glob":    strings.Replace(base, "release_notes: {}}", "release_notes: {}, approved_release: {prefix: Product v, ignore_paths: [/etc/x]}}", 1),
		"approved w/o jira":    strings.Replace(strings.Replace(base, "release_notes: {jira: {project_keys: [PROJ]}}\n", "", 1), "release_notes: {}}", "approved_release: {prefix: Product v}}", 1),
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

func TestParseWindowAndHotfix(t *testing.T) {
	base := `schema: 1
version: {mode: file}
envs: {local: {}}
release_notes: {jira: {project_keys: [PROJ]}}
deploy:
  remote: origin
  hotfix: {branches: ['hotfix/*'], label: hotfix, base: prod}
  targets:
    test: {env: local, run: [sh, d.sh], version: 'cat V', deployed_sha: {path: /x/test}}
    prod: {env: local, run: [sh, d.sh], requires: test, deployed_sha: {path: /x/prod}, window: {days: [mon, thu], at: "15:00", until: "19:00", tz: Europe/Berlin, cutoff: {days_before: 1, at: "18:00"}}}
`
	cfg, err := Parse([]byte(base))
	if err != nil {
		t.Fatalf("base must parse: %v", err)
	}
	w := cfg.Deploy.Targets["prod"].Window
	if w.Start != 15*time.Hour || w.End != 19*time.Hour || w.Cutoff.Time != 18*time.Hour || len(w.Weekdays) != 2 || w.Location.String() != "Europe/Berlin" {
		t.Errorf("window = %+v", w)
	}
	if !cfg.Deploy.IsHotfix("hotfix/PROJ-1") || cfg.Deploy.IsHotfix("main") || !slices.Equal(cfg.Deploy.HoldingWindows("test"), []string{"prod"}) {
		t.Error("hotfix branches or holding windows")
	}
	for name, yaml := range map[string]string{
		"bad day":           strings.Replace(base, "[mon, thu]", "[monday]", 1),
		"day twice":         strings.Replace(base, "[mon, thu]", "[mon, mon]", 1),
		"no days":           strings.Replace(base, "days: [mon, thu], ", "", 1),
		"bad at":            strings.Replace(base, `at: "15:00"`, `at: "3pm"`, 1),
		"until before at":   strings.Replace(base, `until: "19:00"`, `until: "14:00"`, 1),
		"no tz":             strings.Replace(base, "tz: Europe/Berlin, ", "", 1),
		"unknown tz":        strings.Replace(base, "Europe/Berlin", "Mars/Olympus", 1),
		"cutoff after":      strings.Replace(base, "days_before: 1", "days_before: 0", 1),
		"cutoff w/o req":    strings.Replace(base, "requires: test, ", "", 1),
		"window w/o sha":    strings.Replace(base, "deployed_sha: {path: /x/prod}, ", "", 1),
		"held w/o sha":      strings.Replace(base, ", deployed_sha: {path: /x/test}", "", 1),
		"hotfix no label":   strings.Replace(base, "label: hotfix, ", "", 1),
		"hotfix no base":    strings.Replace(base, ", base: prod", "", 1),
		"hotfix bad base":   strings.Replace(base, "base: prod", "base: uat", 1),
		"hotfix no branch":  strings.Replace(base, "branches: ['hotfix/*'], ", "", 1),
		"hotfix one branch": strings.Replace(base, "remote: origin", "remote: origin\n  branch: main", 1),
		"reserved cutoff":   strings.Replace(strings.Replace(base, "    prod:", "    cutoff:", 1), "base: prod", "base: cutoff", 1),
	} {
		if _, err := Parse([]byte(yaml)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestParseIntegrations(t *testing.T) {
	base := `schema: 1
version: {mode: file}
envs: {local: {}, prod: {ssh: {host: h}}}
deploy:
  remote: origin
  targets:
    test: {env: local, run: [sh, d.sh], version: 'cat V'}
    prod: {env: prod, run: [sh, d.sh], requires: test, integrations: {command: 'app --print-integrations', exceptions: x.conf}}
    up: {env: prod, run: [sh, d.sh], requires: test, integrations: {command: 'run {staged} --print-integrations', running: /opt/app}}
`
	cfg, err := Parse([]byte(base))
	if err != nil {
		t.Fatalf("base must parse: %v", err)
	}
	if cfg.Deploy.Targets["prod"].Integrations.Staged() || !cfg.Deploy.Targets["up"].Integrations.Staged() {
		t.Error("Staged")
	}
	for name, yaml := range map[string]string{
		"no command":          strings.Replace(base, "command: 'app --print-integrations', ", "", 1),
		"no requires":         strings.Replace(base, "requires: test, integrations: {command: 'app", "integrations: {command: 'app", 1),
		"staged w/o running":  strings.Replace(base, ", running: /opt/app", "", 1),
		"running w/o staged":  strings.Replace(base, "exceptions: x.conf", "running: /opt/app", 1),
		"remote ${KEY} argv":  strings.Replace(base, "command: 'app --print-integrations'", "command: [app, '${MODE}']", 1),
		"reserved subcommand": strings.Replace(base, "    up:", "    check-integrations:", 1),
	} {
		if _, err := Parse([]byte(yaml)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestParseReleasePage(t *testing.T) {
	base := `schema: 1
version: {mode: file}
envs: {local: {}}
release_notes: {jira: {project_keys: [PROJ]}}
deploy:
  remote: origin
  targets:
    prod: {env: local, run: [sh, d.sh], deployed_sha: {path: /x/prod}, approved_release: {prefix: "Product v"}, release_page: {parent: "500", labels: {fixes: Fixed}}}
`
	cfg, err := Parse([]byte(base))
	if err != nil {
		t.Fatalf("base must parse: %v", err)
	}
	p := cfg.Deploy.Targets["prod"].ReleasePage
	if p.Title != "{release}: release notes" || !slices.Equal(p.FixTypes, []string{"Bug"}) || p.Label("fixes") != "Fixed" || p.Label("changes") != "Changes" {
		t.Errorf("release page = %+v", p)
	}
	for name, yaml := range map[string]string{
		"no parent":     strings.Replace(base, `parent: "500", `, "", 1),
		"bad parent":    strings.Replace(base, `parent: "500"`, `parent: "Changelog"`, 1),
		"no release":    strings.Replace(base, `approved_release: {prefix: "Product v"}`, `approved_release: {statuses: [Done]}`, 1),
		"fixed title":   strings.Replace(base, `parent: "500"`, `parent: "500", title: "Release notes"`, 1),
		"unknown label": strings.Replace(base, "fixes: Fixed", "bugs: Fixed", 1),
	} {
		if _, err := Parse([]byte(yaml)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestVersionName(t *testing.T) {
	n, err := ParseVersionName("Product {dd}.{mm}.{yy}")
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	if got := n.Format(day); got != "Product 19.10.26" {
		t.Errorf("Format = %q", got)
	}
	if got, ok := n.Date("Product 22.10.26"); !ok || !got.Equal(time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("Date = %v, %v", got, ok)
	}
	for name, looks := range map[string]bool{
		"Product.19.10.26":        true,
		"product 19-10-26":        true,
		"Product 19.10.26":        false,
		"Product 19.10.26 hotfix": false,
		"Product v2.14.20":        false,
		"Other 19.10.26":          false,
	} {
		if _, ok := n.Date(name); ok && looks {
			t.Errorf("%q follows the template", name)
		}
		if got := n.LooksLike(name); got != looks {
			t.Errorf("LooksLike(%q) = %v", name, got)
		}
	}
	if _, ok := n.Date("Product 31.02.26"); ok {
		t.Error("31 February read as a day")
	}
	ymd, err := ParseVersionName("R {yy}-{mm}-{dd}")
	if err != nil {
		t.Fatal(err)
	}
	if got := ymd.Format(day); got != "R 26-10-19" || !ymd.LooksLike("R 26.10.19") {
		t.Errorf("year first: %q", got)
	}
	for _, bad := range []string{"", "Product {dd}.{mm}", "Product {dd}.{dd}.{yy}", "Product"} {
		if _, err := ParseVersionName(bad); err == nil {
			t.Errorf("ParseVersionName(%q) should fail", bad)
		}
	}
}
