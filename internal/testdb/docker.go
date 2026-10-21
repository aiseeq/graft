package testdb

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// dockerCheck finds docker once per process and tells whether it may start.
var dockerCheck = sync.OnceValues(func() (string, error) {
	path, err := exec.LookPath("docker")
	if err != nil {
		return "", fmt.Errorf("docker is needed for the test database: %w", err)
	}
	why, err := dockerRefused(path)
	if err != nil {
		return "", err
	}
	if why != "" {
		return "", &RefusedError{Why: why}
	}
	return path, nil
})

// RefusedError: docker may not start in this process. The server, brought
// up from outside, may still answer on the DSN.
type RefusedError struct{ Why string }

func (e *RefusedError) Error() string { return "docker cannot start here: " + e.Why }

// refused is why docker may not start, nil when it may or when it is missing.
func refused() *RefusedError {
	var r *RefusedError
	if _, err := dockerCheck(); errors.As(err, &r) {
		return r
	}
	return nil
}

// dockerCommand is docker with args, or why docker cannot start: refused,
// the attempt would only raise an SELinux alert on the desktop each time.
func dockerCommand(ctx context.Context, args ...string) (*exec.Cmd, error) {
	path, err := dockerCheck()
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, path, args...), nil
}

// runtimeExecType is the SELinux type of container runtimes' binaries
// (docker, podman), the entry point of the domain they run in.
const runtimeExecType = "container_runtime_exec_t"

// refusal is why docker may not start in a process of this status, given
// docker's SELinux label ("" for none): under no_new_privs, which sandboxes
// set, SELinux refuses the move into the runtime's domain.
func refusal(status []byte, label string) string {
	nnp := false
	for line := range strings.SplitSeq(string(status), "\n") {
		if v, ok := strings.CutPrefix(line, "NoNewPrivs:"); ok {
			nnp = strings.TrimSpace(v) == "1"
		}
	}
	if fields := strings.Split(label, ":"); !nnp || len(fields) < 3 || fields[2] != runtimeExecType {
		return ""
	}
	return "this process runs with no_new_privs, a sandbox's, under which SELinux refuses docker (" + label + ") its domain"
}
