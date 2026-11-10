//go:build linux

package tasks

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/proc"
)

// A local step that succeeded is not failed by a daemon it started that
// left its session holding the step's output pipe: Wait gives up on the
// pipe after the wait, and the step counts as passed.
func TestDaemonHoldingPipeDoesNotFailStep(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the wait for the output pipe")
	}
	oldGrace := localStopGrace
	localStopGrace = 80 * time.Millisecond
	defer func() { localStopGrace = oldGrace }()
	dir := t.TempDir()
	cmd := exec.CommandContext(context.Background(), "sh", "-c", `setsid sleep 30 & echo $! > "$1/daemon"`, "sh", dir)
	r := &Runner{}
	r.stopLocalCommand(cmd, true)
	_, err := r.measureWithReason(cmd, "step", true, true, nil, nil, nil)
	data, rerr := os.ReadFile(filepath.Join(dir, "daemon"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	daemon, perr := strconv.Atoi(strings.TrimSpace(string(data)))
	if perr != nil {
		t.Fatal(perr)
	}
	if argv, cerr := proc.Cmdline(daemon); cerr == nil && strings.Join(argv, " ") == "sleep 30" {
		if kerr := syscall.Kill(daemon, syscall.SIGKILL); kerr != nil && !errors.Is(kerr, syscall.ESRCH) {
			t.Error(kerr)
		}
	}
	if err != nil {
		t.Fatalf("the step that exited 0 failed: %v", err)
	}
}
