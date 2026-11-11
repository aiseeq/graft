package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/deploystate"
)

func TestCommitWarnsForStaleInstalledGoBinary(t *testing.T) {
	f := tasksRepo(t, `gate: []
tasks:
  build:
    run: [[go, build, -o, bin/tool, .]]
  install:
    run: [{install: bin/tool}]
`, map[string]string{
		".gitignore": "bin/\n",
		"go.mod":     "module example.org/tool\n\ngo 1.22\n",
		"main.go":    "package main\n\nfunc main() { println(\"one\") }\n",
	})
	f.mustPlainCommit("install fixture")
	userBin, cfgHome := filepath.Join(f.base, "user-bin"), filepath.Join(f.base, "user-config")
	writeUserConfig(t, cfgHome, "tools:\n  bin_dir: "+yamlQuote(userBin)+"\n")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgHome, "APPDATA="+cfgHome)
	f.mustGraft("build")
	f.mustGraft("install")
	f.write("main.go", "package main\n\nfunc main() { println(\"two\") }\n")
	out, code := f.graft("", "commit", "-m", "feat: update tool")
	if code != 0 {
		t.Fatalf("graft commit failed: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "tool is from ") || !strings.Contains(out, "1 commit behind") || !strings.Contains(out, "run graft install") {
		t.Fatalf("commit did not warn about the stale install: exit %d\n%s", code, out)
	}

	f.mustGraft("build")
	f.mustGraft("install")
	out, code = f.graft("", "release")
	if code != 0 || strings.Contains(out, "warning: tool is from") {
		t.Fatalf("fresh tagged binary was reported stale: exit %d\n%s", code, out)
	}

	f.write("main.go", "package main\n\nfunc main() { println(\"three\") }\n")
	out, code = f.graft("", "commit", "-m", "feat: another tool update")
	if code != 0 || strings.Contains(out, "warning: tool is from") {
		t.Fatalf("commit between tags should not report staleness: exit %d\n%s", code, out)
	}
	out, code = f.graft("", "release")
	if code != 0 || !strings.Contains(out, "tool is from ") || !strings.Contains(out, "run graft install") {
		t.Fatalf("new tag did not report the older install: exit %d\n%s", code, out)
	}
}

func TestCommitSkipsInstalledGoBinaryWithoutVCSRevision(t *testing.T) {
	f := tasksRepo(t, `gate: []
tasks:
  build:
    run: [[go, build, -buildvcs=false, -o, bin/tool, .]]
  install:
    run: [{install: bin/tool}]
`, map[string]string{
		".gitignore": "bin/\n",
		"go.mod":     "module example.org/tool\n\ngo 1.22\n",
		"main.go":    "package main\n\nfunc main() {}\n",
	})
	f.mustPlainCommit("install fixture")
	userBin, cfgHome := filepath.Join(f.base, "user-bin"), filepath.Join(f.base, "user-config")
	writeUserConfig(t, cfgHome, "tools:\n  bin_dir: "+yamlQuote(userBin)+"\n")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgHome, "APPDATA="+cfgHome)
	f.mustGraft("build")
	f.mustGraft("install")
	f.write("main.go", "package main\n\nfunc main() { println(1) }\n")
	out, code := f.graft("", "commit", "-m", "feat: update tool")
	if code != 0 || strings.Contains(out, "warning: tool is from") {
		t.Fatalf("binary without vcs.revision should be skipped: exit %d\n%s", code, out)
	}
}

// installRepo is a committed Go module whose task build writes bin/tool;
// the install tasks put it into dest. Its lock timeout is long enough for an
// install waiting for a deploy under load.
func installRepo(t *testing.T, dest string) *fixture {
	t.Helper()
	return installRepoWaiting(t, dest, "1m")
}

