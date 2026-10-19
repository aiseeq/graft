package testdb

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
)

func TestCheckDSN(t *testing.T) {
	cfg := &config.TestDB{Database: "app_test", DSNVar: "TEST_DB_DSN"}
	for dsn, ok := range map[string]bool{
		"postgres://u:p@127.0.0.1:5432/app_test?sslmode=disable": true,
		"postgresql://u@db/app_test":                             true,
		"postgres://u:p@127.0.0.1:5432/app":                      false,
		"postgres://u:p@127.0.0.1:5432/":                         false,
		"host=127.0.0.1 dbname=app_test":                         false,
		"mysql://u@h/app_test":                                   false,
	} {
		if err := CheckDSN(dsn, cfg, cfg.Database); (err == nil) != ok {
			t.Errorf("CheckDSN(%q) = %v", dsn, err)
		}
	}
}

func TestDrift(t *testing.T) {
	d := &DB{Cfg: &config.TestDB{Settings: []string{"fsync=off"}}}
	if got := d.Drift(State{}); got != "" {
		t.Errorf("a missing container has not drifted: %s", got)
	}
	for _, cmd := range [][]string{{"postgres", "-c", "fsync=off"}, {"-c", "fsync=off"}} {
		if got := d.Drift(State{Exists: true, Cmd: cmd}); got != "" {
			t.Errorf("same settings %q reported as drift: %s", cmd, got)
		}
	}
	if got := d.Drift(State{Exists: true, Cmd: []string{"postgres"}}); got != "settings (postgres)" {
		t.Errorf("changed settings: %q", got)
	}
}

func TestDriftTmpfs(t *testing.T) {
	cmd := []string{"-c", "fsync=off"}
	onDisk := &DB{Cfg: &config.TestDB{Settings: []string{"fsync=off"}}}
	inRAM := &DB{Cfg: &config.TestDB{Settings: []string{"fsync=off"}, Tmpfs: "2g"}}
	for _, c := range []struct {
		db    *DB
		tmpfs map[string]string
		want  string
	}{
		{onDisk, nil, ""},
		{inRAM, map[string]string{"/var/lib/postgresql": "rw,size=2g"}, ""},
		{inRAM, map[string]string{"/a": "rw,size=2g", "/b": "rw,size=2g"}, ""},
		{inRAM, nil, "tmpfs (none, the config asks for 2g)"},
		{inRAM, map[string]string{"/var/lib/postgresql": "rw,size=1g"}, "tmpfs (/var/lib/postgresql:rw,size=1g, the config asks for 2g)"},
		{onDisk, map[string]string{"/var/lib/postgresql": "rw,size=2g"}, "tmpfs (/var/lib/postgresql:rw,size=2g, the config asks for none)"},
	} {
		if got := c.db.Drift(State{Exists: true, Cmd: cmd, Tmpfs: c.tmpfs}); got != c.want {
			t.Errorf("config %q, container %v: drift %q, want %q", c.db.Cfg.Tmpfs, c.tmpfs, got, c.want)
		}
	}
	both := inRAM.Drift(State{Exists: true, Cmd: []string{"postgres"}})
	if !strings.HasPrefix(both, "settings (postgres) and tmpfs (") {
		t.Errorf("both drifts: %q", both)
	}
}

func TestRunArgs(t *testing.T) {
	cfg := &config.TestDB{Image: "postgres:18-alpine", Container: "app-test-pg", Port: 55432, Settings: []string{"fsync=off"}}
	want := []string{"run", "-d", "--name", "app-test-pg", "--restart", "unless-stopped",
		"-e", "POSTGRES_USER", "-e", "POSTGRES_PASSWORD", "-e", "POSTGRES_DB",
		"-p", "127.0.0.1:55432:5432", "postgres:18-alpine", "-c", "fsync=off"}
	if got := (&DB{Cfg: cfg}).runArgs(nil); !slices.Equal(got, want) {
		t.Errorf("on disk:\n got %q\nwant %q", got, want)
	}
	cfg.Tmpfs = "2g"
	want = slices.Insert(want, 14, "--tmpfs", "/var/lib/postgresql:rw,size=2g", "--tmpfs", "/x:rw,size=2g")
	if got := (&DB{Cfg: cfg}).runArgs([]string{"/var/lib/postgresql", "/x"}); !slices.Equal(got, want) {
		t.Errorf("tmpfs:\n got %q\nwant %q", got, want)
	}
}

