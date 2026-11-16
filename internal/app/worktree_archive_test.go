package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/gitx"
)

func TestCreateWorktreeArchiveTagRefusesExistingTagWithoutChangingIt(t *testing.T) {
	root := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	run("config", "user.name", "Archive Test")
	run("config", "user.email", "archive@example.test")
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "file.txt")
	run("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "base")
	run("tag", "archive/idea", "main")
	run("switch", "-q", "-c", "idea")
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("commit", "-am", "idea", "-q")
	repo, err := gitx.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	before := run("rev-parse", "refs/tags/archive/idea")
	_, err = createWorktreeArchiveTag(repo, "archive/idea", "replacement", "idea")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing tag was not refused: %v", err)
	}
	if after := run("rev-parse", "refs/tags/archive/idea"); after != before {
		t.Fatalf("existing tag changed: before %s, after %s", before, after)
	}
}
