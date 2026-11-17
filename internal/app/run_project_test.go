package app

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gittest"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

// TestTaskProjectRunsAsRunOpens: graft <task> runs the task in the project
// it opened to find the task, without opening it again, and that run is the
// one graft run <task> gives opening the project itself.
func TestTaskProjectRunsAsRunOpens(t *testing.T) {
	base := t.TempDir()
	// A git in PATH that notes each opening of a repository (gitx.Open).
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin, opens := filepath.Join(base, "bin"), filepath.Join(base, "opens.txt")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// The paths come through the environment: any byte in them stays as is.
	t.Setenv("GRAFT_TEST_OPENS", opens)
	t.Setenv("GRAFT_TEST_REAL_GIT", realGit)
	script := "#!/bin/sh\ncase \"$*\" in *--show-toplevel*) echo open >> \"$GRAFT_TEST_OPENS\";; esac\nexec \"$GRAFT_TEST_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	openings := func() int {
		t.Helper()
		data, err := os.ReadFile(opens)
		if os.IsNotExist(err) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(opens); err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(data), "open\n")
	}
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "cache"))
	t.Setenv("GIT_CEILING_DIRECTORIES", base)
	root := filepath.Join(base, "projectA")
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "init", "-q", "-b", "main")
	yaml := "schema: 1\nversion: {mode: none}\ntasks:\n  where:\n    run:\n" +
		"      - [sh, -c, 'printf \"%s %s\\n\" \"$PWD\" \"$(git rev-parse --git-dir)\" >> runs.txt']\n"
	if err := os.WriteFile(filepath.Join(root, ".graft.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	newApp := func(dir string) *App {
		return &App{Dir: dir, Stdout: io.Discard, Stderr: io.Discard, NoCache: true}
	}
	runs := func() []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, "runs.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(root, "runs.txt")); err != nil {
			t.Fatal(err)
		}
		return data
	}

	a := newApp(sub)
	p, ok, err := a.TaskProject("where")
	if err != nil || !ok {
		t.Fatalf("task where: %v, %v", ok, err)
	}
	repo, err := gitx.Open(sub)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	if *p.repo != *repo || !reflect.DeepEqual(p.cfg, cfg) {
		t.Errorf("the project found %+v, %+v; opening gives %+v, %+v", p.repo, p.cfg, repo, cfg)
	}
	openings()
	if err := a.Run(ctx, p, "where", nil); err != nil {
		t.Fatal(err)
	}
	if n := openings(); n != 0 {
		t.Errorf("Run with the project passed opened the repository %d times; want 0", n)
	}
	passed := runs()
	if err := newApp(sub).Run(ctx, nil, "where", nil); err != nil {
		t.Fatal(err)
	}
	if n := openings(); n != 1 {
		t.Errorf("Run with no project opened the repository %d times; want 1", n)
	}
	if opened := runs(); !bytes.Equal(passed, opened) || len(passed) == 0 {
		t.Errorf("the run in the project passed: %q; opened by Run: %q", passed, opened)
	}

	if p, ok, err := newApp(sub).TaskProject("nowhere"); err != nil || ok || p != nil {
		t.Errorf("no such task: %v, %v, %v; want nil, false, nil", p, ok, err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if p, ok, err := newApp(outside).TaskProject("where"); err != nil || ok || p != nil {
		t.Errorf("outside a repository: %v, %v, %v; want nil, false, nil", p, ok, err)
	}
}
