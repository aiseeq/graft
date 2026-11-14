// Package services starts and stops the project's long-running local
// processes: detached, logged to a file and tracked by a pid file.
package services

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	akitproc "github.com/aiseeq/akit/proc"
	"github.com/aiseeq/graft/v2/internal/atomicfile"
	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/dotenv"
	"github.com/aiseeq/graft/v2/internal/execx"
	"github.com/aiseeq/graft/v2/internal/fsx"
	"github.com/aiseeq/graft/v2/internal/proc"
	"github.com/aiseeq/graft/v2/internal/tasks"
)

var readServiceStat = akitproc.ReadStat

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
		return slices.Sorted(maps.Keys(m.Config.Services)), nil
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
	// Log is the log file, or the journalctl command of a systemd unit.
	Log string
}

// State reports the state of a service, clearing a stale pid file.
func (m *Manager) State(ctx context.Context, name string) (State, error) {
	s := m.Config.Services[name]
	st := State{Name: name, Log: s.Log}
	var pid int
	var err error
	if s.SystemdUnit != "" {
		st.Log = "journalctl --user -u " + s.SystemdUnit
		pid, _, err = unitState(ctx, s.SystemdUnit)
	} else {
		pid, err = m.running(name)
	}
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
	if s.SystemdUnit != "" {
		return m.startUnit(ctx, name, s)
	}
	pid, err := m.running(name)
	if err != nil {
		return err
	}
	if pid != 0 {
		fmt.Fprintf(m.Log, "graft: %s is already running (pid %d)\n", name, pid)
		return nil
	}
	addr, err := m.prepareStart(ctx, name, s)
	if err != nil {
		return err
	}
	cmd, logFile, err := m.command(name, s)
	if err != nil {
		return err
	}
	defer logFile.Close()
	if err := akitproc.BecomeChildSubreaper(); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		return fmt.Errorf("%s: becoming a child subreaper: %w", name, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: starting %q: %w", name, s.Run.Source, err)
	}
	pid = cmd.Process.Pid
	stat, statErr := readServiceStat("/proc", pid)
	if statErr != nil && !errors.Is(statErr, errors.ErrUnsupported) {
		fmt.Fprintf(m.Log, "graft: %s: warning: cannot read process start time: %v; recording PID only\n", name, statErr)
	}
	start := ""
	if statErr == nil {
		start = stat.Start
	}
	if err := writePID(m.path(s.PIDFile), pid, start); err != nil {
		_, stopErr := akitproc.StopCmd(cmd, time.Second, syscall.SIGTERM)
		waitErr := cmd.Wait()
		return errors.Join(fmt.Errorf("%s: %w", name, err), stopErr, waitErr)
	}
	waitResult := make(chan error, 1)
	if start == "" {
		go func() { waitResult <- cmd.Wait() }()
	}
	if err := m.waitStarted(ctx, s, addr, pid, start, waitResult); err != nil {
		_, stopErr := akitproc.StopCmd(cmd, time.Second, syscall.SIGTERM)
		var waitErr error
		if start == "" {
			waitErr = <-waitResult
		} else {
			waitErr = cmd.Wait()
		}
		return errors.Join(fmt.Errorf("%s: %w\n%s", name, err, tail(m.path(s.Log), 20)), stopErr, waitErr, fsx.Remove(m.path(s.PIDFile)), fsx.Remove(m.path(s.PIDFile)+".start"))
	}
	if start != "" {
		go func() { waitResult <- cmd.Wait() }()
	}
	go func() {
		if waitErr := <-waitResult; waitErr != nil {
			fmt.Fprintf(m.Log, "graft: service %s exited: %v\n", name, waitErr)
		}
	}()
	fmt.Fprintf(m.Log, "graft: started %s (pid %d), log %s\n", name, pid, s.Log)
	return nil
}

