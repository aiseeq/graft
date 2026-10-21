package proc

import (
	"errors"
	"strings"
	"testing"
)

// TestRefusal: a program is refused under no_new_privs only when its label
// is a container runtime's, whose domain SELinux would not let the process
// enter.
func TestRefusal(t *testing.T) {
	const runtime = "system_u:object_r:container_runtime_exec_t:s0"
	for _, c := range []struct {
		status, label string
		refused       bool
	}{
		{"Name:\tgraft\nNoNewPrivs:\t1\nSeccomp:\t2\n", runtime, true},
		{"Name:\tgraft\nNoNewPrivs:\t0\n", runtime, false},
		{"NoNewPrivs:\t1\n", "system_u:object_r:bin_t:s0", false},
		{"NoNewPrivs:\t1\n", "", false},
		{"Name:\tgraft\n", runtime, false},
	} {
		r := refusal("/usr/bin/docker", []byte(c.status), c.label)
		if (r != nil) != c.refused || c.refused && !strings.Contains(r.Error(), "/usr/bin/docker cannot start here: this process runs with no_new_privs") {
			t.Errorf("refusal(%q, %q) = %v", c.status, c.label, r)
		}
	}
}

// TestRefusedCommandMissing: a program that is not there is an error, not
// a refusal.
func TestRefusedCommandMissing(t *testing.T) {
	for _, name := range []string{"graft-no-such-program", "./bin/no-such-program"} {
		err := RefusedCommand(t.TempDir(), name)
		var r *RefusedError
		if err == nil || errors.As(err, &r) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