// installRepoWaiting is installRepo with the lock timeout given, which
// bounds how long its install waits for a deploy.
func installRepoWaiting(t *testing.T, dest, timeout string) *fixture {
	t.Helper()
	f := tasksRepo(t, `lock:
  timeout: `+timeout+`
tasks:
  build:
    run:
      - [go, build, -o, bin/tool, .]
  install:
    deps: [build]
    run:
      - {install: bin/tool, to: `+yamlQuote(dest)+`}
  install-only:
    run:
      - {install: bin/tool, to: `+yamlQuote(dest)+`}
  install-script:
    run:
      - {install: scripts/*.sh, to: `+yamlQuote(dest)+`}
  install-none:
    run:
      - {install: bin/absent*, to: `+yamlQuote(dest)+`}
  install-default:
    deps: [build]
    run:
      - install: bin/tool
`, map[string]string{
		".gitignore":     "bin/\n",
		"go.mod":         "module example.com/tool\n\ngo 1.22\n",
		"main.go":        "package main\n\nfunc main() { println(\"one\") }\n",
		"scripts/run.sh": "#!/bin/sh\necho run\n",
	})
	f.git("add", "-A")
	f.git("commit", "-q", "-m", "tool")
	return f
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func deployInstallFixture(t *testing.T, timeout string) (*fixture, string, string) {
	t.Helper()
	cache := shortCache(t)
	t.Setenv("XDG_CACHE_HOME", cache)
	dest := filepath.Join(t.TempDir(), "bin")
	f := installRepoWaiting(t, dest, timeout)
	f.env = append(f.env, "XDG_CACHE_HOME="+cache)
	f.mustGraft("build")
	return f, filepath.Join(dest, "tool"), cache
}

// A held deployment keeps install from changing the binary until it ends.
func TestInstallWaitsForDeploy(t *testing.T) {
	skipWaitInShort(t)
	f, installed, _ := deployInstallFixture(t, "1m")
	release, err := deploystate.Register(f.work, "test")
	if err != nil {
		t.Fatal(err)
	}
	cmd, out := f.start(f.work, "install-only")
	waitFor(t, func() bool { return strings.Contains(out.String(), "waiting for deploy") })
	if _, err := os.Stat(installed); !os.IsNotExist(err) {
		t.Fatalf("install wrote before deploy ended: %v", err)
	}
	if !strings.Contains(out.String(), "project "+f.work+" target test") {
		t.Fatalf("wait output does not identify deployment:\n%s", out.String())
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("install: %v\n%s", err, out.String())
	}
	if !bytes.Equal(readBytes(t, installed), readBytes(t, f.path("bin/tool"))) {
		t.Fatal("install did not write the built binary")
	}
}

func TestInstallTimesOutForDeploy(t *testing.T) {
	skipWaitInShort(t)
	f, installed, _ := deployInstallFixture(t, "400ms")
	before := []byte("old binary")
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, before, 0o755); err != nil {
		t.Fatal(err)
	}
	release, err := deploystate.Register(f.work, "release")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	out, code := f.graft("", "install-only")
	if code == 0 || !strings.Contains(out, "project "+f.work+" target release") || !strings.Contains(out, "timed out") {
		t.Fatalf("install did not refuse with deployment details: exit %d\n%s", code, out)
	}
	if !bytes.Equal(readBytes(t, installed), before) {
		t.Fatal("timed out install changed the binary")
	}
}

func TestInstallInsideDeploySkipsItsOwnMarker(t *testing.T) {
	skipWaitInShort(t)
	f, installed, _ := deployInstallFixture(t, "1m")
	parentProject := filepath.Join(t.TempDir(), "projectA")
	parentRelease, err := deploystate.Register(parentProject, "parent")
	if err != nil {
		t.Fatal(err)
	}
	defer parentRelease()
	identity := deploystate.ChildIdentity(deploystate.Identity(parentProject, "parent"), f.work, "test")
	release, err := deploystate.Register(f.work, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	f.env = append(f.env, deploystate.Env+"="+identity)
	out := f.mustGraft("install-only")
	if !bytes.Equal(readBytes(t, installed), readBytes(t, f.path("bin/tool"))) {
		t.Fatal("install inside deploy did not write the binary")
	}
	if strings.Contains(out, "waiting for deploy") {
		t.Fatalf("install waited for its own deployment:\n%s", out)
	}
}

// A running deploy publishes its machine-wide marker before running its script.
func TestInstallWaitsForRealDeploy(t *testing.T) {
	skipWaitInShort(t)
	cache := shortCache(t)
	t.Setenv("XDG_CACHE_HOME", cache)
	releaseFile := filepath.Join(t.TempDir(), "release")
	deployInstall := filepath.Join(t.TempDir(), "deploy-bin")
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := deployConfig(state, q)
		cfg = strings.Replace(cfg, "    test:\n      env: local\n      run:", "    test:\n      env: local\n      deps: [install-step]\n      run:", 1)
		// A short lock timeout: a deploy waiting for its own marker fails in
		// seconds instead of holding the test for the default 15 minutes.
		return cfg + "\nlock:\n  timeout: 20s\ntasks:\n  install-step:\n    run:\n      - {install: deploy.sh, to: " + yamlQuote(deployInstall) + "}\n"
	})
	d.env = append(d.env, "XDG_CACHE_HOME="+cache, "GRAFT_TEST_DEPLOY_RELEASE="+releaseFile)
	deploy := startGraft(t, d.fixture, "deploy", "test")
	letGo := releaseDeployScript(t, releaseFile)
	waitForFile(t, filepath.Join(d.state, "deploy-ready"))
	if _, err := os.Stat(filepath.Join(deployInstall, "deploy.sh")); err != nil {
		t.Fatalf("deploy's own install step did not finish before its script: %v\n%s", err, deploy.out.String())
	}

	dest := filepath.Join(t.TempDir(), "bin")
	f := installRepo(t, dest)
	f.env = append(f.env, "XDG_CACHE_HOME="+cache)
	f.mustGraft("build")
	installed := filepath.Join(dest, "tool")
	install := startGraft(t, f, "install-only")
	waitFor(t, func() bool { return strings.Contains(install.out.String(), "waiting for deploy") })
	if !strings.Contains(install.out.String(), "project "+d.work+" target test") {
		t.Fatalf("install did not name the active deploy:\n%s", install.out.String())
	}
	if _, err := os.Stat(installed); !os.IsNotExist(err) {
		t.Fatalf("install wrote while deploy was running: %v", err)
	}
	letGo()
	deploy.end(t)
	install.end(t)
	if !bytes.Equal(readBytes(t, installed), readBytes(t, f.path("bin/tool"))) {
		t.Fatal("install did not write the built binary after deploy")
	}
}

