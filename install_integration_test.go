package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// installRepo is a committed Go module whose task build writes bin/tool;
// the install tasks put it into dest.
func installRepo(t *testing.T, dest string) *fixture {
	t.Helper()
	f := tasksRepo(t, `tasks:
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
