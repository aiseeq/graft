//go:build linux

package testdb

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/aiseeq/graft/v2/internal/config"
)

// TestDockerRefusedUnderNoNewPrivs: a process with no_new_privs, as a
// sandbox runs it, is told docker cannot start, without starting it: the
// attempt would raise an SELinux alert. The check runs in a child of the
// test, as no_new_privs cannot be dropped once set.
func TestDockerRefusedUnderNoNewPrivs(t *testing.T) {
	if os.Getenv("GRAFT_TEST_NNP_CHILD") == "1" {
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
		_, err := dockerCommand(t.Context(), "version")
		if err == nil || !strings.Contains(err.Error(), "cannot start here") {
			t.Fatalf("docker under no_new_privs: %v", err)
		}
		return
	}
	needRefusal(t)
	inChild(t, "TestDockerRefusedUnderNoNewPrivs")
}

// needRefusal skips a test of what docker's refusal under no_new_privs does
// where docker would not be refused.
func needRefusal(t *testing.T) {
	t.Helper()
	path, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("no docker")
	}
	if !runtimeLabeled(path) {
		t.Skipf("docker carries no %s label", runtimeExecType)
	}
	if _, err := os.Stat("/sys/fs/selinux/enforce"); errors.Is(err, os.ErrNotExist) {
		t.Skip("no SELinux")
	}
}

// inChild runs the test in a child of this test binary, with
// GRAFT_TEST_NNP_CHILD set, the child's cue to set no_new_privs.
func inChild(t *testing.T, name string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "GRAFT_TEST_NNP_CHILD=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("%v\n%s", err, out)
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
	if os.Getenv("GRAFT_TEST_NNP_CHILD") != "1" {
		needRefusal(t)
		inChild(t, "TestServerWithoutDocker")
		return
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
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

// runtimeLabeled tells whether the file carries the SELinux label of a
// container runtime, the environment the test needs.
func runtimeLabeled(path string) bool {
	buf := make([]byte, 256)
	n, err := unix.Getxattr(path, "security.selinux", buf)
	return err == nil && strings.Contains(string(buf[:n]), ":"+runtimeExecType+":")
}
