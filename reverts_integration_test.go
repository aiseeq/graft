package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestCommitRefusesSilentRevert: a commit whose tree takes back a recent
// commit's change to a file, a stale tree on a fresh parent say, is refused
// unless its message names that commit or the file; a revert git makes names
// the commit and passes, and so does another change to the same file.
func TestCommitRefusesSilentRevert(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\nchecks: {reverts: {exceptions: [{path: 'gen/*', reason: generated}]}}\n",
		"VERSION":     "1.0.0\n",
		"a.txt":       "1\n2\n3\n",
		"gen/out.txt": "x\n",
	}, "origin")
	f.git("checkout", "-q", "-b", "main")
	f.write("VERSION", "1.0.1\n")
	f.mustPlainCommit("feat: base")
	f.write("a.txt", "1\ntwo\n3\n")
	f.write("b.txt", "b\n")
	f.write("gen/out.txt", "y\n")
	f.mustPlainCommit("feat: merged work")
	x := f.git("rev-parse", "HEAD")

	// The tree of before the merged work, committed on top of it.
	f.write("a.txt", "1\n2\n3\n")
	f.write("c.txt", "c\n")
	out, code := f.graft("", "commit", "-m", "feat: other work")
	if code == 0 || !strings.Contains(out, "a.txt: takes back "+x[:12]+" feat: merged work") {
		t.Errorf("a stale tree: exit %d\n%s", code, out)
	}
	if got := f.git("rev-parse", "HEAD"); got != x {
		t.Fatalf("the refused commit was made: %s", got)
	}
	// Another change to the same file passes; so does an excepted file.
	f.write("a.txt", "1\ntwo\nthree\n")
	f.write("gen/out.txt", "x\n")
	f.mustGraft("commit", "-m", "feat: other work")
	y := f.git("rev-parse", "HEAD")

	// Deleting a file a recent commit added takes it back too.
	f.git("rm", "-q", "b.txt")
	if out, code := f.graft("", "commit", "-m", "chore: tidy"); code == 0 || !strings.Contains(out, "b.txt: takes back "+x[:12]) {
		t.Errorf("a deleted file: exit %d\n%s", code, out)
	}
	// Naming the file or the commit says it is meant.
	f.mustGraft("commit", "-m", "chore: b.txt is not needed")
	// The short hash git prints names the commit as well.
	f.git("rm", "-q", "c.txt")
	f.mustGraft("commit", "-m", "chore: undo "+y[:7])

	// git revert names the commit it reverts.
	f.write("a.txt", "1\nTWO\nthree\n")
	f.mustGraft("commit", "-m", "feat: louder")
	f.git("revert", "--no-edit", "HEAD")

	// Appending to a file whose recent commit appended too is no take-back,
	// though that commit's patch, anchored at the end by its leading context,
	// fits the new end, which repeats the old one.
	tail := "\t}))\n}\n"
	f.write("g.go", "func A() {\n\tf(`\n`,\n"+tail)
	f.mustGraft("commit", "-m", "feat: A")
	f.write("g.go", "func A() {\n\tf(`\n`,\n"+tail+"\nfunc B() {\n\ty()\n}\n")
	f.mustPlainCommit("feat: B")
	f.write("g.go", "func A() {\n\tf(`\n`,\n"+tail+"\nfunc B() {\n\ty()\n}\n\nfunc C() {\n\tf(`\n`,\n"+tail)
	f.mustGraft("commit", "-m", "feat: C")
}

// TestAmendRevertsAgainstParent: amend checks the amended commit against its
// parent, so undoing part of the commit being amended is no take-back, and
// taking back the commit before it still is.
func TestAmendRevertsAgainstParent(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\n",
		"a.txt":       "1\n2\n3\n",
		"b.txt":       "b\n",
	})
	f.mustPlainCommit("feat: base")
	f.write("b.txt", "b2\n")
	f.mustPlainCommit("feat: b")
	x := f.git("rev-parse", "HEAD")
	f.write("a.txt", "1\ntwo\n3\n")
	f.mustPlainCommit("feat: summary")

	// The amend puts a.txt back as the parent has it: part of its own
	// commit undone.
	f.write("a.txt", "1\n2\n3\n")
	f.write("c.txt", "c\n")
	f.mustGraft("amend")
	// Taking back the commit before the amended one is refused.
	f.write("b.txt", "b\n")
	if out, code := f.graft("", "amend"); code == 0 || !strings.Contains(out, "b.txt: takes back "+x[:12]+" feat: b") {
		t.Errorf("amend taking back the parent: exit %d\n%s", code, out)
	}
}

