//go:build !windows

package proc

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Alive reports whether a process with pid exists. EPERM means it
// exists under another user.
func Alive(pid int) (bool, error) {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false, fmt.Errorf("finding process %d: %w", pid, err)
	}
	err = p.Signal(syscall.Signal(0))
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, os.ErrProcessDone), errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, fmt.Errorf("probing process %d: %w", pid, err)
	}
}
