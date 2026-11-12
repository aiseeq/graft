//go:build linux

package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"
)

// Refused is a *RefusedError when the program at path may not start in this
// process, nil when it may. Starting it anyway is no error for the program
// alone: the kernel refuses it and SELinux raises an alert. no_new_privs
// belongs to a thread: the calling one is read, the one a start forks from.
func Refused(path string) error {
	status, err := os.ReadFile("/proc/thread-self/status")
	if err != nil {
		return fmt.Errorf("the process status: %w", err)
	}
	label := ""
	if _, err := os.Stat(selinuxEnforce); err == nil {
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

// RefusedSudo tells whether no_new_privs prevents sudo from elevating this
// process. Deploy checks it before any configured sudo step.
// It only gathers the inputs; sudoRefusal decides.
func RefusedSudo() error {
	noNewPrivs := processHasNoNewPrivs()
	sudo, mode, err := setuidSudoOnPath()
	if err != nil {
		// Without no_new_privs sudo is not graft's to judge, nor is a sudo
		// that is not there.
		if !noNewPrivs || errors.Is(err, exec.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("inspecting sudo: %w", err)
	}
	if refusal := sudoRefusal(noNewPrivs, os.Geteuid(), sudo, mode); refusal != nil {
		return refusal
	}
	return nil
}

// processHasNoNewPrivs fails open if the kernel query is unavailable.
func processHasNoNewPrivs() bool {
	value, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
	return err == nil && value == 1
}

// setuidSudoOnPath returns the resolved sudo path and mode.
func setuidSudoOnPath() (string, os.FileMode, error) {
	path, err := exec.LookPath("sudo")
	if err != nil {
		return "", 0, fmt.Errorf("finding sudo in PATH: %w", err)
	}
	info, err := os.Stat(path) // Stat follows a sudo symlink in PATH.
	if err != nil {
		return "", 0, fmt.Errorf("stat sudo executable: %w", err)
	}
	return path, info.Mode(), nil
}

// selinuxEnforce exists where SELinux is on; a test points it elsewhere.
var selinuxEnforce = "/sys/fs/selinux/enforce"

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
