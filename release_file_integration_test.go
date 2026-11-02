package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fileReleaseRepo(t *testing.T, tagOnCommit bool, gate string) *fixture {
	t.Helper()
	config := "schema: 1\nversion:\n  mode: file\n  skip_branches: ['feature/*', 'fix/*']\n  build_branches: ['build/*']\n  sync: [{path: package.json, format: json, key: [version]}]\n"
	if tagOnCommit {
		config += "  tag_on_commit: true\n"
	}
	config += gate
	f := newRepo(t, map[string]string{
		".graft.yaml":  config,
		"VERSION":      "0.7.0\n",
		"package.json": "{\"version\":\"0.7.0\"}\n",
	}, "origin")
	f.mustPlainCommit("chore: initial")
	f.git("branch", "-M", "main")
	f.git("push", "-u", "origin", "main")
	return f
}

func ffFeature(t *testing.T, f *fixture) {
	t.Helper()
	f.git("checkout", "-q", "-b", "feature/x")
	f.write("one.txt", "one\n")
	f.mustPlainCommit("feat: first change")
	f.write("two.txt", "two\n")
	f.mustPlainCommit("feat: second change")
	f.git("checkout", "-q", "main")
	f.git("merge", "-q", "--ff-only", "feature/x")
}

func TestReleaseFileBumpsCommitsAndPushes(t *testing.T) {
	for _, tc := range []struct {
		name, flag, want string
		tagOnCommit      bool
	}{
		{"minor", "--minor", "0.8.0", false},
		{"minor-tag-on-commit", "--minor", "0.8.0", true},
		{"patch", "--patch", "0.7.1", false},
		{"major", "--major", "1.0.0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fileReleaseRepo(t, tc.tagOnCommit, "gate: []\n")
			ffFeature(t, f)
			before := f.git("rev-parse", "HEAD")
			f.mustGraft("release", tc.flag)
			if f.read("VERSION") != tc.want+"\n" || !strings.Contains(f.read("package.json"), tc.want) {
				t.Fatalf("release files: VERSION=%q package.json=%q", f.read("VERSION"), f.read("package.json"))
			}
			head := f.git("rev-parse", "HEAD")
			if got := f.git("show", "-s", "--format=%s", "HEAD"); got != "chore(release): v"+tc.want {
				t.Fatalf("subject = %q", got)
			}
			if got := f.git("rev-parse", "v"+tc.want+"^{commit}"); got != head {
				t.Fatalf("tag points to %s, HEAD is %s", got, head)
			}
			if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != head {
				t.Fatalf("origin/main = %s, want %s", got, head)
			}
			if got := f.gitIn(f.remote("origin"), "rev-parse", "v"+tc.want+"^{commit}"); got != head {
				t.Fatalf("origin tag points to %s, want %s", got, head)
			}
			if got := f.git("rev-parse", "HEAD^"); got != before {
				t.Fatalf("release parent = %s, want pre-release HEAD %s", got, before)
			}
			if got := strings.Fields(f.git("tag", "-l", "v"+tc.want)); len(got) != 1 {
				t.Fatalf("local release tag count = %d", len(got))
			}
		})
	}
}

func TestReleaseFileRefusalsAreAtomic(t *testing.T) {
	cases := []struct {
		name, branch, setup, flag, want string
	}{
		{name: "dirty", flag: "--minor", setup: "dirty", want: "graft commit first"},
		{name: "skip branch", branch: "feature/x", flag: "--minor", want: "version.skip_branches"},
		{name: "build branch", branch: "build/one", flag: "--minor", want: "version.build_branches"},
		{name: "existing next tag", flag: "--minor", setup: "next-tag", want: "tag v0.8.0 already exists"},
		{name: "gate failure", flag: "--minor", setup: "gate-fail", want: "gate"},
		{name: "released head", flag: "--minor", setup: "released", want: "HEAD is already released as v0.7.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := "gate: []\n"
			if tc.setup == "gate-fail" {
				gate = "gate: [[sh, -c, 'exit 1']]\n"
			}
			f := fileReleaseRepo(t, false, gate)
			if tc.branch == "feature/x" {
				f.git("checkout", "-q", "-b", tc.branch)
			} else if tc.branch != "" {
				f.git("checkout", "-q", "-b", tc.branch)
			}
			if tc.setup == "dirty" {
				f.write("dirty.txt", "dirty\n")
			}
			if tc.setup == "next-tag" {
				f.git("tag", "v0.8.0")
				f.write("later.txt", "later\n")
				f.mustPlainCommit("feat: later")
			}
			if tc.setup == "released" {
				f.git("tag", "v0.7.0")
			}
			beforeVersion, beforePackage := f.read("VERSION"), f.read("package.json")
			beforeHead := f.git("rev-parse", "HEAD")
			beforeTags := f.git("tag", "-l")
			beforeStatus := f.git("status", "--porcelain")
			out, code := f.graft("", "release", tc.flag)
			if code == 0 || !strings.Contains(out, tc.want) {
				t.Fatalf("release refusal: exit %d, want %q\n%s", code, tc.want, out)
			}
			if f.read("VERSION") != beforeVersion || f.read("package.json") != beforePackage || f.git("rev-parse", "HEAD") != beforeHead || f.git("tag", "-l") != beforeTags || f.git("status", "--porcelain") != beforeStatus {
				t.Fatalf("refusal changed repository state\n%s", out)
			}
		})
	}
}

