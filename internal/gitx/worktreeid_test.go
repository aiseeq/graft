package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestChangedPathsIncludesIndexAndWorkTreeChanges(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.invalid")
	for name, data := range map[string]string{"modified": "before\n", "deleted": "before\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	if err := os.WriteFile(filepath.Join(dir, "modified"), []byte("after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "deleted")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "staged-add"), []byte("staged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "staged-add")
	if err := os.WriteFile(filepath.Join(dir, "untracked"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "line\nbreak"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	repo, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.changedPaths()
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	want := []string{"deleted", "line\nbreak", "modified", "staged-add", "untracked"}
	if !slices.Equal(got, want) {
		t.Errorf("changed paths = %q, want %q", got, want)
	}
}
