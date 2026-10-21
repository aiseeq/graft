package testdb

import (
	"context"
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
		return "", fmt.Errorf("the test database needs docker, which cannot start here: %s; run the tests that need the database outside this sandbox", why)
	}
	return path, nil
})

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
