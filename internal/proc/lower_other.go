//go:build !linux && !windows

package proc

import (
	"fmt"
	"os/exec"
	"syscall"
)

// StartLow starts cmd and gives it nice 19, inherited by what it starts
// from then on; warn hears when the system refuses.
func StartLow(cmd *exec.Cmd, warn func(string)) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := syscall.Setpriority(syscall.PRIO_PROCESS, cmd.Process.Pid, lowestNice); err != nil {
		warn(fmt.Sprintf("heavy task at nice 0: %v", err))
	}
	return nil
}