func TestReleaseFileWithoutStepExplainsNextStep(t *testing.T) {
	f := fileReleaseRepo(t, false, "gate: []\n")
	f.git("tag", "v0.7.0")
	if out, code := f.graft("", "release"); code == 0 || !strings.Contains(out, "HEAD is already released as v0.7.0") {
		t.Fatalf("release of a released HEAD: exit %d\n%s", code, out)
	}
	ffFeature(t, f)
	f.git("push", "-q", "origin", "main")
	if out, code := f.graft("", "release"); code == 0 || !strings.Contains(out, "--minor") {
		t.Fatalf("release existing HEAD version: exit %d\n%s", code, out)
	}
}

func TestReleasePatchFlagInGitTagMode(t *testing.T) {
	make := func(t *testing.T) *fixture {
		t.Helper()
		f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: git-tag}\ngate: []\n"}, "origin")
		f.mustPlainCommit("chore: initial")
		f.git("push", "-u", "origin", "HEAD")
		return f
	}
	plain, patched := make(t), make(t)
	plain.mustGraft("release")
	patched.mustGraft("release", "--patch")
	if plain.git("tag", "-l") != patched.git("tag", "-l") {
		t.Fatalf("--patch changed git-tag release versions: %q != %q", plain.git("tag", "-l"), patched.git("tag", "-l"))
	}
	f := make(t)
	if out, code := f.graft("", "release", "--patch", "--minor"); code == 0 || !strings.Contains(out, "excludes") {
		t.Fatalf("conflicting release levels: exit %d\n%s", code, out)
	}
	f = make(t)
	if out, code := f.graft("", "release", "--patch", "--version", "1.0.0"); code == 0 || !strings.Contains(out, "--patch and --version") {
		t.Fatalf("conflicting patch and exact version: exit %d\n%s", code, out)
	}
}

func TestVersionBumpThenCommitUsesRaisedVersion(t *testing.T) {
	for _, tagOnCommit := range []bool{false, true} {
		t.Run(fmt.Sprintf("tag_on_commit=%v", tagOnCommit), func(t *testing.T) {
			f := fileReleaseRepo(t, tagOnCommit, "gate: []\n")
			f.mustGraft("version", "bump", "--minor")
			out := f.mustGraft("commit", "-m", "chore: release prep")
			if !strings.Contains(out, "version 0.8.0 (raised before the commit)") {
				t.Fatalf("pre-raised version was not reported:\n%s", out)
			}
			if got := f.read("VERSION"); got != "0.8.0\n" {
				t.Fatalf("VERSION = %q", got)
			}
			if got := f.git("show", "-s", "--format=%s", "HEAD"); got != "chore: release prep" {
				t.Fatalf("subject = %q", got)
			}
			if tagOnCommit && f.git("rev-parse", "v0.8.0^{commit}") != f.git("rev-parse", "HEAD") {
				t.Fatal("tag_on_commit did not tag the raised version")
			}
			f.write("next.txt", "next\n")
			if out := f.mustGraft("commit", "-m", "fix: next change"); !strings.Contains(out, "version 0.8.1") {
				t.Fatalf("ordinary commit did not bump patch:\n%s", out)
			}
		})
	}
}

