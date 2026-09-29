// Package services starts and stops the project's long-running local
// processes: detached, logged to a file and tracked by a pid file.
package services

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/dotenv"
	"github.com/aiseeq/graft/internal/proc"
	"github.com/aiseeq/graft/internal/tasks"
)

// Manager runs the services of one project.
type Manager struct {
	Root   string
	Config *config.Config
	// Runner builds services and holds their locks.
	Runner *tasks.Runner
	Log    io.Writer
	lookup *dotenv.Lookup
}

// New prepares a manager.
func New(root string, cfg *config.Config, runner *tasks.Runner, log io.Writer) *Manager {
	return &Manager{Root: root, Config: cfg, Runner: runner, Log: log, lookup: dotenv.NewLookup(filepath.Join(root, cfg.DotEnv))}
}

// Names resolves the services a command acts on: the one named, or all.
func (m *Manager) Names(name string) ([]string, error) {
	if len(m.Config.Services) == 0 {
		return nil, errors.New("no services in .graft.yaml")
	}
	if name == "" {
		names := make([]string, 0, len(m.Config.Services))
		for n := range m.Config.Services {
			names = append(names, n)
		}
		slices.Sort(names)
		return names, nil
	}
	if _, ok := m.Config.Services[name]; !ok {
		return nil, fmt.Errorf("unknown service %q", name)
	}
	return []string{name}, nil
}

// State is what is known about a service.
type State struct {
	Name string
	// PID is the running process, 0 when the service is stopped.
	PID  int
	Addr string
	// Listening is whether Addr accepts connections.
	Listening bool
	Log       string
}

// State reports the state of a service, clearing a stale pid file.
func (m *Manager) State(name string) (State, error) {
	s := m.Config.Services[name]
	st := State{Name: name, Log: s.Log}
	pid, err := m.running(name)
	if err != nil {
		return st, err
	}
	st.PID = pid
	if s.Addr != "" {
		addr, err := m.addr(s)
		if err != nil {
			return st, err
		}
		st.Addr = addr
		if st.Listening, err = listening(addr); err != nil {
			return st, err
		}
	}
	return st, nil
}

// Start starts a service unless it runs, and waits until it is ready: its
// address accepts connections, or, without an address, it is still alive
// after a second.
func (m *Manager) Start(ctx context.Context, name string) error {
	return m.locked(ctx, name, func() error { return m.start(ctx, name) })
}

// Stop stops a service: SIGTERM to its process group, SIGKILL after
// stop_timeout, then waiting for its address to be released.
func (m *Manager) Stop(ctx context.Context, name string) error {
	return m.locked(ctx, name, func() error { return m.stop(ctx, name) })
}

// Restart stops a service if it runs and starts it.
func (m *Manager) Restart(ctx context.Context, name string) error {
	return m.locked(ctx, name, func() error {
		if err := m.stop(ctx, name); err != nil {
			return err
		}
		return m.start(ctx, name)
	})
}

// locked holds the service's own lock, so two sessions never start it twice,
// and the lock the config names, so builds and tests can wait for it.
func (m *Manager) locked(ctx context.Context, name string, fn func() error) error {
	locks := []*config.TaskLock{{Name: "service-" + name, Mode: "write"}}
	if l := m.Config.Services[name].Lock; l != "" {
		locks = append(locks, &config.TaskLock{Name: l, Mode: "write"})
	}
	var releases []func() error
	releaseAll := func(err error) error {
		for _, release := range slices.Backward(releases) {
			err = errors.Join(err, release())
		}
		return err
	}
	for _, l := range locks {
		release, err := m.Runner.Hold(ctx, "service "+name, l)
		if err != nil {
			return releaseAll(err)
		}
		releases = append(releases, release)
	}
	return releaseAll(fn())
}

