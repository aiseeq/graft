package services

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	akitproc "github.com/aiseeq/akit/proc"
	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/testutil"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	if err := akitproc.BecomeChildSubreaper(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func TestFailedStartStopsServiceAndChild(t *testing.T) {
	t.Run("not listening", func(t *testing.T) {
		failedStartStopsChild(t, "echo $$ > leader\nsleep 30 &\necho $! > child\nwait\n", "not listening")
	})
	// The leader exits first: its child is reparented outside graft and is
	// reached only through its process group.
	t.Run("leader exited", func(t *testing.T) {
		failedStartStopsChild(t, "echo $$ > leader\nsleep 30 &\necho $! > child\nexit 1\n", "exited before listening")
	})
}

func TestReservedTCPAddressCannotBeListenedTo(t *testing.T) {
	reservation, err := testutil.ReserveRefusedTCPAddr()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reservation.Close() })
	addr := reservation.Addr()
	if listener, err := net.Listen("tcp", addr); err == nil {
		_ = listener.Close()
		t.Fatalf("another listener occupied reserved address %s", addr)
	} else if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("listen error = %v, want address in use", err)
	}
	conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("connect to reserved address %s succeeded", addr)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("connect error = %v, want connection refused", err)
	}
}
func failedStartStopsChild(t *testing.T, body, want string) {
	t.Helper()
	dir := t.TempDir()
	// Cleanup also handles the mutant that leaves the service running.
	t.Cleanup(func() {
		for _, name := range []string{"leader", "child"} {
			data, _ := os.ReadFile(filepath.Join(dir, name))
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				_ = akitproc.SignalDescendant(pid, syscall.SIGKILL)
			}
		}
	})
	reservation, err := testutil.ReserveRefusedTCPAddr()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reservation.Close() })
	addr := reservation.Addr()
	script := filepath.Join(dir, "service.sh")
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &config.Service{Run: config.Command{Argv: []string{"sh", script}}, Log: "service.log", PIDFile: "service.pid", Addr: addr, StartTimeout: 2 * time.Second}
	cfg := &config.Config{Services: map[string]*config.Service{"svc": s}}
	m := New(dir, cfg, nil, io.Discard)
	ctx := t.Context()
	result := make(chan error, 1)
	go func() { result <- m.start(ctx, "svc") }()

	started := make(map[int]akitproc.Stat)
	for _, name := range []string{"leader", "child"} {
		var pid int
		deadline := time.Now().Add(time.Second)
		for pid == 0 && time.Now().Before(deadline) {
			data, _ := os.ReadFile(filepath.Join(dir, name))
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if pid == 0 {
				time.Sleep(10 * time.Millisecond)
			}
		}
		if pid == 0 {
			t.Fatalf("the service wrote no %s pid", name)
		}
		stat, err := akitproc.ReadStat("/proc", pid)
		if akitproc.Gone(err) {
			continue // already gone: the leader exited or graft stopped it
		}
		if err != nil {
			t.Fatalf("reading %s process %d: %v", name, pid, err)
		}
		started[pid] = stat
	}
	if err := <-result; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("start error = %v, want %s", err, want)
	}
	for pid, original := range started {
		deadline := time.Now().Add(2 * time.Second)
		for {
			stat, err := akitproc.ReadStat("/proc", pid)
			if akitproc.Gone(err) || err == nil && (stat.Start != original.Start || stat.State == "Z" || stat.State == "X") {
				break
			}
			if err != nil {
				t.Fatalf("reading process %d: %v", pid, err)
			}
			if time.Now().After(deadline) {
				t.Errorf("process %d survived failed start", pid)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestStopWarnsWhenSessionLeaderExitedBeforeStop(t *testing.T) {
	dir := t.TempDir()
	workerFile := filepath.Join(dir, "worker")
	cmd := exec.Command("sh", "-c", `sleep 30 & echo $! > "$1"; echo ready; while :; do sleep 0.02; done`, "sh", workerFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = akitproc.SignalCmd(cmd, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	}()
	var ready string
	if _, err := fmt.Fscan(stdout, &ready); err != nil || ready != "ready" {
		t.Fatalf("leader readiness = %q, %v", ready, err)
	}
	worker := serviceTestPID(t, workerFile)
	stopServiceTestProcess(t, worker)
	stat, err := akitproc.ReadStat("/proc", cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Services: map[string]*config.Service{"svc": {PIDFile: "service.pid", StopTimeout: 200 * time.Millisecond}}}
	m := New(dir, cfg, nil, io.Discard)
	if err := writePID(m.path("service.pid"), cmd.Process.Pid, stat.Start); err != nil {
		t.Fatal(err)
	}
	if err := akitproc.SignalCmd(cmd, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatal(err)
		}
	}
	m.Log = &syncLog{}
	err = m.stop(t.Context(), "svc")
	if err == nil || !strings.Contains(err.Error(), "remaining session processes were not stopped") {
		t.Fatalf("stop error = %v, want warning that session members remain", err)
	}
	if !strings.Contains(m.Log.(*syncLog).String(), "remaining session processes were not stopped") {
		t.Fatalf("stop log lacks orphan warning: %s", m.Log.(*syncLog).String())
	}
	if !serviceTestRunning(worker) {
		t.Fatal("test worker unexpectedly stopped")
	}
}

// The leader leaves on SIGTERM, a worker of its session ignores it: the stop
// begun on the live leader is carried through, the worker killed after grace.
func TestStopFinishesSessionAfterLeaderExits(t *testing.T) {
	dir := t.TempDir()
	leaderFile, workerFile := filepath.Join(dir, "leader"), filepath.Join(dir, "worker")
	script := filepath.Join(dir, "service.sh")
	body := `echo $$ > "$1"; sh -c 'trap "" TERM; exec sleep 30' & echo $! > "$2"; trap 'exit 0' TERM; while :; do sleep 0.02; done`
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &config.Service{Run: config.Command{Argv: []string{"sh", script, leaderFile, workerFile}}, Log: "service.log", PIDFile: "service.pid", StopTimeout: 300 * time.Millisecond}
	cfg := &config.Config{Services: map[string]*config.Service{"svc": s}}
	m := New(dir, cfg, nil, io.Discard)
	if err := m.start(t.Context(), "svc"); err != nil {
		t.Fatal(err)
	}
	leader, worker := serviceTestPID(t, leaderFile), serviceTestPID(t, workerFile)
	stopServiceTestProcess(t, worker)
	m.Log = &syncLog{}
	if err := m.stop(t.Context(), "svc"); err != nil {
		t.Fatalf("stop error = %v, log: %s", err, m.Log.(*syncLog).String())
	}
	// SIGKILL is delivered by the time stop returns, the exit may lag a little.
	for deadline := time.Now().Add(2 * time.Second); serviceTestRunning(worker); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("worker %d of leader %d still runs after the stop; log: %s", worker, leader, m.Log.(*syncLog).String())
		}
	}
}

func TestStopCommandStopsServiceWithoutSignals(t *testing.T) {
	dir := t.TempDir()
	signalFile := filepath.Join(dir, "signal")
	if err := os.WriteFile(signalFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "service.sh")
	workerFile := filepath.Join(dir, "worker")
	workerScript := filepath.Join(dir, "worker.sh")
	workerBody := `trap 'echo TERM >> "$1"; exit 0' TERM; while :; do sleep 1; done`
	if err := os.WriteFile(workerScript, []byte(workerBody), 0o600); err != nil {
		t.Fatal(err)
	}
	body := `sh "$3" "$2" & echo $! > "$4"; trap 'echo TERM >> "$2"; exit 0' TERM; while [ ! -f "$1" ]; do sleep 0.02; done; sleep 0.25; exit 0`
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &config.Service{
		Run: config.Command{Argv: []string{"sh", script, "stop", signalFile, workerScript, workerFile}}, Stop: "touch stop",
		Log: "service.log", PIDFile: "service.pid", StopTimeout: time.Second,
	}
	m := New(dir, &config.Config{Services: map[string]*config.Service{"svc": s}}, nil, io.Discard)
	if err := m.start(t.Context(), "svc"); err != nil {
		t.Fatal(err)
	}
	pid := serviceTestPID(t, m.path(s.PIDFile))
	worker := serviceTestPID(t, workerFile)
	stopServiceTestProcess(t, pid)
	stopServiceTestProcess(t, worker)
	m.Log = &syncLog{}
	if err := m.stop(t.Context(), "svc"); err != nil {
		t.Fatalf("stop error = %v, log: %s", err, m.Log.(*syncLog).String())
	}
	if got, err := os.ReadFile(signalFile); err != nil || len(got) != 0 {
		t.Fatalf("received signals = %q, %v", got, err)
	}
	if !serviceTestRunning(worker) {
		t.Fatal("worker received a signal after the service stopped gracefully")
	}
}

func TestStopCommandFallsBackToSignals(t *testing.T) {
	dir := t.TempDir()
	signalFile := filepath.Join(dir, "signal")
	if err := os.WriteFile(signalFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "service.sh")
	body := `trap 'echo TERM >> "$1"; exit 0' TERM; while :; do sleep 0.02; done`
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &config.Service{
		Run: config.Command{Argv: []string{"sh", script, signalFile}}, Stop: ":",
		Log: "service.log", PIDFile: "service.pid", StopTimeout: 100 * time.Millisecond,
	}
	m := New(dir, &config.Config{Services: map[string]*config.Service{"svc": s}}, nil, io.Discard)
	if err := m.start(t.Context(), "svc"); err != nil {
		t.Fatal(err)
	}
	pid := serviceTestPID(t, m.path(s.PIDFile))
	stopServiceTestProcess(t, pid)
	m.Log = &syncLog{}
	if err := m.stop(t.Context(), "svc"); err != nil {
		t.Fatalf("stop error = %v, log: %s", err, m.Log.(*syncLog).String())
	}
	if got := m.Log.(*syncLog).String(); !strings.Contains(got, "stop command did not stop the service") {
		t.Fatalf("stop log lacks fallback message: %s", got)
	}
	if got, err := os.ReadFile(signalFile); err != nil || !strings.Contains(string(got), "TERM") {
		t.Fatalf("received signals = %q, %v", got, err)
	}
}

func TestStopCommandTimeoutKillsItsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	stopChildFile := filepath.Join(dir, "stop-child")
	s := &config.Service{
		Run:  config.Command{Argv: []string{"sh", "-c", "while :; do sleep 1; done"}},
		Stop: "sleep 30 & echo $! > stop-child; wait", Log: "service.log", PIDFile: "service.pid", StopTimeout: 150 * time.Millisecond,
	}
	m := New(dir, &config.Config{Services: map[string]*config.Service{"svc": s}}, nil, io.Discard)
	if err := m.start(t.Context(), "svc"); err != nil {
		t.Fatal(err)
	}
	servicePID := serviceTestPID(t, m.path(s.PIDFile))
	stopServiceTestProcess(t, servicePID)
	m.Log = io.Discard
	started := time.Now()
	if err := m.stop(t.Context(), "svc"); err != nil {
		t.Fatalf("stop error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stop took %s, want return near stop_timeout", elapsed)
	}
	stopChild := serviceTestPID(t, stopChildFile)
	if serviceTestRunning(stopChild) {
		t.Fatalf("stop command child %d survived timeout", stopChild)
	}
}