// prepareStart refuses an address something else listens on and runs the
// build task; it returns the address to wait for, empty without one.
func (m *Manager) prepareStart(ctx context.Context, name string, s *config.Service) (string, error) {
	var addr string
	if s.Addr != "" {
		var err error
		if addr, err = m.addr(s); err != nil {
			return "", err
		}
		busy, err := listening(addr)
		if err != nil {
			return "", err
		}
		if busy {
			return "", fmt.Errorf("%s: %s is already in use by a process graft did not start", name, addr)
		}
	}
	if s.Build != "" {
		if err := m.Runner.Run(ctx, s.Build, nil); err != nil {
			return "", fmt.Errorf("%s: build: %w", name, err)
		}
	}
	return addr, nil
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
			return nil, nil, fmt.Errorf("%s: creating the directory of %s: %w", name, p, err)
		}
	}
	logFile, err := os.OpenFile(m.path(s.Log), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: opening the log: %w", name, err)
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

func (m *Manager) waitStarted(ctx context.Context, s *config.Service, addr string, pid int, start string, waitResult chan error) error {
	deadline := time.NewTimer(min(aliveGrace, s.StartTimeout))
	if addr != "" {
		deadline.Stop()
		deadline.Reset(s.StartTimeout)
	}
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	// Without a start time Wait runs already: its end is the exit, seen at
	// once, as before. With one the start time tells, checked on each tick
	// and once more at the deadline, so a timeout shorter than a tick still
	// sees an early exit.
	var waited <-chan error
	if start == "" {
		waited = waitResult
	}
	gone := func() error {
		if start == "" {
			return nil
		}
		exited, err := serviceExited(pid, start)
		if err != nil {
			return err
		}
		if exited {
			return serviceStartExitError(addr)
		}
		return nil
	}
	for {
		select {
		case waitErr := <-waited:
			waitResult <- waitErr
			return fmt.Errorf("%w: %v", serviceStartExitError(addr), waitErr)
		case <-deadline.C:
			if err := gone(); err != nil {
				return err
			}
			if addr == "" {
				return nil
			}
			return fmt.Errorf("not listening on %s after %s", addr, s.StartTimeout)
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if err := gone(); err != nil {
				return err
			}
			if addr != "" {
				up, err := listening(addr)
				if err != nil {
					return err
				}
				if up {
					return nil
				}
			}
		}
	}
}

func serviceStartExitError(addr string) error {
	if addr == "" {
		return fmt.Errorf("exited at start")
	}
	return fmt.Errorf("exited before listening on %s", addr)
}

func serviceExited(pid int, start string) (bool, error) {
	stat, err := readServiceStat("/proc", pid)
	if akitproc.Gone(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking process %d: %w", pid, err)
	}
	return stat.Start != start || stat.State == "Z" || stat.State == "X", nil
}

func (m *Manager) stop(ctx context.Context, name string) error {
	s := m.Config.Services[name]
	if s.SystemdUnit != "" {
		return m.stopUnit(ctx, name, s)
	}
	if handled, err := m.removeGoneLeader(name, s); handled {
		return err
	}
	pid, err := m.running(name)
	if err != nil {
		return err
	}
	if pid == 0 {
		fmt.Fprintf(m.Log, "graft: %s is not running\n", name)
		return nil
	}
	start, err := m.serviceStartRecord(name, s)
	if err != nil {
		return err
	}
	result, err := m.stopSession(ctx, name, pid, start, s)
	if err != nil {
		return err
	}
	if result.Degraded {
		fmt.Fprintf(m.Log, "graft: %s: session processes were not stopped because pidfd_open is unavailable\n", name)
	}
	if result.Killed > 0 {
		fmt.Fprintf(m.Log, "graft: %s did not stop in %s, killing it\n", name, s.StopTimeout)
	}
	gone, err := waitFor(ctx, s.StopTimeout, func() (bool, error) { return dead(pid) })
	if err != nil {
		return err
	}
	if !gone {
		return fmt.Errorf("%s: process %d survived session stop", name, pid)
	}
	if err := errors.Join(fsx.Remove(m.path(s.PIDFile)), fsx.Remove(m.path(s.PIDFile)+".start")); err != nil {
		return err
	}
	return m.stopped(ctx, name, s)
}

func (m *Manager) removeGoneLeader(name string, s *config.Service) (bool, error) {
	data, err := os.ReadFile(m.path(s.PIDFile))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading the pid file: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return false, fmt.Errorf("%s: not a pid: %q", s.PIDFile, strings.TrimSpace(string(data)))
	}
	startData, err := os.ReadFile(m.path(s.PIDFile) + ".start")
	if errors.Is(err, os.ErrNotExist) {
		members, scanErr := sessionMembers(pid)
		if scanErr != nil || len(members) == 0 {
			return false, scanErr
		}
		fmt.Fprintf(m.Log, "graft: %s: session leader %d is gone; remaining session processes were not stopped: %v; inspect SID %d and stop them manually\n", name, pid, members, pid)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading the process start time: %w", err)
	}
	start := strings.TrimSpace(string(startData))
	if start == "" {
		return false, nil
	}
	exited, err := serviceExited(pid, start)
	if err != nil {
		return false, err
	}
	if !exited {
		return false, nil
	}
	fmt.Fprintf(m.Log, "graft: %s: session leader %d is gone; remaining session processes were not stopped; inspect SID %d and stop them manually\n", name, pid, pid)
	removeErr := errors.Join(fsx.Remove(m.path(s.PIDFile)), fsx.Remove(m.path(s.PIDFile)+".start"))
	return true, errors.Join(fmt.Errorf("%s: session leader %d is gone; remaining session processes were not stopped", name, pid), removeErr)
}

