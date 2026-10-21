//go:build linux

package proc_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/proc"
	"github.com/aiseeq/graft/v2/internal/proc/proctest"
)

// TestRefusedCommandInASandbox: under no_new_privs docker found in PATH is
// refused, a shell is not.
func TestRefusedCommandInASandbox(t *testing.T) {
	if !proctest.Sandboxed(t, "TestRefusedCommandInASandbox") {
		return
	}
	var r *proc.RefusedError
	if err := proc.RefusedCommand(t.TempDir(), "docker"); !errors.As(err, &r) || !strings.Contains(r.Error(), "no_new_privs") {
		t.Errorf("docker: %v", err)
	}
	if err := proc.RefusedCommand(t.TempDir(), "sh"); err != nil {
		t.Errorf("sh: %v", err)
	}
}
