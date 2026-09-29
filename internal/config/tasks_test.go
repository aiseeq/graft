package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const tasksBase = `schema: 1
version: {mode: git-tag}
gate:
  - task: lint
tasks:
  lint:
    desc: lint
    run:
      - go vet ./...
      - [glint, --strict]
      - sh: echo {args} | tee out.txt
    lock: read
  test:
    deps: [lint]
    run: [{task: lint}]
    lock: {name: db, mode: write}
    test_db: true
services:
  web:
    run: [bin/web]
    dotenv: all
    pidfile: tmp/web.pid
    log: tmp/web.log
    build: lint
test_db:
  image: postgres:18-alpine
  container: app-test-pg
  port: 55432
  database: app_test
  user: app
  password: app
tools:
  linter:
    go_install: example.com/linter/cmd/linter@v1.2.0
    check: [linter, --version]
    expect: v1.2.0
`

func TestParseTasks(t *testing.T) {
	cfg, err := Parse([]byte(tasksBase))
	if err != nil {
		t.Fatal(err)
	}
	lint := cfg.Tasks["lint"]
	if !slices.Equal(lint.Run[0].Argv, []string{"go", "vet", "./..."}) || !slices.Equal(lint.Run[1].Argv, []string{"glint", "--strict"}) ||
		lint.Run[2].Shell != "echo {args} | tee out.txt" {
		t.Errorf("steps = %+v", lint.Run)
	}
	if *lint.Lock != (TaskLock{Name: DefaultLockName, Mode: "read"}) || *cfg.Tasks["test"].Lock != (TaskLock{Name: "db", Mode: "write"}) {
		t.Errorf("locks = %+v, %+v", lint.Lock, cfg.Tasks["test"].Lock)
	}
	if cfg.Gate[0].Task != "lint" || cfg.Tasks["test"].Run[0].Task != "lint" {
		t.Errorf("task steps = %+v %+v", cfg.Gate, cfg.Tasks["test"].Run)
	}
	web := cfg.Services["web"]
	if !web.DotEnv.All || web.StartTimeout != 30*time.Second || web.StopTimeout != 10*time.Second {
		t.Errorf("service defaults = %+v", web)
	}
	db := cfg.TestDB
	if db.DSNVar != "TEST_DB_DSN" || !slices.Equal(db.Settings, DefaultTestDBSettings) || db.ReadyTimeout != time.Minute {
		t.Errorf("test_db defaults = %+v", db)
	}
	if got := db.DSN(); got != "postgres://app:app@127.0.0.1:55432/app_test?sslmode=disable" {
		t.Errorf("DSN = %s", got)
	}
}

func TestParseTasksErrors(t *testing.T) {
	cases := map[string]string{
		"reserved name":     strings.Replace(tasksBase, "  lint:\n    desc", "  commit:\n    desc", 1),
		"bad name":          strings.Replace(tasksBase, "  lint:\n    desc", "  Lint:\n    desc", 1),
		"unknown dep":       strings.Replace(tasksBase, "deps: [lint]", "deps: [nosuch]", 1),
		"unknown step task": strings.Replace(tasksBase, "run: [{task: lint}]", "run: [{task: nosuch}]", 1),
		"unknown gate task": strings.Replace(tasksBase, "  - task: lint", "  - task: nosuch", 1),
		"cycle":             strings.Replace(tasksBase, "      - go vet ./...", "      - task: test", 1),
		"empty task":        strings.Replace(tasksBase, "    deps: [lint]\n    run: [{task: lint}]\n", "", 1),
		"step map two keys": strings.Replace(tasksBase, "run: [{task: lint}]", "run: [{task: lint, sh: x}]", 1),
		"bad lock mode":     strings.Replace(tasksBase, "lock: read", "lock: exclusive", 1),
		"dir escapes":       strings.Replace(tasksBase, "    lock: read", "    lock: read\n    dir: ../x", 1),
		"test_db w/o db":    tasksBase[:strings.Index(tasksBase, "test_db:\n  image")] + tasksBase[strings.Index(tasksBase, "tools:"):],
		"service w/o log":   strings.Replace(tasksBase, "    log: tmp/web.log\n", "", 1),
		"service bad build": strings.Replace(tasksBase, "build: lint", "build: nosuch", 1),
		"service dotenv":    strings.Replace(tasksBase, "dotenv: all", "dotenv: some", 1),
		"db name":           strings.Replace(tasksBase, "database: app_test", "database: app-test", 1),
		"no password":       strings.Replace(tasksBase, "  password: app\n", "", 1),
		"tool latest":       strings.Replace(tasksBase, "@v1.2.0", "@latest", 1),
		"tool w/o expect":   strings.Replace(tasksBase, "    expect: v1.2.0\n", "", 1),
	}
	for name, data := range cases {
		if data == tasksBase {
			t.Fatalf("%s: replacement did not apply", name)
		}
		if _, err := Parse([]byte(data)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestShellLabel(t *testing.T) {
	for in, want := range map[string]string{
		"echo hi": "echo hi",
		"set -eu\n# make room\n\nmkdir x\nmv a b\n": "mkdir x ...",
		"  go build  ": "go build",
	} {
		if got := shellLabel(in); got != want {
			t.Errorf("shellLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseKeysArgsAndUnits(t *testing.T) {
	base := `schema: 1
version: {mode: git-tag}
dotenv_sets:
  db: [DB_URL, 'DB_POOL?']
tasks:
  t:
    dotenv_sets: [db]
    dotenv:
      - DB_POOL
      - EXTRA?
    args: required
    usage: <file>
    run: [[cp, '{args}', '--to={args}']]
services:
  web:
    systemd_unit: web.service
    addr: ':8080'
`
	cfg, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	want := []DotEnvKey{{Name: "DB_URL"}, {Name: "DB_POOL"}, {Name: "EXTRA", Optional: true}}
	if !slices.Equal(cfg.Tasks["t"].Keys, want) {
		t.Errorf("keys = %+v", cfg.Tasks["t"].Keys)
	}
	cases := map[string]string{
		"unknown set":         strings.Replace(base, "dotenv_sets: [db]", "dotenv_sets: [cache]", 1),
		"bad key":             strings.Replace(base, "EXTRA?", "EXTRA??", 1),
		"unquoted flow KEY?":  strings.Replace(base, "'DB_POOL?'", "DB_POOL?", 1),
		"args w/o {args}":     strings.Replace(base, "[[cp, '{args}', '--to={args}']]", "[[cp, a, b]]", 1),
		"bad args value":      strings.Replace(base, "args: required", "args: many", 1),
		"placeholder twice":   strings.Replace(base, "'--to={args}'", "'{args}{args}'", 1),
		"unit with run":       strings.Replace(base, "    systemd_unit: web.service\n", "    systemd_unit: web.service\n    run: [x]\n", 1),
		"bad unit":            strings.Replace(base, "web.service", "web", 1),
		"service without run": strings.Replace(base, "    systemd_unit: web.service\n", "", 1),
	}
	for name, data := range cases {
		if data == base {
			t.Fatalf("%s: replacement did not apply", name)
		}
		if _, err := Parse([]byte(data)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestUnquotedOptionalKeyHint(t *testing.T) {
	_, err := Parse([]byte("schema: 1\nversion: {mode: none}\ndotenv_sets: {db: [A, B?]}\n"))
	if err == nil || !strings.Contains(err.Error(), "must be quoted") {
		t.Errorf("err = %v", err)
	}
}
