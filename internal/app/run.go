package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/gitx"
	"github.com/aiseeq/graft/internal/lock"
	"github.com/aiseeq/graft/internal/services"
	"github.com/aiseeq/graft/internal/tools"
	"github.com/aiseeq/graft/internal/userconfig"
)

// Run runs a task from .graft.yaml.
func (a *App) Run(ctx context.Context, task string, args []string) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	return a.runner(repo, cfg).Run(ctx, task, args)
}

// IsTask reports whether name is a task of the project in the current
// directory; outside a project nothing is.
func (a *App) IsTask(name string) (bool, error) {
	cfg, found, err := a.ProjectConfig()
	if err != nil || !found {
		return false, err
	}
	_, ok := cfg.Tasks[name]
	return ok, nil
}

func sortedNames[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// ServiceAction is start, stop or restart.
type ServiceAction string

// Service actions.
const (
	Start   ServiceAction = "start"
	Stop    ServiceAction = "stop"
	Restart ServiceAction = "restart"
)

// Service starts, stops or restarts one service, or all of them when name is
// empty (stopping in reverse order).
func (a *App) Service(ctx context.Context, action ServiceAction, name string) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	return a.serviceAction(ctx, services.New(repo.Root, cfg, a.runner(repo, cfg), a.Stdout), action, name)
}

// serviceAction acts on one service, or on all of them when name is empty
// (stopping in reverse order).
func (a *App) serviceAction(ctx context.Context, m *services.Manager, action ServiceAction, name string) error {
	names, err := m.Names(name)
	if err != nil {
		return err
	}
	if action == Stop {
		slices.Reverse(names)
	}
	for _, n := range names {
		switch action {
		case Start:
			err = m.Start(ctx, n)
		case Stop:
			err = m.Stop(ctx, n)
		case Restart:
			err = m.Restart(ctx, n)
		default:
			return fmt.Errorf("unknown service action %q", action)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ServiceLogs prints the last lines of a service's log.
func (a *App) ServiceLogs(ctx context.Context, name string, lines int, follow bool) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	m := services.New(repo.Root, cfg, a.runner(repo, cfg), a.Stdout)
	if _, err := m.Names(name); err != nil {
		return err
	}
	return m.Logs(ctx, name, lines, follow, a.Stdout)
}

// Status shows the services, the test database and who holds graft's locks.
func (a *App) Status(ctx context.Context) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	if len(cfg.Services) > 0 {
		m := services.New(repo.Root, cfg, a.runner(repo, cfg), a.Stdout)
		for _, name := range sortedNames(cfg.Services) {
			st, err := m.State(ctx, name)
			if err != nil {
				return err
			}
			a.printf("%s", describeService(st))
		}
	}
	if cfg.TestDB != nil {
		if err := a.TestDB(ctx, "status"); err != nil {
			return err
		}
	}
	return a.Locks()
}

func describeService(st services.State) string {
	var b strings.Builder
	if st.PID == 0 {
		fmt.Fprintf(&b, "%s: stopped", st.Name)
	} else {
		fmt.Fprintf(&b, "%s: running (pid %d)", st.Name, st.PID)
	}
	if st.Addr != "" {
		state := "free"
		if st.Listening {
			state = "listening"
		}
		fmt.Fprintf(&b, ", %s %s", st.Addr, state)
	}
	fmt.Fprintf(&b, ", log %s", st.Log)
	return b.String()
}

// Locks shows the held locks: the repository lock and the named ones.
func (a *App) Locks() error {
	repo, err := gitx.Open(a.Dir)
	if err != nil {
		return err
	}
	paths := []string{filepath.Join(repo.CommonDir, lockFile)}
	named, err := filepath.Glob(filepath.Join(lockDir(repo), "*.lock"))
	if err != nil {
		return err
	}
	paths = append(paths, named...)
	held := 0
	for _, p := range paths {
		holders, err := lock.Holders(p)
		if err != nil {
			return err
		}
		for _, h := range holders {
			a.printf("lock %s held by %s", strings.TrimSuffix(filepath.Base(p), ".lock"), h)
			held++
		}
	}
	if held == 0 {
		a.printf("no locks held")
	}
	return nil
}

// Tools checks the pinned tools; with install it installs those that fail.
func (a *App) Tools(ctx context.Context, install bool) error {
	_, cfg, err := a.open()
	if err != nil {
		return err
	}
	if len(cfg.Tools) == 0 {
		a.printf("no tools in .graft.yaml")
		return nil
	}
	return a.checkTools(ctx, cfg, install)
}

func (a *App) checkTools(ctx context.Context, cfg *config.Config, install bool) error {
	binDir, err := toolsBinDir()
	if err != nil {
		return err
	}
	var failed []string
	for _, name := range sortedNames(cfg.Tools) {
		t := cfg.Tools[name]
		r, err := tools.Check(ctx, name, t)
		if err != nil {
			return err
		}
		if r.OK {
			a.printf("%s: ok (%s)", name, r.Path)
			continue
		}
		if !install {
			a.printf("%s: %s (graft tools install installs %s)", name, r.Problem, t.GoInstall)
			failed = append(failed, name)
			continue
		}
		a.printf("%s: %s, installing", name, r.Problem)
		if err := tools.Install(ctx, name, t, binDir, a.Stderr); err != nil {
			fmt.Fprintln(a.Stderr, "graft:", err)
			failed = append(failed, name)
			continue
		}
		a.printf("%s: installed", name)
	}
	if len(failed) > 0 {
		return fmt.Errorf("tools not ready: %s", strings.Join(failed, ", "))
	}
	return nil
}

// toolsBinDir is tools.bin_dir from the user config; empty without one.
func toolsBinDir() (string, error) {
	ucfg, _, err := userconfig.Load()
	if errors.Is(err, userconfig.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return ucfg.Tools.BinDir, nil
}
