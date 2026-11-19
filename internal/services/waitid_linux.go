//go:build linux

package services

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// serviceExitedWithoutStart checks the child status without consuming it;
// the goroutine running Wait remains responsible for collecting the result.
func serviceExitedWithoutStart(pid int) (bool, error) {
	var info unix.Siginfo
	err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
	if err == unix.ECHILD {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking whether child process %d exited: %w", pid, err)
	}
	return info.Signo != 0, nil
}
