//go:build linux

package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/aiseeq/graft/v2/internal/gittest"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

func TestGlintOldCommitWithoutConfig(t *testing.T) {
	for _, tc := range []struct {
		name     string
		config   string
		wantWarn bool
	}{
		{name: "no config", wantWarn: true},
		{name: "unreadable config", config: "schema: 1\nunknown: true\n", wantWarn: true},
		{name: "no link groups", config: "schema: 1\nversion: {mode: file}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			git := func(args ...string) string { return gittest.Run(t, root, args...) }
			git("init", "-q", "-b", "main")
			if err := os.WriteFile(filepath.Join(root, "old.txt"), []byte("old\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git("add", "old.txt")
			if tc.config != "" {
				if err := os.WriteFile(filepath.Join(root, ".graft.yaml"), []byte(tc.config), 0o644); err != nil {
					t.Fatal(err)
				}
				git("add", ".graft.yaml")
			}
			git("commit", "-q", "-m", "old")
			old := git("rev-parse", "HEAD")
			if err := os.WriteFile(filepath.Join(root, ".graft.yaml"), []byte("schema: 1\nversion: {mode: file}\n# current\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git("add", ".graft.yaml")
			git("commit", "-q", "-m", "config")
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "glint"), []byte("#!/bin/sh\nprintf '{\"summary\":{\"total\":0}}\\n'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			repo, err := gitx.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			g := &glintCheck{a: &App{Stdout: &out, Stderr: &out}, repo: repo, severity: "low", sha: func(context.Context) (string, error) { return old, nil }}
			sha, found, err := g.count(t.Context())
			if err != nil || sha != old || found != 0 || strings.Contains(out.String(), "graft: warning:") != tc.wantWarn || tc.wantWarn && !strings.Contains(out.String(), "glint runs without worktree.link links") {
				t.Fatalf("old commit: sha=%s found=%d err=%v output=%s", sha, found, err, out.String())
			}
		})
	}
}

// Worktree add must give setup the terminal itself and print its output once.
func TestWorktreeSetupKeepsTerminal(t *testing.T) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	num, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", num), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	root := t.TempDir()
	gittest.Run(t, root, "init", "-q", "-b", "main")
	cfg := "schema: 1\nversion: {mode: none}\nworktree: {setup: [setup]}\ntasks:\n  setup: {run: [sh setup.sh]}\n"
	if err := os.WriteFile(filepath.Join(root, ".graft.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nif test -t 1 && test -t 2; then echo tty-ok; else echo tty-lost; fi\necho setup-sentinel\nexit 7\n"
	if err := os.WriteFile(filepath.Join(root, "setup.sh"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	a := &App{Stdout: slave, Stderr: slave}
	err = a.prepareWorktree(t.Context(), root, root, nil)
	if err == nil || !strings.Contains(err.Error(), "worktree.setup setup") || strings.Contains(err.Error(), "setup-sentinel") {
		t.Fatalf("setup error must name the task without its output: %v", err)
	}
	if err := unix.SetNonblock(int(master.Fd()), true); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8192)
	n, err := unix.Read(int(master.Fd()), buf)
	if err != nil {
		t.Fatal(err)
	}
	out := string(buf[:n])
	if !strings.Contains(out, "tty-ok") || strings.Contains(out, "tty-lost") || strings.Count(out, "setup-sentinel") != 1 {
		t.Fatalf("setup lost its terminal or duplicated output: %q", out)
	}
}
