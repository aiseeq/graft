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

	if out, code := f.graft("", "gocache", "trim", "--older", "1h"); code != 2 || !strings.Contains(out, "want more than 1h0m0s") {
		t.Errorf("--older 1h: exit %d\n%s", code, out)
	}
	f.env = append(f.env, "GOCACHE=off")
	if out, code := f.graft("", "gocache", "trim"); code != 1 || !strings.Contains(out, `go env GOCACHE is "off"`) {
		t.Errorf("GOCACHE=off: exit %d\n%s", code, out)
	}
}
