//go:build !linux && !darwin

package stats

import "os"

// maxRSS reports no peak memory: the process state carries none here
// (Windows), or in units this package does not know.
func maxRSS(*os.ProcessState) (int64, bool) { return 0, false }

// blockIO reports nothing either.
func blockIO(*os.ProcessState) (read, write int64, ok bool) { return 0, 0, false }
