// Package testinputs puts what a test's child processes read into go test's
// cache key. The key holds only the files inside the module and the
// variables the test process itself reads, so without it a cached "ok"
// outlives a change the children see: git committing under a new
// GIT_AUTHOR_DATE, a helper built from changed sources.
package testinputs

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// newlySet are the variables that change git and go when set later: go test
// replays the reads of the run it cached, so Env's walk over the environment
// misses a variable that was not there then.
var newlySet = []string{
	"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_DATE",
	"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_COMMITTER_DATE",
	"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS",
	"GIT_EDITOR", "GIT_SEQUENCE_EDITOR", "GIT_TEMPLATE_DIR", "GIT_EXEC_PATH", "GIT_SSH_COMMAND",
	"HOME", "PATH", "TMPDIR", "SHELL", "LANG", "LC_ALL",
	"GOFLAGS", "GOOS", "GOARCH", "GOEXPERIMENT", "GOTOOLCHAIN", "GOWORK", "GOPROXY", "CGO_ENABLED",
}

// perRun are the variables graft sets anew for every run of a task, the
// threads from the machine's load among them: read, they would miss the
// cache every time, and a test whose result hangs on them is the bug.
var perRun = []string{
	"GRAFT_RUN_ID", "GRAFT_TEST_REPORT", "GRAFT_COMMIT_MESSAGE_FILE", "GRAFT_ADMITTED", "GRAFT_NOW",
	"GRAFT_LOAD_PRIORITY", "GRAFT_LOAD_BUDGET", "GRAFT_LOAD_THEN", "GRAFT_LOADAVG",
	"GRAFT_THREADS", "GOMAXPROCS",
}

// Env reads every variable the test's processes inherit, but the perRun
// ones, and the newlySet ones. Call it from a test: go test logs the reads
// only once the tests start, not in TestMain or init.
func Env() {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.ContainsFunc(perRun, func(s string) bool { return strings.EqualFold(s, name) }) {
			os.Getenv(name)
		}
	}
	for _, name := range newlySet {
		os.Getenv(name)
	}
}

// Files opens the files under dir, the sources of a helper the tests build,
// so their change misses the cache too.
func Files(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		return f.Close()
	})
}
