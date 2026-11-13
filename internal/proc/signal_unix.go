//go:build !windows

package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	akitproc "github.com/aiseeq/akit/proc"
)

// Detach makes cmd start in its own session, so it outlives graft and a
// Ctrl-C in graft's terminal does not reach it.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// StopOnCancel makes cmd get SIGTERM when its context ends, in place of
// SIGKILL, so it can clean up. A cmd still running grace later is killed
// with the processes it started (a child it did not stop would run on with
// the work), what fails then goes to warn, and Wait returns soon after.
func StopOnCancel(cmd *exec.Cmd, grace time.Duration, warn func(error)) {
	cmd.Cancel = func() error {
		ended := reaperEnded(cmd)
		result, err := akitproc.SignalCmdWithResult(cmd, syscall.SIGTERM)
		if result.Degraded || errors.Is(err, akitproc.ErrPidfdUnavailable) {
			warn(fmt.Errorf("command %d was signaled directly because pidfd_open is unavailable", cmd.Process.Pid))
		}
		if err != nil && !errors.Is(err, os.ErrProcessDone) {
			warn(err)
		}
		time.AfterFunc(grace, func() { stopOnCancelAfterGrace(cmd, warn) })
		if ended && err == nil {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = grace + killSettle
}

func stopOnCancelAfterGrace(cmd *exec.Cmd, warn func(error)) {
	_, err := akitproc.StopChild(cmd.Process.Pid, 0, func() error {
		return akitproc.SignalCmd(cmd, syscall.SIGKILL)
	})
	if err != nil && !akitproc.Gone(err) && !errors.Is(err, os.ErrProcessDone) {
		warn(err)
	}
}

// killSettle is how long Wait waits for the output of a killed tree.
const killSettle = 5 * time.Second

// ConnRefused reports whether a dial failed because nothing listens.
func ConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}