func (m *Manager) serviceStartRecord(name string, s *config.Service) (string, error) {
	data, err := os.ReadFile(m.path(s.PIDFile) + ".start")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%s: pid file has no process start time; stop the service manually, then remove %s", name, m.path(s.PIDFile))
		}
		return "", fmt.Errorf("%s: reading the process start time: %w", name, err)
	}
	start := strings.TrimSpace(string(data))
	if start == "" {
		return "", fmt.Errorf("%s: pid file has no process start time; stop the service manually, then remove %s", name, m.path(s.PIDFile))
	}
	return start, nil
}

func (m *Manager) stopSession(ctx context.Context, name string, pid int, start string, s *config.Service) (akitproc.StopResult, error) {
	result, err := akitproc.StopSession(akitproc.SessionRecord{Pid: pid, Start: start}, s.StopTimeout, syscall.SIGTERM)
	if errors.Is(err, akitproc.ErrNoStartRecord) {
		return result, fmt.Errorf("%s: pid file has no valid process start time; stop the service manually, then remove %s", name, m.path(s.PIDFile))
	}
	if errors.Is(err, akitproc.ErrSessionLeaderGone) {
		gone, waitErr := waitFor(ctx, s.StopTimeout, func() (bool, error) {
			members, scanErr := sessionMembers(pid)
			return len(members) == 0, scanErr
		})
		if waitErr != nil {
			return result, fmt.Errorf("%s: checking remaining session processes: %w", name, waitErr)
		}
		if !gone {
			members, scanErr := sessionMembers(pid)
			if scanErr != nil {
				return result, scanErr
			}
			fmt.Fprintf(m.Log, "graft: %s: session leader %d is gone; remaining session processes were not stopped: %v; inspect SID %d and stop them manually\n", name, pid, members, pid)
			return result, fmt.Errorf("%s: session leader %d is gone; remaining session processes were not stopped: %v; stop them manually", name, pid, members)
		}
		err = nil
	}
	if errors.Is(err, akitproc.ErrPidfdUnavailable) {
		fmt.Fprintf(m.Log, "graft: %s: session processes were not stopped because pidfd_open is unavailable\n", name)
	}
	if err != nil {
		return result, fmt.Errorf("%s: stopping session: %w", name, err)
	}
	return result, nil
}

// stopped waits for the service's address to be released and reports.
func (m *Manager) stopped(ctx context.Context, name string, s *config.Service) error {
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

// Logs prints the last lines of a service's log; with follow it goes on
// printing what is appended until ctx ends (Ctrl-C).
func (m *Manager) Logs(ctx context.Context, name string, lines int, follow bool, w io.Writer) error {
	s := m.Config.Services[name]
	if s.SystemdUnit != "" {
		args := []string{"--user", "-u", s.SystemdUnit, "-n", strconv.Itoa(lines), "--no-pager"}
		if follow {
			args = append(args, "-f")
		}
		cmd, err := execx.CommandContext(ctx, execx.ShortGrace, "journalctl", args...)
		if err != nil {
			return err
		}
		cmd.Stdout, cmd.Stderr = w, w
		if err := cmd.Run(); err != nil && ctx.Err() == nil {
			return fmt.Errorf("journalctl --user -u %s: %w", s.SystemdUnit, err)
		}
		return nil
	}
	path := m.path(s.Log)
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("service log: %w", err)
	}
	text, err := lastLines(path, lines)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, text); err != nil {
		return fmt.Errorf("printing the service log: %w", err)
	}
	if !follow {
		return nil
	}
	return followFile(ctx, path, info.Size(), w)
}

