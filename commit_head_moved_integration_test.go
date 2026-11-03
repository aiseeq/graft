package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// headMovesRepo is a repository whose gate runs steps, with a branch side one
// commit ahead of the current one for a step to merge while the gate runs.
func headMovesRepo(t *testing.T, gate string) (*fixture, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\ngate:\n" + gate,
		"base.txt":    "base\n",
	})
	f.git("-c", "core.hooksPath=/dev/null", "add", "-A")
	f.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "chore: base")
	branch := f.git("symbolic-ref", "--short", "HEAD")
	f.git("checkout", "-q", "-b", "side")
	f.write("side.txt", "side\n")
	f.git("-c", "core.hooksPath=/dev/null", "add", "-A")
	f.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "feat: side")
	tip := f.git("rev-parse", "HEAD")
	f.git("checkout", "-q", branch)
	return f, tip
}

// checkHeadMovedRefusal checks that a command the merge overtook says so and
// nothing else, and leaves the merge in HEAD and the work on top of it.
func checkHeadMovedRefusal(t *testing.T, f *fixture, out, tip string) {
	t.Helper()
	if !strings.Contains(out, "HEAD moved from ") {
		t.Errorf("output does not say HEAD moved:\n%s", out)
	}
	for _, wrong := range []string{"takes back", "left out"} {
		if strings.Contains(out, wrong) {
			t.Errorf("output says %q about the merged files:\n%s", wrong, out)
		}
	}
	if got := f.git("rev-parse", "HEAD"); got != tip {
		t.Errorf("HEAD = %s, want the merged %s", got, tip)
	}
	if got := f.read("side.txt"); got != "side\n" {
		t.Errorf("side.txt = %q, want what the merge brought", got)
	}
	if got := f.git("diff", "--cached", "--name-only"); got != "mine.txt" {
		t.Errorf("staged against the new HEAD: %q, want only the work, no merged file taken back", got)
	}
}

// TestCommitRefusesHeadMovedDuringTheGate: a merge another session makes while
// the gate runs is named as such, and the commit made again goes on top of it.
func TestCommitRefusesHeadMovedDuringTheGate(t *testing.T) {
	f, tip := headMovesRepo(t, "  - [git, merge, -q, --ff-only, side]\n")
	f.write("mine.txt", "mine\n")
	out, code := f.graft("", "commit", "-m", "feat: mine")
	if code == 0 {
		t.Fatalf("the commit went on over a moved HEAD:\n%s", out)
	}
	checkHeadMovedRefusal(t, f, out, tip)

	f.mustGraft("commit", "-m", "feat: mine")
	if got := f.git("rev-parse", "HEAD~1"); got != tip {
		t.Errorf("the second commit has parent %s, want the merged %s", got, tip)
	}
	if got := f.git("diff", "--name-only", "HEAD~1", "HEAD"); got != "mine.txt" {
		t.Errorf("the second commit changes %q, want mine.txt", got)
	}
}

// TestCommitGateFailureOverMovedHeadKeepsTheMerge: a gate that fails after the
// merge must not put back the index of the old HEAD, which would stage the
// merge's undoing.
func TestCommitGateFailureOverMovedHeadKeepsTheMerge(t *testing.T) {
	f, tip := headMovesRepo(t, "  - [git, merge, -q, --ff-only, side]\n  - [sh, -c, 'exit 3']\n")
	f.write("mine.txt", "mine\n")
	out, code := f.graft("", "commit", "-m", "feat: mine")
	if code == 0 {
		t.Fatalf("the failed gate let the commit through:\n%s", out)
	}
	if got := f.git("rev-parse", "HEAD"); got != tip {
		t.Errorf("HEAD = %s, want the merged %s", got, tip)
	}
	if got := f.git("diff", "--cached", "--name-only"); got != "mine.txt" {
		t.Errorf("staged against the new HEAD: %q, want only the work:\n%s", got, out)
	}
}

