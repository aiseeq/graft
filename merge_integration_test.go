package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mergeRepo(t *testing.T, remotes ...string) *fixture {
	t.Helper()
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none, reason: 'no version'}\ngate: []\n"}, remotes...)
	f.write("base.txt", "base\n")
	f.mustPlainCommit("chore: base")
	f.git("branch", "-m", "main")
	for _, r := range remotes {
		f.git("push", "-q", r, "main")
	}
	f.git("fetch", "--quiet", remotes[0])
	f.git("branch", "--set-upstream-to", remotes[0]+"/main", "main")
	for _, r := range remotes {
		f.git("remote", "set-head", r, "main")
	}
	f.git("switch", "-q", "-c", "feature/PROJ-42")
	f.write("feature.txt", "source\n")
	f.mustPlainCommit("feat: source")
	f.git("switch", "-q", "main")
	return f
}

func TestMergeFastForwardPushesAllRemotes(t *testing.T) {
	f := mergeRepo(t, "origin", "mirror")
	out := f.mustGraft("merge", "feature/PROJ-42")
	head := f.git("rev-parse", "HEAD")
	for _, r := range []string{"origin", "mirror"} {
		if got := f.gitIn(f.remote(r), "rev-parse", "main"); got != head {
			t.Errorf("%s main = %s, want %s", r, got, head)
		}
	}
	if !strings.Contains(out, "installed main") {
		t.Errorf("missing install report:\n%s", out)
	}
}

func TestMergeRefusesDirtyAndOperation(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.write("untracked.txt", "dirty\n")
	if out, code := f.graft("", "merge", "feature/PROJ-42"); code == 0 || !strings.Contains(out, "clean") {
		t.Errorf("dirty tree: exit %d\n%s", code, out)
	}
}

func TestMergeNoDepsNeedsReason(t *testing.T) {
	f := mergeRepo(t, "origin")
	if out, code := f.graft("", "merge", "--no-deps", "feature/PROJ-42"); code == 0 || !strings.Contains(out, "--reason") {
		t.Errorf("missing reason: exit %d\n%s", code, out)
	}
}

func TestMergeModeMergeKeepsCommits(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.mustGraft("merge", "--mode", "merge", "-m", "Merge source", "feature/PROJ-42")
	parents := strings.Fields(f.git("rev-list", "--parents", "-n", "1", "HEAD"))
	if len(parents) != 3 {
		t.Fatalf("merge commit parents = %v", parents)
	}
	if parents[1] == parents[2] {
		t.Fatalf("merge parents are identical: %v", parents)
	}
}

func TestMergeCodeConflictLeavesAll(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.git("switch", "-q", "feature/PROJ-42")
	f.write("base.txt", "source edit\n")
	f.mustPlainCommit("fix: source conflict")
	source := f.git("rev-parse", "HEAD")
	f.git("switch", "-q", "main")
	f.write("base.txt", "trunk edit\n")
	f.mustPlainCommit("fix: trunk conflict")
	f.git("push", "-q", "origin", "main")
	trunk := f.git("rev-parse", "HEAD")
	if out, code := f.graft("", "merge", "feature/PROJ-42"); code == 0 || !strings.Contains(out, "base.txt") {
		t.Fatalf("conflict: exit %d\n%s", code, out)
	}
	if got := f.git("rev-parse", "HEAD"); got != trunk {
		t.Errorf("local trunk moved: %s", got)
	}
	if got := f.git("rev-parse", "refs/heads/feature/PROJ-42"); got != source {
		t.Errorf("source moved: %s", got)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != trunk {
		t.Errorf("remote trunk moved: %s", got)
	}
	if status := f.git("status", "--porcelain"); status != "" {
		t.Errorf("checkout dirtied:\n%s", status)
	}
	if got := f.git("worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
		t.Errorf("temporary worktree remains:\n%s", got)
	}
}

func TestMergeRebaseVersionConflictTakesTrunk(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file, file: VERSION}\ngate: []\n",
		"VERSION":     "1.2.3\n", "base.txt": "base\n",
	}, "origin")
	f.mustPlainCommit("chore: base")
	f.git("branch", "-m", "main")
	f.git("push", "-q", "origin", "main")
	f.git("fetch", "--quiet", "origin")
	f.git("branch", "--set-upstream-to", "origin/main", "main")
	f.git("remote", "set-head", "origin", "main")
	f.git("switch", "-q", "-c", "feature/PROJ-43")
	f.write("VERSION", "1.2.4\n")
	f.write("source.txt", "source\n")
	f.mustPlainCommit("feat: source version")
	source := f.git("rev-parse", "HEAD")
	f.git("switch", "-q", "main")
	f.write("VERSION", "1.2.5\n")
	f.mustPlainCommit("chore: trunk version")
	f.git("push", "-q", "origin", "main")
	out := f.mustGraft("merge", "feature/PROJ-43")
	if got := f.read("VERSION"); got != "1.2.6\n" {
		t.Errorf("VERSION = %q, want trunk version bumped to 1.2.6", got)
	}
	if got := f.git("rev-parse", "refs/heads/feature/PROJ-43"); got != source {
		t.Errorf("source moved: %s", got)
	}
	if !strings.Contains(out, "merge version: 1.2.6") {
		t.Errorf("version report missing:\n%s", out)
	}
}

