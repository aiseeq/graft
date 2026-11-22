package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGocacheTrimTakesTheCacheOfGoEnv: graft gocache trim reports what it
// would remove from go env GOCACHE, removes it with --apply, and refuses an
// age go's hourly mark would make wrong and a cache that is off.
func TestGocacheTrimTakesTheCacheOfGoEnv(t *testing.T) {
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none}\n"})
	cache := t.TempDir()
	old := filepath.Join(cache, "3f", strings.Repeat("3", 64)+"-d")
	fresh := filepath.Join(cache, "3f", strings.Repeat("4", 64)+"-a")
	for path, age := range map[string]time.Duration{old: 48 * time.Hour, fresh: time.Hour, filepath.Join(cache, "README"): 48 * time.Hour} {
		data := "entry"
		if filepath.Base(path) == "README" {
			data = "This directory holds cached build artifacts from the Go build system.\n"
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, time.Now().Add(-age), time.Now().Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	f.env = append(f.env, "GOCACHE="+cache)
	probeArgs, probeDir := installGocacheProbeStub(t, f)

	out := f.mustGraft("gocache", "trim")
	if want := "graft: dry run: graft gocache trim --apply removes 1 entries, 5 B, unused for 25h0m0s in " + cache + "; 1 entries, 5 B kept\n"; out != want {
		t.Errorf("dry run:\n%s\nwant\n%s", out, want)
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("the dry run removed an entry: %v", err)
	}
	if out := f.mustGraft("gocache", "trim", "--apply"); !strings.Contains(out, "graft: removed 1 entries") {
		t.Errorf("apply:\n%s", out)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the old entry is still there: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("the fresh entry removed: %v", err)
	}
	assertGocacheProbeArgs(t, probeArgs, probeDir)

	if out, code := f.graft("", "gocache", "trim", "--older", "1h"); code != 2 || !strings.Contains(out, "want more than 1h0m0s") {
		t.Errorf("--older 1h: exit %d\n%s", code, out)
	}
	if out, code := f.graft("", "gocache", "trim", "--free-below", "nope"); code != 2 || !strings.Contains(out, "--free-below") {
		t.Errorf("bad --free-below: exit %d\n%s", code, out)
	}
	if out, code := f.graft("", "gocache", "trim", "--free-below", ""); code != 2 || !strings.Contains(out, "--free-below") {
		t.Errorf("empty --free-below: exit %d\n%s", code, out)
	}
	for _, bad := range []string{"10%", "0G"} {
		if out, code := f.graft("", "gocache", "trim", "--free-below", bad); code != 2 || !strings.Contains(out, "want a size above zero") || strings.Contains(out, "percentage") {
			t.Errorf("--free-below %s: exit %d\n%s", bad, code, out)
		}
	}
	f.env = append(f.env, "GOCACHE=off")
	if out, code := f.graft("", "gocache", "trim"); code != 1 || !strings.Contains(out, `go env GOCACHE is "off"`) {
		t.Errorf("GOCACHE=off: exit %d\n%s", code, out)
	}
}

// TestGocacheTrimApplyWaitsForTheMachine: --apply waits before a batch
// while the machine has much memory queued to write, and with --max-wait
// spent removes nothing and says so.
func TestGocacheTrimApplyWaitsForTheMachine(t *testing.T) {
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none}\n"})
	cache := t.TempDir()
	old := filepath.Join(cache, "3f", strings.Repeat("3", 64)+"-d")
	if err := os.MkdirAll(filepath.Dir(old), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("entry"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(old, time.Now().Add(-48*time.Hour), time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "README"), []byte("This directory holds cached build artifacts from the Go build system.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	machine := t.TempDir()
	for name, content := range map[string]string{
		"meminfo":         "Dirty: 900000 kB\nWriteback: 0 kB\n",
		"stat":            "cpu  1 1 1 1\ncpu0 1 1 1 1\nprocs_running 1\n",
		"loadavg":         "0.00 0.00 0.00 1/100 1\n",
		"pressure/io":     "some avg10=0.00 avg60=0.00 avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1\n",
		"pressure/memory": "some avg10=0.00 avg60=0.00 avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1\n",
		"vmstat":          "pswpin 0\npswpout 0\n",
	} {
		path := filepath.Join(machine, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.env = append(f.env, "GOCACHE="+cache, "GRAFT_PROC="+machine)
	probeArgs, probeDir := installGocacheProbeStub(t, f)
	out := f.mustGraft("gocache", "trim", "--apply", "--max-wait", "1s")
	if !strings.Contains(out, "graft: removed 0 entries") || !strings.Contains(out, "1 entries, 5 B remain") || !strings.Contains(out, "waited 1s") {
		t.Errorf("a busy machine's trim:\n%s", out)
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("the trim removed an entry while the machine was busy: %v", err)
	}
	assertGocacheProbeArgs(t, probeArgs, probeDir)
}

func installGocacheProbeStub(t *testing.T, f *fixture) (string, string) {
	t.Helper()
	home := t.TempDir()
	bin := filepath.Join(t.TempDir(), "gocache-probe-stub")
	argsFile := filepath.Join(t.TempDir(), "probe-args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$GRAFT_GOCACHE_PROBE_ARGS_FILE\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.env = append(f.env, "HOME="+home, "GRAFT_GOCACHE_PROBE="+bin, "GRAFT_GOCACHE_PROBE_ARGS_FILE="+argsFile)
	return argsFile, filepath.Join(home, ".local", "state", "graft")
}

func assertGocacheProbeArgs(t *testing.T, argsFile, probeDir string) {
	t.Helper()
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read probe stub args: %v", err)
	}
	args := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(args) != 12 || args[0] != "--user" || args[1] != "--collect" || args[2] != "--unit" || !strings.HasPrefix(args[3], "graft-gocache-trim-probe-") || args[4] != "--" || args[6] != "gocache" || args[7] != "probe" || args[8] != "--while" || args[10] != "--dir" {
		t.Fatalf("unexpected probe args: %#v", args)
	}
	unitPID := strings.TrimPrefix(args[3], "graft-gocache-trim-probe-")
	whilePID, startTicks, found := strings.Cut(args[9], ":")
	if !found || whilePID != unitPID || startTicks == "" || !filepath.IsAbs(args[5]) || args[11] != probeDir {
		t.Fatalf("unexpected probe identity or directory: %#v", args)
	}
}

func TestGocacheBatchSyncFlagAndEnvironment(t *testing.T) {
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none}\n"})
	cache := t.TempDir()
	if err := os.WriteFile(filepath.Join(cache, "README"), []byte("This directory holds cached build artifacts from the Go build system.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.env = append(f.env, "GOCACHE="+cache, "GRAFT_GOCACHE_PROBE=off")
	for _, tc := range []struct {
		name, env, flag, want string
		setEnv, setFlag       bool
		code                  int
	}{
		{"default", "", "", "sync fs", false, false, 0},
		{"env none", "none", "", "sync none", true, false, 0},
		{"flag overrides env", "none", "fs", "sync fs", true, true, 0},
		{"flag overrides invalid env", "bad", "none", "sync none", true, true, 0},
		{"bad env", "bad", "", "GRAFT_GOCACHE_BATCH_SYNC \"bad\": want fs or none", true, false, 2},
		{"empty env", "", "", "sync fs", true, false, 0},
		{"bad flag", "fs", "bad", "--batch-sync \"bad\": want fs or none", true, true, 2},
		{"empty flag", "fs", "", "--batch-sync \"\": want fs or none", true, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldEnv := f.env
			f.env = append([]string{}, oldEnv...)
			if tc.setEnv {
				f.env = append(f.env, "GRAFT_GOCACHE_BATCH_SYNC="+tc.env)
			}
			t.Cleanup(func() { f.env = oldEnv })
			args := []string{"gocache", "trim", "--apply"}
			if tc.setFlag {
				args = append(args, "--batch-sync", tc.flag)
			}
			out, code := f.graft("", args...)
			if code != tc.code || !strings.Contains(out, tc.want) {
				t.Fatalf("exit %d, want %d and %q:\n%s", code, tc.code, tc.want, out)
			}
		})
	}
}
