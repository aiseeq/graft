package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/version"
)

// The integration tests drive the built graft binary against throwaway
// repositories with bare remotes, the way a user's shell would.

// testGraftVersion is the version the graft under test reports.
const testGraftVersion = "v1.2.3-4-gabcdef0"

var (
	binDir       string
	graftBin     string
	helperBin    string
	testEnv      []string
	repoTemplate string
	bareTemplate string
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
		return 0, fmt.Errorf("a directory for the tests: %w", err)
	}
	defer os.RemoveAll(dir)
	binDir = filepath.Join(dir, "bin")
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	graftBin = filepath.Join(binDir, "graft"+exe)
	helperBin = filepath.Join(binDir, "gatehelper"+exe)
	// GRAFT_TEST_COVERDIR: the coverage of the graft the tests run, which
	// go test -cover does not see, goes there (go tool covdata percent -i).
	coverDir := os.Getenv("GRAFT_TEST_COVERDIR")
	for target, pkg := range map[string]string{graftBin: ".", helperBin: "./testdata/gatehelper"} {
		// A fixed version, so the graft: minimum version tests know it.
		args := []string{"build", "-ldflags", "-X main.buildVersion=" + testGraftVersion, "-o", target}
		if coverDir != "" && pkg == "." {
			args = append(args, "-cover", "-coverpkg=./...")
		}
		out, err := exec.Command("go", append(args, pkg)...).CombinedOutput()
		if err != nil {
			return 0, fmt.Errorf("go build %s: %v\n%s", pkg, err, out)
		}
	}
	// maintenance.auto off: git would start a detached maintenance run after
	// every commit and push, hundreds of processes the tests never look at.
	gitconfig := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(gitconfig, []byte("[user]\n\tname = Test\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n[protocol \"file\"]\n\tallow = always\n[maintenance]\n\tauto = false\n"), 0o644); err != nil {
		return 0, fmt.Errorf("the tests' git config: %w", err)
	}
	testEnv, err = isolatedEnv(gitconfig, filepath.Join(dir, "userconfig"), filepath.Join(dir, "cache"))
	if err != nil {
		return 0, err
	}
	if coverDir != "" {
		testEnv = append(testEnv, "GOCOVERDIR="+coverDir)
	}
	// Every gate and heavy task queues for the machine: the tests' graft
	// finds a calm one unless a test fakes another, and does not wait out the
	// load of the machine the tests run on.
	machine := filepath.Join(dir, "machine")
	for name, content := range map[string]string{
		"stat":            "cpu  1 1 1 1\ncpu0 1 1 1 1\nprocs_running 1\n",
		"pressure/io":     "some avg10=0.00 avg60=0.00 avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1\n",
		"pressure/memory": "some avg10=0.00 avg60=0.00 avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1\n",
		"vmstat":          "pswpin 0\npswpout 0\n",
	} {
		if err := writeTestFile(filepath.Join(machine, filepath.FromSlash(name)), content); err != nil {
			return 0, err
		}
	}
	testEnv = append(testEnv, "GRAFT_PROC="+machine)
	repoTemplate = filepath.Join(dir, "repo-template")
	bareTemplate = filepath.Join(dir, "bare-template.git")
	for _, args := range [][]string{
		{"init", "-q", "-b", "feature/PROJ-42-thing", repoTemplate},
		{"init", "-q", "--bare", bareTemplate},
	} {
		cmd := exec.Command("git", args...)
		cmd.Env = testEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			return 0, fmt.Errorf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	for _, path := range []string{
		filepath.Join(repoTemplate, ".git", "hooks"),
		filepath.Join(bareTemplate, "hooks"),
	} {
		if err := os.RemoveAll(path); err != nil {
			return 0, fmt.Errorf("removing unused template hooks: %w", err)
		}
		if err := os.Mkdir(path, 0o755); err != nil {
			return 0, fmt.Errorf("creating template hooks directory: %w", err)
		}
	}
	// Short settling: the tests' heavy tasks and gates, one at a time, do
	// not wait out a laptop's 30s for each other.
	if err := writeTestFile(filepath.Join(dir, "userconfig", "graft", "config.yaml"), "load: {settle: 10ms, poll: 20ms, swap_window: 10ms}\n"); err != nil {
		return 0, err
	}
	return m.Run(), nil
}