func TestInstallIgnoresKilledDeployProcess(t *testing.T) {
	skipWaitInShort(t)
	cache := shortCache(t)
	t.Setenv("XDG_CACHE_HOME", cache)
	releaseFile := filepath.Join(t.TempDir(), "release")
	d := newDeployFixture(t, deployConfig)
	d.env = append(d.env, "XDG_CACHE_HOME="+cache, "GRAFT_TEST_DEPLOY_RELEASE="+releaseFile)
	deploy := startGraft(t, d.fixture, "deploy", "test")
	letGo := releaseDeployScript(t, releaseFile)
	waitForFile(t, filepath.Join(d.state, "deploy-ready"))
	if err := deploy.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	// The deploy script outlives graft killed under it and holds its output
	// open: install runs while it lives, and only then is it let go and
	// graft waited for.
	f := installRepo(t, filepath.Join(t.TempDir(), "bin"))
	f.env = append(f.env, "XDG_CACHE_HOME="+cache)
	f.mustGraft("build")
	out := f.mustGraft("install-only")
	letGo()
	deploy.kill()
	if strings.Contains(out, "waiting for deploy") {
		t.Fatalf("the marker of a killed deploy still blocks install:\n%s\ndeploy:\n%s", out, deploy.out.String())
	}
}

// releaseDeployScript returns what lets the stand-in deploy script, waiting
// for releaseFile, go on; it also lets it go when the test ends, so a test
// that fails first leaves no script waiting.
func releaseDeployScript(t *testing.T, releaseFile string) func() {
	t.Helper()
	letGo := func() {
		if err := os.WriteFile(releaseFile, nil, 0o600); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(letGo)
	return letGo
}

// The install step puts a binary built from HEAD into its directory
// atomically and says which revision; it leaves an identical one alone and
// refuses a build from uncommitted changes or from another revision.
func TestInstallStep(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "bin")
	f := installRepo(t, dest)
	installed := filepath.Join(dest, "tool")
	head := f.git("rev-parse", "--short=7", "HEAD")

	out := f.mustGraft("install")
	if !strings.Contains(out, "graft: install: tool "+head+" -> "+installed) {
		t.Errorf("install output lacks the revision and the path:\n%s", out)
	}
	if !bytes.Equal(readBytes(t, installed), readBytes(t, f.path("bin/tool"))) {
		t.Errorf("%s is not bin/tool", installed)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(installed); err != nil || fi.Mode().Perm() != 0o755 {
			t.Errorf("installed mode: %v %v", fi, err)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dest, ".*")); len(left) > 0 {
		t.Errorf("temporary files left in %s: %q", dest, left)
	}

	// The same build again: nothing is written.
	if out := f.mustGraft("install"); !strings.Contains(out, "unchanged") {
		t.Errorf("second install does not say unchanged:\n%s", out)
	}

	// A new commit without a rebuild: bin/tool is of the old revision.
	before := readBytes(t, installed)
	f.write("main.go", "package main\n\nfunc main() { println(\"two\") }\n")
	f.git("commit", "-q", "-am", "two")
	newHead := f.git("rev-parse", "--short=7", "HEAD")
	out, code := f.graft("", "install-only")
	if code == 0 || !strings.Contains(out, head) || !strings.Contains(out, newHead) {
		t.Errorf("a stale build is installed or not named by both revisions: exit %d\n%s", code, out)
	}
	if !bytes.Equal(readBytes(t, installed), before) {
		t.Errorf("a refused install changed %s", installed)
	}

	// Uncommitted changes in the build: refused, the installed one stays.
	f.write("main.go", "package main\n\nfunc main() { println(\"three\") }\n")
	out, code = f.graft("", "install")
	if code == 0 || !strings.Contains(out, "uncommitted") {
		t.Errorf("a build from uncommitted changes is installed: exit %d\n%s", code, out)
	}
	if !bytes.Equal(readBytes(t, installed), before) {
		t.Errorf("a refused install changed %s", installed)
	}
	f.git("checkout", "--", "main.go")

	// A glob that matches nothing is an error, not an empty install.
	if out, code := f.graft("", "install-none"); code == 0 || !strings.Contains(out, "bin/absent*") {
		t.Errorf("an empty match passes: exit %d\n%s", code, out)
	}
}

