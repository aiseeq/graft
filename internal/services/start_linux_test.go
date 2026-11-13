package services

import (
	"errors"
	"fmt"
	"io"
	"net"
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
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
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
	m.Log = &strings.Builder{}
	err = m.stop(t.Context(), "svc")
	if err == nil || !strings.Contains(err.Error(), "remaining session processes were not stopped") {
		t.Fatalf("stop error = %v, want warning that session members remain", err)
	}
	if !strings.Contains(m.Log.(*strings.Builder).String(), "remaining session processes were not stopped") {
		t.Fatalf("stop log lacks orphan warning: %s", m.Log.(*strings.Builder).String())
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
	m.Log = &strings.Builder{}
	if err := m.stop(t.Context(), "svc"); err != nil {
		t.Fatalf("stop error = %v, log: %s", err, m.Log.(*strings.Builder).String())
	}
	// SIGKILL is delivered by the time stop returns, the exit may lag a little.
	for deadline := time.Now().Add(2 * time.Second); serviceTestRunning(worker); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("worker %d of leader %d still runs after the stop; log: %s", worker, leader, m.Log.(*strings.Builder).String())
		}
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
	if string(start) != "456\n" {
		t.Fatalf("start record = %q, want leader start time", start)
	}
}
