package proc

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// StartLow starts cmd in the idle priority class, which the processes it
// starts inherit.
func StartLow(cmd *exec.Cmd, _ func(string)) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.IDLE_PRIORITY_CLASS
	return cmd.Start()
}