// TestCommitTakingAFileOutOfTheIndexIsNoRevert: git rm --cached of a file a
// recent commit added, with a .gitignore rule for it, as graft's hint for a
// committed file says, keeps the file on disk: no take-back. Deleting a file
// still is one.
func TestCommitTakingAFileOutOfTheIndexIsNoRevert(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n",
		"VERSION":     "1.0.0\n",
	}, "origin")
	f.git("checkout", "-q", "-b", "main")
	f.write("VERSION", "1.0.1\n")
	f.mustPlainCommit("feat: base")
	f.write("notes/a.md", "a\n")
	f.write("notes/b.md", "b\n")
	f.write("d.txt", "d\n")
	f.mustPlainCommit("feat: notes")
	x := f.git("rev-parse", "HEAD")

	f.git("rm", "-q", "--cached", "notes/a.md", "notes/b.md")
	f.write(".gitignore", "/notes/\n")
	f.mustGraft("commit", "-m", "chore: notes stay local")
	if out := f.git("ls-files", "notes"); out != "" {
		t.Fatalf("notes still in the index: %q", out)
	}

	f.git("rm", "-q", "d.txt")
	if out, code := f.graft("", "commit", "-m", "chore: tidy"); code == 0 || !strings.Contains(out, "d.txt: takes back "+x[:12]) {
		t.Errorf("a deleted file: exit %d\n%s", code, out)
	}
}

// TestCommitReplacingAFolderIsNoErrorOfTheTakeBackCheck: a tracked folder that
// a plain file replaced is gone, a take-back the subject can name, not an
// error of the check; one that an ignored symbolic link replaced keeps its
// files on disk, no take-back.
func TestCommitReplacingAFolderIsNoErrorOfTheTakeBackCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on windows")
	}
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n",
		"VERSION":     "1.0.0\n",
	}, "origin")
	f.git("checkout", "-q", "-b", "main")
	f.write("VERSION", "1.0.1\n")
	f.mustPlainCommit("feat: base")
	f.write("notes/a.md", "a\n")
	f.write("tasks/one.md", "one\n")
	f.mustPlainCommit("feat: notes and tasks")
	x := f.git("rev-parse", "HEAD")

	store := filepath.Join(f.base, "store")
	if err := os.CopyFS(store, os.DirFS(filepath.Join(f.work, "tasks"))); err != nil {
		t.Fatal(err)
	}
	f.git("rm", "-q", "-r", "--cached", "tasks")
	if err := os.RemoveAll(filepath.Join(f.work, "tasks")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(store, filepath.Join(f.work, "tasks")); err != nil {
		t.Fatal(err)
	}
	f.write(".gitignore", "/tasks\n")
	f.mustGraft("commit", "-m", "chore: tasks move to a store of their own")

	if err := os.RemoveAll(filepath.Join(f.work, "notes")); err != nil {
		t.Fatal(err)
	}
	f.write("notes", "now a file\n")
	if out, code := f.graft("", "commit", "-m", "chore: notes in one file"); code == 0 || !strings.Contains(out, "notes/a.md: takes back "+x[:12]) {
		t.Errorf("a folder a file replaced: exit %d\n%s", code, out)
	}
}

// TestCommitReplacingRecentFilesSilentlyIsATakeBack: a folder of a recent
// commit's files that a symbolic link .gitignore does not ignore replaced, and
// a file that an ignored folder replaced, are take-backs: the files are gone
// from the commit.
func TestCommitReplacingRecentFilesSilentlyIsATakeBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on windows")
	}
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\n",
		"VERSION":     "1.0.0\n",
	}, "origin")
	f.git("checkout", "-q", "-b", "main")
	f.write("VERSION", "1.0.1\n")
	f.mustPlainCommit("feat: base")
	f.write("notes/a.md", "a\n")
	f.write("docs/b.md", "b\n")
	f.mustPlainCommit("feat: notes and docs")
	x := f.git("rev-parse", "HEAD")

	if err := os.CopyFS(filepath.Join(f.work, "store"), os.DirFS(filepath.Join(f.work, "notes"))); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(f.work, "notes")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("store", filepath.Join(f.work, "notes")); err != nil {
		t.Fatal(err)
	}
	f.write(".gitignore", "/store/\n")
	if out, code := f.graft("", "commit", "-m", "chore: notes through a link"); code == 0 || !strings.Contains(out, "notes/a.md: takes back "+x[:12]) {
		t.Errorf("a folder an unignored link replaced: exit %d\n%s", code, out)
	}
	f.git("reset", "-q")
	if err := os.Remove(filepath.Join(f.work, "notes")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(f.work, "store"), filepath.Join(f.work, "notes")); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(f.work, "docs", "b.md")); err != nil {
		t.Fatal(err)
	}
	f.write("docs/b.md/keep", "k\n")
	f.write(".gitignore", "/docs/b.md/\n")
	if out, code := f.graft("", "commit", "-m", "chore: docs kept apart"); code == 0 || !strings.Contains(out, "docs/b.md: takes back "+x[:12]) {
		t.Errorf("a file an ignored folder replaced: exit %d\n%s", code, out)
	}
}
