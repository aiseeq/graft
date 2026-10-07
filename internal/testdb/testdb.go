// Package testdb manages the disposable PostgreSQL the tests run against: a
// docker container tuned for speed over durability, migrated, and the only
// database a test_db task may be pointed at.
package testdb

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/graft/internal/config"
)

// DB is the configured test database.
type DB struct {
	Cfg *config.TestDB
	Log io.Writer
}

// State is what docker reports about the container.
type State struct {
	Exists  bool
	Running bool
	// Cmd is the postgres command line the container was created with.
	Cmd []string
	// Tmpfs maps the tmpfs mount points of the container to their options.
	Tmpfs map[string]string
}

// Inspect asks docker about the container.
func (d *DB) Inspect(ctx context.Context) (State, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return State{}, fmt.Errorf("docker is needed for the test database: %w", err)
	}
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--type", "container", d.Cfg.Container).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && strings.Contains(string(exitErr.Stderr), "No such") {
			return State{}, nil
		}
		return State{}, fmt.Errorf("docker inspect %s: %w (is the docker daemon running?)", d.Cfg.Container, err)
	}
	var info []struct {
		State      struct{ Running bool }
		Config     struct{ Cmd []string }
		HostConfig struct{ Tmpfs map[string]string }
	}
	if err := json.Unmarshal(out, &info); err != nil || len(info) != 1 {
		return State{}, fmt.Errorf("docker inspect %s: unexpected output", d.Cfg.Container)
	}
	return State{Exists: true, Running: info[0].State.Running, Cmd: info[0].Config.Cmd, Tmpfs: info[0].HostConfig.Tmpfs}, nil
}

// wantArgs are the server arguments the config asks for. The image's
// entrypoint prepends postgres to arguments starting with -.
func (d *DB) wantArgs() []string {
	var args []string
	for _, s := range d.Cfg.Settings {
		args = append(args, "-c", s)
	}
	return args
}

// tmpfsOptions are the mount options of each tmpfs the config asks for.
func (d *DB) tmpfsOptions() string {
	return "rw,size=" + d.Cfg.Tmpfs
}

// Drift describes how the container differs from the config: its server
// settings, whether or not its command spelled out postgres, and its tmpfs.
// It is empty when the container matches or does not exist.
func (d *DB) Drift(s State) string {
	if !s.Exists {
		return ""
	}
	var drift []string
	got := s.Cmd
	if len(got) > 0 && got[0] == "postgres" {
		got = got[1:]
	}
	if !slices.Equal(got, d.wantArgs()) {
		drift = append(drift, "settings ("+strings.Join(s.Cmd, " ")+")")
	}
	if !d.tmpfsMatches(s.Tmpfs) {
		have := "none"
		if len(s.Tmpfs) > 0 {
			mounts := make([]string, 0, len(s.Tmpfs))
			for _, path := range slices.Sorted(maps.Keys(s.Tmpfs)) {
				mounts = append(mounts, path+":"+s.Tmpfs[path])
			}
			have = strings.Join(mounts, " ")
		}
		want := cmp.Or(d.Cfg.Tmpfs, "none")
		drift = append(drift, "tmpfs ("+have+", the config asks for "+want+")")
	}
	return strings.Join(drift, " and ")
}

// tmpfsMatches compares the options only: the mount points are the image's
// volumes, which the config does not name.
func (d *DB) tmpfsMatches(got map[string]string) bool {
	if d.Cfg.Tmpfs == "" {
		return len(got) == 0
	}
	if len(got) == 0 {
		return false
	}
	for _, opts := range got {
		if opts != d.tmpfsOptions() {
			return false
		}
	}
	return true
}

// Up makes sure the container runs, is ready and is migrated.
func (d *DB) Up(ctx context.Context, migrate func(context.Context) error) error {
	s, err := d.Inspect(ctx)
	if err != nil {
		return err
	}
	switch {
	case s.Running:
		fmt.Fprintf(d.Log, "graft: %s is running\n", d.Cfg.Container)
	case s.Exists:
		fmt.Fprintf(d.Log, "graft: starting %s\n", d.Cfg.Container)
		if err := docker(ctx, "start", d.Cfg.Container); err != nil {
			return err
		}
	default:
		fmt.Fprintf(d.Log, "graft: creating %s (%s on 127.0.0.1:%d)\n", d.Cfg.Container, d.Cfg.Image, d.Cfg.Port)
		if err := d.create(ctx); err != nil {
			return err
		}
	}
	if drift := d.Drift(s); drift != "" {
		fmt.Fprintf(d.Log, "graft: warning: %s was created with other %s; graft testdb recreate applies the config\n",
			d.Cfg.Container, drift)
	}
	if err := d.waitReady(ctx); err != nil {
		return err
	}
	// Always, not only after create: a container on tmpfs comes back from a
	// restart with an empty cluster.
	if migrate != nil {
		return migrate(ctx)
	}
	return nil
}

