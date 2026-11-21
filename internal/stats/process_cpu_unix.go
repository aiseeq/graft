//go:build linux

package stats

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// ProcessCPU returns the user and system CPU time of this process and its
// waited-for children, in seconds.
func ProcessCPU() (user, system float64, err error) {
	var self, children unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &self); err != nil {
		return 0, 0, fmt.Errorf("get process CPU usage: %w", err)
	}
	if err := unix.Getrusage(unix.RUSAGE_CHILDREN, &children); err != nil {
		return 0, 0, fmt.Errorf("get child process CPU usage: %w", err)
	}
	seconds := func(tv unix.Timeval) float64 { return float64(tv.Sec) + float64(tv.Usec)/1e6 }
	return seconds(self.Utime) + seconds(children.Utime), seconds(self.Stime) + seconds(children.Stime), nil
}
