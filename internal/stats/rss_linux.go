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

// blockIO is what the process and the descendants it waited for read from
// and wrote to the storage, in bytes: Linux counts ru_inblock and ru_oublock
// in 512-byte units, a write when it dirties the page.
func blockIO(ps *os.ProcessState) (read, write int64, ok bool) {
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok || ru == nil {
		return 0, 0, false
	}
	return ru.Inblock * 512, ru.Oublock * 512, true
}
