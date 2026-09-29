// Package testdb manages the disposable PostgreSQL the tests run against: a
// docker container tuned for speed over durability, migrated, and the only
// database a test_db task may be pointed at.
package testdb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
		State  struct{ Running bool }
		Config struct{ Cmd []string }
	}
	if err := json.Unmarshal(out, &info); err != nil || len(info) != 1 {
		return State{}, fmt.Errorf("docker inspect %s: unexpected output", d.Cfg.Container)
	}
	return State{Exists: true, Running: info[0].State.Running, Cmd: info[0].Config.Cmd}, nil
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

// Drifted reports whether the container was created with other settings,
// whether or not its command spelled out postgres.
func (d *DB) Drifted(s State) bool {
	if !s.Exists {
		return false
	}
	got := s.Cmd
	if len(got) > 0 && got[0] == "postgres" {
		got = got[1:]
	}
	return !slices.Equal(got, d.wantArgs())
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
	if d.Drifted(s) {
		fmt.Fprintf(d.Log, "graft: warning: %s was created with other settings (%s); graft testdb recreate applies the config\n",
			d.Cfg.Container, strings.Join(s.Cmd, " "))
	}
	if err := d.waitReady(ctx); err != nil {
		return err
	}
	if migrate != nil {
		return migrate(ctx)
	}
	return nil
}

// create runs the container. The variables are passed by name, docker takes
// their values from its own environment: they stay out of the process list.
func (d *DB) create(ctx context.Context) error {
	args := []string{"run", "-d", "--name", d.Cfg.Container, "--restart", "unless-stopped",
		"-e", "POSTGRES_USER", "-e", "POSTGRES_PASSWORD", "-e", "POSTGRES_DB",
		"-p", "127.0.0.1:" + strconv.Itoa(d.Cfg.Port) + ":5432", d.Cfg.Image}
	cmd := exec.CommandContext(ctx, "docker", append(args, d.wantArgs()...)...)
	cmd.Env = append(os.Environ(), "POSTGRES_USER="+d.Cfg.User, "POSTGRES_PASSWORD="+d.Cfg.Password, "POSTGRES_DB="+d.Cfg.Database)
	return runDocker(cmd)
}

func (d *DB) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(d.Cfg.ReadyTimeout)
	for {
		cmd := exec.CommandContext(ctx, "docker", "exec", d.Cfg.Container, "pg_isready", "-U", d.Cfg.User, "-d", d.Cfg.Database)
		if cmd.Run() == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s is not ready after %s: see docker logs %s", d.Cfg.Container, d.Cfg.ReadyTimeout, d.Cfg.Container)
		}
		time.Sleep(500 * time.Millisecond)
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
