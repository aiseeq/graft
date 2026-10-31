package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestExecCoresReservesGivenCores(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none}\n"})
	cache := filepath.Join(f.base, "cache")
	cfgDir, proc := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "load: {settle: 20s, poll: 20ms}\n")
	writeTestPressure(t, cfgDir)
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, "APPDATA="+cfgDir, fakeMachine(t, proc, 1), "XDG_CACHE_HOME="+cache, "LOCALAPPDATA="+cache)
	statsDir := filepath.Join(cache, "graft")
	if err := os.MkdirAll(statsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statsDir, "load.cores.json"), []byte(`{"work:exec:gatehelper":[4]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(f.base, "exec.log")
	cmd, out := f.start(f.work, "exec", "--heavy", "--cores", "1.5", "--", helperBin, "log", logPath, "5000")
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	reserveDir := filepath.Join(cache, "graft")
	var reserved float64
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		files, err := filepath.Glob(filepath.Join(reserveDir, "load.lock.*.reserve"))
		if err == nil && len(files) > 0 {
			data, err := os.ReadFile(files[0])
			if err == nil {
				var reserve struct {
					Cores float64 `json:"cores"`
				}
				if json.Unmarshal(data, &reserve) == nil {
					reserved = reserve.Cores
					break
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if reserved != 1.5 {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("live reserve cores = %v, want 1.5; log: %q; output:\n%s", reserved, logPath, out.String())
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("exec: %v\n%s", err, out.String())
	}
	var reserveCores float64
	for _, line := range readStats(t, filepath.Join(statsDir, "stats.jsonl")) {
		if line.Kind == "task" && line.Task == "exec" {
			reserveCores = line.ReserveCores
		}
	}
	if reserveCores != 1.5 {
		t.Errorf("stats reserve_cores = %v, want 1.5", reserveCores)
	}
}

func TestExecCoresUsage(t *testing.T) {
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none}\n"})
	for _, args := range [][]string{
		{"exec", "--heavy", "--cores", "0", "--", "true"},
		{"exec", "--heavy", "--cores", "-1", "--", "true"},
		{"exec", "--heavy", "--cores", "x", "--", "true"},
		{"exec", "--heavy", "--cores", strconv.Itoa(runtime.NumCPU() + 1), "--", "true"},
		{"exec", "--cores", "1", "--", "true"},
	} {
		out, code := f.graft("", args...)
		if code != 125 || !strings.Contains(out, "--cores") {
			t.Errorf("graft %q: exit %d, want usage error\n%s", args, code, out)
		}
	}
	if out, code := f.graft("", "exec", "--heavy", "--cores"); code != 125 || !strings.Contains(out, "graft exec") {
		t.Errorf("missing --cores value: exit %d\n%s", code, out)
	}
}
