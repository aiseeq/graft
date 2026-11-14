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

// TestInstallFreshnessNamesTheInstalledFile checks that a glob with no local
// matches finds stale installed binaries from this repository, while skipping
// binaries built in another repository.
func TestInstallFreshnessNamesTheInstalledFile(t *testing.T) {
	if testing.Short() {
		t.Skip("builds synthetic Go binaries")
	}
	build := func(root, out, message string) string {
		t.Helper()
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
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
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build: %v\n%s", err, output)
		}
		return gittest.Run(t, root, "rev-parse", "HEAD")
	}
	root, dest := t.TempDir(), t.TempDir()
	built := build(root, filepath.Join(dest, "tool"), "fixture")
	gittest.Run(t, root, "commit", "-q", "--allow-empty", "-m", "two")
	head := gittest.Run(t, root, "rev-parse", "HEAD")
	build(t.TempDir(), filepath.Join(dest, "other-tool"), "another repository")
	runner := func(path string) *Runner {
		return &Runner{Root: root, Config: &config.Config{Tasks: map[string]*config.Task{
			"install": {Run: []config.Step{{Install: &config.InstallStep{Path: path, To: dest}}}},
		}}}
	}
	want := []string{"tool is from " + shortRevision(built) + ", 1 commit behind; run graft install"}
	if got := runner("bin/tool").InstallFreshnessWarnings(context.Background(), head); !slices.Equal(got, want) {
		t.Errorf("unbuilt path: %q, want %q", got, want)
	}
	if got := runner("bin/*").InstallFreshnessWarnings(context.Background(), head); !slices.Equal(got, want) {
		t.Errorf("unmatched glob: %q, want only the local stale binary %q", got, want)
	}
	// Another message: the same tree, author and second would give the
	// other repository the very same commit.
	build(t.TempDir(), filepath.Join(dest, "tool"), "another repository, same name")
	if got := runner("bin/tool").InstallFreshnessWarnings(context.Background(), head); len(got) != 0 {
		t.Errorf("binary of another repository: %q, want nothing", got)
	}
	if got := runner("bin/*").InstallFreshnessWarnings(context.Background(), head); len(got) != 0 {
		t.Errorf("unmatched glob over binaries of other repositories: %q, want nothing", got)
	}
}
