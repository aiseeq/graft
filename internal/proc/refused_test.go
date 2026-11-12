package proc

import (
	"errors"
	"os"
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

// TestRefusedSudoDecision: sudo is refused only under no_new_privs, for a
// non-root process, when it is a setuid file.
func TestRefusedSudoDecision(t *testing.T) {
	setuid := os.ModeSetuid | 0o4755
	for _, tc := range []struct {
		name       string
		noNewPrivs bool
		euid       int
		path       string
		mode       os.FileMode
		refused    bool
	}{
		{name: "setuid sudo with no_new_privs", noNewPrivs: true, euid: 1000, path: "/usr/bin/sudo", mode: setuid, refused: true},
		{name: "sudo without setuid", noNewPrivs: true, euid: 1000, path: "/tmp/sudo", mode: 0o755},
		{name: "no_new_privs is clear", euid: 1000, path: "/usr/bin/sudo", mode: setuid},
		{name: "root does not need sudo", noNewPrivs: true, euid: 0, path: "/usr/bin/sudo", mode: setuid},
		{name: "sudo is not found", noNewPrivs: true, euid: 1000, mode: setuid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := sudoRefusal(tc.noNewPrivs, tc.euid, tc.path, tc.mode)
			if (err != nil) != tc.refused {
				t.Fatalf("sudoRefusal() = %v, refused = %t", err, tc.refused)
			}
			if tc.refused && err.Error() != "sudo cannot raise privileges here (no_new_privs); run graft outside the sandbox" {
				t.Errorf("sudoRefusal() message = %q", err)
			}
		})
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
