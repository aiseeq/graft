package stats

import (
	"os"
	"syscall"
)

// maxRSS is the peak resident memory of the process, in bytes. Linux reports
// ru_maxrss in kilobytes: the largest of the process itself and of the
// descendants it waited for.
func maxRSS(ps *os.ProcessState) (int64, bool) {
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok || ru == nil {
		return 0, false
	}
	return ru.Maxrss * 1024, true
}
