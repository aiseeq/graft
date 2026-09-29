package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestVersionDescribeAndTaskVersion(t *testing.T) {
	f := tasksRepo(t, "", map[string]string{"a.txt": "a\n"})
	out := f.path("out.txt")
	f.write(".graft.yaml", f.read(".graft.yaml")+`tasks:
  stamp:
    run:
      - `+helperStep("env", out, "GRAFT_VERSION")+`
      - `+helperStep("append", out, "argv=${GRAFT_VERSION}")+`
  env-only:
    run: [`+helperStep("env", out, "GRAFT_VERSION")+`]
`)
	// No commits yet: there is no version, and a step asking for it fails.
	f.mustGraft("env-only")
	if got := f.read("out.txt"); got != "GRAFT_VERSION unset\n" {
		t.Errorf("before the first commit: %q", got)
	}
	if res, code := f.graft("", "stamp"); code != 1 || !strings.Contains(res, "GRAFT_VERSION is not set") {
		t.Errorf("${GRAFT_VERSION} without commits: exit %d\n%s", code, res)
	}
	if res, code := f.graft("", "version", "--describe"); code != 1 || !strings.Contains(res, "no commits yet") {
		t.Errorf("describe without commits: exit %d\n%s", code, res)
	}

	f.git("add", "-A")
	f.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "init")
	short := f.git("rev-parse", "--short=7", "HEAD")
	if got := strings.TrimSpace(f.mustGraft("version", "--describe")); got != "v0.0.0-1-g"+short {
		t.Errorf("untagged describe = %q", got)
	}
	f.write("a.txt", "changed\n")
	want := "v0.0.0-1-g" + short + "-dirty"
	if got := strings.TrimSpace(f.mustGraft("version", "--describe")); got != want {
		t.Errorf("dirty describe = %q, want %q", got, want)
	}
	os.Remove(out)
	f.mustGraft("stamp")
	if got := strings.Join(f.lines("out.txt"), ","); got != "GRAFT_VERSION="+want+",argv="+want {
		t.Errorf("task saw %s", got)
	}
	f.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-am", "second")
	f.git("tag", "-a", "v0.3.0", "-m", "v0.3.0")
	if got := strings.TrimSpace(f.mustGraft("version", "--describe")); got != "v0.3.0" {
		t.Errorf("tagged describe = %q", got)
	}
}

func TestVersionDescribeFileMode(t *testing.T) {
	f := fileModeRepo(t, gateStep("ok"))
	f.git("add", "-A")
	f.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "init")
	if got := strings.TrimSpace(f.mustGraft("version", "--describe")); got != "1.2.3" {
		t.Errorf("describe = %q", got)
	}
	f.write("main.go", "package main\n\nfunc main() {}\n")
	if got := strings.TrimSpace(f.mustGraft("version", "--describe")); got != "1.2.3-dirty" {
		t.Errorf("dirty describe = %q", got)
	}
}

func TestMinimumGraftVersion(t *testing.T) {
	f := tasksRepo(t, "graft: '>=1.2.3'\n", nil)
	f.mustGraft("init")
	if shim := f.read(".githooks/pre-commit"); !strings.Contains(shim, "go install github.com/aiseeq/graft@v1.2.3") {
		t.Errorf("shim hint:\n%s", shim)
	}
	f.write(".graft.yaml", strings.Replace(f.read(".graft.yaml"), ">=1.2.3", ">=1.3.0", 1))
	out, code := f.graft("", "version", "--describe")
	if code != 1 || !strings.Contains(out, "this project needs graft >= 1.3.0, this is graft "+testGraftVersion) ||
		!strings.Contains(out, "graft@v1.3.0") {
		t.Errorf("older graft: exit %d\n%s", code, out)
	}
	f.write(".graft.yaml", strings.Replace(f.read(".graft.yaml"), ">=1.3.0", "1.3", 1))
	if out, code := f.graft("", "gate"); code != 1 || !strings.Contains(out, `is not a minimum version`) {
		t.Errorf("bad constraint: exit %d\n%s", code, out)
	}
}