// TestMergeRebaseNeedsNoKeyInBranchName: with a required ticket, a branch
// whose name has no key merges by rebase: its commits carry their own
// checked messages, only a merge commit would be new.
func TestMergeRebaseNeedsNoKeyInBranchName(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\ngate: []\nticket: {pattern: '[A-Z]+-[0-9]+', required: true}\n",
		"base.txt":    "base\n",
	}, "origin")
	f.mustPlainCommit("chore: base PROJ-1")
	f.git("branch", "-m", "main")
	f.git("push", "-q", "origin", "main")
	f.git("fetch", "--quiet", "origin")
	f.git("branch", "--set-upstream-to", "origin/main", "main")
	f.git("remote", "set-head", "origin", "main")
	f.git("switch", "-q", "-c", "topic")
	f.write("source.txt", "source\n")
	f.mustPlainCommit("PROJ-45 feat: source")
	f.git("switch", "-q", "main")
	f.write("trunk.txt", "trunk\n")
	f.mustPlainCommit("PROJ-46 feat: trunk")
	f.git("push", "-q", "origin", "main")
	f.mustGraft("merge", "topic")
	if got := f.gitIn(f.remote("origin"), "log", "-1", "--format=%s", "main"); got != "PROJ-45 feat: source" {
		t.Errorf("origin main tip = %q, want the rebased source commit", got)
	}
}

// TestMergeRebaseDropsVersionOnlyCommit: a source commit that changed only
// the version file is empty once the trunk's version is taken, and the
// merge drops it instead of failing.
func TestMergeRebaseDropsVersionOnlyCommit(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file, file: VERSION}\ngate: []\n",
		"VERSION":     "1.2.3\n", "base.txt": "base\n",
	}, "origin")
	f.mustPlainCommit("chore: base")
	f.git("branch", "-m", "main")
	f.git("push", "-q", "origin", "main")
	f.git("fetch", "--quiet", "origin")
	f.git("branch", "--set-upstream-to", "origin/main", "main")
	f.git("remote", "set-head", "origin", "main")
	f.git("switch", "-q", "-c", "feature/PROJ-44")
	f.write("source.txt", "source\n")
	f.mustPlainCommit("feat: source")
	f.write("VERSION", "1.2.4\n")
	f.mustPlainCommit("chore: bump")
	f.git("switch", "-q", "main")
	f.write("VERSION", "1.2.5\n")
	f.mustPlainCommit("chore: trunk version")
	f.git("push", "-q", "origin", "main")
	f.mustGraft("merge", "feature/PROJ-44")
	if got := f.read("source.txt"); got != "source\n" {
		t.Errorf("source.txt = %q, want the source's work", got)
	}
	if got := f.git("log", "--format=%s", "origin/main~2..origin/main"); got != "chore: bump version after merge\nfeat: source" {
		t.Errorf("trunk history:\n%s\nwant the bump over the source commit, the version-only one dropped", got)
	}
}

func addMergeTask(f *fixture, taskBody string) {
	f.t.Helper()
	contents := f.read(".graft.yaml") + "\ntasks:\n  check-merge:\n" + taskBody + "\nmerge:\n  deps: [check-merge]\n"
	f.write(".graft.yaml", contents)
	f.mustPlainCommit("chore: configure merge check")
	f.git("push", "-q", "origin", "main")
}