func (m *Manager) start(ctx context.Context, name string) error {
	s := m.Config.Services[name]
	pid, err := m.running(name)
	if err != nil {
		return err
	}
	if pid != 0 {
		fmt.Fprintf(m.Log, "graft: %s is already running (pid %d)\n", name, pid)
		return nil
	}
	var addr string
	if s.Addr != "" {
		if addr, err = m.addr(s); err != nil {
			return err
		}
		busy, err := listening(addr)
		if err != nil {
			return err
		}
		if busy {
			return fmt.Errorf("%s: %s is already in use by a process graft did not start", name, addr)
		}
	}
	if s.Build != "" {
		if err := m.Runner.Run(ctx, s.Build, nil); err != nil {
			return fmt.Errorf("%s: build: %w", name, err)
		}
	}
	cmd, logFile, err := m.command(name, s)
	if err != nil {
		return err
	}
	defer logFile.Close()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: starting %q: %w", name, s.Run.Source, err)
	}
	pid = cmd.Process.Pid
	if err := writePID(m.path(s.PIDFile), pid); err != nil {
		return errors.Join(fmt.Errorf("%s: %w", name, err), proc.Kill(pid))
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	if err := m.waitStarted(ctx, s, addr, exited); err != nil {
		stopErr := proc.Kill(pid)
		return errors.Join(fmt.Errorf("%s: %w\n%s", name, err, tail(m.path(s.Log), 20)), stopErr, removePID(m.path(s.PIDFile)))
	}
	fmt.Fprintf(m.Log, "graft: started %s (pid %d), log %s\n", name, pid, s.Log)
	return nil
}

// command prepares the detached process with its log and environment.
func (m *Manager) command(name string, s *config.Service) (*exec.Cmd, *os.File, error) {
	argv, err := dotenv.ExpandAll(s.Run.Argv, m.lookup)
	if err != nil {
		return nil, nil, fmt.Errorf("%s.run: %w", name, err)
	}
	env, err := m.env(s)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", name, err)
	}
	for _, p := range []string{s.Log, s.PIDFile} {
		if err := os.MkdirAll(filepath.Dir(m.path(p)), 0o755); err != nil {
			return nil, nil, err
		}
	}
	logFile, err := os.OpenFile(m.path(s.Log), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, err
	}
	fmt.Fprintf(logFile, "--- graft start %s at %s ---\n", name, time.Now().Format(time.RFC3339))
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = m.Root, env
	cmd.Stdout, cmd.Stderr = logFile, logFile
	proc.Detach(cmd)
	return cmd, logFile, nil
}

// aliveGrace is how long a service without an address has to survive to be
// considered started: bad config usually kills a process sooner.
const aliveGrace = time.Second

