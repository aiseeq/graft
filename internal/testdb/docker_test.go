package testdb

import (
	"strings"
	"testing"
)

// TestRefusal: docker is refused under no_new_privs only when its label is a
// container runtime's, whose domain SELinux would not let the process enter.
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
		why := refusal([]byte(c.status), c.label)
		if (why != "") != c.refused || c.refused && !strings.Contains(why, "no_new_privs") {
			t.Errorf("refusal(%q, %q) = %q", c.status, c.label, why)
		}
	}
}
