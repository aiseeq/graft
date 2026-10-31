package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pid")
	if err := os.WriteFile(path, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s is still there: %v", path, err)
	}
	if err := Remove(path); err != nil {
		t.Fatalf("a path already gone: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "full")
	if err := os.MkdirAll(filepath.Join(dir, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Remove(dir); err == nil {
		t.Fatal("removing a non-empty directory reported no error")
	}
}
