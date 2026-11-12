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

// Terminate asks the process group a detached process leads to exit
// (SIGTERM): a wrapper such as go run must not leave its child behind.
func Terminate(pid int) error {
	return signalGroup(pid, syscall.SIGTERM)
}

// StopOnCancel makes cmd get SIGTERM when its context ends, in place of
// SIGKILL, so it can clean up. A cmd still running grace later is killed
// with the processes it started (a child it did not stop would run on with
// the work), what fails then goes to warn, and Wait returns soon after.
func StopOnCancel(cmd *exec.Cmd, grace time.Duration, warn func(error)) {
	cmd.Cancel = func() error {
		// Asked before the signal: a reaper whose command ended alone gets
		// it only to end the orphans sooner, and Wait keeps the command's
		// own status, a success too.
		ended := reaperEnded(cmd)
		err := akitproc.SignalChild(cmd.Process.Pid, syscall.SIGTERM)
		if errors.Is(err, errors.ErrUnsupported) {
			// Outside Linux akit cannot verify the child; os.Process signals
			// the child graft started and has not waited for yet.
			err = cmd.Process.Signal(syscall.SIGTERM)
		}
		time.AfterFunc(grace, func() {
			if err := cmd.Process.Signal(syscall.Signal(0)); errors.Is(err, os.ErrProcessDone) {
				return
			} else if err != nil {
				warn(err)
				return
			}
			_, err := akitproc.StopChild(cmd.Process.Pid, 0, nil)
			if errors.Is(err, errors.ErrUnsupported) {
				// Outside Linux the tree cannot be walked: the child alone.
				err = cmd.Process.Kill()
			}
			if err != nil && !akitproc.Gone(err) && !errors.Is(err, os.ErrProcessDone) {
				warn(err)
			}
		})
		if ended && err == nil {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = grace + killSettle
}

// killSettle is how long Wait waits for the output of a killed tree.
const killSettle = 5 * time.Second

// Kill ends the process group a detached process leads (SIGKILL).
func Kill(pid int) error {
	return signalGroup(pid, syscall.SIGKILL)
}

func signalGroup(pid int, sig syscall.Signal) error {
	// The service may have been started by another graft process. akit can
	// stop a group only when its exec.Cmd was started by this process.
	if err := syscall.Kill(-pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("sending %s to process group %d: %w", sig, pid, err)
	}
	return nil
}

// ConnRefused reports whether a dial failed because nothing listens.
func ConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}
