//go:build linux

package testdb

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// dockerRefused tells why this process may not start the docker binary at
// path, "" when it may.
func dockerRefused(path string) (string, error) {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "", fmt.Errorf("the process status: %w", err)
	}
	if _, err := os.Stat("/sys/fs/selinux/enforce"); errors.Is(err, os.ErrNotExist) {
		return refusal(status, ""), nil
	} else if err != nil {
		return "", fmt.Errorf("the SELinux state: %w", err)
	}
	buf := make([]byte, 256)
	n, err := unix.Getxattr(path, "security.selinux", buf)
	switch {
	case errors.Is(err, unix.ENODATA), errors.Is(err, unix.ENOTSUP):
		return refusal(status, ""), nil
	case err != nil:
		return "", fmt.Errorf("the SELinux label of %s: %w", path, err)
	}
	return refusal(status, strings.TrimRight(string(buf[:n]), "\x00")), nil
}
