package app

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiseeq/graft/v2/internal/gittest"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

// TestGateLeftHeadReadsTheIndexAsItIs: whether the gate left the work as
// HEAD has it is read from the index as it is when asked, not from the tree
// stageGateChanges restored: another writer may have changed the index since.
func TestGateLeftHeadReadsTheIndexAsItIs(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) string { return gittest.Run(t, root, args...) }
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	write("a.txt", "base\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	repo, err := gitx.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	head, err := currentHead(repo)
	if err != nil {
		t.Fatal(err)
	}
	if head.tree != git("rev-parse", "HEAD^{tree}") {
		t.Fatalf("HEAD's tree %q", head.tree)
	}
	write("a.txt", "mine\n")
	_, start, err := stageCommitStart(repo, "", head)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{Stdout: io.Discard, Stderr: io.Discard}
	if err := a.stageGateChanges(repo, start, "commit", head, commitAgain); err != nil {
		t.Fatal(err)
	}
	if empty, err := gateLeftHead(repo, head); err != nil || empty {
		t.Errorf("the work staged: left HEAD %v, %v; want false", empty, err)
	}
	// Another writer puts HEAD's tree into the index after the restore.
	git("read-tree", "HEAD")
	if empty, err := gateLeftHead(repo, head); err != nil || !empty {
		t.Errorf("the index is HEAD's now: left HEAD %v, %v; want true", empty, err)
	}
	if empty, err := gateLeftHead(repo, headState{branch: "main"}); err != nil || empty {
		t.Errorf("unborn HEAD: left HEAD %v, %v; want false", empty, err)
	}
}
