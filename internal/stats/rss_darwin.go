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

// blockIO reports nothing: macOS counts ru_inblock and ru_oublock in
// operations, not bytes.
func blockIO(*os.ProcessState) (read, write int64, ok bool) { return 0, 0, false }