// TestAmendRefusesHeadMovedDuringTheGate: amending would replace the merged
// commit, not the one the gate ran for.
func TestAmendRefusesHeadMovedDuringTheGate(t *testing.T) {
	f, tip := headMovesRepo(t, "  - [git, merge, -q, --ff-only, side]\n")
	f.write("mine.txt", "mine\n")
	out, code := f.graft("", "amend")
	if code == 0 {
		t.Fatalf("the amend went on over a moved HEAD:\n%s", out)
	}
	checkHeadMovedRefusal(t, f, out, tip)
	if strings.Contains(out, "run the command again") || !strings.Contains(out, "graft commit") {
		t.Errorf("the refusal must send the work to a commit of its own, an amend run again takes the merged commit:\n%s", out)
	}
}

// moveHeadBeforeGit runs a git command once, right before the first git call
// of graft whose arguments hold call: a move no gate step could make.
func moveHeadBeforeGit(t *testing.T, f *fixture, call, move string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	flag := filepath.Join(dir, "moved")
	script := "#!/bin/sh\ncase \"$*\" in *'" + call + "'*)\n  if [ ! -e '" + flag + "' ]; then : > '" + flag + "'; '" + git + "' " + move + " >&2; fi;;\nesac\nexec '" + git + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.env = append(f.env, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestHeadMovedAfterTheGateSteps: HEAD moves past the check after the gate's
// steps, and the merged files must not read as taken back or left out.
func TestHeadMovedAfterTheGateSteps(t *testing.T) {
	for _, tc := range []struct {
		name, call string
		args       []string
	}{
		{"commit, during the checks", "diff --cached", []string{"commit", "-m", "feat: mine"}},
		{"commit, before the work tree is read", "--git-path index", []string{"commit", "-m", "feat: mine"}},
		{"amend, during the checks", "diff --cached", []string{"amend"}},
		{"amend, before the work tree is read", "--git-path index", []string{"amend"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, tip := headMovesRepo(t, "  - [sh, -c, 'true']\n")
			moveHeadBeforeGit(t, f, tc.call, "merge -q --ff-only side")
			f.write("mine.txt", "mine\n")
			out, code := f.graft("", tc.args...)
			if code == 0 {
				t.Fatalf("the command went on over a moved HEAD:\n%s", out)
			}
			checkHeadMovedRefusal(t, f, out, tip)
		})
	}
}

// TestCommitRefusesAnotherBranchDuringTheGate: the commit would go on a branch
// other than the one it names.
func TestCommitRefusesAnotherBranchDuringTheGate(t *testing.T) {
	f, _ := headMovesRepo(t, "  - [git, checkout, -q, -b, twin]\n")
	before := f.git("rev-parse", "HEAD")
	f.write("mine.txt", "mine\n")
	out, code := f.graft("", "commit", "-m", "feat: mine")
	if code == 0 {
		t.Fatalf("the commit went on over another branch:\n%s", out)
	}
	if !strings.Contains(out, "HEAD moved from ") || !strings.Contains(out, " on twin") {
		t.Errorf("the refusal does not name the branch:\n%s", out)
	}
	if got := f.git("rev-parse", "HEAD"); got != before {
		t.Errorf("HEAD = %s, want it where the gate left it, %s", got, before)
	}
}

// TestCommitRefusesHeadBornDuringTheGate: an unborn branch has no commit for
// the refusal to name, and it says so.
func TestCommitRefusesHeadBornDuringTheGate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\ngate:\n  - [git, -c, core.hooksPath=/dev/null, commit, -q, --allow-empty, -m, 'chore: first']\n",
	})
	f.write("mine.txt", "mine\n")
	out, code := f.graft("", "commit", "-m", "feat: mine")
	if code == 0 {
		t.Fatalf("the commit went on over a born HEAD:\n%s", out)
	}
	if !strings.Contains(out, "HEAD moved from no commit on ") {
		t.Errorf("the refusal does not say HEAD had no commit:\n%s", out)
	}
}

