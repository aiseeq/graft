package proc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RefusedError: a program may not start in this process.
type RefusedError struct {
	Path, Why string
	Message   string
}

func (e *RefusedError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Path + " cannot start here: " + e.Why
}

// runtimeExecType is the SELinux type of container runtimes' binaries
// (docker, podman), the entry point of the domain they run in.
const runtimeExecType = "container_runtime_exec_t"

// hasNoNewPrivs reports whether status contains an enabled no_new_privs flag.
func hasNoNewPrivs(status []byte) bool {
	noNewPrivs := false
	for line := range strings.SplitSeq(string(status), "\n") {
		if v, ok := strings.CutPrefix(line, "NoNewPrivs:"); ok {
			noNewPrivs = strings.TrimSpace(v) == "1"
		}
	}
	return noNewPrivs
}

// sudoRefusal is the refusal of a sudo step, nil when sudo can work: only
// under no_new_privs, for a non-root process, with a setuid sudo at path (a
// non-setuid one is a stand-in that needs no privileges).
func sudoRefusal(noNewPrivs bool, euid int, path string, mode os.FileMode) *RefusedError {
	if !noNewPrivs || euid == 0 || path == "" || mode&os.ModeSetuid == 0 {
		return nil
	}
	return &RefusedError{Path: "sudo", Message: "sudo cannot raise privileges here (no_new_privs); run graft outside the sandbox"}
}

// refusal is why the program at path may not start in a process of this
// status, given the program's SELinux label ("" for none), nil when it may:
// under no_new_privs, which sandboxes set, SELinux refuses the move into a
// container runtime's domain, and the desktop shows an alert each time.
func refusal(path string, status []byte, label string) *RefusedError {
	if fields := strings.Split(label, ":"); !hasNoNewPrivs(status) || len(fields) < 3 || fields[2] != runtimeExecType {
		return nil
	}
	return &RefusedError{Path: path, Why: "this process runs with no_new_privs, a sandbox's, under which SELinux refuses it (" + label + ") its domain"}
}

// RefusedCommand is Refused for the program a command started in dir runs:
// name looked up in PATH, or a path relative to dir.
func RefusedCommand(dir, name string) error {
	path := name
	switch {
	case filepath.Base(name) == name:
		found, err := exec.LookPath(name)
		if err != nil {
			return fmt.Errorf("finding %s: %w", name, err)
		}
		path = found
	case !filepath.IsAbs(name):
		path = filepath.Join(dir, name)
	}
	// A program that is not there is said so on every machine, not only
	// where reading its SELinux label finds it missing.
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("finding %s: %w", name, err)
	}
	return Refused(path)
}
