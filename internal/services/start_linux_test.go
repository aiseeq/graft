package services

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	akitproc "github.com/aiseeq/akit/proc"
	"github.com/aiseeq/graft/v2/internal/config"
)

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
				_, _ = akitproc.StopChild(pid, 0, nil)
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