// create runs the container. The variables are passed by name, docker takes
// their values from its own environment: they stay out of the process list.
func (d *DB) create(ctx context.Context) error {
	var volumes []string
	if d.Cfg.Tmpfs != "" {
		var err error
		if volumes, err = d.imageVolumes(ctx); err != nil {
			return err
		}
	}
	cmd := exec.CommandContext(ctx, "docker", d.runArgs(volumes)...)
	cmd.Env = append(os.Environ(), "POSTGRES_USER="+d.Cfg.User, "POSTGRES_PASSWORD="+d.Cfg.Password, "POSTGRES_DB="+d.Cfg.Database)
	return runDocker(cmd)
}

// runArgs is the docker run command line; each of tmpfsVolumes becomes a
// tmpfs, which takes the place of the image volume at that path.
func (d *DB) runArgs(tmpfsVolumes []string) []string {
	args := []string{"run", "-d", "--name", d.Cfg.Container, "--restart", "unless-stopped",
		"-e", "POSTGRES_USER", "-e", "POSTGRES_PASSWORD", "-e", "POSTGRES_DB",
		"-p", "127.0.0.1:" + strconv.Itoa(d.Cfg.Port) + ":5432"}
	for _, v := range tmpfsVolumes {
		args = append(args, "--tmpfs", v+":"+d.tmpfsOptions())
	}
	args = append(args, d.Cfg.Image)
	return append(args, d.wantArgs()...)
}

// imageVolumes are the volumes the image declares: where it keeps its data
// (/var/lib/postgresql since postgres 18, /var/lib/postgresql/data before).
// A tmpfs at any other path would leave the data in a volume on disk.
func (d *DB) imageVolumes(ctx context.Context) ([]string, error) {
	inspect := func() ([]byte, error) {
		return exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{json .Config.Volumes}}", d.Cfg.Image).Output()
	}
	out, err := inspect()
	if err != nil {
		fmt.Fprintf(d.Log, "graft: pulling %s\n", d.Cfg.Image)
		if err := docker(ctx, "pull", d.Cfg.Image); err != nil {
			return nil, err
		}
		if out, err = inspect(); err != nil {
			return nil, fmt.Errorf("docker image inspect %s: %w", d.Cfg.Image, err)
		}
	}
	var volumes map[string]struct{}
	if err := json.Unmarshal(out, &volumes); err != nil {
		return nil, fmt.Errorf("docker image inspect %s: unexpected output %q", d.Cfg.Image, out)
	}
	if len(volumes) == 0 {
		return nil, fmt.Errorf("test_db.tmpfs: image %s declares no volume, graft does not know where its data lives", d.Cfg.Image)
	}
	return slices.Sorted(maps.Keys(volumes)), nil
}

// readyArgs ask the server over TCP: while the image's entrypoint
// initializes a cluster (every start on tmpfs), a temporary server answers
// on the socket only and then stops.
func (d *DB) readyArgs() []string {
	return []string{"exec", d.Cfg.Container, "pg_isready", "-h", "127.0.0.1", "-U", d.Cfg.User, "-d", d.Cfg.Database}
}

func (d *DB) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(d.Cfg.ReadyTimeout)
	for {
		cmd := exec.CommandContext(ctx, "docker", d.readyArgs()...)
		if cmd.Run() == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s is not ready after %s: see docker logs %s", d.Cfg.Container, d.Cfg.ReadyTimeout, d.Cfg.Container)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Down removes the container and its data.
func (d *DB) Down(ctx context.Context) error {
	s, err := d.Inspect(ctx)
	if err != nil {
		return err
	}
	if !s.Exists {
		fmt.Fprintf(d.Log, "graft: %s does not exist\n", d.Cfg.Container)
		return nil
	}
	if err := docker(ctx, "rm", "-f", "-v", d.Cfg.Container); err != nil {
		return err
	}
	fmt.Fprintf(d.Log, "graft: removed %s\n", d.Cfg.Container)
	return nil
}

func docker(ctx context.Context, args ...string) error {
	return runDocker(exec.CommandContext(ctx, "docker", args...))
}

func runDocker(cmd *exec.Cmd) error {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w: %s", cmd.Args[1], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// CheckDSN refuses a DSN that does not point at the test database: tests
// drop and create databases, and a DSN left over from real work must never
// reach them.
func CheckDSN(dsn string, cfg *config.TestDB) error {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return fmt.Errorf("%s is not a postgres:// URL", cfg.DSNVar)
	}
	if db := strings.TrimPrefix(u.Path, "/"); db != cfg.Database {
		return fmt.Errorf("%s points at database %q, not the test database %q", cfg.DSNVar, db, cfg.Database)
	}
	return nil
}
