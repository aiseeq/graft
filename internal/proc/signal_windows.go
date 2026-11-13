//go:build windows

package proc

import (
	"errors"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// Detach makes cmd start in its own process group without a console, so it
// outlives graft and a Ctrl-C in graft's console does not reach it.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
}

// StopOnCancel keeps the kill on a cancelled cmd: Windows has no signal a
// process without a console can catch.
func StopOnCancel(*exec.Cmd, time.Duration, func(error)) {}

// ConnRefused reports whether a dial failed because nothing listens.
func ConnRefused(err error) bool {
	return errors.Is(err, windows.WSAECONNREFUSED)
}
