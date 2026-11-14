//go:build linux

package stats

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// ProcessCPU returns this process's user and system CPU time in seconds.
func ProcessCPU() (float64, error) {
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
		return 0, fmt.Errorf("get process CPU usage: %w", err)
	}
	user := float64(usage.Utime.Sec) + float64(usage.Utime.Usec)/1e6
	system := float64(usage.Stime.Sec) + float64(usage.Stime.Usec)/1e6
	return user + system, nil
}
