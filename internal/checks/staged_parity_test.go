package checks

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/gitx"
)

type stagedView struct {
	path    string
	content string
	size    int64
	symlink bool
}

func expectStagedViews(t *testing.T, repo *gitx.Repo, want []stagedView) {
	t.Helper()
	files, err := Staged(repo)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]stagedView, 0, len(files))
	for _, file := range files {
		got = append(got, stagedView{file.Path, string(file.Content), file.Size, file.Symlink})
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("staged files: got %#v, want %#v", got, want)
	}
}

func snapshotGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestStagedRawDiffParity(t *testing.T) {
	t.Run("gitlink", func(t *testing.T) {
		dir, repo, git, write := snapshotRepo(t)
		write("base", "base\n")
		git("add", "base")
		git("-c", "core.hooksPath=/dev/null", "commit", "-qm", "base")
		head := snapshotGitOutput(t, dir, "rev-parse", "HEAD")
		git("update-index", "--add", "--cacheinfo", "160000,"+head+",linked")
		write("added", "file\n")
		git("add", "added")
		expectStagedViews(t, repo, []stagedView{{path: "added", content: "file\n", size: 5}})
	})

	t.Run("rename non-UTF8 executable", func(t *testing.T) {
		dir, repo, git, write := snapshotRepo(t)
		git("config", "core.filemode", "true")
		write("source", "script\n")
		git("add", "source")
		git("-c", "core.hooksPath=/dev/null", "commit", "-qm", "base")
		name := "renamed-\xff"
		git("mv", "source", name)
		if err := os.Chmod(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		git("add", "--", name)
		index := snapshotGitOutput(t, dir, "ls-files", "--stage", "-z", "--", name)
		if !strings.HasPrefix(index, "100755 ") {
			t.Fatalf("executable not staged with mode 100755: %q", index)
		}
		expectStagedViews(t, repo, []stagedView{{path: "renamed-\xff", content: "script\n", size: 7}})
	})

	t.Run("conflict and type change", func(t *testing.T) {
		dir, repo, git, write := snapshotRepo(t)
		write("conflict", "base\n")
		write("typed", "regular\n")
		git("add", "--all")
		git("-c", "core.hooksPath=/dev/null", "commit", "-qm", "base")
		if err := os.Remove(filepath.Join(dir, "typed")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("target", filepath.Join(dir, "typed")); err != nil {
			t.Fatal(err)
		}
		git("add", "typed")
		hash := snapshotGitOutput(t, dir, "rev-parse", "HEAD:conflict")
		update := exec.Command("git", "update-index", "--index-info")
		update.Dir = dir
		update.Stdin = strings.NewReader("0 " + hash + "\tconflict\n" +
			"100644 " + hash + " 1\tconflict\n" +
			"100644 " + hash + " 2\tconflict\n" +
			"100644 " + hash + " 3\tconflict\n")
		if out, err := update.CombinedOutput(); err != nil {
			t.Fatalf("creating an unmerged index: %v\n%s", err, out)
		}
		expectStagedViews(t, repo, []stagedView{{path: "typed", content: "target", size: 6, symlink: true}})
	})
}
