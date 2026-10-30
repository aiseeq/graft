//go:build !linux

package tasks

import (
	"errors"
	"runtime"
)

// parentEnv is the environment graft's parent process started with, which
// graft reads from /proc on Linux only.
func parentEnv() ([]string, error) {
	return nil, errors.New("the caller's environment is not readable on " + runtime.GOOS)
}