// writeTestFile writes a file of the tests' setup, its directory too.
func writeTestFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("the tests' %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("the tests' %s: %w", path, err)
	}
	return nil
}

// isolatedEnv keeps the tests away from the user's git config, from the
// user's cache (graft's stats), from the run of a graft the tests run under
// and from the variables git sets when these tests run inside a hook
// (graft's own gate).
func isolatedEnv(gitconfig, userConfig, cache string) ([]string, error) {
	out, err := exec.Command("git", "rev-parse", "--local-env-vars").Output()
	if err != nil {
		return nil, err
	}
	// Go's build cache lives in the user cache too; the tests that build
	// keep the warm one.
	gocache, err := exec.Command("go", "env", "GOCACHE").Output()
	if err != nil {
		return nil, err
	}
	drop := append(strings.Fields(string(out)), "GRAFT_COMMIT", "GRAFT_RUN_ID", "GRAFT_LOAD_PRIORITY", "GRAFT_LOAD_BUDGET", "GRAFT_LOAD_THEN", "GRAFT_LOADAVG", "GRAFT_PROC", "GRAFT_ADMITTED", "GRAFT_HEAVY", "GRAFT_THREADS", "GOMAXPROCS", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM", "PATH", "XDG_CONFIG_HOME", "APPDATA",
		"XDG_CACHE_HOME", "LOCALAPPDATA", "GOCACHE", "GOLANGCI_LINT_CACHE")
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
		// The user's own graft config (Jira, tools.bin_dir) stays out.
		"XDG_CONFIG_HOME="+userConfig,
		"APPDATA="+userConfig,
		"XDG_CACHE_HOME="+cache,
		"LOCALAPPDATA="+cache,
		"GOCACHE="+strings.TrimSpace(string(gocache)),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	), nil
}

type fixture struct {
	t    *testing.T
	base string
	work string
	// env is added to the environment of every process the fixture runs.
	env []string
}

// newRepo makes a work tree on branch feature/PROJ-42-thing with bare remotes
// named after remotes, and writes files into it (not committed).
func newRepo(t *testing.T, files map[string]string, remotes ...string) *fixture {
	t.Helper()
	f := &fixture{t: t, base: t.TempDir()}
	f.work = filepath.Join(f.base, "work")
	if err := os.CopyFS(f.work, os.DirFS(repoTemplate)); err != nil {
		t.Fatalf("copying the repository template: %v", err)
	}
	for _, r := range remotes {
		if err := os.CopyFS(filepath.Join(f.base, r+".git"), os.DirFS(bareTemplate)); err != nil {
			t.Fatalf("copying the bare repository template: %v", err)
		}
		f.addRemote(r, filepath.Join(f.base, r+".git"))
	}
	for name, content := range files {
		f.write(name, content)
	}
	return f
}

func yamlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func (f *fixture) addRemote(name, path string) {
	f.t.Helper()
	config := filepath.Join(f.work, ".git", "config")
	file, err := os.OpenFile(config, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	_, writeErr := fmt.Fprintf(file, "\n[remote %s]\n\turl = %s\n\tfetch = +refs/heads/*:refs/remotes/%s/*\n",
		strconv.Quote(name), strconv.Quote(filepath.ToSlash(path)), name)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		f.t.Fatalf("writing remote %s: %v", name, errors.Join(writeErr, closeErr))
	}
}

// skipGoToolchainInShort skips, under -short, a test whose seconds go to the
// go command fetching, tidying or installing modules: the gate's test-gate
// runs -short, test-race in all runs it before install and release.
func skipGoToolchainInShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("runs the go command against modules; test-race in all runs it")
	}
}

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

