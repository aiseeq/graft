//go:build windows

package lock

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// processAlive reports whether a process with pid exists and has not exited.
func processAlive(pid int) (bool, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	switch {
	case errors.Is(err, windows.ERROR_INVALID_PARAMETER):
		return false, nil // no such process
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("opening process %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false, fmt.Errorf("reading exit code of process %d: %w", pid, err)
	}
	const stillActive = 259
	return code == stillActive, nil
}
