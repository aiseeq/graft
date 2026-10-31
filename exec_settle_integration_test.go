package main

import (
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
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if data, _ := os.ReadFile(logFile); strings.HasPrefix(string(data), "start") {
			return cmd
		}
		if time.Now().After(deadline) {
			t.Fatal("the first exec did not start its command")
		}
	}
}
