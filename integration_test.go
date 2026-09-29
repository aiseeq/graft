package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The integration tests drive the built graft binary against throwaway
// repositories with bare remotes, the way a user's shell would.

var (
	binDir    string
	graftBin  string
	helperBin string
	testEnv   []string
)

func TestMain(m *testing.M) {
	code, err := setup(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "integration setup:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func setup(m *testing.M) (int, error) {
	dir, err := os.MkdirTemp("", "graft-it-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)
	binDir = filepath.Join(dir, "bin")
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	graftBin = filepath.Join(binDir, "graft"+exe)
	helperBin = filepath.Join(binDir, "gatehelper"+exe)
	for target, pkg := range map[string]string{graftBin: ".", helperBin: "./testdata/gatehelper"} {
		out, err := exec.Command("go", "build", "-o", target, pkg).CombinedOutput()
		if err != nil {
			return 0, fmt.Errorf("go build %s: %v\n%s", pkg, err, out)
		}
	}
	gitconfig := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(gitconfig, []byte("[user]\n\tname = Test\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n[protocol \"file\"]\n\tallow = always\n"), 0o644); err != nil {
		return 0, err
	}
	testEnv, err = isolatedEnv(gitconfig)
	if err != nil {
		return 0, err
	}
	return m.Run(), nil
}

// isolatedEnv keeps the tests away from the user's git config and from the
// variables git sets when these tests run inside a hook (graft's own gate).
func isolatedEnv(gitconfig string) ([]string, error) {
	out, err := exec.Command("git", "rev-parse", "--local-env-vars").Output()
	if err != nil {
		return nil, err
	}
	drop := append(strings.Fields(string(out)), "GRAFT_COMMIT", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM", "PATH")
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.ContainsFunc(drop, func(d string) bool { return strings.EqualFold(d, name) }) {
			env = append(env, kv)
		}
	}
	return append(env,
		"GIT_CONFIG_GLOBAL="+gitconfig,
		"GIT_CONFIG_NOSYSTEM=1",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	), nil
}

type fixture struct {
	t    *testing.T
	base string
	work string
}

// newRepo makes a work tree on branch feature/PROJ-42-thing with bare remotes
// named after remotes, and writes files into it (not committed).
func newRepo(t *testing.T, files map[string]string, remotes ...string) *fixture {
	t.Helper()
	f := &fixture{t: t, base: t.TempDir()}
	f.work = filepath.Join(f.base, "work")
	f.run(f.base, "git", "init", "-q", "-b", "feature/PROJ-42-thing", "work")
	for _, r := range remotes {
		f.run(f.base, "git", "init", "-q", "--bare", r+".git")
		f.git("remote", "add", r, filepath.Join(f.base, r+".git"))
	}
	for name, content := range files {
		f.write(name, content)
	}
	return f
}

func yamlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// gateStep is a YAML list entry that runs the helper with args.
func gateStep(args ...string) string {
	parts := []string{yamlQuote(helperBin)}
	for _, a := range args {
		parts = append(parts, yamlQuote(a))
	}
	return "  - [" + strings.Join(parts, ", ") + "]\n"
}

func (f *fixture) run(dir, name string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = testEnv
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (f *fixture) git(args ...string) string {
	f.t.Helper()
	return strings.TrimSpace(f.run(f.work, "git", args...))
}

// gitIn runs git in another directory, such as a bare remote.
func (f *fixture) gitIn(dir string, args ...string) string {
	f.t.Helper()
	return strings.TrimSpace(f.run(dir, "git", args...))
}

func (f *fixture) remote(name string) string { return filepath.Join(f.base, name+".git") }

// tryGit runs git and returns its output and exit code without failing.
func (f *fixture) tryGit(args ...string) (string, int) {
	return f.exec(f.work, "", "git", args...)
}

func (f *fixture) graft(stdin string, args ...string) (string, int) {
	return f.exec(f.work, stdin, graftBin, args...)
}

func (f *fixture) exec(dir, stdin, name string, args ...string) (string, int) {
	f.t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = testEnv
	cmd.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return out.String(), exitErr.ExitCode()
	}
	if err != nil {
		f.t.Fatalf("%s: %v", name, err)
	}
	return out.String(), 0
}

func (f *fixture) mustGraft(args ...string) string {
	f.t.Helper()
	out, code := f.graft("", args...)
	if code != 0 {
		f.t.Fatalf("graft %s: exit %d\n%s", strings.Join(args, " "), code, out)
	}
	return out
}

func (f *fixture) write(name, content string) {
	f.t.Helper()
	p := filepath.Join(f.work, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) read(name string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.work, filepath.FromSlash(name)))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

func (f *fixture) commitCount() int {
	f.t.Helper()
	out, code := f.tryGit("rev-list", "--count", "HEAD")
	if code != 0 {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

const fileModeConfig = `schema: 1
version:
  mode: file
  sync:
    - path: web/package.json
      format: json
      key: [version]
    - path: app.yaml
      format: regex
      pattern: 'version:\s*"([^"]*)"'
ticket:
  pattern: '[A-Z][A-Z]+-[0-9]+'
gate:
`

func fileModeRepo(t *testing.T, gate string, remotes ...string) *fixture {
	return newRepo(t, map[string]string{
		".graft.yaml":      fileModeConfig + gate,
		"VERSION":          "1.2.3\n",
		"web/package.json": "{\n  \"name\": \"web\",\n  \"version\": \"1.2.3\"\n}\n",
		"app.yaml":         "name: app\nversion: \"1.2.3\"\n",
		"main.go":          "package main\n",
	}, remotes...)
}

func TestCommitBumpsSyncsAndPushesEverywhere(t *testing.T) {
	f := fileModeRepo(t, gateStep("ok"), "origin", "mirror")
	out := f.mustGraft("commit", "-m", "feat: first")

	if v := f.read("VERSION"); v != "1.2.4\n" {
		t.Errorf("VERSION = %q", v)
	}
	if !strings.Contains(f.read("web/package.json"), `"version": "1.2.4"`) || !strings.Contains(f.read("app.yaml"), `version: "1.2.4"`) {
		t.Errorf("sync files not bumped:\n%s\n%s", f.read("web/package.json"), f.read("app.yaml"))
	}
	if msg := f.git("log", "-1", "--format=%B"); msg != "feat: first\n\nPROJ-42" {
		t.Errorf("message = %q", msg)
	}
	head := f.git("rev-parse", "HEAD")
	for _, r := range []string{"origin", "mirror"} {
		if got := f.gitIn(f.remote(r), "rev-parse", "feature/PROJ-42-thing"); got != head {
			t.Errorf("%s has %s, want %s", r, got, head)
		}
	}
	if status := f.git("status", "--porcelain"); status != "" {
		t.Errorf("tree not clean after commit:\n%s", status)
	}
	if !strings.Contains(out, "version 1.2.4") {
		t.Errorf("summary missing version:\n%s", out)
	}

	f.write("main.go", "package main\n\nfunc main() {}\n")
	f.mustGraft("commit", "--minor", "-m", "feat: second")
	if v := f.read("VERSION"); v != "1.3.0\n" {
		t.Errorf("minor bump: VERSION = %q", v)
	}
	f.write("main.go", "package main\n\n// x\nfunc main() {}\n")
	f.mustGraft("commit", "--major", "-m", "feat!: third")
	if v := f.read("VERSION"); v != "2.0.0\n" {
		t.Errorf("major bump: VERSION = %q", v)
	}
}

func TestCommitMessageSurvivesVerbatim(t *testing.T) {
	f := fileModeRepo(t, gateStep("ok"))
	msg := "fix: keep $(x) $$HOME ${Y} \"quoted\" 'single' `tick` \\back\n\n# a line starting with hash\n\n\nbody with PROJ-42 already\n\nCo-Authored-By: Someone <s@example.com>"

	file := filepath.Join(f.base, "msg.txt")
	if err := os.WriteFile(file, []byte(msg+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.mustGraft("commit", "-F", file)
	if got := f.git("log", "-1", "--format=%B"); got != msg {
		t.Errorf("-F file:\ngot  %q\nwant %q", got, msg)
	}

	f.write("main.go", "package main // 2\n")
	if out, code := f.graft(msg, "commit", "-F", "-"); code != 0 {
		t.Fatalf("-F -: %d\n%s", code, out)
	}
	if got := f.git("log", "-1", "--format=%B"); got != msg {
		t.Errorf("-F -:\ngot  %q\nwant %q", got, msg)
	}

	f.write("main.go", "package main // 3\n")
	f.mustGraft("commit", "-m", "fix: $x `y`", "-m", "second \"para\"")
	if got := f.git("log", "-1", "--format=%B"); got != "fix: $x `y`\n\nsecond \"para\"\n\nPROJ-42" {
		t.Errorf("-m: %q", got)
	}
}

func TestCommitKeyGoesBeforeTrailers(t *testing.T) {
	f := fileModeRepo(t, gateStep("ok"))
	f.mustGraft("commit", "-m", "feat: x\n\nCo-Authored-By: A <a@example.com>")
	if got := f.git("log", "-1", "--format=%B"); got != "feat: x\n\nPROJ-42\n\nCo-Authored-By: A <a@example.com>" {
		t.Errorf("message = %q", got)
	}
	if got := f.git("log", "-1", "--format=%(trailers:key=Co-Authored-By,valueonly)"); got != "A <a@example.com>" {
		t.Errorf("trailer not recognised by git: %q", got)
	}
}

func TestPushFailureOnOneRemoteStillPushesTheOthers(t *testing.T) {
	f := fileModeRepo(t, gateStep("ok"), "zeta")
	// git lists remotes alphabetically: the broken one is pushed first.
	f.git("remote", "add", "alpha", filepath.Join(f.base, "missing.git"))

	out, code := f.graft("", "commit", "-m", "feat: x")
	if code == 0 {
		t.Fatalf("a failed remote must fail the command:\n%s", out)
	}
	if !strings.Contains(out, "push to alpha FAILED") || !strings.Contains(out, "1 of 2 remotes (alpha)") || !strings.Contains(out, "git push alpha HEAD") {
		t.Errorf("failure report:\n%s", out)
	}
	if got, head := f.gitIn(f.remote("zeta"), "rev-parse", "feature/PROJ-42-thing"), f.git("rev-parse", "HEAD"); got != head {
		t.Errorf("zeta not pushed: %s vs %s", got, head)
	}
}

func TestGateFailureLeavesVersionAlone(t *testing.T) {
	f := fileModeRepo(t, gateStep("fail"))
	out, code := f.graft("", "commit", "-m", "feat: x")
	if code == 0 {
		t.Fatalf("gate failure must fail the commit:\n%s", out)
	}
	if !strings.Contains(out, "gate failed") {
		t.Errorf("output:\n%s", out)
	}
	if v := f.read("VERSION"); v != "1.2.3\n" {
		t.Errorf("VERSION bumped despite the failed gate: %q", v)
	}
	if n := f.commitCount(); n != 0 {
		t.Errorf("%d commits made", n)
	}
}

func TestNothingToCommit(t *testing.T) {
	f := fileModeRepo(t, gateStep("ok"))
	f.mustGraft("commit", "-m", "feat: x")
	out, code := f.graft("", "commit", "-m", "feat: again")
	if code == 0 || !strings.Contains(out, "nothing to commit") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if v := f.read("VERSION"); v != "1.2.4\n" {
		t.Errorf("VERSION = %q", v)
	}
}

func TestEmptyMessageRefused(t *testing.T) {
	f := fileModeRepo(t, gateStep("ok"))
	for _, args := range [][]string{{"commit"}, {"commit", "-m", " "}, {"commit", "-F", "-"}} {
		if out, code := f.graft("", args...); code == 0 {
			t.Errorf("%v accepted:\n%s", args, out)
		}
	}
	if v := f.read("VERSION"); v != "1.2.3\n" {
		t.Errorf("VERSION = %q", v)
	}
}

func TestSecretBlocksCommitAndRollsBackVersion(t *testing.T) {
	f := fileModeRepo(t, gateStep("ok"))
	f.write("config.go", "package main\n\nconst id = \"AKIA"+strings.Repeat("Q", 16)+"\"\n")
	out, code := f.graft("", "commit", "-m", "feat: x")
	if code == 0 {
		t.Fatalf("secret committed:\n%s", out)
	}
	if !strings.Contains(out, "config.go:3: possible AWS access key id") {
		t.Errorf("finding:\n%s", out)
	}
	if strings.Contains(out, strings.Repeat("Q", 16)) {
		t.Errorf("output echoes the secret:\n%s", out)
	}
	if v := f.read("VERSION"); v != "1.2.3\n" {
		t.Errorf("VERSION not rolled back: %q", v)
	}
	if v := f.git("show", ":VERSION"); v != "1.2.3" {
		t.Errorf("staged VERSION not rolled back: %q", v)
	}
	if !strings.Contains(f.read("web/package.json"), `"1.2.3"`) {
		t.Error("sync file not rolled back")
	}
	if n := f.commitCount(); n != 0 {
		t.Errorf("%d commits made", n)
	}
}

func TestBinaryFileBlocked(t *testing.T) {
	f := fileModeRepo(t, gateStep("ok"))
	f.write("tool", "\x7fELF\x00\x00binary")
	out, code := f.graft("", "commit", "-m", "feat: x")
	if code == 0 || !strings.Contains(out, "tool: binary file") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

func TestConcurrentCommitsTakeTurns(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "gate.log")
	f := fileModeRepo(t, gateStep("log", logFile, "300"), "origin")
	f.mustGraft("commit", "-m", "feat: base")
	// A second worktree shares the repository and therefore the lock, but has
	// its own changes, so both commits have something to do.
	other := filepath.Join(f.base, "other")
	f.git("worktree", "add", "-q", "-b", "feature/PROJ-43-other", other)
	f.write("a.txt", "a\n")
	if err := os.WriteFile(filepath.Join(other, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(logFile); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make([]string, 2)
	codes := make([]int, 2)
	for i, dir := range []string{f.work, other} {
		wg.Go(func() {
			results[i], codes[i] = f.exec(dir, "", graftBin, "commit", "-m", fmt.Sprintf("feat: parallel %d", i))
		})
	}
	wg.Wait()
	for i := range codes {
		if codes[i] != 0 {
			t.Fatalf("commit %d: exit %d\n%s", i, codes[i], results[i])
		}
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(data))
	// Four lines of "event pid nanos": the gates ran one after the other.
	var events []string
	for i := 0; i+2 < len(lines); i += 3 {
		events = append(events, lines[i])
	}
	if !slices.Equal(events, []string{"start", "end", "start", "end"}) {
		t.Errorf("gates overlapped: %v\n%s", events, data)
	}
	if !strings.Contains(results[0]+results[1], "waiting for the repository lock") {
		t.Errorf("no process waited for the lock:\n%s\n%s", results[0], results[1])
	}
}

func hookRepo(t *testing.T, gate string) *fixture {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion:\n  mode: none\ngate:\n" + gate,
		"shared.txt":  "base\n",
	})
	f.mustGraft("init")
	f.mustGraft("commit", "-m", "chore: base")
	return f
}

func TestHookRefusesPlainCommit(t *testing.T) {
	f := hookRepo(t, gateStep("ok"))
	f.write("shared.txt", "changed\n")
	f.git("add", "-A")
	out, code := f.tryGit("commit", "-m", "raw")
	if code == 0 {
		t.Fatalf("plain git commit went through:\n%s", out)
	}
	if !strings.Contains(out, "graft commit -m") {
		t.Errorf("no hint:\n%s", out)
	}
	if out, code := f.tryGit("commit", "--amend", "--no-edit"); code == 0 {
		t.Errorf("plain amend went through:\n%s", out)
	}
}

func TestHookGatesConflictResolution(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "gate-ran")
	f := hookRepo(t, gateStep("touch", marker))
	f.git("switch", "-q", "-c", "side")
	f.write("shared.txt", "side\n")
	f.mustGraft("commit", "-m", "feat: side")
	f.git("switch", "-q", "feature/PROJ-42-thing")
	f.write("shared.txt", "main\n")
	f.mustGraft("commit", "-m", "feat: main")
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	if _, code := f.tryGit("merge", "side"); code == 0 {
		t.Fatal("expected a conflict")
	}
	f.write("shared.txt", "resolved\n")
	f.git("add", "-A")
	out, code := f.tryGit("commit", "--no-edit")
	if code != 0 {
		t.Fatalf("merge commit refused:\n%s", out)
	}
	if !strings.Contains(out, "merge commit: running the checks and the gate") {
		t.Errorf("hook output:\n%s", out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("gate did not run for the merge commit")
	}
	if parents := strings.Fields(f.git("log", "-1", "--format=%P")); len(parents) != 2 {
		t.Errorf("not a merge commit: %v", parents)
	}
}

func TestHookBlocksConflictResolutionWithFailingGate(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "broken")
	f := hookRepo(t, gateStep("failif", flag))
	f.git("switch", "-q", "-c", "side")
	f.write("shared.txt", "side\n")
	f.mustGraft("commit", "-m", "feat: side")
	f.git("switch", "-q", "feature/PROJ-42-thing")
	f.write("shared.txt", "main\n")
	f.mustGraft("commit", "-m", "feat: main")
	if _, code := f.tryGit("merge", "side"); code == 0 {
		t.Fatal("expected a conflict")
	}
	f.write("shared.txt", "resolved\n")
	f.git("add", "-A")
	if err := os.WriteFile(flag, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := f.tryGit("commit", "--no-edit"); code == 0 {
		t.Errorf("merge with a failing gate went through:\n%s", out)
	}
}

func TestPostMergeReportsSemanticConflict(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "broken")
	f := hookRepo(t, gateStep("failif", flag))
	f.git("switch", "-q", "-c", "side")
	f.write("side.txt", "side\n")
	f.mustGraft("commit", "-m", "feat: side")
	f.git("switch", "-q", "feature/PROJ-42-thing")
	f.write("main.txt", "main\n")
	f.mustGraft("commit", "-m", "feat: main")
	if err := os.WriteFile(flag, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := f.tryGit("merge", "--no-edit", "side")
	if !strings.Contains(out, "semantic conflict") || !strings.Contains(out, "graft amend") {
		t.Errorf("post-merge did not report the failing gate:\n%s", out)
	}
	if parents := strings.Fields(f.git("log", "-1", "--format=%P")); len(parents) != 2 {
		t.Fatalf("merge commit missing: %v", parents)
	}

	// The documented way out: fix, then fold the fix into the merge commit.
	if err := os.Remove(flag); err != nil {
		t.Fatal(err)
	}
	f.write("fix.txt", "fixed\n")
	f.mustGraft("amend")
	if parents := strings.Fields(f.git("log", "-1", "--format=%P")); len(parents) != 2 {
		t.Errorf("amend lost the merge parents: %v", parents)
	}
	if files := f.git("show", "--name-only", "--format=", "HEAD", "--", "fix.txt"); files != "fix.txt" {
		t.Errorf("fix not folded in: %q", files)
	}
}

func TestPostMergeIgnoresFastForward(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "broken")
	f := hookRepo(t, gateStep("failif", flag))
	f.git("switch", "-q", "-c", "side")
	f.write("side.txt", "side\n")
	f.mustGraft("commit", "-m", "feat: side")
	f.git("switch", "-q", "feature/PROJ-42-thing")
	if err := os.WriteFile(flag, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out := f.git("merge", "--ff-only", "side")
	if strings.Contains(out, "semantic conflict") {
		t.Errorf("fast-forward gated:\n%s", out)
	}
}

func TestAmendRefusesPublishedCommit(t *testing.T) {
	f := fileModeRepo(t, gateStep("ok"), "origin")
	f.mustGraft("commit", "-m", "feat: x")
	f.write("main.go", "package main // changed\n")
	out, code := f.graft("", "amend")
	if code == 0 || !strings.Contains(out, "already on origin") || !strings.Contains(out, "graft commit") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

func TestAmendFoldsIntoLocalCommit(t *testing.T) {
	f := fileModeRepo(t, gateStep("ok"))
	f.mustGraft("commit", "-m", "feat: x")
	f.write("extra.go", "package main\n")
	f.mustGraft("amend")
	if n := f.commitCount(); n != 1 {
		t.Errorf("%d commits, want 1", n)
	}
	if msg := f.git("log", "-1", "--format=%B"); msg != "feat: x\n\nPROJ-42" {
		t.Errorf("message changed: %q", msg)
	}
	if v := f.read("VERSION"); v != "1.2.4\n" {
		t.Errorf("amend must not bump: %q", v)
	}
	if out, code := f.graft("", "amend"); code == 0 || !strings.Contains(out, "nothing to amend") {
		t.Errorf("clean amend: exit %d\n%s", code, out)
	}
}

func TestGitTagMode(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion:\n  mode: git-tag\ngate:\n" + gateStep("ok"),
		"main.go":     "package main\n",
	}, "origin")
	if out, code := f.graft("", "commit", "--minor", "-m", "feat: x"); code == 0 || !strings.Contains(out, "graft release") {
		t.Errorf("--minor in git-tag mode: exit %d\n%s", code, out)
	}
	f.mustGraft("commit", "-m", "feat: x")
	if out := f.mustGraft("version"); !strings.Contains(out, "no version tags yet (HEAD ") {
		t.Errorf("version without tags: %q", out)
	}

	f.mustGraft("release")
	if got := f.gitIn(f.remote("origin"), "tag", "--list"); got != "v0.1.0" {
		t.Errorf("remote tags after first release: %q", got)
	}
	if out := strings.TrimSpace(f.mustGraft("version")); out != "v0.1.0" {
		t.Errorf("version = %q", out)
	}

	f.write("main.go", "package main // 2\n")
	f.mustGraft("commit", "-m", "feat: y")
	f.mustGraft("release", "--minor")
	f.write("main.go", "package main // 3\n")
	f.mustGraft("commit", "-m", "fix: z")
	f.mustGraft("release")
	if got := strings.Fields(f.gitIn(f.remote("origin"), "tag", "--list")); !slices.Equal(got, []string{"v0.1.0", "v0.2.0", "v0.2.1"}) {
		t.Errorf("remote tags: %v", got)
	}
	if out, code := f.graft("", "release", "--version", "0.2.0"); code == 0 {
		t.Errorf("release below the latest tag accepted:\n%s", out)
	}
	f.mustGraft("release", "--version", "1.0.0")
	if out := strings.TrimSpace(f.mustGraft("version")); out != "v1.0.0" {
		t.Errorf("version = %q", out)
	}
}

func TestReleaseNeedsPushedHead(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion:\n  mode: git-tag\ngate: []\n",
		"main.go":     "package main\n",
	}, "origin")
	f.mustGraft("commit", "-m", "feat: x")
	f.write("main.go", "package main // local\n")
	f.git("add", "-A")
	f.run(f.work, "git", "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "local only")
	out, code := f.graft("", "release")
	if code == 0 || !strings.Contains(out, "is not on origin yet") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

func TestFileModeReleaseAndTagOnCommit(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion:\n  mode: file\n  tag_on_commit: true\ngate: []\n",
		"VERSION":     "0.9.0\n",
	}, "origin")
	f.mustGraft("commit", "-m", "feat: x")
	if got := f.gitIn(f.remote("origin"), "tag", "--list"); got != "v0.9.1" {
		t.Errorf("tag_on_commit: remote tags %q", got)
	}
	if out, code := f.graft("", "release"); code == 0 || !strings.Contains(out, "already exists") {
		t.Errorf("release of a tagged version: exit %d\n%s", code, out)
	}
	if out, code := f.graft("", "release", "--minor"); code == 0 {
		t.Errorf("--minor in file mode accepted:\n%s", out)
	}
}

func TestInitIsIdempotentAndRewritesAbsoluteHooksPath(t *testing.T) {
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion:\n  mode: none\n"})
	if err := os.MkdirAll(filepath.Join(f.work, ".githooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.git("config", "core.hooksPath", filepath.Join(f.work, ".githooks"))
	out := f.mustGraft("init")
	if !strings.Contains(out, "rewrote core.hooksPath") {
		t.Errorf("absolute hooksPath not reported:\n%s", out)
	}
	if got := f.git("config", "core.hooksPath"); got != ".githooks" {
		t.Errorf("core.hooksPath = %q", got)
	}
	if out := f.mustGraft("init"); !strings.Contains(out, "nothing changed") {
		t.Errorf("second init:\n%s", out)
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(filepath.Join(f.work, ".githooks", "pre-commit"), 0o644); err != nil {
			t.Fatal(err)
		}
		if out := f.mustGraft("init"); !strings.Contains(out, "made executable") {
			t.Errorf("lost exec bit not restored:\n%s", out)
		}
	}
}

func TestInitRefusesForeignHooks(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml":          "schema: 1\nversion:\n  mode: none\n",
		".githooks/pre-commit": "#!/bin/sh\necho custom\n",
	})
	out, code := f.graft("", "init")
	if code == 0 || !strings.Contains(out, ".githooks/pre-commit") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if got := f.read(".githooks/pre-commit"); got != "#!/bin/sh\necho custom\n" {
		t.Errorf("foreign hook overwritten: %q", got)
	}

	g := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion:\n  mode: none\n"})
	g.git("config", "core.hooksPath", ".husky")
	if out, code := g.graft("", "init"); code == 0 || !strings.Contains(out, ".husky") {
		t.Errorf("foreign hooksPath: exit %d\n%s", code, out)
	}
}

func TestShimFailsClosedWithoutGraft(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH manipulation for sh differs on Windows")
	}
	f := hookRepo(t, gateStep("ok"))
	f.write("shared.txt", "changed\n")
	f.git("add", "-A")
	cmd := exec.Command("git", "commit", "-m", "raw")
	cmd.Dir = f.work
	var env []string
	for _, kv := range testEnv {
		if !strings.HasPrefix(kv, "PATH=") {
			env = append(env, kv)
		}
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(env, "PATH="+filepath.Dir(gitPath)+":/bin:/usr/bin")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("commit went through without graft on PATH:\n%s", out)
	}
	if !strings.Contains(string(out), "go install github.com/aiseeq/graft@latest") {
		t.Errorf("no install hint:\n%s", out)
	}
}

func TestCheckAndGateCommands(t *testing.T) {
	f := fileModeRepo(t, gateStep("ok"))
	f.git("add", "-A")
	f.mustGraft("check")
	f.mustGraft("gate")
	f.write("web/package.json", "{\n  \"version\": \"9.9.9\"\n}\n")
	f.git("add", "-A")
	out, code := f.graft("", "check")
	if code == 0 || !strings.Contains(out, `web/package.json: version "9.9.9", VERSION says "1.2.3"`) {
		t.Errorf("version drift: exit %d\n%s", code, out)
	}
}

func TestMissingConfig(t *testing.T) {
	f := newRepo(t, map[string]string{"a.txt": "a\n"})
	out, code := f.graft("", "commit", "-m", "feat: x")
	if code == 0 || !strings.Contains(out, ".graft.yaml not found") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}
