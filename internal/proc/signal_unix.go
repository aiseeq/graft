//go:build !windows

package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
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
		time.AfterFunc(grace, func() {
			if err := killTree(cmd.Process); err != nil {
				warn(err)
			}
		})
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = grace + killSettle
}

// killSettle is how long Wait waits for the output of a killed tree.
const killSettle = 5 * time.Second

// killTree kills p and the processes it started, if p still runs: os.Process
// signals through a pidfd, so a p already waited for is not taken for a new
// process with its pid.
func killTree(p *os.Process) error {
	if err := p.Signal(syscall.Signal(0)); errors.Is(err, os.ErrProcessDone) {
		return nil
	} else if err != nil {
		return fmt.Errorf("probing process %d: %w", p.Pid, err)
	}
	kids, err := descendants(p.Pid)
	for _, kid := range kids {
		if kerr := syscall.Kill(kid, syscall.SIGKILL); kerr != nil && !errors.Is(kerr, syscall.ESRCH) {
			err = errors.Join(err, fmt.Errorf("killing process %d: %w", kid, kerr))
		}
	}
	if kerr := p.Kill(); kerr != nil && !errors.Is(kerr, os.ErrProcessDone) {
		err = errors.Join(err, fmt.Errorf("killing process %d: %w", p.Pid, kerr))
	}
	return err
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
