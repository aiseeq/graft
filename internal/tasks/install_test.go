package tasks

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gittest"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

func TestInstallFreshnessSkipsBinaryWithoutVCSRevision(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a synthetic Go binary")
	}
	root, dest := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"go.mod":         "module example.org/tool\ngo 1.22\n",
		"main.go":        "package main\nfunc main() {}\n",
		"scripts/run.sh": "#!/bin/sh\necho run\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) string { return gittest.Run(t, root, args...) }
	git("init", "-q", "-b", "main")
	git("add", "go.mod", "main.go")
	git("commit", "-q", "-m", "fixture")
	head := git("rev-parse", "HEAD")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", filepath.Join(root, "bin", "tool"), ".")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	for source, destination := range map[string]string{
		filepath.Join(root, "bin", "tool"):       filepath.Join(dest, "tool"),
		filepath.Join(root, "scripts", "run.sh"): filepath.Join(dest, "run.sh"),
	} {
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r := &Runner{Root: root, Config: &config.Config{Tasks: map[string]*config.Task{
		"install": {Run: []config.Step{{Install: &config.InstallStep{Path: "bin/tool scripts/run.sh", To: dest}}}},
	}}}
	if got := r.InstallFreshnessWarnings(context.Background(), head); len(got) != 0 {
		t.Fatalf("binary without vcs.revision produced warnings: %q", got)
	}
}

func TestHasInstallRevision(t *testing.T) {
	for _, tc := range []struct {
		revision string
		present  bool
		want     bool
	}{
		{revision: "abc1234", present: true, want: true},
		{revision: "", present: true, want: false},
		{revision: "", present: false, want: false},
	} {
		if got := hasInstallRevision(tc.revision, tc.present); got != tc.want {
			t.Errorf("hasInstallRevision(%q, %v) = %v, want %v", tc.revision, tc.present, got, tc.want)
		}
	}
}

func TestInstallRevisionDistance(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) string { return gittest.Run(t, root, args...) }
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "one")
	old := git("rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "two")
	middle := git("rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "three")
	head := git("rev-parse", "HEAD")
	repo, err := gitx.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if count, behind, err := installRevisionDistance(repo, old, head); err != nil || !behind || count != 2 {
		t.Errorf("old ancestor: count=%d behind=%v err=%v", count, behind, err)
	}
	if count, behind, err := installRevisionDistance(repo, head, head); err != nil || behind || count != 0 {
		t.Errorf("equal revision: count=%d behind=%v err=%v", count, behind, err)
	}
	git("checkout", "-q", "--detach", middle)
	git("commit", "-q", "--allow-empty", "-m", "side")
	side := git("rev-parse", "HEAD")
	if count, behind, err := installRevisionDistance(repo, side, head); err != nil || behind || count != 0 {
		t.Errorf("divergent revision: count=%d behind=%v err=%v", count, behind, err)
	}
	if count, behind, err := installRevisionDistance(repo, strings.Repeat("f", 40), head); !errors.Is(err, errInstallRevisionUnknown) || behind || count != 0 {
		t.Errorf("unknown revision: count=%d behind=%v err=%v", count, behind, err)
	}
}

// removeOld removes the programs a Windows install moved aside, by name
// prefix: a name with glob characters in it is matched literally, and the
// other files stay.
func TestRemoveOld(t *testing.T) {
	dir := t.TempDir()
	names := []string{".tool[1].old-1", ".tool[1].old-2", ".tool1.old-3", "tool[1]", ".tool[1].oldish"}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeOld(dir, ".tool[1].old-"); err != nil {
		t.Fatal(err)
	}
	var left []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if want := []string{".tool1.old-3", ".tool[1].oldish", "tool[1]"}; !slices.Equal(left, want) {
		t.Errorf("left %q, want %q", left, want)
	}
	if err := removeOld(filepath.Join(dir, "missing"), ".x.old-"); err == nil {
		t.Error("a directory that cannot be listed is no error")
	}
}

// TestInstallFreshnessNamesTheInstalledFile: the warning names a Go binary
// built from an older commit of this repository, also when the build output
// is not there (a fresh clone): a path without glob characters names its
// file; a pattern matching nothing, or a binary built in another repository,
// says nothing.
func TestInstallFreshnessNamesTheInstalledFile(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a synthetic Go binary")
	}
	build := func(root, out, message string) {
		t.Helper()
		for name, body := range map[string]string{
			"go.mod":  "module example.org/tool\ngo 1.22\n",
			"main.go": "package main\nfunc main() {}\n",
		} {
			if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		gittest.Run(t, root, "init", "-q", "-b", "main")
		gittest.Run(t, root, "add", "go.mod", "main.go")
		gittest.Run(t, root, "commit", "-q", "-m", message)
		cmd := exec.Command("go", "build", "-buildvcs=true", "-o", out, ".")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build: %v\n%s", err, out)
		}
	}
	root, dest := t.TempDir(), t.TempDir()
	build(root, filepath.Join(dest, "tool"), "fixture")
	built := gittest.Run(t, root, "rev-parse", "HEAD")
	gittest.Run(t, root, "commit", "-q", "--allow-empty", "-m", "two")
	head := gittest.Run(t, root, "rev-parse", "HEAD")
	runner := func(path string) *Runner {
		return &Runner{Root: root, Config: &config.Config{Tasks: map[string]*config.Task{
			"install": {Run: []config.Step{{Install: &config.InstallStep{Path: path, To: dest}}}},
		}}}
	}
	want := []string{"tool is from " + shortRevision(built) + ", 1 commit behind; run graft install"}
	if got := runner("bin/tool").InstallFreshnessWarnings(context.Background(), head); !slices.Equal(got, want) {
		t.Errorf("unbuilt path: %q, want %q", got, want)
	}
	if got := runner("bin/*").InstallFreshnessWarnings(context.Background(), head); len(got) != 0 {
		t.Errorf("pattern matching nothing: %q, want nothing", got)
	}
	// Another message: the same tree, author and second would give the
	// other repository the very same commit.
	build(t.TempDir(), filepath.Join(dest, "tool"), "another repository")
	if got := runner("bin/tool").InstallFreshnessWarnings(context.Background(), head); len(got) != 0 {
		t.Errorf("binary of another repository: %q, want nothing", got)
	}
}
