//go:build linux

package proc

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Refused is a *RefusedError when the program at path may not start in this
// process, nil when it may. Starting it anyway is no error for the program
// alone: the kernel refuses it and SELinux raises an alert.
func Refused(path string) error {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return fmt.Errorf("the process status: %w", err)
	}
	label := ""
	if _, err := os.Stat("/sys/fs/selinux/enforce"); err == nil {
		if label, err = selinuxLabel(path); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("the SELinux state: %w", err)
	}
	if r := refusal(path, status, label); r != nil {
		return r
	}
	return nil
}

// selinuxLabel is the SELinux label of the file, "" for none.
func selinuxLabel(path string) (string, error) {
	buf := make([]byte, 256)
	n, err := unix.Getxattr(path, "security.selinux", buf)
	switch {
	case errors.Is(err, unix.ENODATA), errors.Is(err, unix.ENOTSUP):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("the SELinux label of %s: %w", path, err)
	}
	return strings.TrimRight(string(buf[:n]), "\x00"), nil
}