func TestReleaseVersionOnlyNeedsNoTicket(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml":  "schema: 1\nversion: {mode: file, sync: [{path: package.json, format: json, key: [version]}]}\nticket: {pattern: 'PROJ-[0-9]+', required: true}\ngate: []\n",
		"VERSION":      "0.7.0\n",
		"package.json": "{\"version\":\"0.7.0\"}\n",
	}, "origin")
	f.mustPlainCommit("chore: initial")
	f.git("branch", "-M", "main")
	f.git("push", "-u", "origin", "main")
	f.mustGraft("release", "--minor")
	f.write("ordinary.txt", "work\n")
	if out, code := f.graft("", "commit", "-m", "feat: ordinary work"); code == 0 || !strings.Contains(out, "ticket.required") {
		t.Fatalf("ordinary commit without a ticket: exit %d\n%s", code, out)
	}
	if err := os.Remove(filepath.Join(f.work, "ordinary.txt")); err != nil {
		t.Fatal(err)
	}
	// Only a release passes with the version files alone: an ordinary
	// commit of them still names its work item.
	f.write("VERSION", "0.9.0\n")
	f.write("package.json", "{\"version\":\"0.9.0\"}\n")
	if out, code := f.graft("", "commit", "-m", "chore: hand version"); code == 0 || !strings.Contains(out, "ticket.required") {
		t.Fatalf("ordinary commit of the version files without a ticket: exit %d\n%s", code, out)
	}
}

// TestRaisedVersionChecksSyncAndMerges: a version raised before the commit is
// kept only when the synced files agree with it, and in a merge only when the
// merged branch did not bring it.
func TestRaisedVersionChecksSyncAndMerges(t *testing.T) {
	t.Run("sync mismatch", func(t *testing.T) {
		f := fileReleaseRepo(t, false, "gate: []\n")
		head := f.git("rev-parse", "HEAD")
		f.write("VERSION", "0.8.0\n")
		f.write("a.txt", "a\n")
		if out, code := f.graft("", "commit", "-m", "feat: hand raise"); code == 0 || !strings.Contains(out, "make them agree") {
			t.Fatalf("hand-raised VERSION without package.json: exit %d\n%s", code, out)
		}
		if f.git("rev-parse", "HEAD") != head {
			t.Fatal("a commit was made")
		}
	})
	t.Run("merge brings the version", func(t *testing.T) {
		f := fileReleaseRepo(t, false, "gate: []\n")
		f.git("checkout", "-q", "-b", "dev")
		f.write("VERSION", "0.9.0\n")
		f.write("package.json", "{\"version\":\"0.9.0\"}\n")
		f.write("d.txt", "d\n")
		f.mustPlainCommit("feat: dev release")
		f.git("checkout", "-q", "main")
		f.write("m.txt", "m\n")
		f.mustPlainCommit("feat: on main")
		f.git("merge", "-q", "--no-ff", "--no-commit", "dev")
		out := f.mustGraft("commit", "-m", "chore: merge dev")
		if !strings.Contains(out, "version 0.9.1") || strings.Contains(out, "raised before") {
			t.Fatalf("merge that brought 0.9.0:\n%s", out)
		}
	})
	t.Run("merge then bump", func(t *testing.T) {
		f := fileReleaseRepo(t, false, "gate: []\n")
		f.git("checkout", "-q", "-b", "feature/y")
		f.write("y.txt", "y\n")
		f.mustPlainCommit("feat: y")
		f.git("checkout", "-q", "main")
		f.write("m.txt", "m\n")
		f.mustPlainCommit("feat: on main")
		f.git("merge", "-q", "--no-ff", "--no-commit", "feature/y")
		f.mustGraft("version", "bump", "--minor")
		out := f.mustGraft("commit", "-m", "chore: merge y")
		if !strings.Contains(out, "version 0.8.0 (raised before the commit)") || f.read("package.json") != "{\"version\":\"0.8.0\"}\n" {
			t.Fatalf("merge with a bump before the commit:\n%s\npackage.json %q", out, f.read("package.json"))
		}
	})
}

