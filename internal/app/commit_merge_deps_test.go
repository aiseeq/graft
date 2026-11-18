package app

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/message"
)

func configureFixtureCommitDeps(t *testing.T, f *syncPushFixture, script string) {
	t.Helper()
	setFixtureCommitDepsConfig(t, f, commitDepsFixtureConfig(script, "", ""), "main", "origin")
}

func commitDepsFixtureConfig(script, deployRemote, pushRemote string) string {
	data := "schema: 1\nversion: {mode: none}\n"
	if deployRemote != "" {
		data += "envs: {local: {}}\ndeploy:\n  remote: " + deployRemote + "\n  targets:\n    smoke: {env: local, run: [sh, deploy.sh]}\n"
	}
	if pushRemote != "" {
		data += "push: {remotes: [" + pushRemote + "]}\n"
	}
	return data + "merge:\n  deps: [projecta]\ntasks:\n  projecta:\n    run:\n      - {sh: " + strconv.Quote(script) + "}\n"
}

func setFixtureCommitDepsConfig(t *testing.T, f *syncPushFixture, data, branch string, remotes ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.work, ".graft.yaml"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git(f.work, "add", ".graft.yaml")
	f.git(f.work, "commit", "-q", "-m", "configure merge deps")
	for _, remote := range remotes {
		f.git(f.work, "push", "-q", remote, branch)
	}
}

func commitWithDeps(t *testing.T, f *syncPushFixture, opts CommitOptions) (string, error) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.work, "change"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := (&App{Dir: f.work, Stdout: &out, Stderr: io.Discard}).Commit(context.Background(), opts)
	return out.String(), err
}

func commitOptions() CommitOptions {
	return CommitOptions{Message: message.Source{Parts: []string{"feat: change"}}}
}

func TestCommitTrunkDepsFailureKeepsCommitLocal(t *testing.T) {
	f := newSyncPushFixture(t, false)
	configureFixtureCommitDeps(t, f, "echo projecta-failed >&2; exit 7")
	before := f.git(f.remote, "rev-parse", "refs/heads/main")
	out, err := commitWithDeps(t, f, commitOptions())
	if err == nil || !strings.Contains(err.Error(), "projecta") || !strings.Contains(err.Error(), f.work) {
		t.Fatalf("commit output %q, error %v; want failed task and project path", out, err)
	}
	if got := f.git(f.work, "rev-parse", "HEAD"); got == before {
		t.Fatal("failed deps did not leave a local commit")
	}
	if got := f.git(f.remote, "rev-parse", "refs/heads/main"); got != before {
		t.Fatalf("remote advanced to %s after failed deps; want %s", got, before)
	}
}

func TestCommitTrunkDepsCommitGeneratedRefusesPush(t *testing.T) {
	f := newSyncPushFixture(t, false)
	script := `echo generated > generated && git add generated && tree=$(git write-tree) && parent=$(git rev-parse HEAD) && commit=$(git -c user.name=Test -c user.email=test@example.invalid -c commit.gpgsign=false commit-tree "$tree" -p "$parent" -m generated) && git update-ref HEAD "$commit"`
	configureFixtureCommitDeps(t, f, script)
	before := f.git(f.remote, "rev-parse", "refs/heads/main")
	if err := os.WriteFile(filepath.Join(f.work, "change"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	var stdout bytes.Buffer
	err := (&App{Dir: f.work, Stdout: &stdout, Stderr: &stderr}).Commit(context.Background(), commitOptions())
	if err == nil || !strings.Contains(err.Error(), "merge.deps changed HEAD") || !strings.Contains(err.Error(), "generated") {
		t.Fatalf("output = %s\nstderr = %s\nerror = %v; want changed HEAD and generated path", stdout.String(), stderr.String(), err)
	}
	if got := f.git(f.work, "log", "-1", "--format=%s"); got != "generated" {
		t.Fatalf("local HEAD subject = %q, want generated task commit", got)
	}
	if got := f.git(f.remote, "rev-parse", "refs/heads/main"); got != before {
		t.Fatalf("remote advanced to %s after deps changed HEAD; want %s", got, before)
	}
}

func TestCommitTrunkDepsPassPushes(t *testing.T) {
	f := newSyncPushFixture(t, false)
	configureFixtureCommitDeps(t, f, "true")
	if _, err := commitWithDeps(t, f, commitOptions()); err != nil {
		t.Fatal(err)
	}
	if got, want := f.git(f.remote, "rev-parse", "refs/heads/main"), f.git(f.work, "rev-parse", "HEAD"); got != want {
		t.Fatalf("remote main = %s, want committed HEAD %s", got, want)
	}
}

func TestCommitNoDepsRequiresReasonAndReasonIsPrinted(t *testing.T) {
	f := newSyncPushFixture(t, false)
	configureFixtureCommitDeps(t, f, "echo should-not-run >&2; exit 9")
	withoutReason := commitOptions()
	withoutReason.NoDeps = true
	if out, err := commitWithDeps(t, f, withoutReason); err == nil || !strings.Contains(err.Error(), "--reason") {
		t.Fatalf("--no-deps without reason: output %q, error %v", out, err)
	}
	withReason := commitOptions()
	withReason.NoDeps, withReason.NoDepsReason = true, "incident response"
	out, err := commitWithDeps(t, f, withReason)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "incident response") || strings.Contains(out, "should-not-run") {
		t.Fatalf("bypass output = %q; want printed reason and no deps", out)
	}
	if got, want := f.git(f.remote, "rev-parse", "refs/heads/main"), f.git(f.work, "rev-parse", "HEAD"); got != want {
		t.Fatalf("remote main = %s, want committed HEAD %s", got, want)
	}
}

func TestCommitOnNonTrunkDoesNotRunMergeDeps(t *testing.T) {
	f := newSyncPushFixture(t, false)
	configureFixtureCommitDeps(t, f, "echo should-not-run >&2; exit 9")
	f.git(f.work, "checkout", "-q", "-b", "feature")
	out, err := commitWithDeps(t, f, commitOptions())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "merge deps:") || strings.Contains(out, "should-not-run") {
		t.Fatalf("non-trunk commit ran merge deps: %s", out)
	}
}

