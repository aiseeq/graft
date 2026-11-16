package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWorkTreeIDAcrossFirstCommitAndChanges(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "--template=")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.com")
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("tracked", "one\n")
	git("add", "tracked")
	repo, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := func() string {
		t.Helper()
		value, err := repo.WorkTreeID()
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := id()
	git("-c", "core.hooksPath=/dev/null", "commit", "-qm", "first")
	if got := id(); got != before {
		t.Errorf("first commit changed the content ID: %s -> %s", before, got)
	}
	write("tracked", "two\n")
	changed := id()
	if changed == before {
		t.Error("tracked change did not change the content ID")
	}
	write("new", "new\n")
	if got := id(); got == changed {
		t.Error("untracked file did not change the content ID")
	}
	excluded, err := repo.ContentID(func(path string) bool { return path == "new" })
	if err != nil {
		t.Fatal(err)
	}
	if excluded != changed {
		t.Errorf("excluding the new file: got %s, want %s", excluded, changed)
	}
}