// TestReleaseFileEdges: a second release of the same code is refused, a step
// on a version raised before the commit is refused, a version file HEAD
// cannot read does not stop a commit, the release takes the branch's key and
// keeps the module versions.
func TestReleaseFileEdges(t *testing.T) {
	t.Run("second release of the same code", func(t *testing.T) {
		f := fileReleaseRepo(t, false, "gate: []\n")
		ffFeature(t, f)
		f.mustGraft("release", "--minor")
		head := f.git("rev-parse", "HEAD")
		if out, code := f.graft("", "release", "--minor"); code == 0 || !strings.Contains(out, "HEAD is already released as v0.8.0") {
			t.Fatalf("second release: exit %d\n%s", code, out)
		}
		if f.git("rev-parse", "HEAD") != head || f.git("tag", "-l") != "v0.8.0" {
			t.Fatalf("second release changed HEAD or tags: %s", f.git("tag", "-l"))
		}
	})
	t.Run("step on a raised version", func(t *testing.T) {
		f := fileReleaseRepo(t, false, "gate: []\n")
		f.mustGraft("version", "bump")
		f.write("a.txt", "a\n")
		if out, code := f.graft("", "commit", "--minor", "-m", "feat: a"); code == 0 || !strings.Contains(out, "raised above HEAD's version") {
			t.Fatalf("--minor on a raised version: exit %d\n%s", code, out)
		}
	})
	for name, broken := range map[string]string{"garbage": "garbage\n", "empty": ""} {
		t.Run("unreadable version in HEAD: "+name, func(t *testing.T) {
			f := fileReleaseRepo(t, false, "gate: []\n")
			f.write("VERSION", broken)
			f.mustPlainCommit("chore: break the version")
			f.write("VERSION", "0.7.0\n")
			if out := f.mustGraft("commit", "-m", "fix: repair the version"); !strings.Contains(out, "version 0.7.1") || strings.Contains(out, "raised before") {
				t.Fatalf("repair of an unreadable version:\n%s", out)
			}
		})
	}
	t.Run("version bump raises every time", func(t *testing.T) {
		f := fileReleaseRepo(t, false, "gate: []\n")
		f.mustGraft("version", "bump")
		if out := f.mustGraft("version", "bump"); !strings.Contains(out, "0.7.1 -> 0.7.2") {
			t.Fatalf("second version bump:\n%s", out)
		}
		if out := f.mustGraft("version", "bump", "--minor"); !strings.Contains(out, "0.7.2 -> 0.8.0") {
			t.Fatalf("version bump --minor after a bump:\n%s", out)
		}
	})
	t.Run("tag_on_commit makes one tag", func(t *testing.T) {
		f := fileReleaseRepo(t, true, "gate: []\n")
		ffFeature(t, f)
		f.mustGraft("release", "--minor")
		if got := f.git("tag", "-l"); got != "v0.8.0" {
			t.Fatalf("tags = %q", got)
		}
		if got := f.gitIn(f.remote("origin"), "tag", "-l"); got != "v0.8.0" {
			t.Fatalf("origin tags = %q", got)
		}
	})
	t.Run("branch key", func(t *testing.T) {
		f := newRepo(t, map[string]string{
			".graft.yaml": "schema: 1\nversion: {mode: file}\nticket: {pattern: 'PROJ-[0-9]+'}\ngate: []\n",
			"VERSION":     "0.7.0\n",
		}, "origin")
		f.mustPlainCommit("chore: initial")
		f.git("branch", "-M", "PROJ-12-trunk")
		f.git("push", "-u", "origin", "PROJ-12-trunk")
		f.mustGraft("release", "--minor")
		if got := f.git("show", "-s", "--format=%B", "HEAD"); !strings.HasPrefix(got, "chore(release): v0.8.0\n") || !strings.Contains(got, "PROJ-12") {
			t.Fatalf("release message = %q", got)
		}
	})
}

// TestReleaseFileKeepsModules: the release commit carries the version files
// only; the next graft commit raises the project's own modules.
func TestReleaseFileKeepsModules(t *testing.T) {
	skipGoToolchainInShort(t)
	f := modulesRepo(t, modulesProxy(t), latestConfig, "Old")
	f.mustPlainCommit("chore: module sums")
	out := f.mustGraft("release", "--minor")
	if strings.Contains(out, " -> v0.2.0") {
		t.Errorf("release raised modules:\n%s", out)
	}
	if line := requireLine(t, f.git("show", "HEAD:go.mod"), ownLib); !strings.HasSuffix(line, " v0.1.0") {
		t.Errorf("released go.mod: %q", line)
	}
	if got := f.git("show", "--name-only", "--format=", "HEAD"); strings.Contains(got, "go.mod") {
		t.Errorf("release commit files:\n%s", got)
	}
}
