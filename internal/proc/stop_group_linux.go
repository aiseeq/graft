package proc

import (
	"errors"
	"os/exec"
	"syscall"

	akitproc "github.com/aiseeq/akit/proc"
)

// StopStartedGroup ends a failed service start and its verified descendants.
// The caller remains responsible for waiting on cmd.
func StopStartedGroup(cmd *exec.Cmd) error {
	_, err := akitproc.StopChildGroup(cmd, 0, syscall.SIGKILL)
	if errors.Is(err, errors.ErrUnsupported) {
		return Kill(cmd.Process.Pid)
	}
	return err
}
