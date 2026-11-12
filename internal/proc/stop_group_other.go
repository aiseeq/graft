//go:build !linux

package proc

import "os/exec"

// StopStartedGroup retains the platform's existing service stop behavior.
func StopStartedGroup(cmd *exec.Cmd) error {
	return Kill(cmd.Process.Pid)
}
