//go:build unix

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	akitproc "github.com/aiseeq/akit/proc"
	"github.com/gofrs/flock"
)

func TestTempDirTestMainStartupAndSignals(t *testing.T) {
	t.Run("startup-reaps-dead-run", func(t *testing.T) {
		root := t.TempDir()
		dead := makeDeadTempRun(t, root)
		ready := filepath.Join(root, "ready")
		cmd, output, done, finished := startTempDirHelper(t, root, ready, "")
		runDir := readTempHelperFile(t, ready, done, output)
		if _, err := os.Stat(dead); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("dead run remains after TestMain startup: %v", err)
		}
		if _, err := os.Stat(filepath.Join(runDir, testTempLockName)); err != nil {
			t.Fatalf("live helper run has no lock: %v", err)
		}
		stopTempDirHelper(t, cmd, done, output, syscall.SIGTERM, 128+int(syscall.SIGTERM), runDir)
		<-finished
	})

	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			root := t.TempDir()
			makeDeadTempRun(t, root)
			ready := filepath.Join(root, "ready")
			cmd, output, done, finished := startTempDirHelper(t, root, ready, "")
			runDir := readTempHelperFile(t, ready, done, output)
			stopTempDirHelper(t, cmd, done, output, sig, 128+int(sig), runDir)
			<-finished
		})
	}

	t.Run("signal-during-prepare", func(t *testing.T) {
		root := t.TempDir()
		dead := makeDeadTempRun(t, root)
		pause := filepath.Join(root, "pause")
		cmd, output, done, finished := startTempDirHelper(t, root, "", pause)
		runDir := readTempHelperFile(t, pause, done, output)
		if _, err := os.Stat(dead); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("dead run remains after TestMain startup: %v", err)
		}
		if err := akitproc.SignalCmd(cmd, syscall.SIGTERM); err != nil {
			t.Fatalf("signal test process: %v", err)
		}
		if err := os.WriteFile(pause+".continue", nil, 0o600); err != nil {
			t.Fatal(err)
		}
		assertTempHelperExit(t, done, output, 128+int(syscall.SIGTERM))
		assertTempRunRemoved(t, runDir)
		<-finished
	})
}

func TestTempDirProcessHelper(t *testing.T) {}

func makeDeadTempRun(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "graft-it-dead")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(filepath.Join(dir, testTempLockName))
	locked, err := lock.TryLock()
	if err != nil || !locked {
		t.Fatalf("prepare dead run lock: locked=%v err=%v", locked, err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func startTempDirHelper(t *testing.T, root, ready, pause string) (*exec.Cmd, *bytes.Buffer, <-chan error, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestTempDirProcessHelper$")
	cmd.Env = append(os.Environ(), "TMPDIR="+root, testTempHelperEnv+"=wait", testTempReadyEnv+"="+ready, testTempPauseEnv+"="+pause)
	output := &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		done <- cmd.Wait()
		close(finished)
	}()
	t.Cleanup(func() {
		select {
		case <-finished:
		default:
			_ = akitproc.SignalCmd(cmd, syscall.SIGKILL)
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Errorf("test helper did not stop: %s", output.String())
			}
		}
	})
	return cmd, output, done, finished
}

func readTempHelperFile(t *testing.T, path string, done <-chan error, output *bytes.Buffer) string {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			return string(data)
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read helper marker %s: %v", path, err)
		}
		select {
		case err := <-done:
			t.Fatalf("test helper exited before marker: %v\n%s", err, output.String())
		case <-deadline.C:
			t.Fatalf("timed out waiting for helper marker %s\n%s", path, output.String())
		case <-ticker.C:
		}
	}
}

func stopTempDirHelper(t *testing.T, cmd *exec.Cmd, done <-chan error, output *bytes.Buffer, sig syscall.Signal, code int, runDir string) {
	t.Helper()
	if err := akitproc.SignalCmd(cmd, sig); err != nil {
		t.Fatalf("signal test process with %s: %v", sig, err)
	}
	assertTempHelperExit(t, done, output, code)
	assertTempRunRemoved(t, runDir)
}

func assertTempHelperExit(t *testing.T, done <-chan error, output *bytes.Buffer, code int) {
	t.Helper()
	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != code {
			t.Fatalf("test helper exit = %v, want %d\n%s", err, code, output.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("test helper did not exit\n%s", output.String())
	}
}

func assertTempRunRemoved(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("test helper temp directory remains: %s (stat err %v)", dir, err)
	}
}
