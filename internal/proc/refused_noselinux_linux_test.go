//go:build linux

package proc

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestRefusedCommandMissingWithoutSELinux: on a machine without SELinux, as
// in a sandbox that does not mount it, a program that is not there is an
// error all the same, not a program that may start.
func TestRefusedCommandMissingWithoutSELinux(t *testing.T) {
	old := selinuxEnforce
	selinuxEnforce = filepath.Join(t.TempDir(), "no-selinux")
	defer func() { selinuxEnforce = old }()
	for _, name := range []string{"graft-no-such-program", "./bin/no-such-program"} {
		err := RefusedCommand(t.TempDir(), name)
		var r *RefusedError
		if err == nil || errors.As(err, &r) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := RefusedCommand(t.TempDir(), "sh"); err != nil {
		t.Errorf("sh: %v", err)
	}
}
