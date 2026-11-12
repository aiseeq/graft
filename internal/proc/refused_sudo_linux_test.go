package proc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aiseeq/graft/v2/internal/testsudo"
)

// TestRefusedSudoFollowsTheProcess: with a setuid sudo in PATH, RefusedSudo
// refuses exactly when this process runs under no_new_privs, so the test
// checks the flag RefusedSudo reads both under setpriv --no-new-privs and
// without it.
func TestRefusedSudoFollowsTheProcess(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root needs no sudo")
	}
	real := testsudo.Setuid(t)
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(real, filepath.Join(dir, "sudo")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	want := hasNoNewPrivs(status)
	if err := RefusedSudo(); (err != nil) != want {
		t.Errorf("RefusedSudo() = %v under no_new_privs %t", err, want)
	}
}

// TestRefusedSudoLookupError: a sudo that cannot be looked up (one in the
// current directory, which PATH may not reach) is graft's concern only
// under no_new_privs; without it RefusedSudo stays out of the way.
func TestRefusedSudoLookupError(t *testing.T) {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("PATH", ".")
	want := hasNoNewPrivs(status)
	if err := RefusedSudo(); (err != nil) != want {
		t.Errorf("RefusedSudo() = %v under no_new_privs %t", err, want)
	}
}
