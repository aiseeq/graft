//go:build linux

package iosched

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordScopeKindExpiresOldFiles(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	dir := filepath.Join(runtimeDir, "graft", "heavy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "graft-heavy-old.scope")
	fresh := filepath.Join(dir, "graft-heavy-fresh.scope")
	for _, path := range []string{old, fresh} {
		if err := os.WriteFile(path, []byte("stale\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	recordScopeKind("graft-heavy-current.scope", "projectA:exec:go")
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old scope file remains: %v", err)
	}
	if data, err := os.ReadFile(fresh); err != nil || string(data) != "stale\n" {
		t.Fatalf("fresh scope file changed: %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "graft-heavy-current.scope")); err != nil || string(data) != "projectA:exec:go\n" {
		t.Fatalf("new scope kind = %q, %v", data, err)
	}
}
