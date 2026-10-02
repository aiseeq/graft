package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/dotenv"
	"github.com/aiseeq/graft/internal/gitx"
	"github.com/aiseeq/graft/internal/lock"
	"github.com/aiseeq/graft/internal/tasks"
	"github.com/aiseeq/graft/internal/testdb"
)

// testDBLock serializes container changes between worktrees and sessions.
const testDBLock = "test-db"

// ErrUnknownTestDBAction marks a graft testdb action that testDBActions does not name.
var ErrUnknownTestDBAction = errors.New("unknown testdb action")

// testDBAction is one graft testdb subcommand; locked ones run under testDBLock.
type testDBAction struct {
	name   string
	locked bool
	run    func(a *App, ctx context.Context, repo *gitx.Repo, cfg *config.Config, db *testdb.DB) error
}

// testDBActions is the one list of graft testdb actions, in the order the usage line shows them.
var testDBActions = []testDBAction{
	{name: "up", locked: true, run: func(a *App, ctx context.Context, repo *gitx.Repo, cfg *config.Config, db *testdb.DB) error {
		return a.testDBUp(ctx, repo, cfg, db)
	}},
	{name: "down", locked: true, run: func(_ *App, ctx context.Context, _ *gitx.Repo, _ *config.Config, db *testdb.DB) error {
		return db.Down(ctx)
	}},
	{name: "status", run: func(a *App, ctx context.Context, _ *gitx.Repo, _ *config.Config, db *testdb.DB) error {
		return a.testDBStatus(ctx, db)
	}},
	{name: "recreate", locked: true, run: func(a *App, ctx context.Context, repo *gitx.Repo, cfg *config.Config, db *testdb.DB) error {
		if err := db.Down(ctx); err != nil {
			return err
		}
		return a.testDBUp(ctx, repo, cfg, db)
	}},
}

// TestDBUsage is the usage line of graft testdb, built from testDBActions.
func TestDBUsage() string {
	names := make([]string, len(testDBActions))
	for i, action := range testDBActions {
		names[i] = action.name
	}
	return "graft testdb " + strings.Join(names, "|")
}

// TestDB runs one of testDBActions.
func (a *App) TestDB(ctx context.Context, name string) error {
	i := slices.IndexFunc(testDBActions, func(action testDBAction) bool { return action.name == name })
	if i < 0 {
		return fmt.Errorf("%w %q", ErrUnknownTestDBAction, name)
	}
	action := testDBActions[i]
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	if cfg.TestDB == nil {
		return errors.New("no test_db section in .graft.yaml")
	}
	db := &testdb.DB{Cfg: cfg.TestDB, Log: a.Stdout}
	if !action.locked {
		return action.run(a, ctx, repo, cfg, db)
	}
	return a.withTestDBLock(ctx, repo, cfg, func() error { return action.run(a, ctx, repo, cfg, db) })
}

func (a *App) withTestDBLock(ctx context.Context, repo *gitx.Repo, cfg *config.Config, fn func() error) error {
	lk, err := lock.AcquireMode(ctx, filepath.Join(lockDir(repo), testDBLock+".lock"), lock.Exclusive, cfg.Lock.Timeout, "graft testdb", a.Stderr)
	if err != nil {
		return err
	}
	err = fn()
	if releaseErr := lk.Release(); releaseErr != nil {
		err = errors.Join(err, fmt.Errorf("releasing the %s lock: %w", testDBLock, releaseErr))
	}
	return err
}

func (a *App) testDBStatus(ctx context.Context, db *testdb.DB) error {
	s, err := db.Inspect(ctx)
	if err != nil {
		return err
	}
	switch {
	case !s.Exists:
		a.printf("%s does not exist (graft testdb up creates it)", db.Cfg.Container)
	case s.Running:
		a.printf("%s is running, %s", db.Cfg.Container, db.Cfg.DSN())
	default:
		a.printf("%s is stopped (graft testdb up starts it)", db.Cfg.Container)
	}
	if db.Drifted(s) {
		a.printf("warning: created with other settings; graft testdb recreate applies the config")
	}
	return nil
}

// testDBUp brings the database up, migrates it and keeps the DSN in .env in
// step, so tests started outside graft (an IDE) reach the same database.
func (a *App) testDBUp(ctx context.Context, repo *gitx.Repo, cfg *config.Config, db *testdb.DB) error {
	dsn := cfg.TestDB.DSN()
	if err := db.Up(ctx, func(ctx context.Context) error { return a.migrate(ctx, repo, cfg, dsn) }); err != nil {
		return err
	}
	return a.syncDotEnvDSN(repo, cfg, dsn)
}

func (a *App) migrate(ctx context.Context, repo *gitx.Repo, cfg *config.Config, dsn string) error {
	m := cfg.TestDB.Migrate
	if len(m.Argv) == 0 {
		return nil
	}
	a.printf("migrating: %s", m.Source)
	// ${dsn_var} is the database just brought up, not what .env (or the
	// environment) held before: that one is stale, or absent in a fresh clone.
	lookup := dotenv.NewLookup(filepath.Join(repo.Root, cfg.DotEnv)).With(map[string]string{cfg.TestDB.DSNVar: dsn})
	argv, err := dotenv.ExpandAll(m.Argv, lookup)
	if err != nil {
		return fmt.Errorf("test_db.migrate: %w", err)
	}
	env, err := tasks.BaseEnv()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = repo.Root
	cmd.Env = append(env, cfg.TestDB.DSNVar+"="+dsn)
	cmd.Stdout, cmd.Stderr = a.Stdout, a.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("test_db.migrate %q: %w", m.Source, err)
	}
	return nil
}

func (a *App) syncDotEnvDSN(repo *gitx.Repo, cfg *config.Config, dsn string) error {
	path := filepath.Join(repo.Root, cfg.DotEnv)
	f, err := dotenv.Load(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if v, ok := f.Get(cfg.TestDB.DSNVar); ok && v == dsn {
		return nil
	}
	if err := dotenv.Set(path, cfg.TestDB.DSNVar, dsn); err != nil {
		return err
	}
	a.printf("wrote %s to %s", cfg.TestDB.DSNVar, cfg.DotEnv)
	return nil
}

// testDSN is the DSN a test_db task gets. A DSN already in the environment
// (a CI service container) is used as is once it is shown to name the test
// database; otherwise the local container is brought up and migrated.
func (a *App) testDSN(ctx context.Context, repo *gitx.Repo, cfg *config.Config) (string, error) {
	if dsn, ok := os.LookupEnv(cfg.TestDB.DSNVar); ok {
		if err := testdb.CheckDSN(dsn, cfg.TestDB); err != nil {
			return "", err
		}
		return dsn, nil
	}
	db := &testdb.DB{Cfg: cfg.TestDB, Log: a.Stderr}
	if err := a.withTestDBLock(ctx, repo, cfg, func() error { return a.testDBUp(ctx, repo, cfg, db) }); err != nil {
		return "", err
	}
	return cfg.TestDB.DSN(), nil
}

// lockDir holds the named locks, shared by all worktrees.
func lockDir(repo *gitx.Repo) string {
	return filepath.Join(repo.CommonDir, "graft-locks")
}