// A file without Go build info has no revision of its own: it installs
// only from a clean work tree and is named by HEAD.
func TestInstallStepNonGoFile(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "bin")
	f := installRepo(t, dest)
	head := f.git("rev-parse", "--short=7", "HEAD")
	out := f.mustGraft("install-script")
	if !strings.Contains(out, "graft: install: run.sh "+head+" -> "+filepath.Join(dest, "run.sh")) {
		t.Errorf("script install output:\n%s", out)
	}
	f.write("notes.txt", "draft\n")
	out, code := f.graft("", "install-script")
	if code == 0 || !strings.Contains(out, "clean") {
		t.Errorf("a non-Go file installs from a dirty tree: exit %d\n%s", code, out)
	}
}

// Without to: the step installs where graft tools install puts binaries,
// tools.bin_dir of the user config.
func TestInstallStepDefaultDir(t *testing.T) {
	f := installRepo(t, filepath.Join(t.TempDir(), "unused"))
	userBin, cfgHome := t.TempDir(), t.TempDir()
	writeUserConfig(t, cfgHome, "tools:\n  bin_dir: "+userBin+"\n")
	f.env = []string{"XDG_CONFIG_HOME=" + cfgHome, "APPDATA=" + cfgHome}
	f.mustGraft("install-default")
	if _, err := os.Stat(filepath.Join(userBin, "tool")); err != nil {
		t.Errorf("not installed into tools.bin_dir: %v", err)
	}
}

// to: must say where on its own: a relative path is a config error.
func TestInstallStepRelativeTo(t *testing.T) {
	f := tasksRepo(t, `tasks:
  install:
    run:
      - {install: bin/tool, to: bin2}
`, nil)
	if out, code := f.graft("", "install"); code == 0 || !strings.Contains(out, "to:") {
		t.Errorf("a relative to: passes: exit %d\n%s", code, out)
	}
}

// A destination that is a symlink to the built file (an older install
// scheme) has the same bytes, but the next build would change it behind
// the step: it is replaced with a file, never called unchanged.
func TestInstallStepReplacesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dest := filepath.Join(t.TempDir(), "bin")
	f := installRepo(t, dest)
	f.mustGraft("build")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(dest, "tool")
	if err := os.Symlink(f.path("bin/tool"), installed); err != nil {
		t.Fatal(err)
	}
	out := f.mustGraft("install")
	if strings.Contains(out, "unchanged") {
		t.Errorf("a symlink to the build is called unchanged:\n%s", out)
	}
	fi, err := os.Lstat(installed)
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("%s is not a regular file: %v %v", installed, fi, err)
	}
	if !bytes.Equal(readBytes(t, installed), readBytes(t, f.path("bin/tool"))) {
		t.Errorf("%s is not bin/tool", installed)
	}
}

// TestInstallStepRefusesLinkedWorktreeGoBinary: go build stamps no revision
// in a linked worktree, so install from there refuses a Go binary and names
// the main tree; the main tree still installs, and so does a file that is not
// a Go binary from the linked worktree.
func TestInstallStepRefusesLinkedWorktreeGoBinary(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "bin")
	f := installRepo(t, dest)
	linked := filepath.Join(f.base, "linked")
	f.git("worktree", "add", "-q", "-b", "side", linked)

	out, code := f.exec(linked, "", graftBin, "install")
	if code == 0 {
		t.Fatalf("a Go binary without a revision installed from a linked worktree:\n%s", out)
	}
	main, err := filepath.EvalSymlinks(f.work)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "worktree") || !strings.Contains(out, main) {
		t.Errorf("the refusal must say linked worktree and name the main tree %s:\n%s", main, out)
	}
	if _, err := os.Stat(filepath.Join(dest, "tool")); !os.IsNotExist(err) {
		t.Errorf("a refused install wrote %s: %v", filepath.Join(dest, "tool"), err)
	}

	if out, code := f.exec(linked, "", graftBin, "install-script"); code != 0 {
		t.Errorf("a non-Go file from a linked worktree must install as before, exit %d:\n%s", code, out)
	}

	f.mustGraft("install")
	if _, err := os.Stat(filepath.Join(dest, "tool")); err != nil {
		t.Errorf("the main tree must still install: %v", err)
	}
}
