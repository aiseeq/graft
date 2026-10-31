package app

import (
	"bytes"

	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

func TestFreezePolishOldJournalWarning(t *testing.T) {
	repo, cfg, head, remote := freezePolishFixture(t, true)
	tag := "freeze-log/projectA/1.2.3/20261030T100000-abcdef1-advisory"
	freezePolishGit(t, repo.Root, "tag", "-a", tag, head, "-m", "target: commit")
	var output bytes.Buffer
	a := &App{Stdout: &output}
	f := &activeFreeze{target: "projectA", version: "1.2.3", time: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	if err := a.pushPendingFreezeLogs(repo, cfg, f); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), tag) || !strings.Contains(output.String(), "before freeze start") {
		t.Fatalf("old journal skip warning = %q", output.String())
	}
	if got := freezePolishGit(t, remote, "for-each-ref", "--format=%(refname)", "refs/tags/"+tag); got != "" {
		t.Fatalf("old journal tag was published: %s", got)
	}
}

func TestFreezePolishClockSkewAllowance(t *testing.T) {
	freezeTime := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	if !freezeLogIsCurrent(freezeTime.Add(-2*time.Minute), freezeTime) {
		t.Fatal("record at the two-minute allowance boundary was considered old")
	}
	if freezeLogIsCurrent(freezeTime.Add(-2*time.Minute-time.Second), freezeTime) {
		t.Fatal("record older than the two-minute allowance was considered current")
	}
}

func TestFreezePolishAmendKeepsLocalRecordWhenRemoteCheckFails(t *testing.T) {
	repo, cfg, _, _ := freezePolishFixture(t, false)
	f := &activeFreeze{target: "projectA", version: "1.2.3"}
	var output bytes.Buffer
	a := &App{Stdout: &output}
	if err := a.recordCommitAdvisoryLocal(repo, cfg, f, []string{"lint"}); err != nil {
		t.Fatalf("recording a local advisory entry failed the amend: %v", err)
	}
	if !strings.Contains(output.String(), "warning: advisory record") || !strings.Contains(output.String(), "is local") {
		t.Fatalf("local advisory warning = %q", output.String())
	}
	tag := freezePolishGit(t, repo.Root, "tag", "--list", "freeze-log/projectA/1.2.3/*")
	if !strings.HasSuffix(tag, "-advisory") {
		t.Fatalf("local advisory tag = %q", tag)
	}
	if got := freezePolishGit(t, repo.Root, "cat-file", "-t", "refs/tags/"+tag); got != "tag" {
		t.Fatalf("local advisory tag type = %q, want tag", got)
	}
}

func TestFreezePolishThawReasonSurvives(t *testing.T) {
	repo, cfg, _, remote := freezePolishFixture(t, true)
	f := &activeFreeze{target: "projectA", name: "Product 1.2.3", version: "1.2.3"}
	tag, err := writeFreezeThawRecord(repo, cfg, f, "approval received")
	if err != nil {
		t.Fatal(err)
	}
	body := freezePolishGit(t, remote, "cat-file", "-p", "refs/tags/"+tag)
	if !strings.Contains(body, "reason: approval received") {
		t.Fatalf("remote thaw record %s omitted reason: %s", tag, body)
	}
}

func TestFreezePolishAmendRemovesPreviousAdvisoryRecord(t *testing.T) {
	repo, cfg, head, remote := freezePolishFixture(t, true)
	f := &activeFreeze{target: "projectA", version: "1.2.3"}
	tag := "freeze-log/projectA/1.2.3/20261030T100000-abcdef1-advisory"
	freezePolishGit(t, repo.Root, "tag", "-a", tag, head, "-m", "target: commit", "-m", "commit: "+head, "-m", "reason: lint failed")
	freezePolishGit(t, repo.Root, "push", "origin", "refs/tags/"+tag)
	if err := removeCommitAdvisoryLogs(repo, cfg, f, head); err != nil {
		t.Fatal(err)
	}
	if got := freezePolishGit(t, remote, "for-each-ref", "--format=%(refname)", "refs/tags/"+tag); got != "" {
		t.Fatalf("replaced advisory tag remains remote: %s", got)
	}
}

func freezePolishFixture(t *testing.T, withRemote bool) (*gitx.Repo, *config.Config, string, string) {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	remote := filepath.Join(root, "remote.git")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	freezePolishGit(t, root, "init", "--bare", "-q", remote)
	freezePolishGit(t, work, "init", "-q")
	freezePolishGit(t, work, "config", "user.name", "Test User")
	freezePolishGit(t, work, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(work, "file"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	freezePolishGit(t, work, "add", "file")
	freezePolishGit(t, work, "commit", "-q", "-m", "base")
	remoteURL := remote
	if !withRemote {
		remoteURL = filepath.Join(root, "missing.git")
	}
	freezePolishGit(t, work, "remote", "add", "origin", remoteURL)
	repo, err := gitx.Open(work)
	if err != nil {
		t.Fatal(err)
	}
	head := freezePolishGit(t, work, "rev-parse", "HEAD")
	return repo, &config.Config{Deploy: &config.Deploy{Remote: "origin"}}, head, remote
}

func freezePolishGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
