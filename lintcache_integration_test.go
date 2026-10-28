package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestLintCachePerWorktree: each work tree's steps get a golangci-lint cache
// of their own, in the user's cache; one the environment or the task sets
// is kept, but the one graft gave another tree, inherited; graft worktree
// clean --apply removes the cache of a work tree that is gone.
func TestLintCachePerWorktree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the steps run sh")
	}
	f := newRepo(t, map[string]string{".graft.yaml": `schema: 1
version: {mode: none}
tasks:
  show:
    run: [[sh, -c, 'printf "cache=%s\n" "$GOLANGCI_LINT_CACHE"']]
  own:
    env: {GOLANGCI_LINT_CACHE: /elsewhere}
    run: [[sh, -c, 'printf "cache=%s\n" "$GOLANGCI_LINT_CACHE"']]
`}, "origin")
	f.git("checkout", "-q", "-b", "main")
	f.mustPlainCommit("feat: base")
	f.git("push", "-q", "-u", "origin", "HEAD")
	f.git("remote", "set-head", "origin", "main")
	job := filepath.Join(f.base, "job")
	f.git("worktree", "add", "-q", "-b", "w/job", job, "main")

	cache := func(dir, task string, env ...string) string {
		t.Helper()
		out, code := f.exec(dir, "", "env", append(env, graftBin, task)...)
		if code != 0 {
			t.Fatalf("%s in %s: exit %d\n%s", task, dir, code, out)
		}
		for line := range strings.SplitSeq(out, "\n") {
			if v, ok := strings.CutPrefix(line, "cache="); ok {
				return v
			}
		}
		t.Fatalf("%s in %s printed no cache:\n%s", task, dir, out)
		return ""
	}
	main, other := cache(f.work, "show"), cache(job, "show")
	if main == other || !strings.Contains(main, filepath.Join("graft", "golangci-lint")) {
		t.Fatalf("caches %q and %q", main, other)
	}
	if again := cache(job, "show"); again != other {
		t.Errorf("the job's cache moved: %q, then %q", other, again)
	}
	if own := cache(f.work, "own"); own != "/elsewhere" {
		t.Errorf("a task's own cache: %q", own)
	}
	if got := cache(job, "show", "GOLANGCI_LINT_CACHE=/mine"); got != "/mine" {
		t.Errorf("the environment's cache: %q", got)
	}
	if got := cache(job, "show", "GOLANGCI_LINT_CACHE="+main); got != other {
		t.Errorf("the main tree's cache inherited: %q, want %q", got, other)
	}

	f.git("worktree", "remove", job)
	if out := f.mustGraft("worktree", "clean", "--apply"); !strings.Contains(out, "the lint cache of ") {
		t.Errorf("clean:\n%s", out)
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Errorf("the job's cache is left: %v", err)
	}
	if _, err := os.Stat(main); err != nil {
		t.Errorf("the main tree's cache: %v", err)
	}
}