func TestStopServiceAfterExec(t *testing.T) {
	dir := t.TempDir()
	s := &config.Service{
		Run:  config.Command{Argv: []string{"sh", "-c", "exec sleep 30"}},
		Stop: "touch stop-ran", Log: "service.log", PIDFile: "service.pid", StopTimeout: 200 * time.Millisecond,
	}
	var log syncLog
	m := New(dir, &config.Config{Services: map[string]*config.Service{"svc": s}}, nil, &log)
	if err := m.start(t.Context(), "svc"); err != nil {
		t.Fatal(err)
	}
	pid := serviceTestPID(t, m.path(s.PIDFile))
	stopServiceTestProcess(t, pid)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		name, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		if err == nil && strings.TrimSpace(string(name)) == "sleep" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("service did not exec sleep: %q, %v", name, err)
		}
	}
	if err := m.stop(t.Context(), "svc"); err != nil {
		t.Fatalf("stop: %v; log: %s", err, log.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "stop-ran")); err != nil {
		t.Fatalf("stop command did not run: %v; log: %s", err, log.String())
	}
	if strings.Contains(log.String(), "removing stale pid file") || serviceTestRunning(pid) {
		t.Fatalf("service %d survived or was treated as stale: %s", pid, log.String())
	}
}

func TestStopServiceWithLegacyStartRecord(t *testing.T) {
	dir := t.TempDir()
	s := &config.Service{
		Run:  config.Command{Argv: []string{"sh", "-c", "exec sleep 30"}},
		Stop: "touch stop-ran", Log: "service.log", PIDFile: "service.pid", StopTimeout: 200 * time.Millisecond,
	}
	var log syncLog
	m := New(dir, &config.Config{Services: map[string]*config.Service{"svc": s}}, nil, &log)
	if err := m.start(t.Context(), "svc"); err != nil {
		t.Fatal(err)
	}
	pid := serviceTestPID(t, m.path(s.PIDFile))
	stopServiceTestProcess(t, pid)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		name, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		if err == nil && strings.TrimSpace(string(name)) == "sleep" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("service did not exec sleep: %q, %v", name, err)
		}
	}
	stat, err := akitproc.ReadStat("/proc", pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.path(s.PIDFile)+".start", []byte(stat.Start+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.stop(t.Context(), "svc"); err != nil {
		t.Fatalf("stop with legacy start record: %v; log: %s", err, log.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "stop-ran")); err != nil {
		t.Fatalf("stop command did not run: %v; log: %s", err, log.String())
	}
	if serviceTestRunning(pid) {
		t.Fatalf("service %d survived stop", pid)
	}
}

func TestLegacyStartRecordRejectsReusedPID(t *testing.T) {
	cmd := startTestSleep(t)
	stat, err := akitproc.ReadStat("/proc", cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	s := &config.Service{Run: config.Command{Argv: []string{"sleep", "30"}}, PIDFile: "service.pid"}
	m := New(dir, &config.Config{Services: map[string]*config.Service{"svc": s}}, nil, io.Discard)
	if err := os.WriteFile(m.path(s.PIDFile), []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.path(s.PIDFile)+".start", []byte(stat.Start+"0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pid, err := m.running("svc"); err != nil || pid != 0 {
		t.Fatalf("running = %d, %v, want stale PID removed", pid, err)
	}
	for _, path := range []string{m.path(s.PIDFile), m.path(s.PIDFile) + ".start"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale record %s remains: %v", path, err)
		}
	}
	if !serviceTestRunning(cmd.Process.Pid) {
		t.Fatal("unrelated process was signaled")
	}
}

func TestRunningRejectsReusedPIDWithMatchingArgv(t *testing.T) {
	cmd := startTestSleep(t)
	stat, err := akitproc.ReadStat("/proc", cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	s := &config.Service{Run: config.Command{Argv: []string{"sleep", "30"}}, PIDFile: "service.pid"}
	m := New(dir, &config.Config{Services: map[string]*config.Service{"svc": s}}, nil, io.Discard)
	if err := writePID(m.path(s.PIDFile), cmd.Process.Pid, stat.Start+"0"); err != nil {
		t.Fatal(err)
	}
	if pid, err := m.running("svc"); err != nil || pid != 0 {
		t.Fatalf("running = %d, %v, want stale PID removed", pid, err)
	}
	for _, path := range []string{m.path(s.PIDFile), m.path(s.PIDFile) + ".start"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale record %s remains: %v", path, err)
		}
	}
	if !serviceTestRunning(cmd.Process.Pid) {
		t.Fatal("unrelated process was stopped")
	}
	if err := writePID(m.path(s.PIDFile), cmd.Process.Pid, stat.Start+"0"); err != nil {
		t.Fatal(err)
	}
	if err := m.stop(t.Context(), "svc"); err == nil {
		t.Fatal("stop accepted a different process")
	}
	if _, err := os.Stat(m.path(s.PIDFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale PID file remains after stop: %v", err)
	}
	if !serviceTestRunning(cmd.Process.Pid) {
		t.Fatal("stop signaled the unrelated process")
	}
	t.Run("legacy PID file", checkRunningLegacyPIDFileChecksArgv)
}

func checkRunningLegacyPIDFileChecksArgv(t *testing.T) {
	cmd := startTestSleep(t)
	dir := t.TempDir()
	s := &config.Service{Run: config.Command{Argv: []string{"sleep", "30"}}, PIDFile: "service.pid"}
	m := New(dir, &config.Config{Services: map[string]*config.Service{"svc": s}}, nil, io.Discard)
	if err := writePID(m.path(s.PIDFile), cmd.Process.Pid, ""); err != nil {
		t.Fatal(err)
	}
	if pid, err := m.running("svc"); err != nil || pid != cmd.Process.Pid {
		t.Fatalf("matching legacy PID = %d, %v; the process now: %s", pid, err, describeTestProcess(cmd.Process.Pid))
	}
	s.Run.Argv[0] = "sh"
	if pid, err := m.running("svc"); err == nil || pid != 0 || !strings.Contains(err.Error(), "cannot confirm process") {
		t.Fatalf("mismatched legacy PID = %d, %v, want unconfirmed error", pid, err)
	}
	if _, err := os.Stat(m.path(s.PIDFile)); err != nil {
		t.Fatalf("legacy PID file was removed: %v", err)
	}
}

func TestRunningLegacyPIDExitsBeforeCmdline(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = akitproc.SignalCmd(cmd, syscall.SIGKILL); _ = cmd.Wait() })
	dir := t.TempDir()
	s := &config.Service{Run: config.Command{Argv: []string{"sleep", "30"}}, PIDFile: "service.pid"}
	m := New(dir, &config.Config{Services: map[string]*config.Service{"svc": s}}, nil, io.Discard)
	if err := writePID(m.path(s.PIDFile), cmd.Process.Pid, ""); err != nil {
		t.Fatal(err)
	}
	old := readServiceCmdline
	readServiceCmdline = func(pid int) ([]string, error) {
		if err := akitproc.SignalCmd(cmd, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		var info unix.Siginfo
		if err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil); err != nil {
			t.Fatal(err)
		}
		return old(pid)
	}
	t.Cleanup(func() { readServiceCmdline = old })
	if pid, err := m.running("svc"); err != nil || pid != 0 {
		t.Fatalf("running after exit = %d, %v, want stale PID removed", pid, err)
	}
	if _, err := os.Stat(m.path(s.PIDFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale PID file remains: %v", err)
	}
}

func TestStopTimeoutCoversCommandAndServiceWaitTogether(t *testing.T) {
	dir := t.TempDir()
	signalFile := filepath.Join(dir, "signal")
	if err := os.WriteFile(signalFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &config.Service{
		Run:  config.Command{Argv: []string{"sh", "-c", `trap 'echo TERM >> "$1"; exit 0' TERM; while :; do sleep 0.02; done`, "sh", signalFile}},
		Stop: "touch stop-started; sleep 3; touch stop-finished", Log: "service.log", PIDFile: "service.pid", StopTimeout: 5 * time.Second,
	}
	serviceScript := filepath.Join(dir, "service.sh")
	body := `trap 'echo TERM >> "$1"; exit 0' TERM; while [ ! -f stop-finished ]; do sleep 0.02; done; sleep 3; exit 0`
	if err := os.WriteFile(serviceScript, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s.Run = config.Command{Argv: []string{"sh", serviceScript, signalFile}}
	m := New(dir, &config.Config{Services: map[string]*config.Service{"svc": s}}, nil, io.Discard)
	if err := m.start(t.Context(), "svc"); err != nil {
		t.Fatal(err)
	}
	servicePID := serviceTestPID(t, m.path(s.PIDFile))
	stopServiceTestProcess(t, servicePID)
	m.Log = &syncLog{}
	if err := m.stop(t.Context(), "svc"); err != nil {
		t.Fatalf("stop error = %v, log: %s", err, m.Log.(*syncLog).String())
	}
	if _, err := os.Stat(filepath.Join(dir, "stop-started")); err != nil {
		t.Fatalf("stop command did not start: %v; log: %s", err, m.Log.(*syncLog).String())
	}
	if _, err := os.Stat(filepath.Join(dir, "stop-finished")); err != nil {
		t.Fatalf("stop command did not finish: %v", err)
	}
	if got := m.Log.(*syncLog).String(); !strings.Contains(got, "stop command did not stop the service in 5s") {
		t.Fatalf("stop log lacks shared-deadline fallback: %s", got)
	}
	if got, err := os.ReadFile(signalFile); err != nil || !strings.Contains(string(got), "TERM") {
		t.Fatalf("service received signals = %q, %v", got, err)
	}
}

func TestStopCommandReapsOrphansAfterLeaderExits(t *testing.T) {
	for _, logMode := range []string{"pipe", "file"} {
		t.Run(logMode, func(t *testing.T) {
			dir := t.TempDir()
			s := &config.Service{
				Run:  config.Command{Argv: []string{"sh", "-c", "exec sleep 30"}},
				Stop: "sh -c 'sleep 8; touch natural-exit' & echo $! > child; exit 0", Log: "service.log", PIDFile: "service.pid", StopTimeout: 2 * time.Second,
			}
			m := New(dir, &config.Config{Services: map[string]*config.Service{"svc": s}}, nil, io.Discard)
			if err := m.start(t.Context(), "svc"); err != nil {
				t.Fatal(err)
			}
			pid := serviceTestPID(t, m.path(s.PIDFile))
			stopServiceTestProcess(t, pid)
			var logFile *os.File
			var pipeLog syncLog
			if logMode == "pipe" {
				m.Log = &pipeLog
			} else {
				var err error
				logFile, err = os.Create(filepath.Join(dir, "stop.log"))
				if err != nil {
					t.Fatal(err)
				}
				defer logFile.Close()
				m.Log = logFile
			}
			result := make(chan error, 1)
			go func() { result <- m.stop(t.Context(), "svc") }()
			child := serviceTestPID(t, filepath.Join(dir, "child"))
			stopServiceTestProcess(t, child)
			if err := akitproc.SignalDescendant(pid, syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			log := pipeLog.String()
			if logFile != nil {
				data, err := os.ReadFile(logFile.Name())
				if err != nil {
					t.Fatal(err)
				}
				log = string(data)
			}
			if strings.Contains(log, "falling back to signals") {
				t.Fatalf("stop command fell back to signals: %s", log)
			}
			if _, err := os.Stat(filepath.Join(dir, "natural-exit")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stop command child reached its natural exit: %v", err)
			}
			if serviceTestRunning(child) {
				t.Fatalf("stop command child %d survived return with %s log", child, logMode)
			}
		})
	}
}

func TestStopCommandFailureFallsBackToStopSession(t *testing.T) {
	dir := t.TempDir()
	signalFile := filepath.Join(dir, "signal")
	if err := os.WriteFile(signalFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &config.Service{
		Run:  config.Command{Argv: []string{"sh", "-c", `trap 'echo TERM >> "$1"; exit 0' TERM; while :; do sleep 0.02; done`, "sh", signalFile}},
		Stop: "exit 17", Log: "service.log", PIDFile: "service.pid", StopTimeout: time.Second,
	}
	m := New(dir, &config.Config{Services: map[string]*config.Service{"svc": s}}, nil, io.Discard)
	if err := m.start(t.Context(), "svc"); err != nil {
		t.Fatal(err)
	}
	servicePID := serviceTestPID(t, m.path(s.PIDFile))
	stopServiceTestProcess(t, servicePID)
	m.Log = &syncLog{}
	if err := m.stop(t.Context(), "svc"); err != nil {
		t.Fatalf("stop error = %v, log: %s", err, m.Log.(*syncLog).String())
	}
	got := m.Log.(*syncLog).String()
	if !strings.Contains(got, "stop command did not succeed") {
		t.Fatalf("stop log lacks command failure: %s", got)
	}
	if data, err := os.ReadFile(signalFile); err != nil || !strings.Contains(string(data), "TERM") {
		t.Fatalf("service received signals = %q, %v", data, err)
	}
}

func TestStopCommandUsesServiceEnvironmentAndDirectory(t *testing.T) {
	dir := t.TempDir()
	s := &config.Service{
		Run:  config.Command{Argv: []string{"sh", "-c", `while [ ! -f stop ]; do sleep 0.02; done`}},
		Stop: `printf '%s:%s' "$SERVICE_MARKER" "$PWD" > stop-result; touch stop`,
		Env:  map[string]string{"SERVICE_MARKER": "service-env"},
		Log:  "service.log", PIDFile: "service.pid", StopTimeout: time.Second,
	}
	m := New(dir, &config.Config{Services: map[string]*config.Service{"svc": s}}, nil, io.Discard)
	if err := m.start(t.Context(), "svc"); err != nil {
		t.Fatal(err)
	}
	servicePID := serviceTestPID(t, m.path(s.PIDFile))
	stopServiceTestProcess(t, servicePID)
	if err := m.stop(t.Context(), "svc"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "stop-result"))
	if err != nil || string(got) != "service-env:"+dir {
		t.Fatalf("stop result = %q, %v; want service env and directory %q", got, err, dir)
	}
}

func TestStopCommandIsNotRunForDeadService(t *testing.T) {
	dir := t.TempDir()
	s := &config.Service{
		Run: config.Command{Argv: []string{"sh", "-c", "exec sleep 30"}}, Stop: "touch stop-ran",
		Log: "service.log", PIDFile: "service.pid", StopTimeout: time.Second,
	}
	m := New(dir, &config.Config{Services: map[string]*config.Service{"svc": s}}, nil, io.Discard)
	if err := writePID(m.path(s.PIDFile), 2_147_483_647, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.stop(t.Context(), "svc"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stop-ran")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stop command was run for a dead service: stat error = %v", err)
	}
}

func serviceTestPID(t *testing.T, path string) int {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		data, err := os.ReadFile(path)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				return pid
			}
		}
	}
	t.Fatalf("%s contains no process ID", path)
	return 0
}

// describeTestProcess tells the state, start time and argv of pid, so a
// failure shows whether the process had exited, was another one or was
// read in an unexpected state.
func describeTestProcess(pid int) string {
	stat, statErr := akitproc.ReadStat("/proc", pid)
	argv, argvErr := readServiceCmdline(pid)
	return fmt.Sprintf("stat %+v (%v), argv %q (%v)", stat, statErr, argv, argvErr)
}

// startTestSleep starts sleep 30 and returns once /proc shows its argv:
// right after fork, and for a moment inside execve, the comm is already
// sleep while cmdline is still empty, so a check of the argv there fails.
func startTestSleep(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = akitproc.SignalCmd(cmd, syscall.SIGKILL); _ = cmd.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	for {
		argv, err := readServiceCmdline(cmd.Process.Pid)
		if err == nil && len(argv) == 2 && filepath.Base(argv[0]) == "sleep" {
			return cmd
		}
		if time.Now().After(deadline) {
			t.Fatalf("sleep did not show its argv: %s", describeTestProcess(cmd.Process.Pid))
		}
		time.Sleep(time.Millisecond)
	}
}

func serviceTestRunning(pid int) bool {
	stat, err := akitproc.ReadStat("/proc", pid)
	return err == nil && stat.State != "Z" && stat.State != "X"
}

func stopServiceTestProcess(t *testing.T, pid int) {
	t.Helper()
	t.Cleanup(func() {
		if serviceTestRunning(pid) {
			if err := akitproc.SignalDescendant(pid, syscall.SIGKILL); err != nil && !akitproc.Gone(err) {
				t.Errorf("stopping test process %d: %v", pid, err)
			}
		}
	})
}

func TestStartWithoutProcRecordsPIDOnly(t *testing.T) {
	oldReadStat := readServiceStat
	readServiceStat = func(string, int) (akitproc.Stat, error) { return akitproc.Stat{}, errors.ErrUnsupported }
	t.Cleanup(func() { readServiceStat = oldReadStat })
	dir := t.TempDir()
	s := &config.Service{Run: config.Command{Argv: []string{"sh", "-c", "sleep 30"}}, Log: "service.log", PIDFile: "service.pid", StartTimeout: time.Second}
	cfg := &config.Config{Services: map[string]*config.Service{"svc": s}}
	m := New(dir, cfg, nil, io.Discard)
	if err := m.start(t.Context(), "svc"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(m.path(s.PIDFile))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("PID record %q, %v", data, err)
	}
	if _, err := os.Stat(m.path(s.PIDFile) + ".start"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("start record exists without /proc: %v", err)
	}
	t.Cleanup(func() {
		if err := akitproc.SignalDescendant(pid, syscall.SIGKILL); err != nil && !akitproc.Gone(err) {
			t.Errorf("stopping service test child %d: %v", pid, err)
		}
	})
}

func TestExecServiceWithoutStartRecordIsNotDiscarded(t *testing.T) {
	dir := t.TempDir()
	s := &config.Service{Run: config.Command{Argv: []string{"sh", "-c", "exec sleep 30"}}, Stop: "touch stop-ran", Log: "service.log", PIDFile: "service.pid", StopTimeout: time.Second}
	m := New(dir, &config.Config{Services: map[string]*config.Service{"svc": s}}, nil, io.Discard)
	oldReadStat := readServiceStat
	readServiceStat = func(string, int) (akitproc.Stat, error) { return akitproc.Stat{}, os.ErrPermission }
	t.Cleanup(func() { readServiceStat = oldReadStat })
	err := m.start(t.Context(), "svc")
	if err != nil {
		t.Fatal(err)
	}
	pid := serviceTestPID(t, m.path(s.PIDFile))
	stopServiceTestProcess(t, pid)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		name, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		if err == nil && strings.TrimSpace(string(name)) == "sleep" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("service did not exec sleep: %q, %v", name, err)
		}
	}
	want := fmt.Sprintf("cannot confirm process %d is svc: no start record; check it and remove %s", pid, m.path(s.PIDFile))
	if _, err := m.State(t.Context(), "svc"); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("state error = %v, want %q", err, want)
	}
	if err := m.stop(t.Context(), "svc"); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("stop error = %v, want %q", err, want)
	}
	if _, err := os.Stat(m.path(s.PIDFile)); err != nil {
		t.Errorf("PID file was removed: %v", err)
	}
	if !serviceTestRunning(pid) {
		t.Error("service was signaled")
	}
	if _, err := os.Stat(filepath.Join(dir, "stop-ran")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stop command ran: %v", err)
	}
}

