package app

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/gomods"
	"github.com/aiseeq/graft/v2/internal/lock"
	"github.com/aiseeq/graft/v2/internal/services"
	"github.com/aiseeq/graft/v2/internal/tools"
	"github.com/aiseeq/graft/v2/internal/userconfig"
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
	paths := []string{repoLock(repo)}
	// A worktree's own commit lock (lock: {scope: worktree}); in the main
	// worktree it is the repository lock itself.
	if own := filepath.Join(repo.GitDir, lockFile); own != paths[0] {
		paths = append(paths, own)
	}
	named, err := filepath.Glob(filepath.Join(lockDir(repo), "*.lock"))
	if err != nil {
		return fmt.Errorf("listing named locks: %w", err)
	}
	paths = append(paths, named...)
	dir, err := config.TestDBDir()
	if err != nil {
		return fmt.Errorf("the test database locks: %w", err)
	}
	dbs, err := filepath.Glob(filepath.Join(dir, "*.lock"))
	if err != nil {
		return fmt.Errorf("listing the test database locks: %w", err)
	}
	paths = append(paths, dbs...)
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
	binDir, err := tools.UserBinDir()
	if err != nil {
		return err
	}
	moduleSettings, err := userconfig.ModulesSettings()
	if err != nil {
		return err
	}
	results := make(map[string]tools.Result, len(cfg.Tools))
	var modules []string
	for _, name := range sortedNames(cfg.Tools) {
		t := cfg.Tools[name]
		r, err := tools.Check(ctx, name, t)
		if err != nil {
			return err
		}
		results[name] = r
		if r.LatestModule != "" && !slices.Contains(modules, r.LatestModule) {
			modules = append(modules, r.LatestModule)
		}
	}
	latest := map[string]gomods.LatestModule{}
	if len(modules) > 0 {
		latest, err = gomods.LatestVersions(ctx, a.Dir, modules)
		if err != nil {
			return fmt.Errorf("looking up latest tool versions: %w", err)
		}
	}
	var failed []string
	for _, name := range sortedNames(cfg.Tools) {
		t := cfg.Tools[name]
		r := results[name]
		if module := r.LatestModule; module != "" {
			m := latest[module]
			if m.Version == "" {
				reason, _, _ := strings.Cut(strings.TrimSpace(m.Err), "\n")
				if reason == "" {
					reason = "go list -m gave no version"
				}
				a.warn("%s: the latest version of %s is unknown (%s); the build on PATH is kept", name, module, reason)
			} else {
				tools.CheckLatest(&r, m.Version)
			}
		}
		if r.OK {
			a.printf("%s: ok (%s)", name, r.Path)
			continue
		}
		if t.Manual != "" {
			a.printf("%s: %s (install it yourself: %s)", name, r.Problem, t.Manual)
			failed = append(failed, name)
			continue
		}
		if !install {
			a.printf("%s: %s (graft tools install installs %s)", name, r.Problem, t.GoInstall)
			failed = append(failed, name)
			continue
		}
		a.printf("%s: %s, installing", name, r.Problem)
		installArg := t.InstallArg()
		pkg, _ := t.Pin()
		if moduleSettings.IsLatest(pkg) {
			installArg = pkg + "@latest"
		}
		if err := tools.Install(ctx, name, t, binDir, a.Stderr, installArg); err != nil {
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
