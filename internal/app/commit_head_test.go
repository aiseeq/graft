package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gittest"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

func TestInstallFreshnessReference(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) string { return gittest.Run(t, root, args...) }
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "one")
	git("tag", "-a", "v1.0.0", "-m", "v1.0.0")
	git("commit", "-q", "--allow-empty", "-m", "two")
	git("tag", "-a", "v1.1.0", "-m", "v1.1.0")
	latest := git("rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "three")
	repo, err := gitx.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"latest reachable tag", &config.Config{Version: config.Version{Mode: config.ModeGitTag, TagPrefix: "v"}}, latest},
		{"HEAD for other modes", &config.Config{Version: config.Version{Mode: config.ModeFile}}, git("rev-parse", "HEAD")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := installFreshnessReference(repo, test.cfg)
			if err != nil || got != test.want {
				t.Fatalf("reference = %s, %v; want %s", got, err, test.want)
			}
		})
	}
}

// TestCurrentHead: the branch and commit HEAD is on, on a branch, detached
// and unborn, as a commit compares them around its gate.
func TestCurrentHead(t *testing.T) {
	work := t.TempDir()
	git := func(args ...string) string { return gittest.Run(t, work, args...) }
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
	git("checkout", "-q", "feature/PROJ-1-x")
	// A branch whose commit is gone counts as having none, as an unborn one.
	ref := filepath.Join(work, ".git", "refs", "heads", "feature", "PROJ-1-x")
	if err := os.WriteFile(ref, []byte(strings.Repeat("a", 40)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := currentHead(repo); err != nil || got != (headState{branch: "feature/PROJ-1-x"}) {
		t.Errorf("a missing commit: %+v, %v", got, err)
	}
}
