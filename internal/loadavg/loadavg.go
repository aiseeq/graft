// Package loadavg reads the machine's load average.
package loadavg

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// FileEnv, when set, is read instead of /proc/loadavg: for tests.
const FileEnv = "GRAFT_LOADAVG"

const procLoadavg = "/proc/loadavg"

// ErrNone: the system has no /proc/loadavg to read.
var ErrNone = errors.New(runtime.GOOS + " has no " + procLoadavg)

// Read is the 1-minute load average, the first field of the file.
func Read() (float64, error) {
	path := procLoadavg
	if p := os.Getenv(FileEnv); p != "" {
		path = p
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && path == procLoadavg {
		return 0, ErrNone
	}
	if err != nil {
		return 0, fmt.Errorf("the load average: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, fmt.Errorf("%s is empty", path)
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	return load, nil
}
