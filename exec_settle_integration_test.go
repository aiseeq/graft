package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestExecSettleLetsOtherHeavyPass: a plain heavy task passes while an
// exec holds the separate settling lock.
func TestExecSettleLetsOtherHeavyPass(t *testing.T) {
	skipWaitInShort(t)
	f := execSettleRepo(t)
	_ = startExecSettle(t, f, "30s", 30_000)
	started := time.Now()
	if out, code := f.graft("", "exec", "--heavy", "--", helperBin, "ok"); code != 0 {
		t.Fatalf("plain heavy exec: exit %d\n%s", code, out)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Errorf("plain heavy exec waited for the separate settle lock: %s", took)
	}
}

// TestExecSettleHoldsNextSettle: an exec with --settle waits for the prior
// --settle holder, while ordinary heavy tasks can pass it.
func TestExecSettleHoldsNextSettle(t *testing.T) {
	skipWaitInShort(t)
	f := execSettleRepo(t)
	_ = startExecSettle(t, f, "700ms", 30_000)
	started := time.Now()
	if out, code := f.graft("", "exec", "--heavy", "--settle", "700ms", "--", helperBin, "ok"); code != 0 {
		t.Fatalf("settled exec: exit %d\n%s", code, out)
	}
	if took := time.Since(started); took < 500*time.Millisecond {
		t.Errorf("next --settle exec waited only %s", took)
	}
}

// TestExecSettleReleasesOnEnd: ending the command releases both locks before
// its requested settling period elapses.
func TestExecSettleReleasesOnEnd(t *testing.T) {
	skipWaitInShort(t)
	f := execSettleRepo(t)
	cmd := startExecSettle(t, f, "1h", 500)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("first exec: %v", err)
	}
	started := time.Now()
	if out, code := f.graft("", "exec", "--heavy", "--settle", "1h", "--", helperBin, "ok"); code != 0 {
		t.Fatalf("next settled exec: exit %d\n%s", code, out)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Errorf("next --settle exec waited after the command ended: %s", took)
	}
}

func TestExecSettleUsage(t *testing.T) {
	f := execSettleRepo(t)
	for _, args := range [][]string{
		{"exec", "--settle", "1s", "--", helperBin, "ok"},
		{"exec", "--heavy", "--settle", "0s", "--", helperBin, "ok"},
		{"exec", "--heavy", "--settle", "soon", "--", helperBin, "ok"},
		{"exec", "--heavy", "--settle"},
	} {
		if out, code := f.graft("", args...); code != 125 || !strings.Contains(out, "graft exec") && !strings.Contains(out, "--settle") {
			t.Errorf("graft %q: exit %d, want the usage with 125\n%s", args, code, out)
		}
	}
}

func TestExecPastLoadOption(t *testing.T) {
	f := execSettleRepo(t)
	cache := t.TempDir()
	f.env = append(f.env, "XDG_CACHE_HOME="+cache, "LOCALAPPDATA="+cache)
	reason := "tl dd4a9367:6208"
	if out, code := f.graft("", "exec", "--heavy", "--past-load", reason, "--", helperBin, "ok"); code != 0 || !strings.Contains(out, "passed CPU load limit by --past-load ("+reason+")") {
		t.Fatalf("authorized exec: exit %d\n%s", code, out)
	}
	statsFile, err := os.Open(filepath.Join(cache, "graft", "stats.jsonl"))
	if err != nil {
		t.Fatalf("open stats.jsonl: %v", err)
	}
	defer statsFile.Close()
	seen := map[string]bool{}
	scanner := bufio.NewScanner(statsFile)
	for scanner.Scan() {
		var row struct {
			Kind     string `json:"kind"`
			PastLoad string `json:"past_load"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			t.Fatalf("decode stats line: %v", err)
		}
		if row.PastLoad == reason {
			seen[row.Kind] = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read stats.jsonl: %v", err)
	}
	if !seen["task"] || !seen["step"] {
		t.Errorf("stats.jsonl past_load kinds = %v; want task and step", seen)
	}
	for _, args := range [][]string{
		{"exec", "--heavy", "--past-load", "", "--", helperBin, "ok"},
		{"exec", "--heavy", "--past-load"},
		{"exec", "--heavy", "--past-load", "--", helperBin, "ok"},
		{"exec", "--heavy", "--past-load", "--future-option", "--", helperBin, "ok"},
	} {
		if out, code := f.graft("", args...); code != 125 || !strings.Contains(out, "graft exec") {
			t.Errorf("graft %q: exit %d, want usage with 125\n%s", args, code, out)
		}
	}
}

func execSettleRepo(t *testing.T) *fixture {
	t.Helper()
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none}\n"})
	userConfig := filepath.Join(t.TempDir(), "user-config")
	writeUserConfig(t, userConfig, "")
	f.env = append(f.env, "XDG_CONFIG_HOME="+userConfig, "APPDATA="+userConfig)
	return f
}

func startExecSettle(t *testing.T, f *fixture, settle string, ms int) *exec.Cmd {
	t.Helper()
	logFile := filepath.Join(t.TempDir(), "first.log")
	cmd := exec.Command(graftBin, "exec", "--heavy", "--settle", settle, "--", helperBin, "log", logFile, strconv.Itoa(ms))
	cmd.Dir, cmd.Env = f.work, append(slices.Clone(testEnv), f.env...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopTestChild(cmd); _ = cmd.Wait() })
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if data, _ := os.ReadFile(logFile); strings.HasPrefix(string(data), "start") {
			return cmd
		}
		if time.Now().After(deadline) {
			t.Fatal("the first exec did not start its command")
		}
	}
}
