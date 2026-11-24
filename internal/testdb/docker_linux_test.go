//go:build linux

package testdb

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/lock"
	"github.com/aiseeq/graft/v2/internal/proc/proctest"
)

// TestDockerRefusedUnderNoNewPrivs: a process with no_new_privs, as a
// sandbox runs it, is told docker cannot start, without starting it: the
// attempt would raise an SELinux alert.
func TestDockerRefusedUnderNoNewPrivs(t *testing.T) {
	if !proctest.Sandboxed(t, "TestDockerRefusedUnderNoNewPrivs") {
		return
	}
	_, err := dockerCommand(t.Context(), "version")
	if err == nil || !strings.Contains(err.Error(), "cannot start here") {
		t.Fatalf("docker under no_new_privs: %v", err)
	}
}

// TestServerWithoutDocker: where docker may not start, a server brought up
// from outside is used as it answers on the DSN, a migration set's source
// made on it, and what only docker can do fails saying where to do it.
func TestServerWithoutDocker(t *testing.T) {
	dsn := os.Getenv("GRAFT_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("GRAFT_TEST_PG_DSN is not set: run graft test")
	}
	if !proctest.Sandboxed(t, "TestServerWithoutDocker") {
		return
	}
	cfg := socketConfig(t, dsn)
	var log bytes.Buffer
	// Up and cleanup modify the shared server, just as graft testdb does.
	// Hold its lock throughout the test so another Up cannot sweep the source
	// between CREATE and COMMENT, or while cleanup is removing it.
	lockDir, err := config.TestDBDir()
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(lockDir, cfg.Container+".lock")
	lk, err := lock.Acquire(t.Context(), lockPath, 15*time.Minute, t.Name(), &log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lk.Release(); err != nil {
			t.Error(err)
		}
	})
	source := sandboxTestSource(t, cfg)
	db := &DB{Cfg: cfg, Log: &log, Source: source}
	ctx := t.Context()
	t.Cleanup(func() {
		conn, err := db.connect(context.Background())
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close(context.Background())
		if _, err := query(context.Background(), conn, "DROP DATABASE IF EXISTS "+source+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	migrated := false
	if err := db.Up(ctx, func(context.Context) error { migrated = true; return nil }); err != nil {
		t.Fatalf("up: %v\n%s", err, log.String())
	}
	if !migrated || !strings.Contains(log.String(), "answers on") || !strings.Contains(log.String(), "creating "+source) {
		t.Errorf("up, migrated %v:\n%s", migrated, log.String())
	}
	s, err := db.Inspect(ctx)
	if err != nil || s.Refused == nil || !s.Running {
		t.Fatalf("state %+v: %v", s, err)
	}
	if drift := db.Drift(s); drift != "" {
		t.Errorf("the server runs with the default settings, drift %q", drift)
	}
	other := *cfg
	other.Settings = []string{"jit=on"}
	if drift := (&DB{Cfg: &other}).Drift(s); drift != "settings (jit=off)" {
		t.Errorf("the server runs with jit=off, the config asks for jit=on: drift %q", drift)
	}
	if _, err := db.Cgroup(ctx); !errors.Is(err, ErrNoCgroup) {
		t.Errorf("cgroup: %v", err)
	}
	if err := db.Down(ctx); err == nil || !strings.Contains(err.Error(), "graft testdb down outside the sandbox") {
		t.Errorf("down: %v", err)
	}
	gone := *cfg
	gone.SocketDir = t.TempDir()
	err = (&DB{Cfg: &gone, Log: &log, Source: gone.Database}).Up(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), "does not answer") || !strings.Contains(err.Error(), "outside the sandbox: graft testdb up") {
		t.Errorf("up with no server: %v", err)
	}
}

// sandboxTestSource gives each test invocation its own migration set and
// source, so its cleanup cannot remove another invocation's database.
func sandboxTestSource(t *testing.T, cfg *config.TestDB) string {
	t.Helper()
	migrations := t.TempDir()
	if err := os.WriteFile(filepath.Join(migrations, "1_init.up.sql"), []byte("-- "+migrations), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Migrations = migrations
	source, err := SourceFor(cfg, "/")
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func TestSandboxTestSourcesAreDistinct(t *testing.T) {
	cfg := &config.TestDB{Database: "graft"}
	first := sandboxTestSource(t, cfg)
	second := sandboxTestSource(t, cfg)
	if first == second || first == cfg.Database || second == cfg.Database {
		t.Fatalf("migration sources are not distinct: %q, %q", first, second)
	}
}

// socketConfig is the config of the socket database dsn names.
func socketConfig(t *testing.T, dsn string) *config.TestDB {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	host := u.Query().Get("host")
	if host == "" {
		t.Skipf("%s is not a socket database", dsn)
	}
	password, _ := u.User.Password()
	return &config.TestDB{Container: "graft-test-pg", Socket: true, SocketDir: host, User: u.User.Username(), Password: password,
		Database: strings.TrimPrefix(u.Path, "/"), Settings: config.DefaultTestDBSettings, ReadyTimeout: 10 * time.Second}
}

// TestInspectNamesDockersReason: when docker inspect fails for a reason other
// than a missing container, the error carries the last line docker wrote, so
// a closed socket is told apart from a stopped daemon.
func TestInspectNamesDockersReason(t *testing.T) {
	dir := t.TempDir()
	saved := dockerCheck
	t.Cleanup(func() { dockerCheck = saved })
	inspect := func(stderr string) (State, error) {
		script := filepath.Join(dir, "docker")
		if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '"+stderr+"' >&2\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		dockerCheck = func() (string, error) { return script, nil }
		return (&DB{Cfg: &config.TestDB{Container: "projectA-test-pg"}}).Inspect(t.Context())
	}
	if _, err := inspect("first line\\npermission denied while trying to connect to the docker API\\n"); err == nil ||
		!strings.HasSuffix(err.Error(), ": permission denied while trying to connect to the docker API") {
		t.Errorf("Inspect with a refused socket: %v", err)
	}
	for _, stderr := range []string{"", " \\n\\n"} {
		if _, err := inspect(stderr); err == nil || !strings.HasSuffix(err.Error(), "(is the docker daemon running?)") {
			t.Errorf("Inspect with stderr %q: %v", stderr, err)
		}
	}
	if st, err := inspect("Error: No such container: projectA-test-pg\\n"); err != nil || st.Exists {
		t.Errorf("Inspect of a missing container: %+v, %v", st, err)
	}
	dockerCheck = func() (string, error) { return filepath.Join(dir, "missing"), nil }
	if _, err := (&DB{Cfg: &config.TestDB{Container: "projectA-test-pg"}}).Inspect(t.Context()); err == nil ||
		!strings.HasSuffix(err.Error(), "(is the docker daemon running?)") {
		t.Errorf("Inspect when docker does not start: %v", err)
	}
}
