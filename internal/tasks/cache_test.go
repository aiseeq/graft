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
	initial, dependencyChanged, versionChanged := syncKeys(t, "", nil)
	if dependencyChanged == initial {
		t.Fatal("changing a dependency did not change the cache key")
	}
	if versionChanged != dependencyChanged {
		t.Fatal("changing only the version changed the cache key")
	}
}

// TestCacheVersionSyncKeyOutsideTheTree: a sync file that cache_exclude
// names, or that git ignores, is no part of the key, as any other such file.
func TestCacheVersionSyncKeyOutsideTheTree(t *testing.T) {
	for what, c := range map[string]struct {
		config string
		files  map[string]string
	}{
		"cache_exclude": {config: "cache_exclude: [lock.json]\n"},
		"ignored":       {files: map[string]string{".gitignore": "lock.json\n"}},
	} {
		initial, dependencyChanged, _ := syncKeys(t, c.config, c.files)
		if dependencyChanged != initial {
			t.Errorf("%s: changing a dependency in the sync file changed the cache key", what)
		}
	}
}

// syncKeys is the cache key of a task in a repository whose lock.json is a
// version sync file: at first, after a dependency changed in it, and after
// only the version changed too.
func syncKeys(t *testing.T, extraConfig string, files map[string]string) (initial, dependencyChanged, versionChanged string) {
	t.Helper()
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
	for path, content := range files {
		write(path, content)
	}

	cfg, err := config.Parse([]byte(`schema: 1
version:
  mode: file
  file: VERSION
  sync:
    - {path: lock.json, format: json, key: [version]}
` + extraConfig + `tasks:
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

	initial = key()
	write("lock.json", `{"version":"1.0.0","dependency":"new"}`)
	dependencyChanged = key()
	write("lock.json", `{"version":"2.0.0","dependency":"new"}`)
	write("VERSION", "2.0.0\n")
	return initial, dependencyChanged, key()
}
