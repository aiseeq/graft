package app

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/gitx"
)

func TestWarnFirstCommitWithoutRemote(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "no-global-config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	git := func(args ...string) string { return authorTestGit(t, root, args...) }
	git("init", "-q", "-b", "main")
	git("config", "user.email", "author@example.com")
	repo, err := gitx.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	a := &App{Stderr: &stderr}
	a.warnFirstCommitWithoutRemote(repo, commitPlan{head: headState{branch: "main"}})
	if got := stderr.String(); !strings.Contains(got, "user.email is author@example.com") || !strings.Contains(got, "graft amend --reset-author") {
		t.Fatalf("warning = %q", got)
	}
	if strings.ContainsAny(stderr.String(), "абвгдежзийклмнопрстуфхцчшщъыьэюя") {
		t.Fatalf("warning contains Russian text: %q", stderr.String())
	}

	stderr.Reset()
	git("commit", "-q", "--allow-empty", "-m", "first")
	a.warnFirstCommitWithoutRemote(repo, commitPlan{head: headState{branch: "main", sha: git("rev-parse", "HEAD")}})
	if stderr.Len() != 0 {
		t.Fatalf("warning after first commit = %q", stderr.String())
	}

	git("symbolic-ref", "HEAD", "refs/heads/other")
	git("remote", "add", "origin", "https://example.com/projectA/repo.git")
	stderr.Reset()
	a.warnFirstCommitWithoutRemote(repo, commitPlan{head: headState{branch: "other"}})
	if stderr.Len() != 0 {
		t.Fatalf("warning with remote = %q", stderr.String())
	}
}

func TestWarnFirstCommitWithoutRemoteMissingEmail(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup []string
		want  string
	}{
		{name: "unset", want: "user.email is not set"},
		{name: "empty", setup: []string{"config", "user.email", ""}, want: "user.email is set but empty"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "no-global-config"))
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			git := func(args ...string) string { return authorTestGit(t, root, args...) }
			git("init", "-q", "-b", "main")
			if len(test.setup) > 0 {
				git(test.setup...)
			}
			repo, err := gitx.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			(&App{Stderr: &stderr}).warnFirstCommitWithoutRemote(repo, commitPlan{head: headState{branch: "main"}})
			if got := stderr.String(); !strings.Contains(got, test.want) || !strings.Contains(got, "graft amend --reset-author") {
				t.Fatalf("warning = %q, want %q and repair hint", got, test.want)
			}
		})
	}
}

func authorTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "GIT_CONFIG_GLOBAL=") && !strings.HasPrefix(env, "GIT_CONFIG_SYSTEM=") && env != "GIT_CONFIG_NOSYSTEM=1" {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_GLOBAL="+filepath.Join(dir, "no-global-config"), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
