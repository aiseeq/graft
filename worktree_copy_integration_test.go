package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/app"
)

// A file under worktree.copy goes with the tree only while it is what the main
// worktree holds; clean and check judge it alike.
func TestWorktreeCopyComparedWithItsSource(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("clean's process check reads /proc")
	}
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\nworktree:\n  copy: [.env, conf]\n",
		".gitignore":  ".env\nconf/\n", "VERSION": "1.0.0\n",
	})
	writeFile(t, filepath.Join(f.work, ".env"), "PORT=1\n")
	writeFile(t, filepath.Join(f.work, "conf", "a"), "a\n")
	copies := func(path string) {
		t.Helper()
		writeFile(t, filepath.Join(path, ".env"), "PORT=1\n")
		writeFile(t, filepath.Join(path, "conf", "a"), "a\n")
	}
	kept := func(what, path, name string) {
		t.Helper()
		answer, code, out := worktreeCheckJSON(t, f, f.work, path)
		if code != app.ExitWorktreeHolds || answer.Clear {
			t.Errorf("%s: check let it go; exit %d:\n%s", what, code, out)
		}
		out, code = f.graft("", "worktree", "clean", "--min-age", "0s")
		line := outputLineForPath(out, path)
		if code != 0 || !strings.Contains(line, "check") || !strings.Contains(line, name) {
			t.Errorf("%s: clean does not keep and name %s; exit %d:\n%s", what, name, code, out)
		}
	}

	same := add("same")
	copies(same)
	if answer, code, out := worktreeCheckJSON(t, f, f.work, same); code != 0 || !answer.Clear {
		t.Errorf("copies matching the source hold nothing; exit %d:\n%s", code, out)
	}
	if out, code := f.graft("", "worktree", "clean", "--min-age", "0s"); code != 0 || !strings.Contains(outputLineForPath(out, same), "would remove") {
		t.Errorf("clean keeps copies matching the source; exit %d:\n%s", code, out)
	}

	edited := add("edited")
	copies(edited)
	writeFile(t, filepath.Join(edited, ".env"), "PORT=2\n")
	kept("an edited copy", edited, ".env")

	longer := add("longer")
	copies(longer)
	writeFile(t, filepath.Join(longer, ".env"), "PORT=1\nMORE=1\n")
	kept("a copy with a line added", longer, ".env")

	inFolder := add("in-folder")
	copies(inFolder)
	writeFile(t, filepath.Join(inFolder, "conf", "a"), "changed\n")
	kept("a changed file of a copied folder", inFolder, "conf/")

	extra := add("extra")
	copies(extra)
	writeFile(t, filepath.Join(extra, "conf", "only-here"), "x\n")
	answer, code, out := worktreeCheckJSON(t, f, f.work, extra)
	if code != app.ExitWorktreeHolds || answer.Files != 1 {
		t.Errorf("a file the source lacks is not the only one counted; exit %d:\n%s", code, out)
	}

	if err := os.Remove(filepath.Join(f.work, ".env")); err != nil {
		t.Fatal(err)
	}
	kept("a copy whose source is gone", same, ".env")
}

// disposable comes before copy: a project whose setup rewrites a copy says so.
func TestWorktreeDisposableBeforeCopy(t *testing.T) {
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\nworktree:\n  copy: [.env]\n  disposable: [/.env]\n",
		".gitignore":  ".env\n", "VERSION": "1.0.0\n",
	})
	writeFile(t, filepath.Join(f.work, ".env"), "PORT=1\n")
	path := add("rewritten")
	writeFile(t, filepath.Join(path, ".env"), "PORT=2\n")
	if answer, code, out := worktreeCheckJSON(t, f, f.work, path); code != 0 || !answer.Clear {
		t.Errorf("a disposable copy holds nothing; exit %d:\n%s", code, out)
	}
}

// A leading / roots a name: the file at the root goes, a file of that name in a
// folder stays, unlike the same name without it.
func TestWorktreeDisposableRootedName(t *testing.T) {
	f, add := newWorktreeCleanFixture(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\nworktree:\n  disposable: [/config.yaml, any.yaml]\n",
		".gitignore":  "config.yaml\nany.yaml\n", "VERSION": "1.0.0\n",
	})
	path := add("rooted")
	writeFile(t, filepath.Join(path, "config.yaml"), "x")
	writeFile(t, filepath.Join(path, "deeper", "any.yaml"), "x")
	if answer, code, out := worktreeCheckJSON(t, f, f.work, path); code != 0 || !answer.Clear {
		t.Fatalf("the rooted file and a name at any depth are disposable; exit %d:\n%s", code, out)
	}
	writeFile(t, filepath.Join(path, "deeper", "config.yaml"), "x")
	answer, code, out := worktreeCheckJSON(t, f, f.work, path)
	if code != app.ExitWorktreeHolds || answer.Files != 1 || len(answer.Roots) != 1 || answer.Roots[0].Name != "deeper" {
		t.Fatalf("a file of that name in a folder went with the rooted pattern; exit %d:\n%s", code, out)
	}
}
