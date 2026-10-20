package main

import (
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
	f.mustGraft("commit", "-m", "feat: base")
	f.write("a.txt", "1\ntwo\n3\n")
	f.write("b.txt", "b\n")
	f.write("gen/out.txt", "y\n")
	f.mustGraft("commit", "-m", "feat: merged work")
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
}
