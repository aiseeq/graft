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

// TestFreezePolishAmendWithoutRemotesRecordsHere: amend under a freeze with
// its remote unreachable makes the amended commit's advisory record here,
// takes the replaced commit's local record away, and says the remote part
// failed; before, the failed listing of the remote left no record at all.
func TestFreezePolishAmendWithoutRemotesRecordsHere(t *testing.T) {
	repo, cfg, oldHead, _ := freezePolishFixture(t, false)
	f := &activeFreeze{target: "projectA", version: "1.2.3"}
	old := "freeze-log/projectA/1.2.3/20261030T100000-abcdef1-advisory"
	freezePolishGit(t, repo.Root, "tag", "-a", old, oldHead, "-m", "target: commit", "-m", "commit: "+oldHead, "-m", "reason: lint failed")
	var output bytes.Buffer
	a := &App{Stdout: &output}
	err := a.finishAmend(repo, cfg, "base, amended\n", f, oldHead, []string{"lint"}, savedIndex{})
	if err == nil || !strings.Contains(err.Error(), "amend is made, but the freeze advisory record for lint is made, while the replaced commit's could not be removed") {
		t.Fatalf("amend error = %v", err)
	}
	newHead := freezePolishGit(t, repo.Root, "rev-parse", "HEAD")
	if newHead == oldHead {
		t.Fatal("the commit was not amended")
	}
	tags := strings.Fields(freezePolishGit(t, repo.Root, "tag", "--list", "freeze-log/projectA/1.2.3/*"))
	if len(tags) != 1 || tags[0] == old {
		t.Fatalf("local records = %q, want the amended commit's only", tags)
	}
	if body := freezePolishGit(t, repo.Root, "cat-file", "tag", tags[0]); !strings.Contains(body, "commit: "+newHead) {
		t.Fatalf("the record names another commit:\n%s", body)
	}
}

// TestFreezePolishUnreadRemoteRecordsKeepNoLocalOne: the remotes answer, but
// their records cannot be read (the same tag differs between two remotes): the
// replaced commit's record here goes all the same, the remotes' tags stay, and
// the error says the remote part failed.
func TestFreezePolishUnreadRemoteRecordsKeepNoLocalOne(t *testing.T) {
	repo, cfg, head, remote := freezePolishFixture(t, true)
	backup := filepath.Join(filepath.Dir(remote), "backup.git")
	freezePolishGit(t, repo.Root, "init", "--bare", "-q", backup)
	freezePolishGit(t, repo.Root, "remote", "add", "backup", backup)
	f := &activeFreeze{target: "projectA", version: "1.2.3"}
	tag := "freeze-log/projectA/1.2.3/20261030T100000-abcdef1-advisory"
	freezePolishGit(t, repo.Root, "tag", "-a", tag, head, "-m", "target: commit", "-m", "commit: "+head, "-m", "reason: other")
	freezePolishGit(t, repo.Root, "push", "-q", "backup", "refs/tags/"+tag)
	freezePolishGit(t, repo.Root, "tag", "-f", "-a", tag, head, "-m", "target: commit", "-m", "commit: "+head, "-m", "reason: lint failed")
	freezePolishGit(t, repo.Root, "push", "-q", "origin", "refs/tags/"+tag)

	err := removeCommitAdvisoryLogs(repo, cfg, f, head)
	if err == nil || !strings.Contains(err.Error(), "the remotes' records: tag "+tag+" differs between remotes") {
		t.Fatalf("error = %v", err)
	}
	if got := freezePolishGit(t, repo.Root, "tag", "--list", tag); got != "" {
		t.Fatalf("the replaced commit's record stays here: %s", got)
	}
	for _, r := range []string{remote, backup} {
		if got := freezePolishGit(t, r, "for-each-ref", "--format=%(refname)", "refs/tags/"+tag); got == "" {
			t.Fatalf("an unread tag went from %s", r)
		}
	}
}