func TestChangeDirectoryFlag(t *testing.T) {
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none}\n"})
	if out, code := f.graft("", "-C", f.work, "status"); code != 0 {
		t.Fatalf("-C status: exit %d\n%s", code, out)
	}
	if out, code := f.graft("", "-C", f.base, "-C", "work", "status"); code != 0 {
		t.Fatalf("repeated -C status: exit %d\n%s", code, out)
	}
	// After the command -C is refused with the form that works, before
	// anything runs; after -- it belongs to the task.
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"commit", "-C", f.work, "-m", "feat: x"}, "graft: -C goes before the command: graft -C " + f.work + " commit -m 'feat: x' (see graft help)\n"},
		{[]string{"test", "-C", f.work, "--", "-run", "TestA|TestB", "./cmd/x/"}, "graft: -C goes before the command: graft -C " + f.work + " test -- -run 'TestA|TestB' ./cmd/x/ (see graft help)\n"},
	} {
		if out, code := f.graft("", c.args...); code != 2 || out != c.want {
			t.Errorf("graft %q: exit %d\n%s", c.args, code, out)
		}
	}
	if out, _ := f.graft("", "test", "--", "-C", f.work); strings.Contains(out, "-C goes before") {
		t.Errorf("-C after -- is the task's:\n%s", out)
	}
	missing := filepath.Join(f.base, "missing-dir")
	if out, code := f.graft("", "-C", missing, "status"); code != 1 || !strings.Contains(out, missing) {
		t.Errorf("missing -C directory: exit %d\n%s", code, out)
	}
}

func (f *fixture) exec(dir, stdin, name string, args ...string) (string, int) {
	f.t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(slices.Clone(testEnv), f.env...)
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

// start runs graft in dir in the background; its output gathers in the
// buffer while it runs.
func (f *fixture) start(dir string, args ...string) (*exec.Cmd, *syncBuffer) {
	f.t.Helper()
	cmd := exec.Command(graftBin, args...)
	cmd.Dir = dir
	cmd.Env = append(slices.Clone(testEnv), f.env...)
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		f.t.Fatal(err)
	}
	return cmd, out
}

func (f *fixture) mustGraft(args ...string) string {
	f.t.Helper()
	out, code := f.graft("", args...)
	if code != 0 {
		f.t.Fatalf("graft %s: exit %d\n%s", strings.Join(args, " "), code, out)
	}
	return out
}

// mustVersionedCommit commits the work tree of dir with plain git as graft
// commit does in version mode file, without its checks: VERSION's build part
// raised when build, its patch otherwise, then pushed to origin. A build
// version off a build branch fails the test, as graft commit refuses it.
func (f *fixture) mustVersionedCommit(dir, msg string, build bool) {
	f.t.Helper()
	path := filepath.Join(dir, "VERSION")
	v, err := version.ParseFile(strings.TrimSpace(readFile(f.t, path)))
	if err != nil {
		f.t.Fatal(err)
	}
	if !build && v.Build != 0 {
		f.t.Fatalf("a build version %s off a build branch", v)
	}
	if build {
		v = v.BumpBuild()
	} else {
		v = v.Bump(version.Patch)
	}
	writeFile(f.t, path, v.String()+"\n")
	for _, args := range [][]string{{"add", "-A"}, {"-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", msg}, {"push", "-q", "-u", "origin", "HEAD"}} {
		if out, code := f.exec(dir, "", "git", args...); code != 0 {
			f.t.Fatalf("git %s: exit %d\n%s", args[0], code, out)
		}
	}
}

// mustPlainCommit makes setup history when the test does not check graft commit behavior.
func (f *fixture) mustPlainCommit(message string) {
	f.t.Helper()
	f.git("add", "-A")
	f.run(f.work, "git", "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", message)
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
	f.write("VERSION", "1.2.4\n")
	f.write("web/package.json", "{\n  \"name\": \"web\",\n  \"version\": \"1.2.4\"\n}\n")
	f.write("app.yaml", "name: app\nversion: \"1.2.4\"\n")
	f.mustPlainCommit("feat: x")
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

// A symlink to an absolute path, made by mistake inside the tree, is
// refused; a relative one is a link the repository carries.
func TestAbsoluteSymlinkBlocked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege on Windows")
	}
	f := fileModeRepo(t, gateStep("ok"))
	lib := t.TempDir()
	if err := os.Symlink(lib, f.path("lib")); err != nil {
		t.Fatal(err)
	}
	out, code := f.graft("", "commit", "-m", "feat: x")
	if code == 0 || !strings.Contains(out, "lib: symlink to the absolute path "+lib+", which leads nowhere in another clone") {
		t.Errorf("absolute: exit %d:\n%s", code, out)
	}
	if err := os.Remove(f.path("lib")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("docs", f.path("lib")); err != nil {
		t.Fatal(err)
	}
	f.mustGraft("commit", "-m", "feat: x")
}

