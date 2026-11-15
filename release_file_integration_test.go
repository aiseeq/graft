package main

import (
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

func TestReleaseFileChecksAuthorsBeforeBump(t *testing.T) {
	f := fileReleaseRepo(t, false, "release:\n  authors: [test@example.com]\ngate: []\n")
	f.git("tag", "v0.7.0")
	prior := f.env
	f.env = append(f.env, "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_EMAIL=test@example.com")
	f.commitWithAuthor("other@example.com")
	f.env = prior
	before := f.git("rev-list", "--count", "HEAD")
	version := f.read("VERSION")
	out, code := f.graft("", "release", "--patch")
	if code == 0 || !strings.Contains(out, "author other@example.com") {
		t.Fatalf("file release with foreign author: exit %d\n%s", code, out)
	}
	if got := f.git("rev-list", "--count", "HEAD"); got != before {
		t.Errorf("release created a commit: count %s, was %s", got, before)
	}
	if got := f.read("VERSION"); got != version {
		t.Errorf("VERSION changed from %q to %q", version, got)
	}
	if _, code := f.tryGit("show-ref", "--verify", "--quiet", "refs/tags/v0.7.1"); code == 0 {
		t.Error("release created tag v0.7.1 after author refusal")
	}
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
		{name: "open merge", flag: "--minor", setup: "merge", want: "merge is in progress"},
		{name: "open cherry-pick", flag: "--minor", setup: "cherry-pick", want: "cherry-pick is in progress"},
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
			if tc.setup == "merge" || tc.setup == "cherry-pick" {
				f.git("checkout", "-q", "-b", "other")
				f.write("same.txt", "same\n")
				f.mustPlainCommit("feat: other")
				f.git("checkout", "-q", "main")
				if tc.setup == "merge" {
					f.git("merge", "-q", "-s", "ours", "--no-commit", "other")
				} else {
					// The same change on main leaves the cherry-pick empty
					// and open, with a clean tree.
					f.write("same.txt", "same\n")
					f.mustPlainCommit("feat: same on main")
					f.exec(f.work, "", "git", "-c", "core.hooksPath=/dev/null", "cherry-pick", "other")
				}
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
	if out, code := f.graft("", "release"); code == 0 || !strings.Contains(out, "HEAD is already released as v0.7.0") || strings.Contains(out, "pass --version") {
		t.Fatalf("release of a released HEAD: exit %d\n%s", code, out)
	}
	if out, code := f.graft("", "release", "--version", "0.8.0", "--minor"); code == 0 || !strings.Contains(out, "in version.mode file release takes no --version") || strings.Contains(out, "already released") {
		t.Fatalf("file-mode flags should be validated before release status: exit %d\n%s", code, out)
	}
	ffFeature(t, f)
	f.git("push", "-q", "origin", "main")
	if out, code := f.graft("", "release"); code == 0 || !strings.Contains(out, "--minor") {
		t.Fatalf("release existing HEAD version: exit %d\n%s", code, out)
	}
}

func TestReleaseFileRefusalFollowsMainVersionChecks(t *testing.T) {
	t.Run("old tag does not block the version in VERSION", func(t *testing.T) {
		f := fileReleaseRepo(t, false, "gate: []\n")
		f.git("tag", "v0.6.0")
		f.mustGraft("release")
		if got := f.git("tag", "-l", "v0.7.0"); got != "v0.7.0" {
			t.Fatalf("release tag = %q, want v0.7.0", got)
		}
	})
	t.Run("invalid VERSION is reported before existing HEAD tag", func(t *testing.T) {
		f := fileReleaseRepo(t, false, "gate: []\n")
		f.write("VERSION", "bad\n")
		f.git("add", "VERSION")
		f.git("commit", "-q", "-m", "chore: invalid version")
		f.git("tag", "v0.6.0")
		out, code := f.graft("", "release")
		if code == 0 || !strings.Contains(out, "is not a MAJOR.MINOR.PATCH") || strings.Contains(out, "already released") {
			t.Fatalf("invalid VERSION should be reported first: exit %d\n%s", code, out)
		}
	})
}

func TestReleaseVersionModeNoneIgnoresTags(t *testing.T) {
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none}\ngate: []\n"}, "origin")
	f.mustPlainCommit("chore: initial")
	f.git("tag", "v1.0.0")
	for _, args := range [][]string{{"release"}, {"release", "--minor"}, {"release", "--version", "1.0.0"}} {
		out, code := f.graft("", args...)
		if code == 0 || !strings.Contains(out, "version.mode none has no releases") || strings.Contains(out, "already released") || strings.Contains(out, "git push") {
			t.Errorf("release %v in none mode: exit %d\n%s", args, code, out)
		}
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

// TestReleaseFileEdges: a second release of the same code is refused, a
// commit still bumps over a version file HEAD cannot read, graft version bump
// raises every time, the release takes the branch's key and makes one tag.
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
	for name, broken := range map[string]string{"garbage": "garbage\n", "empty": "", "out of range": "99999999999999999999.0.0\n"} {
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

// TestReleaseFileRemoteBranch: a release over a branch the remote moved past
// is refused before any change, and a branch push the remote rejects leaves
// no tag on the remote and none here.
func TestReleaseFileRemoteBranch(t *testing.T) {
	t.Run("remote ahead", func(t *testing.T) {
		f := fileReleaseRepo(t, false, "gate: []\n")
		other := filepath.Join(t.TempDir(), "other")
		if out, code := f.exec(f.work, "", "git", "clone", "-q", f.remote("origin"), other); code != 0 {
			t.Fatalf("clone: %s", out)
		}
		if err := os.WriteFile(filepath.Join(other, "n.txt"), []byte("n\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"add", "-A"}, {"-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "feat: neighbour"}, {"push", "-q", "origin", "main"}} {
			if out, code := f.exec(other, "", "git", args...); code != 0 {
				t.Fatalf("git %v: %s", args, out)
			}
		}
		ffFeature(t, f)
		head, version := f.git("rev-parse", "HEAD"), f.read("VERSION")
		if out, code := f.graft("", "release", "--minor"); code == 0 || !strings.Contains(out, "which HEAD does not contain") {
			t.Fatalf("release over a moved remote branch: exit %d\n%s", code, out)
		}
		if f.git("rev-parse", "HEAD") != head || f.read("VERSION") != version || f.git("tag", "-l") != "" || f.gitIn(f.remote("origin"), "tag", "-l") != "" {
			t.Fatal("the refusal changed the repository or the remote")
		}
	})
	t.Run("branch push rejected", func(t *testing.T) {
		f := fileReleaseRepo(t, false, "gate: []\n")
		ffFeature(t, f)
		hook := filepath.Join(f.remote("origin"), "hooks", "pre-receive")
		if err := os.WriteFile(hook, []byte("#!/bin/sh\nwhile read old new ref; do case $ref in refs/heads/*) echo rejected >&2; exit 1;; esac; done\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		out, code := f.graft("", "release", "--minor")
		if code == 0 || !strings.Contains(out, "tag v0.8.0 is not") {
			t.Fatalf("release with a rejected branch push: exit %d\n%s", code, out)
		}
		retryAt := strings.Index(out, "retry with: git push origin HEAD")
		tagAt := strings.Index(out, "tag v0.8.0 is not")
		if retryAt < 0 || tagAt <= retryAt || strings.Contains(out, "local refs are kept") || !strings.Contains(out, "local tag v0.8.0 was removed") {
			t.Fatalf("branch failure should explain the absent local tag after the retry command: exit %d\n%s", code, out)
		}
		if got := f.gitIn(f.remote("origin"), "tag", "-l"); got != "" {
			t.Fatalf("remote tags = %q", got)
		}
		if got := f.git("tag", "-l"); got != "" {
			t.Fatalf("local tags = %q", got)
		}
		if f.read("VERSION") != "0.8.0\n" {
			t.Fatalf("the release commit is gone: VERSION %q", f.read("VERSION"))
		}
		if err := os.Remove(hook); err != nil {
			t.Fatal(err)
		}
		f.git("push", "-q", "origin", "main")
		f.mustGraft("release")
		if got := f.gitIn(f.remote("origin"), "tag", "-l"); got != "v0.8.0" {
			t.Fatalf("release after the push: remote tags %q", got)
		}
	})
	t.Run("tag push rejected and retry is suggested", func(t *testing.T) {
		f := fileReleaseRepo(t, false, "gate: []\n")
		ffFeature(t, f)
		hook := filepath.Join(f.remote("origin"), "hooks", "pre-receive")
		if err := os.WriteFile(hook, []byte("#!/bin/sh\nwhile read old new ref; do case $ref in refs/tags/*) echo rejected >&2; exit 1;; esac; done\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		out, code := f.graft("", "release", "--minor")
		if code == 0 || !strings.Contains(out, "git push origin refs/tags/v0.8.0") {
			t.Fatalf("release with a rejected tag push: exit %d\n%s", code, out)
		}
		if got := f.git("tag", "-l"); got != "v0.8.0" {
			t.Fatalf("local tags = %q", got)
		}
		if err := os.Remove(hook); err != nil {
			t.Fatal(err)
		}
		trace := filepath.Join(t.TempDir(), "git-trace")
		f.env = append(f.env, "GIT_TRACE="+trace)
		out, code = f.graft("", "release")
		if code == 0 || !strings.Contains(out, "already released as v0.8.0") || !strings.Contains(out, "if a push of v0.8.0 stopped") || !strings.Contains(out, "git push --no-follow-tags -- origin refs/tags/v0.8.0") {
			t.Fatalf("retry after tag push failure: exit %d\n%s", code, out)
		}
		if traceOutput, err := os.ReadFile(trace); err != nil || containsRemoteGitOperation(string(traceOutput)) {
			t.Fatalf("already-released refusal queried a remote; trace err=%v\n%s", err, traceOutput)
		}
		if got := f.git("ls-remote", "--tags", "origin", "refs/tags/v0.8.0"); strings.TrimSpace(got) != "" {
			t.Fatalf("repeat release pushed the rejected tag: %q", got)
		}
		f.git("push", "-q", "origin", "refs/tags/v0.8.0")
		out, code = f.graft("", "release")
		if code == 0 || !strings.Contains(out, "already released as v0.8.0") || !strings.Contains(out, "if a push of v0.8.0 stopped") {
			t.Fatalf("release after tag reached all remotes: exit %d\n%s", code, out)
		}
	})
	t.Run("patch on a build branch", func(t *testing.T) {
		f := fileReleaseRepo(t, false, "gate: []\n")
		f.git("checkout", "-q", "-b", "build/one")
		if out, code := f.graft("", "release", "--patch"); code == 0 || !strings.Contains(out, "version.build_branches") {
			t.Fatalf("--patch on a build branch: exit %d\n%s", code, out)
		}
	})
}

func TestReleaseReportsUnpublishedTagWithoutPushing(t *testing.T) {
	t.Run("manual tag on unpublished commit", func(t *testing.T) {
		f := fileReleaseRepo(t, false, "gate: []\n")
		f.write("local.txt", "not published\n")
		f.mustPlainCommit("feat: local release candidate")
		f.git("tag", "v0.7.0")
		out, code := f.graft("", "release", "--minor")
		if code == 0 || !strings.Contains(out, "already released as v0.7.0") || !strings.Contains(out, "git push --no-follow-tags -- origin refs/tags/v0.7.0") {
			t.Fatalf("release with manual tag on unpublished HEAD: exit %d\n%s", code, out)
		}
		if got := f.git("ls-remote", "--tags", "origin", "refs/tags/v0.7.0"); strings.TrimSpace(got) != "" {
			t.Fatalf("release published manual tag: %q", got)
		}
	})
	t.Run("one hint per remote", func(t *testing.T) {
		f := fileReleaseRepo(t, false, "gate: []\n")
		f.git("tag", "v0.7.0")
		backup := filepath.Join(t.TempDir(), "backup.git")
		if err := os.CopyFS(backup, os.DirFS(bareTemplate)); err != nil {
			t.Fatal(err)
		}
		f.addRemote("backup", backup)
		out, code := f.graft("", "release")
		if code == 0 || !strings.Contains(out, "git push --no-follow-tags -- origin refs/tags/v0.7.0") || !strings.Contains(out, "git push --no-follow-tags -- backup refs/tags/v0.7.0") {
			t.Fatalf("release refusal did not name each remote: exit %d\n%s", code, out)
		}
		for _, remote := range []string{"origin", "backup"} {
			if got := f.git("ls-remote", "--tags", remote, "refs/tags/v0.7.0"); strings.TrimSpace(got) != "" {
				t.Fatalf("release pushed tag to %s: %q", remote, got)
			}
		}
	})
	t.Run("shell quotes remote names", func(t *testing.T) {
		f := fileReleaseRepo(t, false, "gate: []\n")
		backup := filepath.Join(t.TempDir(), "backup.git")
		if err := os.CopyFS(backup, os.DirFS(bareTemplate)); err != nil {
			t.Fatal(err)
		}
		f.addRemote("backup;true", backup)
		gitConfig := filepath.Join(f.work, ".git", "config")
		data, err := os.ReadFile(gitConfig)
		if err != nil {
			t.Fatal(err)
		}
		data = []byte(strings.ReplaceAll(string(data), "\tfetch = +refs/heads/*:refs/remotes/backup;true/*\n", ""))
		if err := os.WriteFile(gitConfig, data, 0o600); err != nil {
			t.Fatal(err)
		}
		f.git("tag", "v0.7.0")
		out, code := f.graft("", "release")
		if code == 0 || !strings.Contains(out, "git push --no-follow-tags -- 'backup;true' refs/tags/v0.7.0") {
			t.Fatalf("unsafe remote was not quoted: exit %d\n%s", code, out)
		}
		const marker = "finish it with: "
		_, after, ok := strings.Cut(out, marker)
		if !ok {
			t.Fatalf("retry command missing: %s", out)
		}
		command := strings.SplitN(after, "\n", 2)[0]
		if output, code := f.exec(f.work, "", "sh", "-c", command); code != 0 {
			t.Fatalf("suggested shell command failed: exit %d\n%s", code, output)
		}
		if _, code := f.exec(backup, "", "git", "show-ref", "--verify", "--quiet", "refs/tags/v0.7.0"); code != 0 {
			t.Fatal("quoted command did not push tag to the intended remote")
		}
	})
	t.Run("remote beginning with dash works in shell", func(t *testing.T) {
		f := fileReleaseRepo(t, false, "gate: []\n")
		backup := filepath.Join(t.TempDir(), "backup.git")
		if err := os.CopyFS(backup, os.DirFS(bareTemplate)); err != nil {
			t.Fatal(err)
		}
		f.addRemote("-backup", backup)
		f.git("tag", "v0.7.0")
		out, code := f.graft("", "release")
		if code == 0 || !strings.Contains(out, "git push --no-follow-tags -- -backup refs/tags/v0.7.0") {
			t.Fatalf("dash-prefixed remote lacks option separator: exit %d\n%s", code, out)
		}
		const marker = "finish it with: "
		_, after, ok := strings.Cut(out, marker)
		if !ok {
			t.Fatalf("retry command missing: %s", out)
		}
		command := strings.SplitN(after, "\n", 2)[0]
		if output, code := f.exec(f.work, "", "sh", "-c", command); code != 0 {
			t.Fatalf("suggested command for dash-prefixed remote failed: exit %d\n%s", code, output)
		}
		if _, code := f.exec(backup, "", "git", "show-ref", "--verify", "--quiet", "refs/tags/v0.7.0"); code != 0 {
			t.Fatal("suggested command did not push tag to -backup")
		}
	})
}

func containsRemoteGitOperation(trace string) bool {
	for _, operation := range []string{"ls-remote", "git fetch", "upload-pack", "receive-pack", "git-remote-"} {
		if strings.Contains(trace, operation) {
			return true
		}
	}
	return false
}

func TestReleaseGitTagReportsRejectedTagPush(t *testing.T) {
	f := releaseBranchRepo(t)
	hook := filepath.Join(f.remote("origin"), "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nwhile read old new ref; do case $ref in refs/tags/*) echo rejected >&2; exit 1;; esac; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := f.graft("", "release", "--version", "1.0.0")
	if code == 0 || !strings.Contains(out, "git push origin refs/tags/v1.0.0") {
		t.Fatalf("git-tag release with rejected tag push: exit %d\n%s", code, out)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	out, code = f.graft("", "release")
	if code == 0 || !strings.Contains(out, "already released as v1.0.0") || !strings.Contains(out, "git push --no-follow-tags -- origin refs/tags/v1.0.0") {
		t.Fatalf("git-tag retry after rejected push: exit %d\n%s", code, out)
	}
	if got := f.git("ls-remote", "--tags", "origin", "refs/tags/v1.0.0"); strings.TrimSpace(got) != "" {
		t.Fatalf("repeat release pushed the rejected tag: %q", got)
	}
	f.git("push", "-q", "origin", "refs/tags/v1.0.0")
	out, code = f.graft("", "release")
	if code == 0 || !strings.Contains(out, "already released as v1.0.0") || !strings.Contains(out, "if a push of v1.0.0 stopped") {
		t.Fatalf("git-tag release after manual tag push: exit %d\n%s", code, out)
	}
}

func TestReleaseGitTagChecksEveryVersionOnHEAD(t *testing.T) {
	f := releaseBranchRepo(t)
	f.mustGraft("release", "--version", "1.0.0")
	hook := filepath.Join(f.remote("origin"), "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nwhile read old new ref; do case $ref in refs/tags/*) echo rejected >&2; exit 1;; esac; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := f.graft("", "release", "--version", "1.1.0")
	if code == 0 || !strings.Contains(out, "git push origin refs/tags/v1.1.0") {
		t.Fatalf("second version tag push rejected: exit %d\n%s", code, out)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"release"}} {
		out, code = f.graft("", args...)
		if code == 0 || !strings.Contains(out, "already released") || !strings.Contains(out, "git push --no-follow-tags -- origin refs/tags/v1.0.0") || !strings.Contains(out, "git push --no-follow-tags -- origin refs/tags/v1.1.0") {
			t.Fatalf("retry %v did not report the missing HEAD tag: exit %d\n%s", args, code, out)
		}
	}
	out, code = f.graft("", "release", "--version", "1.1.0")
	if code == 0 || !strings.Contains(out, "not above the latest tag") || strings.Contains(out, "git push") {
		t.Fatalf("exact version should retain main's refusal: exit %d\n%s", code, out)
	}
	out, code = f.graft("", "release", "--version", "1.0.0", "--minor")
	if code == 0 || !strings.Contains(out, "--version and --minor/--major exclude each other") || strings.Contains(out, "already released") {
		t.Fatalf("incompatible flags were not rejected first: exit %d\n%s", code, out)
	}
}

func TestReleaseGitTagChecksLatestTagBeforeFlagConflict(t *testing.T) {
	f := releaseBranchRepo(t)
	tag := "v999999999999999999999.0.0"
	f.git("tag", tag)
	out, code := f.graft("", "release", "--version", "1.0.0", "--minor")
	want := "tag " + tag + ": \"999999999999999999999.0.0\": version part \"999999999999999999999\" out of range"
	if code == 0 || !strings.Contains(out, want) || strings.Contains(out, "--version and --minor/--major exclude each other") {
		t.Fatalf("release should report main's reachable tag error before flag conflict: exit %d\nwant %q\n%s", code, want, out)
	}
}