func TestMismatchedStartRecordDoesNotIdentifyPID(t *testing.T) {
	for _, op := range []string{"state", "stop"} {
		t.Run(op, func(t *testing.T) {
			cmd := startTestSleep(t)
			stat, err := akitproc.ReadStat("/proc", cmd.Process.Pid)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			s := &config.Service{Run: config.Command{Argv: []string{"sh", "-c", "exec sleep 30"}}, PIDFile: "service.pid"}
			m := New(dir, &config.Config{Services: map[string]*config.Service{"svc": s}}, nil, io.Discard)
			if err := os.WriteFile(m.path(s.PIDFile), fmt.Appendf(nil, "%d\n", cmd.Process.Pid), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(m.path(s.PIDFile)+".start", fmt.Appendf(nil, "%d %s\n", cmd.Process.Pid+1, stat.Start), 0o600); err != nil {
				t.Fatal(err)
			}
			if op == "state" {
				_, err = m.State(t.Context(), "svc")
			} else {
				err = m.stop(t.Context(), "svc")
			}
			want := fmt.Sprintf("cannot confirm process %d is svc: no start record; check it and remove %s", cmd.Process.Pid, m.path(s.PIDFile))
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s error = %v, want %q", op, err, want)
			}
			if _, err := os.Stat(m.path(s.PIDFile)); err != nil || !serviceTestRunning(cmd.Process.Pid) {
				t.Errorf("PID file or process lost: %v", err)
			}
		})
	}
}