func TestConcurrentCommitsTakeTurns(t *testing.T) {
	state := t.TempDir()
	logFile, release := filepath.Join(state, "gate.log"), filepath.Join(state, "release")
	f := fileModeRepo(t, gateStep("await", logFile, release), "origin")
	f.mustPlainCommit("feat: base")
	// A second worktree shares the repository and therefore the lock, but has
	// its own changes, so both commits have something to do.
	other := filepath.Join(f.base, "other")
	f.git("worktree", "add", "-q", "-b", "feature/PROJ-43-other", other)
	f.write("a.txt", "a\n")
	writeFile(t, filepath.Join(other, "b.txt"), "b\n")

	// The first gate holds until one commit waits for the other's lock.
	type run struct {
		cmd *exec.Cmd
		out *syncBuffer
	}
	var runs []run
	for i, dir := range []string{f.work, other} {
		cmd, out := f.start(dir, "commit", "-m", fmt.Sprintf("feat: parallel %d", i))
		runs = append(runs, run{cmd, out})
	}
	waitFor(t, func() bool {
		return strings.Contains(runs[0].out.String()+runs[1].out.String(), "waiting for the write lock graft.lock")
	})
	writeFile(t, release, "")
	for i, r := range runs {
		if err := r.cmd.Wait(); err != nil {
			t.Fatalf("commit %d: %v\n%s", i, err, r.out.String())
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
}

func hookRepo(t *testing.T, gate string) *fixture {
	return newHookRepo(t, gate, false)
}

func hookRepoWithGraftedBase(t *testing.T, gate string) *fixture {
	return newHookRepo(t, gate, true)
}

func newHookRepo(t *testing.T, gate string, graftBase bool) *fixture {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion:\n  mode: none\ngate:\n" + gate,
		"shared.txt":  "base\n",
	})
	f.mustGraft("init")
	if graftBase {
		f.mustGraft("commit", "-m", "chore: base")
	} else {
		f.mustPlainCommit("chore: base")
	}
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

// TestHookLetsLocalBranchCommit: on a branch of the user's
// commit.local_branches a plain git commit passes after the checks, without
// the gate; a secret is still refused, and other branches still refuse it.
func TestHookLetsLocalBranchCommit(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "gate-ran")
	f := hookRepoWithGraftedBase(t, gateStep("touch", marker))
	cfgDir := t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "commit: {local_branches: ['nightly/*']}\n")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir)
	// The base commit ran the gate.
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	f.git("switch", "-q", "-c", "nightly/2026-01-02")
	f.write("shared.txt", "nightly\n")
	f.git("add", "-A")
	if out, code := f.tryGit("commit", "-q", "-m", "notes of the night"); code != 0 {
		t.Fatalf("a commit on a local branch: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("the gate ran on a local branch: %v", err)
	}
	f.write("config.go", "package main\n\nconst id = \"AKIA"+strings.Repeat("Q", 16)+"\"\n")
	f.git("add", "-A")
	if out, code := f.tryGit("commit", "-q", "-m", "a key"); code == 0 {
		t.Errorf("a secret went in on a local branch:\n%s", out)
	}
	f.git("reset", "-q", "--hard")

	f.git("switch", "-q", "-c", "nightlyish")
	f.write("shared.txt", "other\n")
	f.git("add", "-A")
	if out, code := f.tryGit("commit", "-q", "-m", "raw"); code == 0 {
		t.Errorf("a plain commit on another branch went through:\n%s", out)
	}
}

func TestHookGatesConflictResolution(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "gate-ran")
	f := hookRepoWithGraftedBase(t, gateStep("touch", marker))
	f.git("switch", "-q", "-c", "side")
	f.write("shared.txt", "side\n")
	f.mustPlainCommit("feat: side")
	f.git("switch", "-q", "feature/PROJ-42-thing")
	f.write("shared.txt", "main\n")
	f.mustPlainCommit("feat: main")
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

