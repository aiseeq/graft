package app

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

// Exec runs argv in the directory graft started in, a command that is not a
// task of the project (a one-off go test, build or e2e run): heavy, it waits
// for the machine in the queue of heavy tasks and gets {threads} and
// GRAFT_THREADS from the user config's load.threads. The stats record it as
// the task exec of the work tree around the directory, or of the directory
// outside one. A heavy one in a project with remote_heavy runs on its host
// first, as a heavy task's steps do. The command's exit status is graft's.
func (a *App) Exec(ctx context.Context, argv []string, heavy bool) error {
	root := a.Dir
	cfg := &config.Config{}
	repo, err := gitx.Open(a.Dir)
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
	err = a.newRunner(root, cfg).Exec(ctx, a.Dir, argv, heavy)
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return &ExitCodeError{Code: exit.ExitCode(), Err: fmt.Errorf("exec: %s: %w", strings.Join(argv, " "), err)}
	}
	return err
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
