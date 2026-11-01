package tasks

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
)

func TestCacheVersionSyncKey(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("VERSION", "1.0.0\n")
	write("lock.json", `{"version":"1.0.0","dependency":"old"}`)

	cfg, err := config.Parse([]byte(`schema: 1
version:
  mode: file
  file: VERSION
  sync:
    - {path: lock.json, format: json, key: [version]}
tasks:
  verify:
    cache: tree
    run: [[true]]
`))
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Root: root, Config: cfg, GraftVersion: "test"}
	key := func() string {
		t.Helper()
		key, _, err := r.cacheKey(context.Background(), "verify", nil)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}

	initial := key()
	write("lock.json", `{"version":"1.0.0","dependency":"new"}`)
	dependencyChanged := key()
	if dependencyChanged == initial {
		t.Fatal("changing a dependency did not change the cache key")
	}
	write("lock.json", `{"version":"2.0.0","dependency":"new"}`)
	write("VERSION", "2.0.0\n")
	if got := key(); got != dependencyChanged {
		t.Fatal("changing only the version changed the cache key")
	}
}
