package proc

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

	akitproc "github.com/aiseeq/akit/proc"
	"golang.org/x/sys/unix"
)

// TestStopOnCancelKillsTheTreeAfterGrace: a command that ignores SIGTERM is
// killed grace after its context ends, and so is the child it started.
func TestStopOnCancelKillsTheTreeAfterGrace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	childFile := filepath.Join(t.TempDir(), "child")
	cmd := exec.CommandContext(ctx, "sh", "-c", `trap '' TERM; sleep 60 & echo $! > "$1"; wait`, "sh", childFile)
	warned := make(chan error, 4)
	StopOnCancel(cmd, 200*time.Millisecond, func(err error) { warned <- err })
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var child int
	for deadline := time.Now().Add(10 * time.Second); child == 0; time.Sleep(10 * time.Millisecond) {
		if data, err := os.ReadFile(childFile); err == nil && strings.HasSuffix(string(data), "\n") {
			child, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		if time.Now().After(deadline) {
			t.Fatal("the command did not start its child")
		}
	}
	cancel()
	start := time.Now()
	_ = cmd.Wait()
	if took := time.Since(start); took < 150*time.Millisecond || took > 5*time.Second {
		t.Errorf("Wait returned %s after the cancel, want about the 200ms grace", took)
	}
	for deadline := time.Now().Add(5 * time.Second); running(child); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("child %d runs on after its parent was killed", child)
		}
	}
	select {
	case err := <-warned:
		t.Errorf("warned: %v", err)
	default:
	}
}

// When StopChild is unsupported, StopCmd kills a command that ignores SIGTERM
// after the grace period.
func TestStopOnCancelUnsupportedStopChildKillsCommandAfterGrace(t *testing.T) {
	for _, refusal := range []error{errors.ErrUnsupported, akitproc.ErrPidfdUnavailable} {
		t.Run(refusal.Error(), func(t *testing.T) { stopOnCancelFallsBackToStopCmd(t, refusal) })
	}
}

// stopOnCancelFallsBackToStopCmd: StopChild refusing the command with
// refusal, akit's StopCmd stops it after grace.
func stopOnCancelFallsBackToStopCmd(t *testing.T, refusal error) {
	oldStopChild, oldStopCmd := stopChild, stopCmd
	stopChild = func(int, time.Duration, func() error) (akitproc.StopResult, error) {
		return akitproc.StopResult{}, refusal
	}
	stopCalled := make(chan struct{}, 1)
	stopCmd = func(cmd *exec.Cmd, grace time.Duration, sig syscall.Signal) (akitproc.StopResult, error) {
		stopCalled <- struct{}{}
		return oldStopCmd(cmd, grace, sig)
	}
	t.Cleanup(func() { stopChild, stopCmd = oldStopChild, oldStopCmd })

	ctx, cancel := context.WithCancel(context.Background())
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.CommandContext(ctx, "sh", "-c", `trap '' TERM; echo ready > "$1"; exec sleep 30`, "sh", ready)
	StopOnCancel(cmd, 100*time.Millisecond, func(error) {})
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = akitproc.SignalCmd(cmd, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
		if time.Now().After(deadline) {
			t.Fatal("command did not install its SIGTERM handler")
		}
	}
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for running(cmd.Process.Pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if running(cmd.Process.Pid) {
		t.Fatalf("command %d survived the grace after StopChild was unsupported", cmd.Process.Pid)
	}
	select {
	case <-stopCalled:
	default:
		t.Fatal("StopCmd fallback was not called")
	}
	_ = cmd.Wait()
}

func TestStopOnCancelRequestsSIGTERM(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	cmd := exec.CommandContext(ctx, "sh", "-c", `trap 'echo term > "$1/term"; exit 0' TERM
echo ready > "$1/ready"
while :; do sleep 0.02; done`, "sh", dir)
	warned := make(chan error, 1)
	StopOnCancel(cmd, 2*time.Second, func(err error) { warned <- err })
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFile(t, filepath.Join(dir, "ready"))
	cancel()
	if err := cmd.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("cooperative command: got %v, want context cancellation", err)
	}
	if got := waitFile(t, filepath.Join(dir, "term")); got != "term" {
		t.Fatalf("signal marker = %q, want term", got)
	}
	select {
	case err := <-warned:
		t.Errorf("warned: %v", err)
	default:
	}
}

// Once the command has exited, cancellation must not kill the daemon it
// deliberately detached before the grace elapsed.
func TestStopOnCancelKeepsDetachedAfterCommandExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	cmd := exec.CommandContext(ctx, "sh", "-c", `trap 'exit 0' TERM
setsid sleep 30 & echo $! > "$1/daemon"
while :; do sleep 0.02; done`, "sh", dir)
	StopOnCancel(cmd, 300*time.Millisecond, func(err error) { t.Error(err) })
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := pidIn(t, filepath.Join(dir, "daemon"))
	stopAfter(t, pid)
	cancel()
	if err := cmd.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("command: got %v, want context cancellation", err)
	}
	time.Sleep(500 * time.Millisecond) //nolint:test-fixed-sleep-before-assert // observe after the 300ms stop grace
	if !running(pid) {
		t.Fatal("detached daemon was killed after its command exited")
	}
}

func TestStopOnCancelKeepsCallerProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30")
	StopOnCancel(cmd, time.Second, func(err error) { t.Error(err) })
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	got, err := unix.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	want, err := unix.Getpgid(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("command process group = %d, caller group = %d", got, want)
	}
}

func TestStopOnCancelReturnsProcessDoneAfterWait(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "true")
	StopOnCancel(cmd, time.Second, func(err error) { t.Error(err) })
	if err := cmd.Run(); err != nil {
		t.Fatalf("completed command: %v", err)
	}
	if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Cancel after successful Wait = %v, want os.ErrProcessDone", err)
	}
}

func TestStopOnCancelPreservesDeadlineError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", `trap 'exit 0' TERM; while :; do sleep 0.02; done`)
	StopOnCancel(cmd, time.Second, func(err error) { t.Error(err) })
	if err := cmd.Run(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait error = %v, want context.DeadlineExceeded", err)
	}
}

// running tells whether pid is a process that is not a zombie.
func running(pid int) bool {
	zombie, err := isZombie(pid)
	return err == nil && !zombie
}