// TestRunArgsSocket: a socket database runs without a network, so docker
// gives it no veth on the host, and serves the socket in a bind mount next
// to the image's own directory.
func TestRunArgsSocket(t *testing.T) {
	cfg := &config.TestDB{Image: "postgres:18-alpine", Container: "app-test-pg", Socket: true, SocketDir: "/c/graft/testdb/app-test-pg",
		Settings: []string{"fsync=off"}}
	want := []string{"run", "-d", "--name", "app-test-pg", "--restart", "unless-stopped",
		"-e", "POSTGRES_USER", "-e", "POSTGRES_PASSWORD", "-e", "POSTGRES_DB",
		"--network", "none", "-v", "/c/graft/testdb/app-test-pg:/run/graft-testdb:z", "postgres:18-alpine",
		"-c", "fsync=off", "-c", "unix_socket_directories=/var/run/postgresql,/run/graft-testdb"}
	if got := (&DB{Cfg: cfg}).runArgs(nil); !slices.Equal(got, want) {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
}

func TestDriftListening(t *testing.T) {
	tcp := &DB{Cfg: &config.TestDB{Port: 55432}}
	sock := &DB{Cfg: &config.TestDB{Socket: true, SocketDir: "/c/s"}}
	sockCmd := []string{"-c", "unix_socket_directories=/var/run/postgresql,/run/graft-testdb"}
	for _, c := range []struct {
		db   *DB
		s    State
		want string
	}{
		{tcp, State{Exists: true, Network: "bridge"}, ""},
		{tcp, State{Exists: true, Network: "none"}, "listening (network none, the config asks for 127.0.0.1:55432)"},
		{sock, State{Exists: true, Cmd: sockCmd, Network: "none", Binds: []string{"/c/s:/run/graft-testdb:z"}}, ""},
		{sock, State{Exists: true, Cmd: sockCmd, Network: "bridge"}, "listening (network bridge, the config asks for a unix socket in /c/s)"},
		{sock, State{Exists: true, Cmd: sockCmd, Network: "none", Binds: []string{"/old/s:/run/graft-testdb:z"}},
			"listening (network none, binds /old/s:/run/graft-testdb:z, the config asks for a unix socket in /c/s)"},
	} {
		if got := c.db.Drift(c.s); got != c.want {
			t.Errorf("%+v: drift %q, want %q", c.s, got, c.want)
		}
	}
}

// The image's entrypoint initializes the cluster with a server listening on
// the socket only: readiness is asked over TCP, the way migrate connects.
func TestReadyArgs(t *testing.T) {
	d := &DB{Cfg: &config.TestDB{Container: "c", User: "app", Database: "app_test"}}
	want := []string{"exec", "c", "pg_isready", "-h", "127.0.0.1", "-U", "app", "-d", "app_test"}
	if got := d.readyArgs(); !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestUnifiedPath: the container's cgroup is the 0:: line; a host on cgroup
// v1 has none, which is no error.
func TestUnifiedPath(t *testing.T) {
	got, err := unifiedPath("0::/system.slice/docker-c2.scope\n")
	if err != nil || got != "/system.slice/docker-c2.scope" {
		t.Errorf("v2: %q, %v", got, err)
	}
	if _, err := unifiedPath("12:memory:/docker/c2\n1:name=systemd:/docker/c2\n"); !errors.Is(err, ErrNoCgroup) {
		t.Errorf("v1: %v", err)
	}
}
