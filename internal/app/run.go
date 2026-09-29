package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/gitx"
	"github.com/aiseeq/graft/internal/lock"
	"github.com/aiseeq/graft/internal/services"
	"github.com/aiseeq/graft/internal/tools"
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
	_, cfg, err := a.open()
	if errors.Is(err, gitx.ErrNotRepo) || errors.Is(err, config.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, ok := cfg.Tasks[name]
	return ok, nil
}

// ProjectHelp lists the project's tasks and services. Outside a project it
// prints nothing: the built-in usage is all there is.
func (a *App) ProjectHelp() error {
	_, cfg, err := a.open()
	if errors.Is(err, gitx.ErrNotRepo) || errors.Is(err, config.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	if len(cfg.Tasks) > 0 {
		fmt.Fprintln(w, "\nTasks (graft <task> or graft run <task> [-- args]):")
		for _, name := range sortedNames(cfg.Tasks) {
			// A task without desc is a building block of others.
			if desc := cfg.Tasks[name].Desc; desc != "" {
				fmt.Fprintf(w, "  %s\t%s\n", name, desc)
			}
		}
	}
	if len(cfg.Services) > 0 {
		fmt.Fprintln(w, "\nServices (graft start|stop|restart [service]):")
		for _, name := range sortedNames(cfg.Services) {
			fmt.Fprintf(w, "  %s\t%s\n", name, cfg.Services[name].Desc)
		}
	}
	return w.Flush()
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
	m := services.New(repo.Root, cfg, a.runner(repo, cfg), a.Stdout)
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

// Status shows the services, the test database and who holds graft's locks.
func (a *App) Status(ctx context.Context) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	if len(cfg.Services) > 0 {
		m := services.New(repo.Root, cfg, a.runner(repo, cfg), a.Stdout)
		for _, name := range sortedNames(cfg.Services) {
			st, err := m.State(name)
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
		if err := tools.Install(ctx, name, t, a.Stderr); err != nil {
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
