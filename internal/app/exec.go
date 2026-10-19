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
// outside one. The command's exit status is graft's.
func (a *App) Exec(ctx context.Context, argv []string, heavy bool) error {
	root := a.Dir
	repo, err := gitx.Open(a.Dir)
	switch {
	case err == nil:
		root = repo.Root
	case !errors.Is(err, gitx.ErrNotRepo):
		return err
	}
	err = a.newRunner(root, &config.Config{}).Exec(ctx, a.Dir, argv, heavy)
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return &ExitCodeError{Code: exit.ExitCode(), Err: fmt.Errorf("exec: %s: %w", strings.Join(argv, " "), err)}
	}
	return err
}