func TestCommitRefusesFeatureWithoutRemoteHead(t *testing.T) {
	f := newSyncPushFixture(t, false)
	configureFixtureCommitDeps(t, f, "echo should-not-run >&2; exit 9")
	f.git(f.work, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	f.git(f.work, "checkout", "-q", "-b", "feature")
	_, err := commitWithDeps(t, f, commitOptions())
	if err == nil || !strings.Contains(err.Error(), "the trunk of origin is unknown") {
		t.Fatalf("feature commit error = %v; want missing remote HEAD refusal", err)
	}
}

func TestCommitWithoutMergeDepsIgnoresMissingRemoteHead(t *testing.T) {
	f := newSyncPushFixture(t, false)
	f.git(f.work, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	out, err := commitWithDeps(t, f, commitOptions())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "trunk unknown") {
		t.Fatalf("commit without merge.deps reported unknown trunk: %s", out)
	}
}

func TestCommitMainWithoutOriginHeadStillRunsDeps(t *testing.T) {
	f := newSyncPushFixture(t, false)
	configureFixtureCommitDeps(t, f, "exit 7")
	f.git(f.work, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	before := f.git(f.remote, "rev-parse", "refs/heads/main")
	_, err := commitWithDeps(t, f, commitOptions())
	if err == nil || !strings.Contains(err.Error(), "the trunk of origin is unknown") || !strings.Contains(err.Error(), "then commit again") {
		t.Fatalf("commit error = %v; want missing remote HEAD refusal", err)
	}
	if got := f.git(f.remote, "rev-parse", "refs/heads/main"); got != before {
		t.Fatalf("remote advanced to %s after failed deps; want %s", got, before)
	}
}

func TestCommitUsesPrimaryPushRemoteTrunk(t *testing.T) {
	f := newSyncPushFixture(t, false)
	mirror := filepath.Join(f.root, "mirror.git")
	f.git(f.root, "init", "--bare", "-q", "-b", "main", mirror)
	f.git(f.work, "remote", "add", "mirror", mirror)
	setFixtureCommitDepsConfig(t, f, commitDepsFixtureConfig("exit 7", "origin", "mirror"), "main", "origin", "mirror")
	f.git(f.work, "push", "-q", "origin", "main:dev")
	f.git(f.work, "fetch", "-q", "origin", "dev")
	f.git(f.work, "fetch", "-q", "mirror", "main")
	f.git(f.work, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/dev")
	f.git(f.work, "symbolic-ref", "refs/remotes/mirror/HEAD", "refs/remotes/mirror/main")
	before := f.git(mirror, "rev-parse", "refs/heads/main")
	if _, err := commitWithDeps(t, f, commitOptions()); err == nil || !strings.Contains(err.Error(), "projecta") {
		t.Fatalf("commit error = %v; want primary remote trunk deps to fail", err)
	}
	if got := f.git(mirror, "rev-parse", "refs/heads/main"); got != before {
		t.Fatalf("mirror main advanced to %s after failed deps; want %s", got, before)
	}
}

func TestCommitRefusesMirrorWithoutLocalHead(t *testing.T) {
	f := newSyncPushFixture(t, false)
	mirror := filepath.Join(f.root, "mirror.git")
	f.git(f.root, "init", "--bare", "-q", "-b", "main", mirror)
	f.git(f.work, "remote", "add", "mirror", mirror)
	setFixtureCommitDepsConfig(t, f, commitDepsFixtureConfig("exit 7", "", "mirror"), "main", "origin", "mirror")
	f.git(f.work, "push", "-q", "origin", "main:dev")
	f.git(f.work, "fetch", "-q", "origin", "dev")
	f.git(f.work, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/dev")
	localBefore := f.git(f.work, "rev-parse", "HEAD")
	before := f.git(mirror, "rev-parse", "refs/heads/main")
	out, err := commitWithDeps(t, f, commitOptions())
	if err == nil || !strings.Contains(err.Error(), "the trunk of mirror is unknown") || !strings.Contains(err.Error(), "git remote set-head mirror --auto") {
		t.Fatalf("commit output %q, error %v; want mirror HEAD refusal", out, err)
	}
	if strings.Contains(out, "merge deps: projecta") {
		t.Fatalf("deps ran despite unknown mirror HEAD: %s", out)
	}
	if got := f.git(mirror, "rev-parse", "refs/heads/main"); got != before {
		t.Fatalf("mirror main advanced to %s; want %s", got, before)
	}
	if got := f.git(f.work, "rev-parse", "HEAD"); got == localBefore {
		t.Fatal("commit was not kept locally after missing mirror HEAD")
	}
}

func TestCommitRefusesDanglingMirrorHead(t *testing.T) {
	f := newSyncPushFixture(t, false)
	mirror := filepath.Join(f.root, "mirror.git")
	f.git(f.root, "init", "--bare", "-q", "-b", "main", mirror)
	f.git(f.work, "remote", "add", "mirror", mirror)
	setFixtureCommitDepsConfig(t, f, commitDepsFixtureConfig("exit 7", "", "mirror"), "main", "origin", "mirror")
	f.git(f.work, "symbolic-ref", "refs/remotes/mirror/HEAD", "refs/remotes/mirror/missing")
	before := f.git(mirror, "rev-parse", "refs/heads/main")
	_, err := commitWithDeps(t, f, commitOptions())
	if err == nil || !strings.Contains(err.Error(), "the trunk of mirror is unknown") {
		t.Fatalf("commit error = %v; want dangling mirror HEAD refusal", err)
	}
	if got := f.git(mirror, "rev-parse", "refs/heads/main"); got != before {
		t.Fatalf("mirror main advanced to %s; want %s", got, before)
	}
}

func TestCommitRefusesDeployRemoteWithoutLocalHead(t *testing.T) {
	f := newSyncPushFixture(t, false)
	mirror := filepath.Join(f.root, "mirror.git")
	f.git(f.root, "init", "--bare", "-q", "-b", "stable", mirror)
	f.git(f.work, "remote", "add", "mirror", mirror)
	f.git(f.work, "checkout", "-q", "-b", "stable")
	setFixtureCommitDepsConfig(t, f, commitDepsFixtureConfig("exit 7", "mirror", ""), "stable", "origin", "mirror")
	f.git(f.work, "push", "-q", "origin", "stable:dev")
	f.git(f.work, "fetch", "-q", "origin", "dev")
	f.git(f.work, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/dev")
	originBefore := f.git(f.remote, "rev-parse", "refs/heads/stable")
	mirrorBefore := f.git(mirror, "rev-parse", "refs/heads/stable")
	localBefore := f.git(f.work, "rev-parse", "HEAD")
	_, err := commitWithDeps(t, f, commitOptions())
	if err == nil || !strings.Contains(err.Error(), "the trunk of mirror is unknown") {
		t.Fatalf("commit error = %v; want deploy remote HEAD refusal", err)
	}
	if got := f.git(f.remote, "rev-parse", "refs/heads/stable"); got != originBefore {
		t.Fatalf("origin stable advanced to %s; want %s", got, originBefore)
	}
	if got := f.git(mirror, "rev-parse", "refs/heads/stable"); got != mirrorBefore {
		t.Fatalf("mirror stable advanced to %s; want %s", got, mirrorBefore)
	}
	if got := f.git(f.work, "rev-parse", "HEAD"); got == localBefore {
		t.Fatal("commit was not kept locally after missing deploy remote HEAD")
	}
}

func TestMergeUnknownOriginHeadRefusesUnpublishedLocalCommit(t *testing.T) {
	f := newSyncPushFixture(t, false)
	configureFixtureCommitDeps(t, f, "true")
	f.git(f.work, "push", "-q", "origin", "main:release")
	f.git(f.remote, "symbolic-ref", "HEAD", "refs/heads/release")
	f.git(f.work, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	f.git(f.work, "config", "remote.origin.followRemoteHEAD", "never")

	f.commit(f.work, "local-unpublished")
	f.git(f.work, "checkout", "-q", "-b", "feature")
	f.commit(f.work, "feature-change")
	f.git(f.work, "checkout", "-q", "main")
	mainBefore := f.git(f.remote, "rev-parse", "refs/heads/main")
	releaseBefore := f.git(f.remote, "rev-parse", "refs/heads/release")
	var out bytes.Buffer
	err := (&App{Dir: f.work, Stdout: &out, Stderr: io.Discard}).Merge(context.Background(), MergeOptions{Branch: "feature"})
	if err == nil || !strings.Contains(err.Error(), "origin/HEAD is not set, the trunk is unknown") {
		t.Fatalf("merge output %q, error %v; want unknown origin HEAD refusal", out.String(), err)
	}
	if got := f.git(f.remote, "rev-parse", "refs/heads/main"); got != mainBefore {
		t.Fatalf("remote main advanced to %s; want %s", got, mainBefore)
	}
	if got := f.git(f.remote, "rev-parse", "refs/heads/release"); got != releaseBefore {
		t.Fatalf("remote release advanced to %s; want %s", got, releaseBefore)
	}
}
