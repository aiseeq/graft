package checks

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

func snapshotRepo(t *testing.T) (string, *gitx.Repo, func(...string), func(string, string)) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "--template=")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.com")
	repo, err := gitx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, repo, git, write
}

func TestStagedReadsCurrentIndexAcrossChanges(t *testing.T) {
	dir, repo, git, write := snapshotRepo(t)
	write("line\nbreak", "first\n")
	if err := os.Symlink("line\nbreak", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	git("add", "--all")
	check := func(want string) {
		t.Helper()
		files, err := Staged(repo)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 2 || files[0].Path != "line\nbreak" || string(files[0].Content) != want || files[1].Path != "link" || !files[1].Symlink || string(files[1].Content) != "line\nbreak" {
			t.Fatalf("staged files: %+v", files)
		}
	}
	check("first\n")
	write("line\nbreak", "second\n")
	check("first\n") // An unstaged edit does not change the index.
	git("add", "line\nbreak")
	check("second\n") // The next call sees the new staged object.
	git("-c", "core.hooksPath=/dev/null", "commit", "-qm", "first")
	git("rm", "-q", "link")
	files, err := Staged(repo)
	if err != nil || len(files) != 0 {
		t.Fatalf("staged deletion: %+v, %v", files, err)
	}
}

func TestRevertsSeesIndexChangedAfterNoCandidate(t *testing.T) {
	_, repo, git, write := snapshotRepo(t)
	write("tracked", "original\n")
	git("add", "tracked")
	git("-c", "core.hooksPath=/dev/null", "commit", "-qm", "first")
	write("tracked", "changed\n")
	git("add", "tracked")
	git("-c", "core.hooksPath=/dev/null", "commit", "-qm", "second")
	write("other", "new\n")
	git("add", "other")
	cfg := config.Reverts{Depth: 60}
	findings, err := Reverts(repo, cfg, nil, "unrelated", "HEAD")
	if err != nil || len(findings) != 0 {
		t.Fatalf("unrelated file: %+v, %v", findings, err)
	}
	write("tracked", "original\n")
	git("add", "tracked")
	findings, err = Reverts(repo, cfg, nil, "unrelated", "HEAD")
	if err != nil || len(findings) != 1 || findings[0].Path != "tracked" {
		t.Fatalf("staged revert after index change: %+v, %v", findings, err)
	}
}

func TestRevertsRejectsUnmergedIndexWithoutCandidate(t *testing.T) {
	dir, repo, git, write := snapshotRepo(t)
	write("conflict", "base\n")
	git("add", "conflict")
	git("-c", "core.hooksPath=/dev/null", "commit", "-qm", "first")
	write("history", "recent\n")
	git("add", "history")
	git("-c", "core.hooksPath=/dev/null", "commit", "-qm", "second")
	write("unrelated", "new\n")
	git("add", "unrelated")
	lookup := exec.Command("git", "rev-parse", "HEAD:conflict")
	lookup.Dir = dir
	oid, err := lookup.Output()
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.TrimSpace(string(oid))
	update := exec.Command("git", "update-index", "--index-info")
	update.Dir = dir
	update.Stdin = strings.NewReader("0 " + hash + "\tconflict\n" +
		"100644 " + hash + " 1\tconflict\n" +
		"100644 " + hash + " 2\tconflict\n" +
		"100644 " + hash + " 3\tconflict\n")
	if out, err := update.CombinedOutput(); err != nil {
		t.Fatalf("creating an unmerged index: %v\n%s", err, out)
	}
	if _, err := Reverts(repo, config.Reverts{Depth: 1}, nil, "unrelated change", "HEAD"); err == nil {
		t.Fatal("an unmerged index must still fail tree validation")
	}
}