// versionedHeadMoves is a file mode repository at 1.2.4, a commit above 1.2.3,
// whose gate moves HEAD; side, a commit ahead, raises the version to
// sideVersion when that is given.
func versionedHeadMoves(t *testing.T, gate, sideVersion string) *fixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\ngate:\n" + gate,
		"VERSION":     "1.2.3\n",
	}, "origin")
	f.mustPlainCommit("feat: base")
	f.write("VERSION", "1.2.4\n")
	f.mustPlainCommit("fix: second")
	f.git("push", "-q", "-u", "origin", "HEAD")
	branch := f.git("symbolic-ref", "--short", "HEAD")
	f.git("checkout", "-q", "-b", "side")
	f.write("side.txt", "side\n")
	if sideVersion != "" {
		f.write("VERSION", sideVersion+"\n")
	}
	f.mustPlainCommit("feat: side")
	f.git("checkout", "-q", branch)
	return f
}

// checkVersionFollowsHead checks a release the moved HEAD stopped: the version
// is the new HEAD's in the tree and the index, nothing is left to commit.
func checkVersionFollowsHead(t *testing.T, f *fixture, out string, want string) {
	t.Helper()
	if !strings.Contains(out, "HEAD moved from ") {
		t.Errorf("output does not say HEAD moved:\n%s", out)
	}
	if got := strings.TrimSpace(f.read("VERSION")); got != want {
		t.Errorf("VERSION in the tree = %q, want the new HEAD's %q", got, want)
	}
	if got := f.git("status", "--porcelain"); got != "" {
		t.Errorf("the release left in the tree and the index:\n%s\n%s", got, out)
	}
}

