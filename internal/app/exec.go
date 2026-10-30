package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/tasks"
	"github.com/aiseeq/graft/v2/internal/userconfig"
)

// Exec runs argv in the directory graft started in, a command that is not a
// task of the project (a one-off go test, build or e2e run): heavy, it waits
// for the machine in the queue of heavy tasks and gets {threads} and
// GRAFT_THREADS from the user config's load.threads. The stats record it as
// the task exec of the work tree around the directory, or of the directory
// outside one. A heavy one in a project with remote_heavy runs on its host
// first, as a heavy task's steps do. The command's exit status is graft's;
// its own failures exit as env's do (ExitGraft, ExitCannotRun,
// ExitNotFound), so a caller tells them from the command's.
func (a *App) Exec(ctx context.Context, argv []string, opts ExecOptions) error {
	err := a.exec(ctx, argv, opts)
	var cmdErr *tasks.CommandError
	var exit *exec.ExitError
	switch {
	case err == nil:
		return nil
	case !errors.As(err, &cmdErr), ctx.Err() != nil:
		// Interrupted, graft ended the command itself.
		return &ExitCodeError{Code: ExitGraft, Err: err}
	case errors.As(cmdErr.Err, &exit):
		return &ExitCodeError{Code: exitStatus(exit), Err: fmt.Errorf("exec: %s: %w", strings.Join(argv, " "), err)}
	case cmdErr.Started:
		return &ExitCodeError{Code: ExitGraft, Err: fmt.Errorf("exec: %s: %w", strings.Join(argv, " "), err)}
	case errors.Is(cmdErr.Err, exec.ErrNotFound), errors.Is(cmdErr.Err, fs.ErrNotExist):
		return &ExitCodeError{Code: ExitNotFound, Err: fmt.Errorf("exec: %w", err)}
	default:
		return &ExitCodeError{Code: ExitCannotRun, Err: fmt.Errorf("exec: %w", err)}
	}
}

// The exit statuses of graft exec's own failures, those of env and timeout.
const (
	// ExitGraft is graft failing: its config, the queue, a usage error.
	ExitGraft = 125
	// ExitCannotRun is a command found but not started (not executable).
	ExitCannotRun = 126
	// ExitNotFound is a command not found.
	ExitNotFound = 127
)

// exitStatus is the command's exit status, 128 and the signal for one a
// signal ended, as a shell says.
func exitStatus(exit *exec.ExitError) int {
	if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return exit.ExitCode()
}

// ExecOptions are graft exec's flags.
type ExecOptions struct {
	// Heavy queues the command as a heavy task.
	Heavy bool
	// Settle, when set, replaces load.settle for this command.
	Settle time.Duration
}

func (a *App) exec(ctx context.Context, argv []string, opts ExecOptions) error {
	heavy := opts.Heavy
	root := a.Dir
	cfg := &config.Config{}
	repo, err := a.openRepo(a.Dir)
	switch {
	case err == nil:
		root = repo.Root
		// A heavy command goes where the project sends heavy work.
		if heavy {
			if cfg, err = projectConfig(root); err != nil {
				return err
			}
			if err := a.requireGraft(cfg); err != nil {
				return err
			}
		}
	case !errors.Is(err, gitx.ErrNotRepo):
		return err
	}
	r := a.newRunner(root, cfg)
	if opts.Settle > 0 {
		load := r.Load
		r.Load = func() (userconfig.MachineLoad, error) {
			l, err := load()
			l.Settle = opts.Settle
			return l, err
		}
	}
	return r.Exec(ctx, a.Dir, argv, heavy)
}

// projectConfig is the work tree's config, an empty one when it has none:
// graft exec runs in any directory.
func projectConfig(root string) (*config.Config, error) {
	cfg, err := config.Load(root)
	if errors.Is(err, config.ErrNotFound) {
		return &config.Config{}, nil
	}
	return cfg, err
}