func TestStartWithoutProcReportsExitedService(t *testing.T) {
	oldReadStat := readServiceStat
	readServiceStat = func(string, int) (akitproc.Stat, error) { return akitproc.Stat{}, errors.ErrUnsupported }
	t.Cleanup(func() { readServiceStat = oldReadStat })

	dir := t.TempDir()
	s := &config.Service{Run: config.Command{Argv: []string{"sh", "-c", "exit 42"}}, Log: "service.log", PIDFile: "service.pid", StartTimeout: time.Second}
	cfg := &config.Config{Services: map[string]*config.Service{"svc": s}}
	m := New(dir, cfg, nil, io.Discard)
	if err := m.start(t.Context(), "svc"); err == nil || !strings.Contains(err.Error(), "exited at start") {
		t.Fatalf("start error = %v, want exited-at-start error", err)
	}
}

// A service that exits at once is reported with the start time too, as
// TestStartWithoutProcReportsExitedService checks it without one.
func TestStartWithProcReportsExitedService(t *testing.T) {
	dir := t.TempDir()
	s := &config.Service{Run: config.Command{Argv: []string{"sh", "-c", "exit 42"}}, Log: "service.log", PIDFile: "service.pid", StartTimeout: time.Second}
	cfg := &config.Config{Services: map[string]*config.Service{"svc": s}}
	log := &syncLog{}
	m := New(dir, cfg, nil, log)
	if err := m.start(t.Context(), "svc"); err == nil || !strings.Contains(err.Error(), "exited at start") {
		t.Fatalf("start error = %v, want exited-at-start error", err)
	}
	if strings.Contains(log.String(), "recording PID only") {
		t.Fatal("start did not record a process start time")
	}
}

