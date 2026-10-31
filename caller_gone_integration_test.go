package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/loadgate"
	"github.com/aiseeq/graft/v2/internal/proc"
)

func callerCommand(t *testing.T, args ...string) (*exec.Cmd, *os.File, *os.File) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses Unix signals and pipes")
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(graftBin, args...)
	cmd.Dir = t.TempDir()
	cmd.Env = append(append([]string{}, testEnv...), "GRAFT_TEST_CALLER_GONE_INTERVAL=30ms")
	cmd.Stdout, cmd.Stderr = stdoutW, stderrW
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = stdoutW.Close()
	_ = stderrW.Close()
	t.Cleanup(func() {
		_ = stdoutR.Close()
		_ = stderrR.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return cmd, stdoutR, stderrR
}

func waitProcess(t *testing.T, cmd *exec.Cmd) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(4 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("graft did not exit")
		return nil
	}
}

func TestSignalStopsTheCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses Unix signals")
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			t.Run("exec", func(t *testing.T) {
				marker := filepath.Join(t.TempDir(), "finished")
				cmd, out, errOut := callerCommand(t, "exec", "--", "sh", "-c", `trap 'kill "$child"; wait "$child"; exit 0' TERM; (sleep 0.45; : > "$1"; sleep 20) & child=$!; echo ready; wait "$child"`, "sh", marker)
				ready, err := bufio.NewReader(out).ReadString('\n')
				if err != nil || ready != "ready\n" {
					t.Fatalf("command did not start: %q, %v", ready, err)
				}
				time.Sleep(80 * time.Millisecond)
				if err := cmd.Process.Signal(sig); err != nil {
					t.Fatal(err)
				}
				waitForFileAbsent(t, marker, 500*time.Millisecond)
				waitForEOF(t, out, errOut)
				if err := waitProcess(t, cmd); err == nil {
					t.Fatal("graft exited successfully after cancellation")
				} else if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 125 {
					t.Fatalf("graft exit = %v, want 125", err)
				}
			})
			t.Run("task", func(t *testing.T) {
				marker := filepath.Join(t.TempDir(), "finished")
				script := `trap 'kill "$child"; wait "$child"; exit 0' TERM; (sleep 0.6; : > ` + marker + `) & child=$!; echo ready; wait "$child"`
				f := tasksRepo(t, "tasks:\n  wait:\n    run: [{sh: "+yamlQuote(script)+"}]\n", nil)
				cmd, out, errOut := startPiped(t, f.work, f.env, "wait")
				ready, err := bufio.NewReader(out).ReadString('\n')
				if err != nil || ready != "ready\n" {
					t.Fatalf("task step did not start: %q, %v", ready, err)
				}
				if err := cmd.Process.Signal(sig); err != nil {
					t.Fatal(err)
				}
				waitForFileAbsent(t, marker, 800*time.Millisecond)
				if err := waitProcess(t, cmd); err == nil {
					t.Fatal("graft task exited successfully after cancellation")
				}
				_ = out.Close()
				_ = errOut.Close()
			})
		})
	}
}

func waitForFileAbsent(t *testing.T, path string, duration time.Duration) {
	t.Helper()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	for {
		select {
		case <-ticker.C:
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("command reached its marker: %v", err)
			}
		case <-timer.C:
			return
		}
	}
}

func waitForEOF(t *testing.T, files ...*os.File) {
	t.Helper()
	done := make(chan struct{}, len(files))
	for _, file := range files {
		go func(file *os.File) {
			_, _ = io.Copy(io.Discard, file)
			done <- struct{}{}
		}(file)
	}
	for range files {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("output pipe stayed open after graft exited")
		}
	}
}

func TestCallerGoneIntervalRejectsInvalidValue(t *testing.T) {
	t.Setenv("GRAFT_TEST_CALLER_GONE_INTERVAL", "invalid")
	if _, err := proc.CallerGoneInterval(); err == nil || !strings.Contains(err.Error(), `GRAFT_TEST_CALLER_GONE_INTERVAL="invalid"`) {
		t.Fatalf("invalid interval error = %v", err)
	}
}

func TestIgnoredHangupKeepsRunning(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "finished")
	cmd := exec.Command("sh", "-c", "trap '' HUP; exec \"$1\" exec -- sh -c 'sleep 0.15; : > \"$1\"' sh \"$2\"", "sh", graftBin, marker)
	cmd.Dir = t.TempDir()
	cmd.Env = append(append([]string{}, testEnv...), "GRAFT_TEST_CALLER_GONE_INTERVAL=30ms")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	_ = cmd.Process.Signal(syscall.SIGHUP)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("graft: %v\n%s", err, output.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("command did not finish: %v", err)
	}
}

func TestCallerGoneStopsTheCommand(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "finished")
	cmd, out, errOut := callerCommand(t, "exec", "--", "sh", "-c", `trap 'kill "$child"; wait "$child"; exit 0' TERM; (sleep 0.45; : > "$1"; sleep 20) & child=$!; echo ready; wait "$child"`, "sh", marker)
	_ = out.Close()
	_ = errOut.Close()
	waitForFileAbsent(t, marker, 500*time.Millisecond)
	if err := waitProcess(t, cmd); err == nil {
		t.Fatal("graft exited successfully after both readers closed")
	}
}

func TestCallerGoneWaitingDoesNotStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses Unix pipes")
	}
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none}\n"})
	procDir, cfgDir := t.TempDir(), t.TempDir()
	writeTestPressure(t, cfgDir)
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, fakeMachine(t, procDir, 30), "GRAFT_TEST_CALLER_GONE_INTERVAL=30ms")
	marker := filepath.Join(t.TempDir(), "started")
	cmd, out, errOut := startPiped(t, f.work, f.env, "exec", "--heavy", "--local", "--", "sh", "-c", ": > \"$1\"; sleep 20", "sh", marker)
	waitForPipeText(t, "runnable", out, errOut)
	_ = out.Close()
	_ = errOut.Close()
	_ = waitProcess(t, cmd)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("queued command started: %v", err)
	}
}

func TestCallerGoneOneChannelKeepsRunning(t *testing.T) {
	cmd, out, errOut := callerCommand(t, "exec", "--", "sh", "-c", "sleep 0.15; printf 'alive\\n'")
	_ = errOut.Close()
	data, _ := bufio.NewReader(out).ReadString('\n')
	_ = out.Close()
	if err := waitProcess(t, cmd); err != nil || data != "alive\n" {
		t.Fatalf("exit %v, stdout %q", err, data)
	}
}

func TestCallerGoneHeadWithStderrFile(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "finished")
	stderrPath := filepath.Join(t.TempDir(), "stderr")
	out, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.Create(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(graftBin, "exec", "--", "sh", "-c", "echo first; sleep 0.15; : > \"$1\"", "sh", marker)
	cmd.Dir = t.TempDir()
	cmd.Env = append(append([]string{}, testEnv...), "GRAFT_TEST_CALLER_GONE_INTERVAL=30ms")
	cmd.Stdout, cmd.Stderr = writer, stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	_ = stderr.Close()
	line, _ := bufio.NewReader(out).ReadString('\n')
	_ = out.Close()
	if line != "first\n" {
		t.Fatalf("first line %q", line)
	}
	if err := waitProcess(t, cmd); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("command did not finish: %v", err)
	}
}

// TestSigtermWhileWaitingDoesNotStart: a cancel that lands at admission
// itself needs no check of graft's: exec.Cmd.Start of a cancelled context
// starts nothing, and a started one StopOnCancel stops.
func TestSigtermWhileWaitingDoesNotStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses Unix signals")
	}
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none}\n"})
	procDir, cfgDir := t.TempDir(), t.TempDir()
	writeTestPressure(t, cfgDir)
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, fakeMachine(t, procDir, 30), "GRAFT_TEST_CALLER_GONE_INTERVAL=30ms")
	marker := filepath.Join(t.TempDir(), "started")
	cmd, out, errOut := startPiped(t, f.work, f.env, "exec", "--heavy", "--local", "--", "sh", "-c", ": > \"$1\"", "sh", marker)
	waitForPipeText(t, "runnable", out, errOut)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = out.Close()
	_ = errOut.Close()
	_ = waitProcess(t, cmd)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("command unexpectedly started")
	}
}

func TestLocalStepTreeKilledAfterGrace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses Unix process groups")
	}
	marker := filepath.Join(t.TempDir(), "finished")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", `trap '' TERM; (sleep 0.4; : > "$1"; sleep 20) & wait`, "sh", marker)
	proc.StopOnCancel(cmd, 200*time.Millisecond, func(error) {})
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waitForFileAbsent(t, marker, 450*time.Millisecond)
	if err := <-done; err == nil {
		t.Fatal("cancelled step returned successfully")
	}
}

func TestDeployKeepsTodaysSignals(t *testing.T) {
	t.Setenv(loadgate.EnvPriority, "")
	got := signalSet([]string{"deploy", "prod"})
	if len(got) != 1 || got[0] != os.Interrupt {
		t.Fatalf("deploy signals = %v", got)
	}
	t.Setenv(loadgate.EnvPriority, "task")
	got = signalSet([]string{"exec", "--", "true"})
	if len(got) != 1 || got[0] != os.Interrupt || callerGoneWatch([]string{"exec"}) {
		t.Fatalf("priority signals/watch = %v/%v", got, callerGoneWatch([]string{"exec"}))
	}
	if callerGoneWatch([]string{"build"}) {
		t.Fatal("priority task enabled caller watcher")
	}
}

func startPiped(t *testing.T, dir string, env []string, args ...string) (*exec.Cmd, *os.File, *os.File) {
	t.Helper()
	r1, w1, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	r2, w2, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(graftBin, args...)
	cmd.Dir = dir
	cmd.Env = append(append([]string{}, testEnv...), env...)
	cmd.Stdout = w1
	cmd.Stderr = w2
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	_ = w1.Close()
	_ = w2.Close()
	t.Cleanup(func() {
		_ = r1.Close()
		_ = r2.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return cmd, r1, r2
}

func waitForPipeText(t *testing.T, text string, files ...*os.File) {
	t.Helper()
	lines := make(chan string, len(files)*8)
	for _, file := range files {
		go func(file *os.File) {
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				lines <- scanner.Text()
			}
		}(file)
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var seen []string
	for {
		select {
		case line := <-lines:
			seen = append(seen, line)
			if strings.Contains(line, text) {
				return
			}
		case <-timer.C:
			t.Fatalf("did not see %q while waiting for queue admission; output: %s", text, strings.Join(seen, "\n"))
		}
	}
}
