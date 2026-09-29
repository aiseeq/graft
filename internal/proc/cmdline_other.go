//go:build !linux

package proc

import "errors"

// Cmdline is available on Linux only.
func Cmdline(int) ([]string, error) {
	return nil, errors.ErrUnsupported
}
