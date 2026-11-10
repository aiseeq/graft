//go:build !linux

package proc

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// Reap leaves cmd as it is: only Linux has a subreaper to make orphans the
// reaper's children instead of the session manager's.
func Reap(*exec.Cmd) {}

// reaperEnded is false: without a reaper a command ends with its process.
func reaperEnded(*exec.Cmd) bool { return false }

// ExitReaper is the reaper's own failure: here, that there is none.
const ExitReaper = 125

// RunReaper fails: only Linux has a subreaper.
func RunReaper(path string, _ []string) int {
	fmt.Fprintf(os.Stderr, "graft: reaper: %s has no subreaper to run %s under\n", runtime.GOOS, path)
	return ExitReaper
}
