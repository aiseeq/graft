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
	migrations := t.TempDir()
	if err := os.WriteFile(filepath.Join(migrations, "1_init.up.sql"), []byte("-- "+t.Name()), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Migrations = migrations
	source, err := SourceFor(cfg, "/")
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
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
