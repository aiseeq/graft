package app

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type syncPushFixture struct {
	t      *testing.T
	root   string
	work   string
	remote string
}

func syncPushGitEnv(root string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key == "GIT_CONFIG" || strings.HasPrefix(key, "GIT_CONFIG_") || key == "GIT_DIR" || key == "GIT_WORK_TREE" || key == "GIT_INDEX_FILE" {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(root, "gitconfig"))
}

func newSyncPushFixture(t *testing.T, singleBranch bool) *syncPushFixture {
	t.Helper()
	root := t.TempDir()
	remote, work := filepath.Join(root, "remote.git"), filepath.Join(root, "work")
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = syncPushGitEnv(root)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(root, "init", "--bare", "-q", "-b", "main", remote)
	run(root, "init", "-q", "-b", "main", filepath.Join(root, "seed"))
	seed := filepath.Join(root, "seed")
	run(seed, "config", "user.name", "Test")
	run(seed, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(seed, ".graft.yaml"), []byte("schema: 1\nversion: {mode: none}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "base"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(seed, "add", ".")
	run(seed, "commit", "-q", "-m", "base")
	run(seed, "remote", "add", "origin", remote)
	run(seed, "push", "-q", "origin", "main")
	cloneArgs := []string{"clone", "-q"}
	if singleBranch {
		cloneArgs = append(cloneArgs, "--single-branch")
	}
	cloneArgs = append(cloneArgs, remote, work)
	run(root, cloneArgs...)
	run(work, "config", "user.name", "Test")
	run(work, "config", "user.email", "test@example.invalid")
	return &syncPushFixture{t: t, root: root, work: work, remote: remote}
}

func (f *syncPushFixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = syncPushGitEnv(f.root)
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *syncPushFixture) commit(dir, name string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
	f.git(dir, "add", name)
	f.git(dir, "commit", "-q", "-m", name)
}

func (f *syncPushFixture) sync(trunk string) error {
	f.t.Helper()
	return (&App{Dir: f.work, Stdout: io.Discard, Stderr: io.Discard}).WorktreeSync(context.Background(), WorktreeSyncOptions{Push: true, Trunk: trunk})
}

func TestWorktreeSyncPushRejectsTrunkUpstreamBeforeRebase(t *testing.T) {
	f := newSyncPushFixture(t, false)
	f.git(f.work, "checkout", "-q", "-b", "trunk", "--track", "origin/main")
	f.git(f.work, "worktree", "add", "-q", filepath.Join(f.root, "wt"), "main")
	f.commit(f.work, "trunk-advance")
	wt := filepath.Join(f.root, "wt")
	f.git(wt, "config", "user.name", "Test")
	f.git(wt, "config", "user.email", "test@example.invalid")
	f.commit(wt, "work")
	head := f.git(wt, "rev-parse", "HEAD")
	if err := (&App{Dir: wt, Stdout: io.Discard, Stderr: io.Discard}).WorktreeSync(context.Background(), WorktreeSyncOptions{Push: true, Trunk: "trunk"}); err == nil || !strings.Contains(err.Error(), "upstream of trunk") {
		t.Fatalf("sync error = %v, want trunk upstream refusal", err)
	}
	if got := f.git(wt, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD changed before trunk refusal: %s -> %s", head, got)
	}
}

func TestWorktreeSyncPushRejectsPlainRemoteHeadRefBeforeRebase(t *testing.T) {
	f := newSyncPushFixture(t, false)
	f.git(f.work, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	f.git(f.work, "update-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	f.git(f.work, "checkout", "-q", "-b", "feat")
	f.git(f.work, "push", "-q", "-u", "origin", "feat")
	f.commit(f.work, "work")
	head := f.git(f.work, "rev-parse", "HEAD")
	err := f.sync("origin/HEAD")
	if err == nil || !strings.Contains(err.Error(), "trunk to be a branch") {
		t.Fatalf("sync error = %v, want non-branch trunk refusal", err)
	}
	if got := f.git(f.work, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD changed before trunk refusal: %s -> %s", head, got)
	}
}

func TestWorktreeSyncPushRejectsMultiplePushURLsBeforeRebase(t *testing.T) {
	f := newSyncPushFixture(t, false)
	f.git(f.work, "checkout", "-q", "-b", "release", "origin/main")
	f.commit(f.work, "release-advance")
	f.git(f.work, "push", "-q", "-u", "origin", "release")
	f.git(f.work, "checkout", "-q", "-b", "feat", "origin/main")
	f.git(f.work, "push", "-q", "-u", "origin", "feat")
	f.commit(f.work, "work")
	second := filepath.Join(f.root, "second.git")
	f.git(f.root, "init", "--bare", "-q", "-b", "feat", second)
	f.git(f.work, "config", "--add", "remote.origin.url", second)
	head := f.git(f.work, "rev-parse", "HEAD")
	err := f.sync("origin/release")
	if err == nil || !strings.Contains(err.Error(), "multiple push URLs") || !strings.Contains(err.Error(), "git remote set-url --delete origin") {
		t.Fatalf("sync error = %v, want multiple push URL refusal", err)
	}
	if got := f.git(f.work, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD changed before URL refusal: %s -> %s", head, got)
	}
}

func TestWorktreeSyncPushMultiplePushURLHintDeletesPushURL(t *testing.T) {
	f := newSyncPushFixture(t, false)
	f.git(f.work, "checkout", "-q", "-b", "feat", "origin/main")
	f.git(f.work, "push", "-q", "-u", "origin", "feat")
	f.commit(f.work, "work")
	second := filepath.Join(f.root, "second.git")
	f.git(f.root, "init", "--bare", "-q", "-b", "main", second)
	url := f.git(f.work, "remote", "get-url", "origin")
	f.git(f.work, "remote", "set-url", "--add", "--push", "origin", url)
	f.git(f.work, "remote", "set-url", "--add", "--push", "origin", second)
	err := f.sync("origin/main")
	if err == nil || !strings.Contains(err.Error(), "git remote set-url --delete --push origin") {
		t.Fatalf("sync error = %v, want a hint that deletes a pushurl", err)
	}
}

func TestWorktreeSyncPushChecksServerHeadBeforeRebase(t *testing.T) {
	f := newSyncPushFixture(t, false)
	f.git(f.work, "checkout", "-q", "-b", "release", "origin/main")
	f.commit(f.work, "release-advance")
	f.git(f.work, "push", "-q", "-u", "origin", "release")
	f.git(f.work, "checkout", "-q", "-b", "feat", "origin/main")
	f.git(f.work, "push", "-q", "-u", "origin", "feat")
	f.commit(f.work, "work")
	// Move the server's default branch to feat while the chosen trunk remains release.
	f.git(f.remote, "symbolic-ref", "HEAD", "refs/heads/feat")
	head := f.git(f.work, "rev-parse", "HEAD")
	err := f.sync("origin/release")
	if err == nil || !strings.Contains(err.Error(), "remote origin default branch") {
		t.Fatalf("sync error = %v, want server HEAD refusal", err)
	}
	if got := f.git(f.work, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD changed before server HEAD refusal: %s -> %s", head, got)
	}
}

func TestWorktreeSyncPushChecksHeadAtPushURLAndKeepsLease(t *testing.T) {
	f := newSyncPushFixture(t, false)
	pushRemote := filepath.Join(f.root, "push.git")
	f.git(f.root, "clone", "-q", "--bare", f.remote, pushRemote)
	f.git(f.work, "checkout", "-q", "-b", "feat")
	f.git(f.work, "push", "-q", "-u", "origin", "feat")
	f.git(f.work, "push", "-q", pushRemote, "feat")
	f.commit(f.work, "work")
	f.git(pushRemote, "symbolic-ref", "HEAD", "refs/heads/feat")
	f.git(f.work, "remote", "set-url", "--push", "origin", pushRemote)
	head := f.git(f.work, "rev-parse", "HEAD")
	remoteRefs := f.git(pushRemote, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads")
	err := f.sync("origin/main")
	if err == nil || !strings.Contains(err.Error(), "remote origin default branch") {
		t.Fatalf("sync error = %v, want push URL default branch refusal", err)
	}
	if got := f.git(f.work, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD changed before push URL refusal: %s -> %s", head, got)
	}
	if got := f.git(pushRemote, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads"); got != remoteRefs {
		t.Fatalf("push URL refs changed: before %q, after %q", remoteRefs, got)
	}
}

func TestWorktreeSyncPushAcceptsSinglePushURLContainingSpaces(t *testing.T) {
	f := newSyncPushFixture(t, false)
	pushRemote := filepath.Join(f.root, "push remote.git")
	f.git(f.root, "clone", "-q", "--bare", f.remote, pushRemote)
	f.git(f.work, "checkout", "-q", "-b", "feat")
	f.git(f.work, "push", "-q", "-u", "origin", "feat")
	f.git(f.work, "push", "-q", pushRemote, "feat")
	f.commit(f.work, "work")
	f.git(f.work, "remote", "set-url", "--push", "origin", pushRemote)
	if err := f.sync("origin/main"); err != nil {
		t.Fatalf("sync with one push URL containing spaces: %v", err)
	}
	if got, want := f.git(pushRemote, "rev-parse", "refs/heads/feat"), f.git(f.work, "rev-parse", "HEAD"); got != want {
		t.Fatalf("push URL feat = %s, worktree HEAD = %s", got, want)
	}
}

func TestWorktreeSyncPushAcceptsRemoteHeadSymref(t *testing.T) {
	f := newSyncPushFixture(t, false)
	f.git(f.work, "checkout", "-q", "-b", "feat")
	f.git(f.work, "push", "-q", "-u", "origin", "feat")
	f.commit(f.work, "work")
	if err := f.sync("origin/HEAD"); err != nil {
		t.Fatalf("sync with origin/HEAD symref: %v", err)
	}
}

func TestWorktreeSyncPushIgnoresUnselectedLocalBranchUpstream(t *testing.T) {
	f := newSyncPushFixture(t, false)
	f.git(f.work, "checkout", "-q", "-b", "feat")
	f.git(f.work, "push", "-q", "-u", "origin", "feat")
	f.git(f.work, "checkout", "-q", "main")
	f.git(f.work, "branch", "--set-upstream-to=origin/feat", "main")
	f.git(f.work, "checkout", "-q", "feat")
	f.commit(f.work, "work")
	if err := f.sync("origin/main"); err != nil {
		t.Fatalf("sync to remote trunk with unrelated local main upstream: %v", err)
	}
}

func TestWorktreeSyncPushSingleBranchHintNamesFetchRefspec(t *testing.T) {
	f := newSyncPushFixture(t, true)
	f.git(f.work, "checkout", "-q", "-b", "feat")
	f.git(f.work, "push", "-q", "origin", "feat")
	f.commit(f.work, "work")
	err := f.sync("origin/main")
	if err == nil || !strings.Contains(err.Error(), "+refs/heads/feat:refs/remotes/origin/feat") || strings.Contains(err.Error(), "git push -u") {
		t.Fatalf("sync error = %v, want fetch refspec guidance", err)
	}
}

func TestWorktreeSyncPushUnpublishedBranchHintNamesFirstPush(t *testing.T) {
	f := newSyncPushFixture(t, true)
	f.git(f.work, "checkout", "-q", "-b", "feat")
	f.commit(f.work, "work")
	err := f.sync("origin/main")
	if err == nil || !strings.Contains(err.Error(), "publish this new branch") || !strings.Contains(err.Error(), "git push -u origin feat") {
		t.Fatalf("sync error = %v, want first-push guidance", err)
	}
}
