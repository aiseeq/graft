//go:build linux

package tasks

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	akitproc "github.com/aiseeq/akit/proc"
	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/proc"
)

func TestMain(m *testing.M) {
	if err := akitproc.BecomeChildSubreaper(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// TestTaskConfirmChecksNoNewPrivs tests the default refusal with a synthetic
// setuid file. It never executes sudo and runs no_new_privs in a child only.
func TestTaskConfirmChecksNoNewPrivs(t *testing.T) {
	const child = "GRAFT_TEST_TASK_CONFIRM_NNP"
	if os.Getenv(child) == "1" {
		r := NewRunner(t.TempDir(), &config.Config{Tasks: map[string]*config.Task{
			"projectA": {Confirm: "sudo", Run: []config.Step{{Argv: []string{"/bin/true"}}}},
		}}, io.Discard, io.Discard)
		r.ConfirmTask = func(context.Context, string, io.Writer) error {
			t.Fatal("confirmation was called under no_new_privs")
			return nil
		}
		err := r.Run(context.Background(), "projectA", nil)
		if err == nil || !strings.Contains(err.Error(), "no_new_privs") || !strings.Contains(err.Error(), "outside the sandbox") {
			t.Fatalf("refusal: %v", err)
		}
		return
	}
	if os.Geteuid() == 0 {
		t.Skip("root needs no sudo")
	}
	setpriv, err := exec.LookPath("setpriv")
	if err != nil {
		t.Skipf("setpriv unavailable: %v", err)
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "sudo")
	if err := os.WriteFile(fake, []byte("synthetic executable; never run"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fake, os.ModeSetuid|0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(setpriv, "--no-new-privs", os.Args[0], "-test.run=^TestTaskConfirmChecksNoNewPrivs$", "-test.count=1")
	cmd.Env = append(os.Environ(), child+"=1", "PATH="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sandboxed child: %v\n%s", err, out)
	}
}

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
	_, err := r.measureWithReason(context.Background(), cmd, "step", true, true, nil, nil, nil)
	data, rerr := os.ReadFile(filepath.Join(dir, "daemon"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	daemon, perr := strconv.Atoi(strings.TrimSpace(string(data)))
	if perr != nil {
		t.Fatal(perr)
	}
	if argv, cerr := proc.Cmdline(daemon); cerr == nil && strings.Join(argv, " ") == "sleep 30" {
		if kerr := akitproc.SignalDescendant(daemon, syscall.SIGKILL); kerr != nil && !errors.Is(kerr, os.ErrProcessDone) {
			t.Error(kerr)
		}
	}
	if err != nil {
		t.Fatalf("the step that exited 0 failed: %v", err)
	}
}