func (m *Manager) waitStarted(ctx context.Context, s *config.Service, addr string, exited <-chan error) error {
	if addr == "" {
		select {
		case err := <-exited:
			return fmt.Errorf("exited at start: %w", err)
		case <-time.After(min(aliveGrace, s.StartTimeout)):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	deadline := time.After(s.StartTimeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-exited:
			return fmt.Errorf("exited before listening on %s: %w", addr, err)
		case <-deadline:
			return fmt.Errorf("not listening on %s after %s", addr, s.StartTimeout)
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if up, err := listening(addr); up || err != nil {
				return err
			}
		}
	}
}

func (m *Manager) stop(ctx context.Context, name string) error {
	s := m.Config.Services[name]
	pid, err := m.running(name)
	if err != nil {
		return err
	}
	if pid == 0 {
		fmt.Fprintf(m.Log, "graft: %s is not running\n", name)
		return nil
	}
	if err := proc.Terminate(pid); err != nil {
		return err
	}
	gone, err := waitFor(ctx, s.StopTimeout, func() (bool, error) { return dead(pid) })
	if err != nil {
		return err
	}
	if !gone {
		fmt.Fprintf(m.Log, "graft: %s did not stop in %s, killing it\n", name, s.StopTimeout)
		if err := proc.Kill(pid); err != nil {
			return err
		}
		if gone, err = waitFor(ctx, s.StopTimeout, func() (bool, error) { return dead(pid) }); err != nil {
			return err
		}
		if !gone {
			return fmt.Errorf("%s: process %d survived SIGKILL", name, pid)
		}
	}
	if err := removePID(m.path(s.PIDFile)); err != nil {
		return err
	}
	if s.Addr != "" {
		addr, err := m.addr(s)
		if err != nil {
			return err
		}
		released, err := waitFor(ctx, s.StopTimeout, func() (bool, error) {
			up, err := listening(addr)
			return !up, err
		})
		if err != nil {
			return err
		}
		if !released {
			return fmt.Errorf("%s stopped, but %s is still in use (a child it left behind?)", name, addr)
		}
	}
	fmt.Fprintf(m.Log, "graft: stopped %s\n", name)
	return nil
}

// running returns the pid of the running service or 0. A pid file naming a
// dead process, or a process that is now another program, is removed.
func (m *Manager) running(name string) (int, error) {
	s := m.Config.Services[name]
	path := m.path(s.PIDFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("%s: not a pid: %q", s.PIDFile, strings.TrimSpace(string(data)))
	}
	alive, err := proc.Alive(pid)
	if err != nil {
		return 0, err
	}
	if alive {
		same, err := m.isService(s, pid)
		if err != nil {
			return 0, err
		}
		if same {
			return pid, nil
		}
	}
	fmt.Fprintf(m.Log, "graft: %s: removing stale pid file (process %d is gone)\n", name, pid)
	return 0, removePID(path)
}

// isService guards against pid reuse where the platform can tell: the
// process must still run the service's program.
func (m *Manager) isService(s *config.Service, pid int) (bool, error) {
	cmdline, err := proc.Cmdline(pid)
	if errors.Is(err, errors.ErrUnsupported) {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil // exited meanwhile
	}
	if err != nil {
		return false, err
	}
	argv0, err := dotenv.Expand(s.Run.Argv[0], m.lookup)
	if err != nil {
		return false, err
	}
	// A script started through its #! line shows as [interpreter, (arg),
	// script]: the program may be any of the first three words.
	return slices.Contains(cmdline[:min(3, len(cmdline))], argv0), nil
}

func dead(pid int) (bool, error) {
	alive, err := proc.Alive(pid)
	return !alive, err
}

// env is the base environment plus the service's .env keys and variables.
func (m *Manager) env(s *config.Service) ([]string, error) {
	env, err := tasks.BaseEnv()
	if err != nil {
		return nil, err
	}
	keys := s.DotEnv.Keys
	if s.DotEnv.All {
		f, err := dotenv.Load(filepath.Join(m.Root, m.Config.DotEnv))
		if err != nil {
			return nil, fmt.Errorf("dotenv: all: %w", err)
		}
		keys = f.Keys()
	}
	values, err := m.lookup.Values(keys)
	if err != nil {
		return nil, err
	}
	env = append(env, values...)
	names := make([]string, 0, len(s.Env))
	for k := range s.Env {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		v, err := dotenv.Expand(s.Env[k], m.lookup)
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", k, err)
		}
		env = append(env, k+"="+v)
	}
	return env, nil
}

func (m *Manager) addr(s *config.Service) (string, error) {
	addr, err := dotenv.Expand(s.Addr, m.lookup)
	if err != nil {
		return "", fmt.Errorf("addr: %w", err)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("addr %q: %w", addr, err)
	}
	if host == "" {
		host = "localhost"
	}
	return net.JoinHostPort(host, port), nil
}

func (m *Manager) path(p string) string {
	return filepath.Join(m.Root, filepath.FromSlash(p))
}

// listening reports whether addr accepts connections. Only a refused
// connection means no: a timeout or a name that does not resolve is an
// error, not a free port.
func listening(addr string) (bool, error) {
	c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if proc.ConnRefused(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("probing %s: %w", addr, err)
	}
	return true, c.Close()
}

func waitFor(ctx context.Context, timeout time.Duration, done func() (bool, error)) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := done()
		if err != nil || ok {
			return ok, err
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func writePID(path string, pid int) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func removePID(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// tail returns the last n lines of a log for an error message.
func tail(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return "(log unreadable: " + err.Error() + ")"
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	return "last lines of " + path + ":\n" + strings.Join(lines, "\n")
}