func TestWaitStartedDetectsZombieWithoutStartTime(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 42")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	pid := cmd.Process.Pid
	var stat akitproc.Stat
	for deadline := time.Now().Add(10 * time.Second); ; {
		var err error
		if stat, err = akitproc.ReadStat("/proc", pid); err != nil {
			t.Fatal(err)
		}
		if stat.State == "Z" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d state %q, want a zombie", pid, stat.State)
		}
		time.Sleep(time.Millisecond)
	}

	waitResult := make(chan error, 1)
	go func() { waitResult <- cmd.Wait() }()
	s := &config.Service{StartTimeout: time.Nanosecond}
	err := (&Manager{}).waitStarted(t.Context(), s, "", pid, "", waitResult)
	if err == nil || !strings.Contains(err.Error(), "exited at start") {
		t.Fatalf("start error = %v, want exited-at-start error", err)
	}
	select {
	case waitErr := <-waitResult:
		if waitErr == nil {
			t.Fatal("wait result = nil, want exit status 42")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("wait result was not delivered after start error")
	}
}

// A start timeout shorter than the 100ms tick still sees a service that has
// already exited, with the start time or without it. The exit is made ready
// before waitStarted runs, so the check does not depend on how fast the
// machine forks: a real start under a 50ms timeout did (19.11, test-gate).
func TestWaitStartedSeesExitAtDeadlineShorterThanTick(t *testing.T) {
	s := &config.Service{StartTimeout: time.Nanosecond}
	t.Run("proc=true", func(t *testing.T) {
		cmd := exec.Command("sh", "-c", "exit 42")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		pid := cmd.Process.Pid
		t.Cleanup(func() { _ = cmd.Wait() })
		var stat akitproc.Stat
		for deadline := time.Now().Add(10 * time.Second); ; {
			var err error
			if stat, err = akitproc.ReadStat("/proc", pid); err != nil {
				t.Fatal(err)
			}
			if stat.State == "Z" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("process %d state %q, want a zombie", pid, stat.State)
			}
			time.Sleep(time.Millisecond)
		}
		m := &Manager{}
		if err := m.waitStarted(t.Context(), s, "", pid, stat.Start, make(chan error, 1)); err == nil || !strings.Contains(err.Error(), "exited at start") {
			t.Fatalf("start error = %v, want exited-at-start error", err)
		}
	})
	// Without a start time the exit is the Wait result: ready together with
	// the deadline, it wins the select every time, not at random.
	t.Run("proc=false", func(t *testing.T) {
		m := &Manager{}
		for range 100 {
			result := make(chan error, 1)
			result <- errors.New("service exited")
			if err := m.waitStarted(t.Context(), s, "", 0, "", result); err == nil || !strings.Contains(err.Error(), "exited at start") {
				t.Fatalf("start error = %v, want exited-at-start error", err)
			}
			if err := <-result; err == nil || err.Error() != "service exited" {
				t.Fatalf("wait result = %v, want service exit", err)
			}
		}
	})
}

