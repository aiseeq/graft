package stats

import (
	"os"
	"syscall"
)

// maxRSS is the peak resident memory of the process, in bytes; macOS reports
// ru_maxrss in bytes.
func maxRSS(ps *os.ProcessState) (int64, bool) {
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok || ru == nil {
		return 0, false
	}
	return ru.Maxrss, true
}
