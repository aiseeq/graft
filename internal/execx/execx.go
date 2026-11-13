// Package execx runs a command with a chosen working directory and
// environment, capturing both output streams.
package execx

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"github.com/aiseeq/akit/proc"
)

// ShortGrace is the stop window for short utility commands.
const ShortGrace = time.Second

// CommandContext creates a command whose cancellation uses akit's checked
// stopper instead of os/exec's default immediate kill.
func CommandContext(ctx context.Context, grace time.Duration, executable string, args ...string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, executable, args...)
	if err := proc.CancelByStop(cmd, grace, syscall.SIGTERM); err != nil {
		return nil, fmt.Errorf("installing command cancellation: %w", err)
	}
	return cmd, nil
}

// Run captures stdout and stderr from executable.
func Run(ctx context.Context, dir, executable string, env []string, args ...string) (string, string, error) {
	cmd, err := CommandContext(ctx, ShortGrace, executable, args...)
	if err != nil {
		return "", "", err
	}
	cmd.Dir, cmd.Env = dir, env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	return stdout.String(), stderr.String(), err
}
