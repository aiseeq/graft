package testdb

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"

	"github.com/aiseeq/graft/v2/internal/proc"
)

// dockerCheck finds docker once per process and tells whether it may start.
var dockerCheck = sync.OnceValues(func() (string, error) {
	path, err := exec.LookPath("docker")
	if err != nil {
		return "", fmt.Errorf("docker is needed for the test database: %w", err)
	}
	if err := proc.Refused(path); err != nil {
		return "", err
	}
	return path, nil
})

// dockerCommand is docker with args, or why docker cannot start: refused,
// the attempt would only raise an SELinux alert on the desktop each time.
func dockerCommand(ctx context.Context, args ...string) (*exec.Cmd, error) {
	path, err := dockerCheck()
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, path, args...), nil
}

// refused is why docker may not start, nil when it may or when it is missing.
// The server, brought up from outside, may still answer on the DSN.
func refused() *proc.RefusedError {
	var r *proc.RefusedError
	if _, err := dockerCheck(); errors.As(err, &r) {
		return r
	}
	return nil
}