func TestInitRewritesShimsOfOtherGraftVersions(t *testing.T) {
	f := tasksRepo(t, "", nil)
	old := "#!/bin/sh\n# Installed by graft init. The hook logic lives in graft itself.\nif ! command -v graft >/dev/null 2>&1; then\n\techo \"graft: not found in PATH, the pre-commit hook cannot run.\" >&2\n\techo \"Install it: go install github.com/aiseeq/graft@latest (or make install in a graft checkout)\" >&2\n\texit 1\nfi\nexec graft hook pre-commit \"$@\"\n"
	f.write(".githooks/pre-commit", old)
	out := f.mustGraft("init")
	if !strings.Contains(out, "rewrote .githooks/pre-commit") || strings.Contains(f.read(".githooks/pre-commit"), "make install") {
		t.Errorf("init:\n%s\n%s", out, f.read(".githooks/pre-commit"))
	}
}

func TestHelpAndHookFollowTheConfig(t *testing.T) {
	f := tasksRepo(t, "", nil)
	f.write(".graft.yaml", f.read(".graft.yaml")+`tasks:
  build:
    desc: build it
    run: [`+helperStep("ok")+`]
  helper:
    run: [`+helperStep("ok")+`]
`)
	help := f.mustGraft("help")
	for _, absent := range []string{"graft commit [--minor", "bump the version", "work item key", "graft flags", "graft deploy", "graft start", "testdb", "  helper"} {
		if strings.Contains(help, absent) {
			t.Errorf("help of a git-tag project without those sections mentions %q:\n%s", absent, help)
		}
	}
	for _, want := range []string{"graft commit (-m", "graft tasks", "build  build it"} {
		if !strings.Contains(help, want) {
			t.Errorf("help lacks %q:\n%s", want, help)
		}
	}
	if tasks := f.mustGraft("tasks"); !strings.Contains(tasks, "helper") {
		t.Errorf("tasks does not list building blocks:\n%s", tasks)
	}

	f.mustGraft("init")
	f.git("add", "-A")
	out, code := f.tryGit("commit", "-m", "plain")
	if code == 0 || strings.Contains(out, "--minor") || strings.Contains(out, "bumps") || !strings.Contains(out, "graft commit: gate, stage all, check, commit, push to every remote") {
		t.Errorf("hook refusal in git-tag mode: exit %d\n%s", code, out)
	}
	g := fileModeRepo(t, gateStep("ok"))
	g.mustGraft("init")
	g.git("add", "-A")
	out, _ = g.tryGit("commit", "-m", "plain")
	if !strings.Contains(out, "--minor") || !strings.Contains(out, "bump the version") || !strings.Contains(out, "work item key") {
		t.Errorf("hook refusal in file mode:\n%s", out)
	}
}

func TestUsageErrorsAreOneLine(t *testing.T) {
	f := tasksRepo(t, "", nil)
	for _, args := range [][]string{{"nosuch"}, {"commit", "--bogus"}, {"testdb", "sideways"}} {
		out, code := f.graft("", args...)
		if code != 2 || strings.Count(strings.TrimSpace(out), "\n") != 0 || !strings.HasSuffix(strings.TrimSpace(out), "(see graft help)") {
			t.Errorf("graft %s: exit %d\n%s", strings.Join(args, " "), code, out)
		}
	}
	out, code := f.graft("", "commit", "-h")
	if code != 0 || !strings.Contains(out, "-minor") {
		t.Errorf("commit -h: exit %d\n%s", code, out)
	}
	if out := f.mustGraft("tasks"); !regexp.MustCompile(`no tasks`).MatchString(out) {
		t.Errorf("tasks without tasks:\n%s", out)
	}
	_ = filepath.Join
}
