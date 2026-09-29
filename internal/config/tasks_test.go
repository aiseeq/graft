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
