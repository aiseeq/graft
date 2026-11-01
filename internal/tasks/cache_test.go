package tasks

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
)

func TestCacheVersionSyncKey(t *testing.T) {
	for what, c := range map[string]syncCase{
		"one target": {},
		// package-lock.json keeps the version twice: both are masked.
		"two targets in one file": {
			sync: "    - {path: lock.json, format: json, key: [version]}\n" +
				"    - {path: lock.json, format: json, key: [packages, \"\", version]}\n",
			lock: `{"version":"V","packages":{"":{"version":"V"}},"dependency":"D"}`,
		},
		"inside cache.paths": {cache: "{paths: [lock.json]}"},
	} {
		initial, dependencyChanged, versionChanged := syncKeys(t, c)
		if dependencyChanged == initial {
			t.Errorf("%s: changing a dependency did not change the cache key", what)
		}
		if versionChanged != dependencyChanged {
			t.Errorf("%s: changing only the version changed the cache key", what)
		}
	}
}

// TestCacheVersionSyncKeyOutsideTheTree: a sync file that cache_exclude
// names, that git ignores or that the task's cache.paths leave out is no part
// of the key, as any other such file.
func TestCacheVersionSyncKeyOutsideTheTree(t *testing.T) {
	for what, c := range map[string]syncCase{
		"cache_exclude":       {config: "cache_exclude: [lock.json]\n"},
		"ignored":             {files: map[string]string{".gitignore": "lock.json\n"}},
		"outside cache.paths": {cache: "{paths: ['*.go']}"},
	} {
		initial, dependencyChanged, _ := syncKeys(t, c)
		if dependencyChanged != initial {
			t.Errorf("%s: changing a dependency in the sync file changed the cache key", what)
		}
	}
}

// syncCase is a repository whose lock.json is a version sync file. Empty
// fields take one json target at [version], lock.json with a version and a
// dependency, and `cache: tree`.
type syncCase struct {
	sync   string            // version.sync entries
	lock   string            // lock.json, V standing for the version and D for the dependency
	cache  string            // the task's cache setting
	config string            // more top-level config
	files  map[string]string // more work tree files
}

// syncKeys is the cache key of the task: at first, after a dependency
// changed in lock.json, and after only the version changed too.
func syncKeys(t *testing.T, c syncCase) (initial, dependencyChanged, versionChanged string) {
	t.Helper()
	if c.sync == "" {
		c.sync = "    - {path: lock.json, format: json, key: [version]}\n"
	}
	if c.lock == "" {
		c.lock = `{"version":"V","dependency":"D"}`
	}
	if c.cache == "" {
		c.cache = "tree"
	}
	lock := func(version, dependency string) string {
		return strings.NewReplacer(`"V"`, `"`+version+`"`, `"D"`, `"`+dependency+`"`).Replace(c.lock)
	}
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
	write("lock.json", lock("1.0.0", "old"))
	for path, content := range c.files {
		write(path, content)
	}

	cfg, err := config.Parse([]byte(`schema: 1
version:
  mode: file
  file: VERSION
  sync:
` + c.sync + c.config + `tasks:
  verify:
    cache: ` + c.cache + `
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
	write("lock.json", lock("1.0.0", "new"))
	dependencyChanged = key()
	write("lock.json", lock("2.0.0", "new"))
	write("VERSION", "2.0.0\n")
	return initial, dependencyChanged, key()
}
