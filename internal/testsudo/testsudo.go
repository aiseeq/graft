// Package testsudo finds the system's setuid sudo for tests of how graft
// treats it.
package testsudo

import (
	"os"
	"path/filepath"
	"testing"
)

// System is the sudo tests look for.
const System = "/usr/bin/sudo"

// Setuid returns the file System resolves to; the test is skipped when
// there is none or it is not setuid, and fails when it cannot be read.
func Setuid(t testing.TB) string {
	t.Helper()
	if _, err := os.Lstat(System); os.IsNotExist(err) {
		t.Skipf("%s is unavailable", System)
	} else if err != nil {
		t.Fatalf("checking %s: %v", System, err)
	}
	real, err := filepath.EvalSymlinks(System)
	if err != nil {
		t.Fatalf("resolving %s: %v", System, err)
	}
	info, err := os.Stat(real)
	if err != nil {
		t.Fatalf("stat %s: %v", real, err)
	}
	if info.Mode()&os.ModeSetuid == 0 {
		t.Skipf("%s is not setuid: %s", System, real)
	}
	return real
}
