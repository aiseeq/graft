//go:build linux

package proc

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
)

// Cmdline returns the argv a process was started with.
func Cmdline(pid int) ([]string, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil, fmt.Errorf("reading the command line of process %d: %w", pid, err)
	}
	var argv []string
	for part := range bytes.SplitSeq(bytes.TrimSuffix(data, []byte{0}), []byte{0}) {
		argv = append(argv, string(part))
	}
	return argv, nil
}
