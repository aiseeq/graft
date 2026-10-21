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

// PressureEnv and VMStatEnv, when set, are read instead of
// /proc/pressure/io and /proc/vmstat: for tests.
const (
	PressureEnv = "GRAFT_PRESSURE_IO"
	VMStatEnv   = "GRAFT_VMSTAT"
)

// ErrNoPressure: the kernel reports no IO pressure (no /proc/pressure/io:
// not Linux, or a kernel without PSI).
var ErrNoPressure = errors.New("no /proc/pressure/io to read the IO pressure from")

// ErrNoVMStat: the system has no /proc/vmstat to count swapping from.
var ErrNoVMStat = errors.New("no /proc/vmstat to count swapping from")

// IOFull is the share of the last minute, in percent, during which every
// running process stalled on IO: avg60 of the full line of
// /proc/pressure/io.
func IOFull() (float64, error) {
	data, err := readProc(PressureEnv, "/proc/pressure/io", ErrNoPressure)
	if err != nil {
		return 0, err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "full" {
			continue
		}
		for _, f := range fields[1:] {
			if v, ok := strings.CutPrefix(f, "avg60="); ok {
				pct, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return 0, fmt.Errorf("IO pressure: avg60 %q: %w", v, err)
				}
				return pct, nil
			}
		}
	}
	return 0, errors.New("IO pressure: no full avg60 in the pressure file")
}

// SwapPages is how many pages the system swapped in and out since boot:
// pswpin + pswpout of /proc/vmstat.
func SwapPages() (int64, error) {
	data, err := readProc(VMStatEnv, "/proc/vmstat", ErrNoVMStat)
	if err != nil {
		return 0, err
	}
	var pages int64
	seen := 0
	for line := range strings.SplitSeq(string(data), "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || (name != "pswpin" && name != "pswpout") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("vmstat: %s %q: %w", name, value, err)
		}
		pages += n
		seen++
	}
	if seen != 2 {
		return 0, errors.New("vmstat: no pswpin and pswpout")
	}
	return pages, nil
}

// readProc reads the file of env or, unset, the proc file; a missing proc
// file is none.
func readProc(env, proc string, none error) ([]byte, error) {
	path := proc
	if p := os.Getenv(env); p != "" {
		path = p
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && path == proc {
		return nil, none
	}
	return data, err
}
