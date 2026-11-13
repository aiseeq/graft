//go:build windows

package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	akitproc "github.com/aiseeq/akit/proc"
	"golang.org/x/sys/windows"
)

// Detach makes cmd start in its own process group without a console, so it
// outlives graft and a Ctrl-C in graft's console does not reach it.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
}

// killSettle is how long Wait waits for the output of a killed command.
const killSettle = 5 * time.Second

// StopOnCancel stops a cancelled command through akit on every platform.
func StopOnCancel(cmd *exec.Cmd, grace time.Duration, warn func(error)) {
	if err := akitproc.CancelByStopWithObserver(cmd, grace, syscall.SIGTERM, func(degraded bool, err error) {
		if degraded || errors.Is(err, akitproc.ErrPidfdUnavailable) {
			warn(fmt.Errorf("command %d was not stopped: %v", cmd.Process.Pid, err))
			return
		}
		if err != nil && !errors.Is(err, os.ErrProcessDone) {
			warn(err)
		}
	}); err != nil {
		warn(err)
	}
	cmd.WaitDelay = grace + killSettle
}

// ConnRefused reports whether a dial failed because nothing listens.
func ConnRefused(err error) bool {
	return errors.Is(err, windows.WSAECONNREFUSED)
}