func TestMergeDepsFailNoPublish(t *testing.T) {
	f := mergeRepo(t, "origin")
	addMergeTask(f, "    run: [[sh, -c, 'exit 1']]")
	trunk := f.git("rev-parse", "HEAD")
	if out, code := f.graft("", "merge", "feature/PROJ-42"); code == 0 || !strings.Contains(out, "merge deps failed") {
		t.Fatalf("failing dependency: exit %d\n%s", code, out)
	}
	if got := f.git("rev-parse", "HEAD"); got != trunk {
		t.Errorf("local trunk moved: %s", got)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != trunk {
		t.Errorf("remote trunk moved: %s", got)
	}
}

func TestMergeDepsChangeTrackedFileRefuses(t *testing.T) {
	f := mergeRepo(t, "origin")
	addMergeTask(f, "    run: [[sh, -c, 'printf changed > base.txt']]")
	trunk := f.git("rev-parse", "HEAD")
	if out, code := f.graft("", "merge", "feature/PROJ-42"); code == 0 || !strings.Contains(out, "base.txt") {
		t.Fatalf("dependency changed tracked file: exit %d\n%s", code, out)
	}
	if got := f.git("rev-parse", "HEAD"); got != trunk {
		t.Errorf("local trunk moved: %s", got)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != trunk {
		t.Errorf("remote trunk moved: %s", got)
	}
}

func TestMergeTrunkMovedDuringChecksRefusesPush(t *testing.T) {
	f := mergeRepo(t, "origin")
	addMergeTask(f, "    run: [[git, update-ref, refs/heads/main, HEAD]]")
	remote := f.gitIn(f.remote("origin"), "rev-parse", "main")
	if out, code := f.graft("", "merge", "feature/PROJ-42"); code == 0 || !strings.Contains(out, "trunk moved") {
		t.Fatalf("moved trunk: exit %d\n%s", code, out)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != remote {
		t.Errorf("remote trunk moved: %s", got)
	}
}

func TestMergeRemoteAdvancedRefusesWithoutPublishingElsewhere(t *testing.T) {
	f := mergeRepo(t, "origin", "mirror")
	addMergeTask(f, `    run: [[sh, -c, 'rival=$(git commit-tree "$(git rev-parse HEAD^{tree})" -p "$(git rev-parse HEAD)" -m rival) && git push -q origin "$rival:refs/heads/main"']]`)
	old := f.git("rev-parse", "HEAD")
	mirror := f.gitIn(f.remote("mirror"), "rev-parse", "main")
	if out, code := f.graft("", "merge", "feature/PROJ-42"); code == 0 || !strings.Contains(out, "git fetch, then graft merge again") {
		t.Fatalf("advanced remote: exit %d\n%s", code, out)
	}
	if got := f.git("rev-parse", "HEAD"); got != old {
		t.Errorf("local trunk moved: %s", got)
	}
	if got := f.gitIn(f.remote("mirror"), "rev-parse", "main"); got != mirror {
		t.Errorf("mirror was published after primary refusal: %s", got)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got == old {
		t.Errorf("rival update disappeared: %s", got)
	}
}

func TestMergeMirrorRejectsReports(t *testing.T) {
	f := mergeRepo(t, "origin", "mirror")
	addMergeTask(f, `    run: [[sh, -c, 'rival=$(git commit-tree "$(git rev-parse HEAD^{tree})" -p "$(git rev-parse HEAD)" -m rival) && git push -q mirror "$rival:refs/heads/main"']]`)
	if out, code := f.graft("", "merge", "feature/PROJ-42"); code == 0 || !strings.Contains(out, "push to mirror FAILED") {
		t.Fatalf("mirror refusal: exit %d\n%s", code, out)
	}
	local := f.git("rev-parse", "HEAD")
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != local {
		t.Errorf("primary trunk = %s, local = %s", got, local)
	}
	if got := f.gitIn(f.remote("mirror"), "rev-parse", "main"); got == local {
		t.Errorf("mirror was overwritten after advancing: %s", got)
	}
}

func TestMergeAlreadyMerged(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.mustGraft("merge", "feature/PROJ-42")
	out := f.mustGraft("merge", "feature/PROJ-42")
	if !strings.Contains(out, "already merged") {
		t.Errorf("missing already-merged report:\n%s", out)
	}
}

func TestMergeAlreadyMergedByContent(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.git("switch", "-q", "feature/PROJ-42")
	source := f.git("rev-parse", "HEAD")
	f.git("switch", "-q", "main")
	f.write("feature.txt", "source\n")
	f.mustPlainCommit("feat: same content")
	f.git("push", "-q", "origin", "main")
	head := f.git("rev-parse", "HEAD")
	out := f.mustGraft("merge", "feature/PROJ-42")
	if !strings.Contains(out, "already merged by content") {
		t.Errorf("missing content report:\n%s", out)
	}
	if got := f.git("rev-parse", "HEAD"); got != head {
		t.Errorf("trunk moved: %s", got)
	}
	if got := f.git("rev-parse", "refs/heads/feature/PROJ-42"); got != source {
		t.Errorf("source moved: %s", got)
	}
}

func TestMergeRefusesNonBranchSource(t *testing.T) {
	f := mergeRepo(t, "origin")
	sha := f.git("rev-parse", "refs/heads/feature/PROJ-42")
	if out, code := f.graft("", "merge", sha); code == 0 || !strings.Contains(out, "not a branch") {
		t.Errorf("commit source: exit %d\n%s", code, out)
	}
}

func TestMergeRefusesOperation(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.git("switch", "-q", "feature/PROJ-42")
	f.write("base.txt", "source conflict\n")
	f.mustPlainCommit("fix: source")
	f.git("switch", "-q", "main")
	f.write("base.txt", "trunk conflict\n")
	f.mustPlainCommit("fix: trunk")
	if _, code := f.tryGit("merge", "--no-commit", "feature/PROJ-42"); code == 0 {
		t.Fatal("merge unexpectedly had no conflict")
	}
	if out, code := f.graft("", "merge", "feature/PROJ-42"); code == 0 || !strings.Contains(out, "in progress") {
		t.Errorf("operation: exit %d\n%s", code, out)
	}
}

func TestMergeSourceMovedAfterSnapshot(t *testing.T) {
	f := mergeRepo(t, "origin")
	addMergeTask(f, `    run: [[sh, -c, 'source=$(git rev-parse refs/heads/feature/PROJ-42) && tree=$(git rev-parse "$source^{tree}") && next=$(git commit-tree "$tree" -p "$source" -m moved) && git update-ref refs/heads/feature/PROJ-42 "$next"']]`)
	snapshot := f.git("rev-parse", "refs/heads/feature/PROJ-42")
	f.mustGraft("merge", "feature/PROJ-42")
	if got := f.git("rev-parse", "refs/heads/feature/PROJ-42"); got == snapshot {
		t.Fatal("source ref did not move during the check")
	}
	if got := f.read("feature.txt"); got != "source\n" {
		t.Errorf("installed tree did not contain snapshot source content: %q", got)
	}
}

func TestMergeVersionSyncConflictRefuses(t *testing.T) {
	f := fileModeRepo(t, "", "origin")
	f.mustPlainCommit("chore: base")
	f.git("branch", "-m", "main")
	f.git("push", "-q", "origin", "main")
	f.git("fetch", "--quiet", "origin")
	f.git("branch", "--set-upstream-to", "origin/main", "main")
	f.git("remote", "set-head", "origin", "main")
	f.git("switch", "-q", "-c", "feature/PROJ-44")
	f.write("app.yaml", "name: app\nversion: \"1.2.4\"\n")
	f.mustPlainCommit("fix: source sync")
	f.git("switch", "-q", "main")
	f.write("app.yaml", "name: app\nversion: \"1.2.5\"\n")
	f.mustPlainCommit("fix: trunk sync")
	f.git("push", "-q", "origin", "main")
	trunk := f.git("rev-parse", "HEAD")
	if out, code := f.graft("", "merge", "feature/PROJ-44"); code == 0 || !strings.Contains(out, "app.yaml") {
		t.Fatalf("version sync conflict: exit %d\n%s", code, out)
	}
	if got := f.git("rev-parse", "HEAD"); got != trunk {
		t.Errorf("trunk moved: %s", got)
	}
}

func TestMergeUsesResultConfig(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.git("switch", "-q", "feature/PROJ-42")
	f.write(".graft.yaml", "schema: 1\nversion: {mode: none, reason: test}\ngate:\n  - sh: 'echo RESULT-GATE; exit 1'\ntasks:\n  fail: {run: [{sh: 'echo RESULT-DEP; exit 1'}]}\nmerge: {deps: [fail]}\n")
	f.mustPlainCommit("chore: result config")
	f.git("switch", "-q", "main")
	remote := f.gitIn(f.remote("origin"), "rev-parse", "main")
	if out, code := f.graft("", "merge", "feature/PROJ-42"); code == 0 || !strings.Contains(out, "RESULT-GATE") {
		t.Fatalf("result gate not used: exit %d\n%s", code, out)
	}
	if strings.Contains(f.gitIn(f.remote("origin"), "show", "main:.graft.yaml"), "RESULT-GATE") {
		t.Fatal("source config was published")
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != remote {
		t.Errorf("remote trunk moved: %s", got)
	}
}

func TestMergeGateChangeRefusesAtGateBoundary(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.git("switch", "-q", "feature/PROJ-42")
	f.write(".graft.yaml", "schema: 1\nversion: {mode: none, reason: test}\ngate:\n  - sh: 'printf changed > base.txt'\n")
	f.mustPlainCommit("chore: change result gate")
	f.git("switch", "-q", "main")
	remote := f.gitIn(f.remote("origin"), "rev-parse", "main")
	if out, code := f.graft("", "merge", "feature/PROJ-42"); code == 0 || !strings.Contains(out, "merge gate changed") || !strings.Contains(out, "base.txt") {
		t.Fatalf("gate mutation: exit %d\n%s", code, out)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != remote {
		t.Errorf("gate mutation was published: %s", got)
	}
}

func TestMergeUsesOnlyPushRemotes(t *testing.T) {
	f := mergeRepo(t, "origin", "upstream")
	f.write(".graft.yaml", strings.TrimSuffix(f.read(".graft.yaml"), "\n")+"\npush: {remotes: [origin]}\n")
	f.mustPlainCommit("chore: select push remote")
	f.git("push", "-q", "origin", "main")
	upstream := f.gitIn(f.remote("upstream"), "rev-parse", "main")
	out := f.mustGraft("merge", "feature/PROJ-42")
	if !strings.Contains(out, "pushed to origin") {
		t.Errorf("origin push missing:\n%s", out)
	}
	if got := f.gitIn(f.remote("upstream"), "rev-parse", "main"); got != upstream {
		t.Errorf("unconfigured push remote was updated: %s", got)
	}
}

func TestMergeIgnoresUnavailableBootstrapRemoteOutsideTrunkPushSet(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.write(".graft.yaml", "schema: 1\nversion: {mode: none, reason: 'no version'}\ngate: []\npush: {remotes: [origin, offline]}\n")
	f.mustPlainCommit("chore: configure push remotes")
	f.git("push", "-q", "origin", "main")
	f.git("remote", "add", "offline", filepath.Join(f.base, "missing.git"))

	remoteWork := filepath.Join(f.base, "remote-work")
	f.run(f.base, "git", "clone", "--branch", "main", f.remote("origin"), remoteWork)
	f.gitIn(remoteWork, "config", "user.name", "Merge Test")
	f.gitIn(remoteWork, "config", "user.email", "merge@example.invalid")
	if err := os.WriteFile(filepath.Join(remoteWork, ".graft.yaml"), []byte("schema: 1\nversion: {mode: none, reason: 'no version'}\ngate: []\npush: {remotes: [origin]}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.gitIn(remoteWork, "add", ".graft.yaml")
	f.gitIn(remoteWork, "commit", "-m", "chore: narrow push remotes")
	f.gitIn(remoteWork, "push", "origin", "main")

	out := f.mustGraft("merge", "feature/PROJ-42")
	if !strings.Contains(out, "initial fetch offline failed") || !strings.Contains(out, "installed main") {
		t.Errorf("unavailable bootstrap remote was not warned about or merge did not finish:\n%s", out)
	}
}

func TestMergeSourceLocalWithUpstream(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.git("switch", "-q", "feature/PROJ-42")
	f.git("push", "-q", "-u", "origin", "feature/PROJ-42")
	f.write("local-only.txt", "local source commit\n")
	f.mustPlainCommit("feat: local source commit")
	f.git("switch", "-q", "main")
	f.mustGraft("merge", "feature/PROJ-42")
	if got := f.read("local-only.txt"); got != "local source commit\n" {
		t.Errorf("short branch name did not resolve to local source: %q", got)
	}
}

func TestMergeRefusesLocalTrunkAheadOfRemote(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.write("local-trunk.txt", "unpublished\n")
	f.mustPlainCommit("chore: unpublished trunk")
	local := f.git("rev-parse", "HEAD")
	if out, code := f.graft("", "merge", "feature/PROJ-42"); code == 0 || !strings.Contains(out, "local trunk has commits absent") {
		t.Fatalf("unpublished trunk: exit %d\n%s", code, out)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got == local {
		t.Fatal("local-only trunk commit was published")
	}
}

func TestMergeRefusesRemoteTrunkAsSource(t *testing.T) {
	f := mergeRepo(t, "origin")
	if out, code := f.graft("", "merge", "origin/main"); code == 0 || !strings.Contains(out, "source is the trunk") {
		t.Errorf("remote trunk source: exit %d\n%s", code, out)
	}
}

func TestMergeRefusesCheckoutDirtiedByDeps(t *testing.T) {
	f := mergeRepo(t, "origin")
	path := filepath.ToSlash(filepath.Join(f.work, "base.txt"))
	addMergeTask(f, "    run: [[sh, -c, 'printf changed > "+path+"']]")
	remote := f.gitIn(f.remote("origin"), "rev-parse", "main")
	if out, code := f.graft("", "merge", "feature/PROJ-42"); code == 0 || !strings.Contains(out, "local trunk became dirty") {
		t.Fatalf("checkout dirtied by deps: exit %d\n%s", code, out)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != remote {
		t.Errorf("dirty checkout was published: %s", got)
	}
}

func TestMergeRequiredTicketForMergeAndBump(t *testing.T) {
	for _, branch := range []string{"PROJ-8-x", "feature/PROJ-9"} {
		t.Run(branch, func(t *testing.T) {
			f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: file, file: VERSION}\nticket: {pattern: 'PROJ-[0-9]+', required: true}\ngate: []\n", "VERSION": "1.0.0\n", "base.txt": "base\n"}, "origin")
			f.mustPlainCommit("chore: base PROJ-1")
			f.git("branch", "-m", "main")
			f.git("push", "-q", "origin", "main")
			f.git("fetch", "--quiet", "origin")
			f.git("branch", "--set-upstream-to", "origin/main", "main")
			f.git("remote", "set-head", "origin", "main")
			f.git("switch", "-q", "-c", branch)
			f.write("source.txt", "source\n")
			f.mustPlainCommit("feat: source PROJ-4")
			f.git("push", "-q", "-u", "origin", branch)
			f.git("switch", "-q", "main")
			f.write("trunk.txt", "trunk\n")
			f.mustPlainCommit("feat: trunk PROJ-5")
			f.git("push", "-q", "origin", "main")
			f.mustGraft("merge", "--mode", "merge", "origin/"+branch)
			key := "PROJ-8"
			if strings.Contains(branch, "PROJ-9") {
				key = "PROJ-9"
			}
			for _, rev := range []string{"HEAD", "HEAD^"} {
				mergeMsg := f.git("log", "-1", "--format=%B", rev)
				if !strings.Contains(mergeMsg, key) {
					t.Errorf("%s message does not name %s:\n%s", rev, key, mergeMsg)
				}
			}
		})
	}
}

func TestMergeWorktreeLinkPreparesGate(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none, reason: test}\ngate:\n  - sh: 'test -f deps/installed || { echo missing-linked-deps; exit 1; }'\ntasks:\n  install-deps: {run: [{sh: 'mkdir -p deps && touch deps/installed'}]}\nworktree:\n  link:\n    - key: [deps.lock]\n      paths: [deps]\n      setup: install-deps\n",
		".gitignore":  "deps/\n", "deps.lock": "v1\n", "base.txt": "base\n",
	}, "origin")
	f.mustPlainCommit("chore: base")
	f.git("branch", "-m", "main")
	f.git("push", "-q", "origin", "main")
	f.git("fetch", "--quiet", "origin")
	f.git("branch", "--set-upstream-to", "origin/main", "main")
	f.git("remote", "set-head", "origin", "main")
	f.mustGraft("install-deps")
	f.git("switch", "-q", "-c", "feat")
	f.write("feature.txt", "source\n")
	f.mustPlainCommit("feat: source")
	f.git("switch", "-q", "main")
	out := f.mustGraft("merge", "feat")
	if !strings.Contains(out, "linked deps") {
		t.Errorf("worktree link missing:\n%s", out)
	}
	if got := f.git("worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
		t.Errorf("temporary worktree/link remains:\n%s", got)
	}
}

func TestMergePostMergeHookRunsGateOnce(t *testing.T) {
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none, reason: test}\ngate: []\n"}, "origin")
	logPath := filepath.Join(f.base, "gate.log")
	f.write(".graft.yaml", "schema: 1\nversion: {mode: none, reason: test}\ngate:\n  - ["+yamlQuote(helperBin)+", append, "+yamlQuote(logPath)+", gate]\n")
	f.mustPlainCommit("chore: base")
	f.git("branch", "-m", "main")
	f.git("push", "-q", "origin", "main")
	f.git("fetch", "--quiet", "origin")
	f.git("branch", "--set-upstream-to", "origin/main", "main")
	f.git("remote", "set-head", "origin", "main")
	f.mustGraft("init")
	f.git("switch", "-q", "-c", "feat")
	f.write("feature.txt", "source\n")
	f.mustPlainCommit("feat: source")
	f.git("push", "-q", "origin", "feat")
	f.git("switch", "-q", "main")
	f.write("trunk.txt", "trunk\n")
	f.mustPlainCommit("feat: trunk")
	f.git("push", "-q", "origin", "main")
	f.mustGraft("merge", "--mode", "merge", "origin/feat")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "gate\n"); got != 1 {
		t.Errorf("gate ran %d times; log:\n%s", got, data)
	}
}

func TestMergeRebaseSkipsOriginallyEmptyCommit(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.git("switch", "-q", "feature/PROJ-42")
	f.run(f.work, "git", "-c", "core.hooksPath=/dev/null", "commit", "--allow-empty", "-q", "-m", "chore: empty marker")
	f.write("after-empty.txt", "source\n")
	f.mustPlainCommit("feat: after empty")
	f.git("switch", "-q", "main")
	f.write("trunk.txt", "trunk\n")
	f.mustPlainCommit("feat: trunk")
	f.git("push", "-q", "origin", "main")
	out := f.mustGraft("merge", "feature/PROJ-42")
	if !strings.Contains(out, "skipped empty commit") {
		t.Errorf("empty source commit was not reported:\n%s", out)
	}
}

func TestMergeVersionConflictKeepsGreaterSourceVersion(t *testing.T) {
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: file, file: VERSION}\ngate: []\n", "VERSION": "1.0.0\n", "base.txt": "base\n"}, "origin")
	f.mustPlainCommit("chore: base")
	f.git("branch", "-m", "main")
	f.git("push", "-q", "origin", "main")
	f.git("fetch", "--quiet", "origin")
	f.git("branch", "--set-upstream-to", "origin/main", "main")
	f.git("remote", "set-head", "origin", "main")
	f.git("switch", "-q", "-c", "major")
	f.write("VERSION", "2.0.0\n")
	f.write("api.txt", "new api\n")
	f.mustPlainCommit("feat!: major version")
	f.git("switch", "-q", "main")
	f.write("VERSION", "1.0.2\n")
	f.write("trunk.txt", "trunk\n")
	f.mustPlainCommit("feat: trunk version")
	f.git("push", "-q", "origin", "main")
	out := f.mustGraft("merge", "major")
	if got := f.read("VERSION"); got != "2.0.0\n" {
		t.Errorf("VERSION = %q, want larger source version", got)
	}
	if !strings.Contains(out, "merge version: 2.0.0 (source 2.0.0, trunk 1.0.2)") {
		t.Errorf("version intent missing:\n%s", out)
	}
}
