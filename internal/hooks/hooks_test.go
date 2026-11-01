package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestShimInstallsTheLatest: the install hint of a shim installs the latest
// from a checkout: graft versions are not pinned, a project's minimum
// included.
func TestShimInstallsTheLatest(t *testing.T) {
	for _, name := range Names {
		if shim := string(Shim(name)); !strings.Contains(shim, "git pull && go run . install") {
			t.Errorf("the %s shim:\n%s", name, shim)
		}
	}
}

func TestInstallGlobalIsIdempotentAndKeepsEvents(t *testing.T) {
	config := filepath.Join(t.TempDir(), "gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	first, err := InstallGlobal()
	if err != nil || len(first) != 2 {
		t.Fatalf("first install: changes=%v err=%v", first, err)
	}
	second, err := InstallGlobal()
	if err != nil || len(second) != 0 {
		t.Fatalf("second install: changes=%v err=%v", second, err)
	}
	if err := exec.Command("git", "config", "--global", "--add", "hook.graft-leaks.event", "pre-commit").Run(); err != nil {
		t.Fatal(err)
	}
	third, err := InstallGlobal()
	if err != nil || len(third) != 0 {
		t.Fatalf("install with another event: changes=%v err=%v", third, err)
	}
	if err := exec.Command("git", "config", "--global", "hook.graft-leaks.command", "other hook").Run(); err != nil {
		t.Fatal(err)
	}
	fourth, err := InstallGlobal()
	if err != nil || len(fourth) != 1 || !strings.Contains(fourth[0].What, `from "other hook" to "graft hook pre-push"`) {
		t.Fatalf("command replacement: changes=%v err=%v", fourth, err)
	}
	out, err := exec.Command("git", "config", "--global", "--get-all", "hook.graft-leaks.event").Output()
	if err != nil || string(out) != "pre-push\npre-commit\n" {
		t.Fatalf("events = %q, err=%v", out, err)
	}
	repo := t.TempDir()
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}
	list := exec.Command("git", "hook", "list", "pre-push")
	list.Dir = repo
	out, err = list.Output()
	if err != nil || !strings.Contains(string(out), "graft-leaks") {
		t.Fatalf("git hook list pre-push = %q, err=%v", out, err)
	}
	if _, err := os.Stat(config); err != nil {
		t.Fatal(err)
	}
}
