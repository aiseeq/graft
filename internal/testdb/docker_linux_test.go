//go:build linux

package testdb

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestDockerRefusedUnderNoNewPrivs: a process with no_new_privs, as a
// sandbox runs it, is told docker cannot start, without starting it: the
// attempt would raise an SELinux alert. The check runs in a child of the
// test, as no_new_privs cannot be dropped once set.
func TestDockerRefusedUnderNoNewPrivs(t *testing.T) {
	if os.Getenv("GRAFT_TEST_NNP_CHILD") == "1" {
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
		_, err := dockerCommand(t.Context(), "version")
		if err == nil || !strings.Contains(err.Error(), "cannot start here") {
			t.Fatalf("docker under no_new_privs: %v", err)
		}
		return
	}
	path, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("no docker")
	}
	if !runtimeLabeled(path) {
		t.Skipf("docker carries no %s label", runtimeExecType)
	}
	if _, err := os.Stat("/sys/fs/selinux/enforce"); errors.Is(err, os.ErrNotExist) {
		t.Skip("no SELinux")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDockerRefusedUnderNoNewPrivs$", "-test.count=1")
	cmd.Env = append(os.Environ(), "GRAFT_TEST_NNP_CHILD=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("%v\n%s", err, out)
	}
}

// runtimeLabeled tells whether the file carries the SELinux label of a
// container runtime, the environment the test needs.
func runtimeLabeled(path string) bool {
	buf := make([]byte, 256)
	n, err := unix.Getxattr(path, "security.selinux", buf)
	return err == nil && strings.Contains(string(buf[:n]), ":"+runtimeExecType+":")
}
