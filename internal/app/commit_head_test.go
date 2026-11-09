package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/gitx"
)

// TestCurrentHead: the branch and commit HEAD is on, on a branch, detached
// and unborn, as a commit compares them around its gate.
func TestCurrentHead(t *testing.T) {
	work := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+filepath.Join(work, ".none"), "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "feature/PROJ-1-x")
	repo, err := gitx.Open(work)
	if err != nil {
		t.Fatal(err)
	}

	if got, err := currentHead(repo); err != nil || got != (headState{branch: "feature/PROJ-1-x"}) {
		t.Errorf("unborn: %+v, %v", got, err)
	}
	git("commit", "-q", "--allow-empty", "-m", "first")
	sha := git("rev-parse", "HEAD")
	if got, err := currentHead(repo); err != nil || got != (headState{branch: "feature/PROJ-1-x", sha: sha}) {
		t.Errorf("on a branch: %+v, %v", got, err)
	}
	git("checkout", "-q", "--detach")
	if got, err := currentHead(repo); err != nil || got != (headState{sha: sha}) {
		t.Errorf("detached: %+v, %v", got, err)
	}
}
