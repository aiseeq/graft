package app

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreeNeighbourInsideTreeIsReportedAsSkipped(t *testing.T) {
	tree := filepath.Join(t.TempDir(), "tree")
	at := filepath.Join(tree, "vendor", "example.com", "kit")
	links, err := normalizeNeighbourLinks([]link{{at: at, to: filepath.Join(t.TempDir(), "kit")}}, tree)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := linkNeighbours(&App{Stdout: &out}, links, "", false); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "not linking "+at) || !strings.Contains(got, "resolves inside the worktree") {
		t.Fatalf("missing skip explanation: %q", got)
	}
}
