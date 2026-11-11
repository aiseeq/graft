// Package execx runs a command with a chosen working directory and
// environment, capturing both output streams.
package execx

import (
	"bytes"
	"context"
	"os/exec"
)

// Run captures stdout and stderr from executable.
func Run(ctx context.Context, dir, executable string, env []string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Dir, cmd.Env = dir, env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}