func TestOldPIDWithoutStartWarnsForRemainingSessionMembers(t *testing.T) {
	dir := t.TempDir()
	workerFile := filepath.Join(dir, "worker")
	cmd := exec.Command("sh", "-c", `trap "" HUP; sh -c 'exec sleep 30' & echo $! > "$1"; exit 0`, "sh", workerFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	worker := serviceTestPID(t, workerFile)
	stopServiceTestProcess(t, worker)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Services: map[string]*config.Service{"svc": {PIDFile: "service.pid"}}}
	m := New(dir, cfg, nil, io.Discard)
	if err := os.WriteFile(m.path("service.pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	m.Log = &syncLog{}
	err := m.stop(t.Context(), "svc")
	if err == nil || !strings.Contains(err.Error(), strconv.Itoa(worker)) {
		t.Fatalf("stop error = %v, want remaining worker PID %d", err, worker)
	}
	if !strings.Contains(m.Log.(*syncLog).String(), "remaining session processes were not stopped") {
		t.Fatalf("stop log lacks warning for old PID file: %s", m.Log.(*syncLog).String())
	}
	if _, err := os.Stat(m.path("service.pid")); err != nil {
		t.Fatalf("stop removed PID file for live session member: %v", err)
	}
}

func TestWritePIDStoresLeaderStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.pid")
	if err := writePID(path, 123, "456"); err != nil {
		t.Fatal(err)
	}
	pid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(pid) != "123\n" {
		t.Fatalf("pid file = %q, want PID only", pid)
	}
	start, err := os.ReadFile(path + ".start")
	if err != nil {
		t.Fatal(err)
	}
	if string(start) != "123 456\n" {
		t.Fatalf("start record = %q, want PID and leader start time", start)
	}
}

func TestWritePIDPublishesStartBeforePID(t *testing.T) {
	dir := t.TempDir()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if _, err := unix.InotifyAddWatch(fd, dir, unix.IN_MOVED_TO); err != nil {
		t.Fatal(err)
	}
	if err := writePID(filepath.Join(dir, "service.pid"), 123, "456"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	n, err := unix.Read(fd, buf)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for pos := 0; pos+unix.SizeofInotifyEvent <= n; {
		size := int(binary.NativeEndian.Uint32(buf[pos+12 : pos+16]))
		end := pos + unix.SizeofInotifyEvent + size
		if end > n {
			t.Fatal("truncated inotify event")
		}
		names = append(names, strings.TrimRight(string(buf[pos+unix.SizeofInotifyEvent:end]), "\x00"))
		pos = end
	}
	if len(names) != 2 || names[0] != "service.pid.start" || names[1] != "service.pid" {
		t.Fatalf("publication order = %q, want start record then PID", names)
	}
	data, err := os.ReadFile(filepath.Join(dir, "service.pid.start"))
	if err != nil || string(data) != "123 456\n" {
		t.Fatalf("start record = %q, %v; want PID and start time", data, err)
	}
}

// syncLog is a test log that the exit report of a started service may write
// while the test reads it.
type syncLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
