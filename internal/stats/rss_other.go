//go:build !linux && !darwin

package stats

import "os"

// maxRSS reports no peak memory: the process state carries none here
// (Windows), or in units this package does not know.
func maxRSS(*os.ProcessState) (int64, bool) { return 0, false }
