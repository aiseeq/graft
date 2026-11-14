package tasks

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	akitproc "github.com/aiseeq/akit/proc"
)

// TestMain makes the test binary a subreaper on Linux and gives the tests a
// cache directory of their own: a Runner makes a golangci-lint cache for each
// work tree it runs a step in, and the tests' temporary trees would leave
// theirs in the user's cache.
func TestMain(m *testing.M) {
	if runtime.GOOS == "linux" {
		if err := akitproc.BecomeChildSubreaper(); err != nil {
			panic(err)
		}
	}
	cache, err := os.MkdirTemp("", "graft-tasks-cache-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "tasks test cache:", err)
		os.Exit(1)
	}
	for _, name := range []string{"XDG_CACHE_HOME", "LOCALAPPDATA"} {
		if err := os.Setenv(name, filepath.Clean(cache)); err != nil {
			fmt.Fprintln(os.Stderr, "tasks test cache:", err)
			os.RemoveAll(cache)
			os.Exit(1)
		}
	}
	code := m.Run()
	if err := os.RemoveAll(cache); err != nil {
		fmt.Fprintln(os.Stderr, "remove tasks test cache:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
