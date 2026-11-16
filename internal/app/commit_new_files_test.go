package app

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/gittest"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

func TestNoteCommitAdds(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) string { return gittest.Run(t, root, args...) }
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "tracked"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked")
	git("commit", "-q", "-m", "base")
	repo, err := gitx.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	a := &App{Stderr: &stderr}

	a.noteCommitAdds(repo)
	if stderr.Len() != 0 {
		t.Fatalf("no additions: got %q", stderr.String())
	}

	if err := os.WriteFile(filepath.Join(root, "added"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	a.noteCommitAdds(repo)
	if got, want := stderr.String(), "graft: commit adds new files: added\n"; got != want {
		t.Fatalf("tree addition: got %q, want %q", got, want)
	}

	stderr.Reset()
	if err := os.WriteFile(filepath.Join(root, "pre-added"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "pre-added")
	a.noteCommitAdds(repo)
	if !strings.Contains(stderr.String(), "pre-added") {
		t.Fatalf("pre-staged addition not named: %q", stderr.String())
	}

	stderr.Reset()
	git("commit", "-q", "-m", "add files")
	git("mv", "tracked", "renamed")
	git("config", "diff.renames", "false")
	a.noteCommitAdds(repo)
	if stderr.Len() != 0 {
		t.Fatalf("rename should not be named as a new file: %q", stderr.String())
	}

	stderr.Reset()
	for i := range 30 {
		name := fmt.Sprintf("file-%02d", i)
		if err := os.WriteFile(filepath.Join(root, name), []byte("new\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	a.noteCommitAdds(repo)
	lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "30 new files") {
		t.Fatalf("30 additions should produce one counted line: %q", stderr.String())
	}
	if strings.Count(lines[0], "file-") != 5 {
		t.Fatalf("expected five listed paths: %q", lines[0])
	}
}

func TestNoteCommitAddsOnFirstCommit(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) string { return gittest.Run(t, root, args...) }
	git("init", "-q", "-b", "main")
	for _, name := range []string{"first", "second"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("new\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "first", "second")
	repo, err := gitx.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	(&App{Stderr: &stderr}).noteCommitAdds(repo)
	if got, want := stderr.String(), "graft: commit adds new files: first, second\n"; got != want {
		t.Fatalf("first commit: got %q, want %q", got, want)
	}
}
