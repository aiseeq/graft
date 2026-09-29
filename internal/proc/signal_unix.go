//go:build !windows

package proc

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// Detach makes cmd start in its own session, so it outlives graft and a
// Ctrl-C in graft's terminal does not reach it.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// Terminate asks the process group a detached process leads to exit
// (SIGTERM): a wrapper such as go run must not leave its child behind.
func Terminate(pid int) error {
	return signalGroup(pid, syscall.SIGTERM)
}

// Kill ends the process group a detached process leads (SIGKILL).
func Kill(pid int) error {
	return signalGroup(pid, syscall.SIGKILL)
}

func signalGroup(pid int, sig syscall.Signal) error {
	if err := syscall.Kill(-pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("sending %s to process group %d: %w", sig, pid, err)
	}
	return nil
}

// ConnRefused reports whether a dial failed because nothing listens.
func ConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}
