//go:build !unix

package stats

import "errors"

// ProcessCPU is unavailable on platforms without a supported process CPU
// counter. Built-in steps remain ineligible for light-gate history there.
func ProcessCPU() (float64, error) { return 0, errors.New("not supported") }
