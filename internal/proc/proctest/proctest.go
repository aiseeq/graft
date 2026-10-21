//go:build linux

// Package proctest runs a test as a sandbox would run it: with no_new_privs,
// in a child of the test binary, as no_new_privs cannot be dropped once set.
package proctest

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// childEnv is set in the child, the cue to set no_new_privs.
const childEnv = "GRAFT_TEST_NNP_CHILD"

// Sandboxed runs the test name in a child with no_new_privs, where SELinux
// refuses docker its domain, and tells whether this is that child: the
// caller then tests, and otherwise returns. It skips where docker would not
// be refused. no_new_privs is set on one thread, so the child's test
// goroutine stays on it: every process the test starts forks from it.
func Sandboxed(t *testing.T, name string) bool {
	t.Helper()
	if _, child := os.LookupEnv(childEnv); child {
		runtime.LockOSThread()
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
		return true
	}
	path, err := exec.LookPath("docker")
	if err != nil {
		t.Skipf("no docker: %v", err)
	}
	if !runtimeLabeled(path) {
		t.Skip("docker carries no container_runtime_exec_t label")
	}
	if _, err := os.Stat("/sys/fs/selinux/enforce"); errors.Is(err, os.ErrNotExist) {
		t.Skip("no SELinux")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("%v\n%s", err, out)
	}
	return false
}

// runtimeLabeled tells whether the file carries the SELinux label of a
// container runtime, the environment the tests need.
func runtimeLabeled(path string) bool {
	buf := make([]byte, 256)
	n, err := unix.Getxattr(path, "security.selinux", buf)
	return err == nil && strings.Contains(string(buf[:n]), ":container_runtime_exec_t:")
}
