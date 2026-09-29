//go:build windows

package proc

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Detach makes cmd start in its own process group without a console, so it
// outlives graft and a Ctrl-C in graft's console does not reach it.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
}

// Terminate ends the process. Windows has no signal a console-less process
// can catch, so it is the same as Kill; children the process started are not
// ended with it, so a service should be the program itself, not a wrapper.
func Terminate(pid int) error {
	return Kill(pid)
}

// Kill ends the process.
func Kill(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil // already gone
	}
	if err != nil {
		return fmt.Errorf("opening process %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("terminating process %d: %w", pid, err)
	}
	return nil
}

// ConnRefused reports whether a dial failed because nothing listens.
func ConnRefused(err error) bool {
	return errors.Is(err, windows.WSAECONNREFUSED)
}