func TestReleaseOverMovedHeadTakesItsVersion(t *testing.T) {
	t.Run("the merge leaves the version alone, and the release run again goes", func(t *testing.T) {
		f := versionedHeadMoves(t, "  - [git, merge, -q, --ff-only, side]\n", "")
		out, code := f.graft("", "release", "--minor")
		if code == 0 {
			t.Fatalf("the release went on over a moved HEAD:\n%s", out)
		}
		checkVersionFollowsHead(t, f, out, "1.2.4")
		f.mustGraft("release", "--minor")
		if got := strings.TrimSpace(f.git("show", "HEAD:VERSION")); got != "1.3.0" {
			t.Errorf("HEAD:VERSION = %q, want 1.3.0", got)
		}
	})
	t.Run("the merge raises the version to the same", func(t *testing.T) {
		f := versionedHeadMoves(t, "  - [git, merge, -q, --ff-only, side]\n", "1.3.0")
		out, code := f.graft("", "release", "--minor")
		if code == 0 {
			t.Fatalf("the release went on over a moved HEAD:\n%s", out)
		}
		checkVersionFollowsHead(t, f, out, "1.3.0")
	})
	t.Run("the gate resets HEAD back", func(t *testing.T) {
		f := versionedHeadMoves(t, "  - [git, reset, -q, --hard, HEAD~1]\n", "")
		out, code := f.graft("", "release", "--minor")
		if code == 0 {
			t.Fatalf("the release went on over a moved HEAD:\n%s", out)
		}
		checkVersionFollowsHead(t, f, out, "1.2.3")
	})
	t.Run("the tree holds the version in CRLF, the commits in LF", func(t *testing.T) {
		f := versionedHeadMoves(t, "  - [git, merge, -q, --ff-only, side]\n", "1.3.0")
		f.git("config", "core.autocrlf", "true")
		if err := os.Remove(filepath.Join(f.work, "VERSION")); err != nil {
			t.Fatal(err)
		}
		f.git("checkout", "-q", "--", "VERSION")
		out, code := f.graft("", "release", "--minor")
		if code == 0 {
			t.Fatalf("the release went on over a moved HEAD:\n%s", out)
		}
		checkVersionFollowsHead(t, f, out, "1.3.0")
		if got := f.read("VERSION"); got != "1.3.0\r\n" {
			t.Errorf("VERSION in the tree = %q, want the commit's through the filters", got)
		}
	})
	t.Run("a soft reset keeps the sync file the new HEAD has not", func(t *testing.T) {
		f := newRepo(t, map[string]string{
			".graft.yaml": "schema: 1\nversion:\n  mode: file\n  sync:\n    - {path: pkg.json, format: json, key: [version]}\ngate:\n  - [git, reset, -q, --soft, HEAD~1]\n",
			"VERSION":     "1.2.3\n",
		}, "origin")
		f.mustPlainCommit("feat: base")
		f.write("pkg.json", "{\"version\":\"1.2.3\"}\n")
		f.mustPlainCommit("feat: pkg")
		f.git("push", "-q", "-u", "origin", "HEAD")
		out, code := f.graft("", "release", "--minor")
		if code == 0 {
			t.Fatalf("the release went on over a moved HEAD:\n%s", out)
		}
		if got := f.read("pkg.json"); !strings.Contains(got, `"1.2.3"`) {
			t.Errorf("pkg.json = %q, want the reset's file with the bump taken back:\n%s", got, out)
		}
	})
	t.Run("the checks see a reset", func(t *testing.T) {
		f := versionedHeadMoves(t, "  - [sh, -c, 'true']\n", "")
		moveHeadBeforeGit(t, f, "diff --cached", "reset -q --hard HEAD~1")
		f.write("mine.txt", "mine\n")
		out, code := f.graft("", "commit", "-m", "feat: mine")
		if code == 0 {
			t.Fatalf("the commit went on over a moved HEAD:\n%s", out)
		}
		checkVersionFollowsHead(t, f, out, "1.2.3")
	})
	t.Run("the new HEAD has no sync file", func(t *testing.T) {
		f := newRepo(t, map[string]string{
			".graft.yaml": "schema: 1\nversion:\n  mode: file\n  sync:\n    - {path: pkg.json, format: json, key: [version]}\ngate:\n  - [git, reset, -q, --hard, HEAD~1]\n",
			"VERSION":     "1.2.3\n",
		}, "origin")
		f.mustPlainCommit("feat: base")
		f.write("pkg.json", "{\"version\":\"1.2.3\"}\n")
		f.mustPlainCommit("feat: pkg")
		f.git("push", "-q", "-u", "origin", "HEAD")
		out, code := f.graft("", "release", "--minor")
		if code == 0 {
			t.Fatalf("the release went on over a moved HEAD:\n%s", out)
		}
		if strings.Contains(out, "restoring version files") {
			t.Errorf("the refusal has a second cause, for a file the new HEAD does not have:\n%s", out)
		}
		checkVersionFollowsHead(t, f, out, "1.2.3")
		if _, err := os.Stat(filepath.Join(f.work, "pkg.json")); !os.IsNotExist(err) {
			t.Errorf("pkg.json is back in the tree over a HEAD without it: %v", err)
		}
	})
	t.Run("a failed rebase still takes the bump back", func(t *testing.T) {
		f := versionedHeadMoves(t, "  - [git, merge, -q, --ff-only, side]\n  - [git, config, filter.stamp.clean, 'false']\n", "")
		f.git("config", "filter.stamp.clean", "cat")
		f.git("config", "filter.stamp.smudge", "cat")
		f.git("config", "filter.stamp.required", "true")
		f.write(".gitattributes", "VERSION filter=stamp\n")
		f.mustPlainCommit("chore: filter")
		f.git("branch", "-f", "side", "HEAD")
		branch := f.git("symbolic-ref", "--short", "HEAD")
		f.git("checkout", "-q", "side")
		f.write("side.txt", "side\n")
		f.mustPlainCommit("feat: side")
		tip := f.git("rev-parse", "HEAD")
		f.git("checkout", "-q", branch)
		out, code := f.graft("", "release", "--minor")
		if code == 0 {
			t.Fatalf("the release went on over a moved HEAD:\n%s", out)
		}
		if got := f.git("rev-parse", "HEAD"); got != tip {
			t.Fatalf("HEAD = %s, want the merged %s: the rebase never ran:\n%s", got, tip, out)
		}
		if got := strings.TrimSpace(f.read("VERSION")); got != "1.2.4" {
			t.Errorf("VERSION in the tree = %q, want the bump taken back:\n%s", got, out)
		}
	})
}
