package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHeavyTaskStatsOmitDiskProfile: admission reads the disk state source,
// and the task's stats line has no disk_profile key.
func TestHeavyTaskStatsOmitDiskProfile(t *testing.T) {
	f, statsFile := statsRepo(t, "tasks:\n  build:\n    heavy: true\n    run: ["+helperStep("ok")+"]\n")
	// A socket's path is short: the test's temporary directory may not fit.
	run, err := os.MkdirTemp("", "disk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	ln, err := net.Listen("unix", filepath.Join(run, "io-state.sock"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var kinds []string
	var served sync.WaitGroup
	served.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, err := bufio.NewReader(conn).ReadBytes('\n')
			var req struct {
				Kind string `json:"kind"`
			}
			if err == nil {
				err = json.Unmarshal(line, &req)
			}
			if err != nil {
				t.Errorf("disk source: %v", err)
			}
			mu.Lock()
			kinds = append(kinds, req.Kind)
			mu.Unlock()
			answer := map[string]any{"v": 1, "at": time.Now().Format(time.RFC3339Nano),
				"open": map[string]any{"open": false, "ms": 0}}
			if err := json.NewEncoder(conn).Encode(answer); err != nil {
				t.Errorf("disk source: %v", err)
			}
			conn.Close()
		}
	})
	t.Cleanup(func() { ln.Close(); served.Wait() })
	f.env = append(f.env, "XDG_RUNTIME_DIR="+run)

	if out := f.mustGraft("build"); strings.Contains(out, "PSI total used") {
		t.Errorf("the source was not read:\n%s", out)
	}
	mu.Lock()
	asked := kinds
	mu.Unlock()
	if len(asked) == 0 {
		t.Errorf("the disk state source was not asked")
	}
	data, err := os.ReadFile(statsFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var task map[string]json.RawMessage
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &task); err != nil {
		t.Fatal(err)
	}
	if string(task["kind"]) != `"task"` {
		t.Errorf("task line %s", lines[len(lines)-1])
	}
	if _, ok := task["disk_profile"]; ok {
		t.Errorf("task line kept the disk profile: %s", lines[len(lines)-1])
	}
}
