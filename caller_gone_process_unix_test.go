//go:build !windows

package main

import (
	"syscall"

	akitproc "github.com/aiseeq/akit/proc"
)

// signalProcessZero probes pid; a zombie counts as gone. On Linux TestMain
// makes the test binary a subreaper, so a killed grandchild becomes its
// zombie: it is reaped here, or signal 0 would keep finding it.
func signalProcessZero(pid int) error {
	if stat, err := akitproc.ReadStat("/proc", pid); err == nil && stat.State == "Z" {
		_, _ = syscall.Wait4(pid, nil, syscall.WNOHANG, nil)
		return syscall.ESRCH
	}
	return syscall.Kill(pid, 0)
}
