package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommitNamesNewFilesBeforeGate(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "gate-ran")
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\ngate:\n" + gateStep("touch", marker),
		"tracked.txt": "base\n",
	})
	f.mustPlainCommit("chore: base")
	f.write("tree-added.txt", "tree\n")
	f.write("index-added.txt", "index\n")
	f.git("add", "index-added.txt")

	out, code := f.graft("", "commit", "-m", "feat: add files")
	if code != 0 {
		t.Fatalf("commit failed: exit %d\n%s", code, out)
	}
	line := "graft: commit adds new files: index-added.txt, tree-added.txt"
	noteAt, gateAt := strings.Index(out, line), strings.Index(out, "graft: gate 1/1:")
	if noteAt < 0 {
		t.Fatalf("commit did not name both added files:\n%s", out)
	}
	if gateAt < 0 || noteAt > gateAt {
		t.Fatalf("new-file note was not printed before the gate:\n%s", out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("gate task did not write its marker: %v", err)
	}

	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	f.write("tracked.txt", "changed\n")
	out, code = f.graft("", "commit", "-m", "fix: edit tracked file")
	if code != 0 {
		t.Fatalf("commit without additions failed: exit %d\n%s", code, out)
	}
	if strings.Contains(out, "graft: commit adds ") {
		t.Fatalf("commit without additions printed a note:\n%s", out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("gate task did not run on commit without additions: %v", err)
	}
}