// TestCommitFinishesAMerge: graft commit finishes a merge git left open, a
// conflict resolved or a merge made with --no-commit, as any commit: the
// gate, the version bump and the push; a conflict not resolved yet is
// refused with its files.
func TestCommitFinishesAMerge(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "gate-ran")
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\ngate:\n" + gateStep("touch", marker),
		"VERSION":     "1.0.0\n",
		"shared.txt":  "base\n",
	}, "origin")
	f.git("checkout", "-q", "-b", "main")
	f.mustGraft("init")
	f.mustPlainCommit("chore: base")
	f.git("switch", "-q", "-c", "side")
	f.write("shared.txt", "side\n")
	f.write("side.txt", "s\n")
	f.write("VERSION", "1.0.2\n")
	f.mustPlainCommit("feat: side")
	f.git("switch", "-q", "main")
	f.write("shared.txt", "main\n")
	f.mustPlainCommit("feat: main")

	if _, code := f.tryGit("merge", "side"); code == 0 {
		t.Fatal("expected a conflict")
	}
	if out, code := f.graft("", "commit", "-m", "merge: side"); code == 0 || !strings.Contains(out, "the merge has unresolved conflicts in shared.txt") {
		t.Errorf("an unresolved conflict: exit %d\n%s", code, out)
	}
	f.write("shared.txt", "resolved\n")
	f.git("add", "shared.txt")
	out := f.mustGraft("commit", "-m", "merge: side")
	if parents := strings.Fields(f.git("log", "-1", "--format=%P")); len(parents) != 2 {
		t.Errorf("not a merge commit: %v\n%s", parents, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the gate did not run:\n%s", out)
	}
	if got := f.read("VERSION"); got != "1.0.3\n" {
		t.Errorf("VERSION = %q", got)
	}
	if !strings.Contains(out, "pushed to origin") || f.git("rev-parse", "origin/main") != f.git("rev-parse", "HEAD") {
		t.Errorf("not pushed:\n%s", out)
	}
	if got := f.git("log", "-1", "--format=%s"); got != "merge: side" {
		t.Errorf("subject %q", got)
	}

	// A merge git stopped before committing, with nothing to resolve.
	f.git("switch", "-q", "side")
	f.write("other.txt", "o\n")
	f.mustPlainCommit("feat: other")
	f.git("switch", "-q", "main")
	f.git("merge", "--no-commit", "--no-ff", "side")
	f.mustGraft("commit", "-m", "merge: side again")
	if parents := strings.Fields(f.git("log", "-1", "--format=%P")); len(parents) != 2 {
		t.Errorf("not a merge commit: %v", parents)
	}
}

func TestHookBlocksConflictResolutionWithFailingGate(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "broken")
	f := hookRepo(t, gateStep("failif", flag))
	f.git("switch", "-q", "-c", "side")
	f.write("shared.txt", "side\n")
	f.mustPlainCommit("feat: side")
	f.git("switch", "-q", "feature/PROJ-42-thing")
	f.write("shared.txt", "main\n")
	f.mustPlainCommit("feat: main")
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
	f.mustPlainCommit("feat: side")
	f.git("switch", "-q", "feature/PROJ-42-thing")
	f.write("main.txt", "main\n")
	f.mustPlainCommit("feat: main")
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
	f.mustPlainCommit("feat: side")
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
	f.mustPlainCommit("feat: x")
	f.git("push", "-u", "origin", "HEAD")
	f.write("main.go", "package main // changed\n")
	out, code := f.graft("", "amend")
	if code == 0 || !strings.Contains(out, "already on origin") || !strings.Contains(out, "graft commit") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