// followFile copies what is appended to path after offset until ctx ends.
// A file that shrank was truncated or replaced: it is read from the start.
func followFile(ctx context.Context, path string, offset int64, w io.Writer) error {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("following the log: %w", err)
		}
		if info.Size() < offset {
			offset = 0
		}
		if info.Size() > offset {
			n, err := copyFrom(path, offset, w)
			if err != nil {
				return err
			}
			offset += n
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func copyFrom(path string, offset int64, w io.Writer) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("reading the log: %w", err)
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("reading the log: %w", err)
	}
	return io.Copy(w, f)
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
		return 0, fmt.Errorf("%s: reading the pid file: %w", name, err)
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, fmt.Errorf("%s: not a pid: %q", s.PIDFile, strings.TrimSpace(string(data)))
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
	return 0, errors.Join(fsx.Remove(path), fsx.Remove(path+".start"))
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

func sessionMembers(sid int) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("reading /proc: %w", err)
	}
	var members []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == sid {
			continue
		}
		stat, err := readServiceStat("/proc", pid)
		if akitproc.Gone(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading process %d: %w", pid, err)
		}
		if stat.Sid != sid || stat.State == "Z" || stat.State == "X" {
			continue
		}
		members = append(members, pid)
	}
	slices.Sort(members)
	return members, nil
}

func dead(pid int) (bool, error) {
	stat, err := readServiceStat("/proc", pid)
	if akitproc.Gone(err) {
		return true, nil
	}
	if err == nil {
		return stat.State == "Z" || stat.State == "X", nil
	}
	if errors.Is(err, errors.ErrUnsupported) {
		alive, aliveErr := proc.Alive(pid)
		return !alive, aliveErr
	}
	return false, err
}

// env is the base environment plus the service's .env keys and variables.
func (m *Manager) env(s *config.Service) ([]string, error) {
	env, err := tasks.BaseEnv()
	if err != nil {
		return nil, err
	}
	keys := s.Keys
	if s.DotEnv.All {
		f, err := dotenv.Load(filepath.Join(m.Root, m.Config.DotEnv))
		if err != nil {
			return nil, fmt.Errorf("dotenv: all: %w", err)
		}
		keys = nil
		for _, k := range f.Keys() {
			keys = append(keys, config.DotEnvKey{Name: k})
		}
	}
	values, err := tasks.KeyValues(m.lookup, keys)
	if err != nil {
		return nil, err
	}
	env = append(env, values...)
	names := slices.Sorted(maps.Keys(s.Env))
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

func writePID(path string, pid int, start string) error {
	if err := atomicfile.Write(path, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		return fmt.Errorf("writing the pid file: %w", err)
	}
	if start != "" {
		if err := atomicfile.Write(path+".start", []byte(start+"\n"), 0o644); err != nil {
			return errors.Join(fmt.Errorf("writing the process start time: %w", err), fsx.Remove(path))
		}
	} else {
		if err := fsx.Remove(path + ".start"); err != nil {
			return err
		}
	}
	return nil
}

// tail returns the last n lines of a log for an error message.
func tail(path string, n int) string {
	text, err := lastLines(path, n)
	if err != nil {
		return "(log unreadable: " + err.Error() + ")"
	}
	return "last lines of " + path + ":\n" + text
}

// lastLines returns the last n lines of a file.
func lastLines(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("reading the log: %w", err)
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
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("reading the log %s: %w", path, err)
	}
	if len(lines) == 0 {
		return "", nil
	}
	return strings.Join(lines, "\n") + "\n", nil
}