func TestAmendFoldsIntoLocalCommit(t *testing.T) {
	f := fileModeRepo(t, gateStep("ok"))
	f.write("VERSION", "1.2.4\n")
	f.write("web/package.json", "{\n  \"name\": \"web\",\n  \"version\": \"1.2.4\"\n}\n")
	f.write("app.yaml", "name: app\nversion: \"1.2.4\"\n")
	f.mustPlainCommit("feat: x\n\nPROJ-42")
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
	f.mustPlainCommit("feat: y")
	f.git("push", "-q", "origin", "HEAD")
	f.mustGraft("release", "--minor")
	f.write("main.go", "package main // 3\n")
	f.mustPlainCommit("fix: z")
	f.git("push", "-q", "origin", "HEAD")
	f.mustGraft("release")
	if got := strings.Fields(f.gitIn(f.remote("origin"), "tag", "--list")); !slices.Equal(got, []string{"v0.1.0", "v0.2.0", "v0.2.1"}) {
		t.Errorf("remote tags: %v", got)
	}
	if out, code := f.graft("", "release", "--version", "0.2.0"); code == 0 {
		t.Errorf("release below the latest tag accepted:\n%s", out)
	}
	// A release of a commit already released is a mistake (a commit that
	// failed before it), unless the version is named.
	if out, code := f.graft("", "release", "--minor"); code == 0 || !strings.Contains(out, "HEAD is already released as v0.2.1") {
		t.Errorf("a second release of the same commit: exit %d\n%s", code, out)
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
	f.mustPlainCommit("feat: x")
	f.git("push", "-u", "origin", "HEAD")
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

func TestInitRefusesToDisableDefaultHooks(t *testing.T) {
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion:\n  mode: none\n"})
	hook := filepath.Join(f.work, ".git", "hooks", "pre-push")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := f.graft("", "init")
	if code == 0 || !strings.Contains(out, "pre-push") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if got, code := f.tryGit("config", "core.hooksPath"); code == 0 {
		t.Errorf("core.hooksPath set anyway: %q", got)
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
	if !strings.Contains(string(out), "git pull && go run . install") {
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

func TestCommitBumpsTwoKeysInOneFile(t *testing.T) {
	lock := "{\n  \"version\": \"1.2.3\",\n  \"packages\": {\n    \"\": {\n      \"version\": \"1.2.3\"\n    }\n  }\n}\n"
	f := newRepo(t, map[string]string{
		".graft.yaml": `schema: 1
version:
  mode: file
  sync:
    - {path: web/package-lock.json, format: json, key: [version]}
    - {path: web/package-lock.json, format: json, key: [packages, "", version]}
gate:
` + gateStep("ok"),
		"VERSION":               "1.2.3\n",
		"web/package-lock.json": lock,
	}, "origin")
	f.mustGraft("commit", "-m", "feat: first")
	if got, want := f.read("web/package-lock.json"), strings.ReplaceAll(lock, "1.2.3", "1.2.4"); got != want {
		t.Errorf("package-lock.json:\n%s", got)
	}
	f.mustGraft("check")
}

func TestCommitWarnsAboutOutdatedHooks(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\ngraft: '>=0.1.0'\nversion:\n  mode: none\ngate: []\n",
		"a.txt":       "a\n",
	}, "origin")
	out := f.mustGraft("commit", "-m", "feat: before init")
	if !strings.Contains(out, `core.hooksPath is "", not ".githooks"`) || !strings.Contains(out, "run graft init") {
		t.Errorf("no warning without hooks:\n%s", out)
	}
	f.mustGraft("init")
	f.write("a.txt", "b\n")
	if out := f.mustGraft("commit", "-m", "feat: after init"); strings.Contains(out, "out of date") {
		t.Errorf("warning after init:\n%s", out)
	}
	// A shim of an older pin still runs, but init would rewrite it.
	f.write(".githooks/pre-commit", strings.Replace(f.read(".githooks/pre-commit"), "git pull && ", "", 1))
	f.write("a.txt", "c\n")
	if out := f.mustGraft("commit", "-m", "feat: stale shim"); !strings.Contains(out, ".githooks/pre-commit differs from the shim of this graft") {
		t.Errorf("stale shim not reported:\n%s", out)
	}
}

// TestCommitConventional: with commit.conventional in the user config a
// subject that is not Conventional Commits is refused before the gate and
// nothing is committed; without it any subject goes.
func TestCommitConventional(t *testing.T) {
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none}\n"})
	f.mustGraft("commit", "-m", "first commit, any subject")
	cfgDir := t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "commit: {conventional: true}\n")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir)
	f.write("a.txt", "a\n")
	before := f.gitIn(f.work, "rev-parse", "HEAD")
	if out, code := f.graft("", "commit", "-m", "add a"); code == 0 || !strings.Contains(out, `the subject "add a" is not Conventional Commits (user config commit.conventional)`) {
		t.Errorf("a plain subject: exit %d\n%s", code, out)
	}
	if got := f.gitIn(f.work, "rev-parse", "HEAD"); got != before {
		t.Errorf("a refused message committed %s", got)
	}
	f.mustGraft("commit", "-m", "feat: add a")
}
