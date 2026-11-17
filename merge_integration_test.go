package main

import (
	"os"
	"os/exec"
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
	if !strings.Contains(out, "local main moved to") {
		t.Errorf("missing install report:\n%s", out)
	}
}

func TestMergeAllowsSourceTrackingTheTrunk(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.git("branch", "--set-upstream-to=origin/main", "feature/PROJ-42")
	f.git("switch", "-q", "main")
	f.write("trunk.txt", "trunk\n")
	f.mustPlainCommit("feat: trunk moved")
	f.git("push", "-q", "origin", "main")

	out, code := f.graft("", "merge", "feature/PROJ-42")
	if code != 0 {
		t.Fatalf("merge with trunk upstream: exit %d\n%s", code, out)
	}
	if got := f.gitIn(f.remote("origin"), "show", "main:feature.txt"); got != "source" {
		t.Errorf("merged source content = %q", got)
	}
	if got := f.gitIn(f.remote("origin"), "show", "main:trunk.txt"); got != "trunk" {
		t.Errorf("trunk content = %q", got)
	}
}

func TestMergeStillRejectsSourceDivergedFromItsOwnUpstream(t *testing.T) {
	f := mergeRepo(t, "origin")
	localTip := f.git("rev-parse", "feature/PROJ-42")
	f.git("switch", "-q", "-c", "remote-feature", "main")
	f.write("remote.txt", "remote\n")
	f.mustPlainCommit("feat: remote feature update")
	f.git("push", "-q", "origin", "HEAD:refs/heads/feature/PROJ-42")
	f.git("switch", "-q", "main")
	f.git("fetch", "--quiet", "origin")
	f.git("branch", "--set-upstream-to=origin/feature/PROJ-42", "feature/PROJ-42")

	out, code := f.graft("", "merge", "feature/PROJ-42")
	if code == 0 || !strings.Contains(out, "behind or diverged from refs/remotes/origin/feature/PROJ-42") {
		t.Fatalf("merge behind own upstream: exit %d\n%s", code, out)
	}
	if got := f.git("rev-parse", "feature/PROJ-42"); got != localTip {
		t.Errorf("local source moved: %s -> %s", localTip, got)
	}
}

func TestMergeLinksGoWorkNeighbour(t *testing.T) {
	skipGoToolchainInShort(t)
	marker := filepath.Join(t.TempDir(), "merge-tree")
	f := mergeRepo(t, "origin")
	kit := filepath.Join(filepath.Dir(f.work), "kit")
	f.git("switch", "-q", "feature/PROJ-42")
	writeMergeNeighbour(t, kit)
	f.write("go.mod", "module example.com/project\n\ngo 1.23\n")
	f.write("go.work", "go 1.23\n\nuse (\n .\n ../kit\n)\n")
	f.write("main.go", "package project\n\nimport _ \"example.com/kit\"\n")
	f.write(".graft.yaml", neighbourGate(marker, false))
	f.write("feature.txt", "source with workspace neighbour\n")
	f.mustPlainCommit("feat: use neighbouring module")
	f.git("switch", "-q", "main")

	out, code := f.graft("", "merge", "feature/PROJ-42")
	if code != 0 {
		t.Fatalf("merge failed: exit %d\n%s", code, out)
	}
	treeBytes, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("gate did not record its temporary tree: %v", err)
	}
	tree := strings.TrimSpace(string(treeBytes))
	tmp := mergeTempAncestor(tree)
	if tmp == "" {
		t.Fatalf("gate tree %s is not under a graft-merge temporary directory", tree)
	}
	if _, err := os.Lstat(tmp); !os.IsNotExist(err) {
		t.Errorf("merge temporary directory remains at %s: %v", tmp, err)
	}
	if rel, err := filepath.Rel(tmp, tree); err != nil || len(strings.Split(rel, string(filepath.Separator))) > 2 {
		t.Errorf("merge tree path is not short for ../kit: relative path %q, error %v", rel, err)
	}
	assertKitIntact(t, kit)
}

func TestMergeNestedNeighbourDoesNotWriteThroughParent(t *testing.T) {
	skipGoToolchainInShort(t)
	f, neighbor, _, scratch := mergeNeighbourFixturePaths(t, []string{"../kit", "../kit/new-child"}, false)
	out, code := f.graft("", "merge", "feature/PROJ-42")
	if code != 0 && !strings.Contains(out, "listing the modules of go.work") {
		t.Fatalf("unexpected merge failure: exit %d\n%s", code, out)
	}
	if entries, err := os.ReadDir(scratch); err != nil || len(entries) != 0 {
		t.Errorf("merge scratch remains: entries=%v err=%v", entries, err)
	}
	if got := strings.Count(f.git("worktree", "list", "--porcelain"), "worktree "); got != 1 {
		t.Errorf("failed merge left a registered worktree:\n%s", f.git("worktree", "list", "--porcelain"))
	}
	assertKitIntact(t, neighbor)
	if _, err := os.Lstat(filepath.Join(neighbor, "new-child")); !os.IsNotExist(err) {
		t.Errorf("nested neighbour was created through the parent link: %v", err)
	}
}

func TestMergeDeepGoWorkNeighbourCleanup(t *testing.T) {
	skipGoToolchainInShort(t)
	for _, failGate := range []bool{false, true} {
		name := "success"
		if failGate {
			name = "gate failure"
		}
		t.Run(name, func(t *testing.T) {
			f, kit, marker, scratch := mergeNeighbourFixture(t, "../../kit", failGate)
			out, code := f.graft("", "merge", "feature/PROJ-42")
			if failGate {
				if code == 0 || !strings.Contains(out, "gate failed") {
					t.Fatalf("merge should fail its gate: exit %d\n%s", code, out)
				}
			} else if code != 0 {
				t.Fatalf("merge failed: exit %d\n%s", code, out)
			}
			assertNoMergeScratch(t, marker, scratch)
			assertKitIntact(t, kit)
		})
	}
}

func TestMergeVeryDeepNeighbourLinkStaysTemporary(t *testing.T) {
	f, _, _, scratch := mergeNeighbourFixture(t, "../../../../../../../../escape-kit", false)
	out, code := f.graft("", "merge", "feature/PROJ-42")
	if code == 0 || !strings.Contains(out, "listing the modules of go.work") {
		t.Fatalf("merge should reach the gate with a bounded link: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "linked "+filepath.Join(scratch, "graft-merge-")) {
		t.Errorf("deep neighbour link was not placed under merge scratch:\n%s", out)
	}
	entries, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("merge scratch was not cleaned after refusing the link: %v", entries)
	}
	if got := strings.Count(f.git("worktree", "list", "--porcelain"), "worktree "); got != 1 {
		t.Errorf("failed merge left a registered worktree:\n%s", f.git("worktree", "list", "--porcelain"))
	}
}

func TestMergeFromLinkedWorktreeUsesMainNeighbour(t *testing.T) {
	skipGoToolchainInShort(t)
	f, kit, marker, scratch := mergeNeighbourFixture(t, "../kit", false)
	linked := filepath.Join(f.base, "other", "linked")
	if err := os.MkdirAll(filepath.Dir(linked), 0o755); err != nil {
		t.Fatal(err)
	}
	f.git("worktree", "add", "--detach", linked, "main")
	f.git("switch", "--detach", "main")
	if _, code := f.exec(linked, "", "git", "switch", "main"); code != 0 {
		t.Fatal("could not put trunk in linked worktree")
	}
	out, code := f.exec(linked, "", graftBin, "merge", "feature/PROJ-42")
	if code != 0 {
		t.Fatalf("merge from linked worktree failed: exit %d\n%s", code, out)
	}
	assertNoMergeScratch(t, marker, scratch)
	assertKitIntact(t, kit)
}

func mergeNeighbourFixture(t *testing.T, usePath string, failGate bool) (*fixture, string, string, string) {
	return mergeNeighbourFixturePaths(t, []string{usePath}, failGate)
}

func mergeNeighbourFixturePaths(t *testing.T, usePaths []string, failGate bool) (*fixture, string, string, string) {
	t.Helper()
	f := mergeRepo(t, "origin")
	marker := filepath.Join(f.base, "merge-tree")
	scratch := filepath.Join(f.base, "merge-scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	f.env = append(f.env, "TMPDIR="+scratch)
	kit := filepath.Clean(filepath.Join(f.work, usePaths[0]))
	if !strings.Contains(strings.Join(usePaths, " "), "escape-kit") {
		writeMergeNeighbour(t, kit)
	}
	f.git("switch", "-q", "feature/PROJ-42")
	f.write("go.mod", "module example.com/project\n\ngo 1.23\n")
	var work strings.Builder
	work.WriteString("go 1.23\n\nuse (\n .\n")
	for _, usePath := range usePaths {
		work.WriteString(" " + usePath + "\n")
	}
	work.WriteString(")\n")
	f.write("go.work", work.String())
	f.write("main.go", "package project\n\nimport _ \"example.com/kit\"\n")
	f.write(".graft.yaml", neighbourGate(marker, failGate))
	f.mustPlainCommit("feat: use neighbouring module")
	f.git("switch", "-q", "main")
	return f, kit, marker, scratch
}

func writeMergeNeighbour(t *testing.T, kit string) {
	t.Helper()
	if err := os.MkdirAll(kit, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kit, "go.mod"), []byte("module example.com/kit\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kit, "kit.go"), []byte("package kit\n\nconst Value = \"present\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func neighbourGate(marker string, fail bool) string {
	gate := "  - [go, list, -m, all]\n  - [sh, -c, 'pwd > " + marker + "']\n"
	if fail {
		gate += "  - [sh, -c, 'exit 1']\n"
	}
	return "schema: 1\nversion: {mode: none, reason: 'no version'}\ngate:\n" + gate
}

func mergeTempAncestor(tree string) string {
	for p := filepath.Dir(tree); p != filepath.Dir(p); p = filepath.Dir(p) {
		if strings.HasPrefix(filepath.Base(p), "graft-merge-") {
			return p
		}
	}
	return ""
}

func assertNoMergeScratch(t *testing.T, marker, scratch string) {
	t.Helper()
	treeBytes, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("gate did not record its temporary tree: %v", err)
	}
	tree := strings.TrimSpace(string(treeBytes))
	tmp := mergeTempAncestor(tree)
	if tmp == "" {
		t.Fatalf("gate tree %s is not under a graft-merge temporary directory", tree)
	}
	if _, err := os.Lstat(tmp); !os.IsNotExist(err) {
		t.Errorf("merge temporary directory remains at %s: %v", tmp, err)
	}
	if entries, err := os.ReadDir(scratch); err != nil || len(entries) != 0 {
		t.Errorf("merge scratch remains: entries=%v err=%v", entries, err)
	}
}

func assertKitIntact(t *testing.T, kit string) {
	t.Helper()
	for name, want := range map[string]string{
		"go.mod": "module example.com/kit\n\ngo 1.23\n",
		"kit.go": "package kit\n\nconst Value = \"present\"\n",
	} {
		if got, err := os.ReadFile(filepath.Join(kit, name)); err != nil || string(got) != want {
			t.Errorf("neighbour %s was changed or removed: %q, %v", name, got, err)
		}
	}
}

func TestMergeRefusesDirtyAndOperation(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.git("switch", "-q", "feature/PROJ-42")
	f.write("untracked.txt", "source\n")
	f.mustPlainCommit("feat: add untracked collision")
	f.git("switch", "-q", "main")
	f.write("untracked.txt", "dirty\n")
	if out, code := f.graft("", "merge", "feature/PROJ-42"); code == 0 || !strings.Contains(out, "untracked.txt") {
		t.Errorf("dirty tree: exit %d\n%s", code, out)
	}
}

func TestMergeKeepsDisjointDirtyFiles(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.write("docs/tasks/X.md", "base\n")
	f.mustPlainCommit("docs: add task journal")
	f.git("push", "-q", "origin", "main")
	f.git("fetch", "--quiet", "origin")
	f.git("branch", "--set-upstream-to", "origin/main", "main")
	f.git("switch", "-q", "feature/PROJ-42")
	f.git("merge", "-q", "main")
	f.git("switch", "-q", "main")
	f.write("docs/tasks/X.md", "local unstaged\n")
	f.git("add", "docs/tasks/X.md")
	f.write("docs/tasks/X.md", "local staged and unstaged\n")
	f.write("loose.txt", "untracked\n")

	if out, code := f.graft("", "merge", "feature/PROJ-42"); code != 0 {
		t.Fatalf("disjoint dirty paths blocked merge: exit %d\n%s", code, out)
	}
	if got := f.read("docs/tasks/X.md"); got != "local staged and unstaged\n" {
		t.Errorf("dirty journal = %q", got)
	}
	if got := f.read("loose.txt"); got != "untracked\n" {
		t.Errorf("untracked file = %q", got)
	}
	if got := f.git("diff", "--cached", "--", "docs/tasks/X.md"); !strings.Contains(got, "+local unstaged") {
		t.Errorf("staged journal edit was lost: %s", got)
	}
	if got := f.git("diff", "--", "docs/tasks/X.md"); !strings.Contains(got, "+local staged and unstaged") {
		t.Errorf("unstaged journal edit was lost: %s", got)
	}
}

func addMergeGateMarker(t *testing.T, f *fixture, marker string) {
	t.Helper()
	config := strings.Replace(f.read(".graft.yaml"), "gate: []", "gate:\n  - [sh, -c, 'touch "+marker+"']", 1)
	if config == f.read(".graft.yaml") {
		t.Fatal("fixture has no empty gate to replace")
	}
	f.write(".graft.yaml", config)
	f.mustPlainCommit("chore: add merge gate marker")
	f.git("push", "-q", "origin", "main")
	f.git("fetch", "--quiet", "origin")
}

func TestMergeIgnoredUntrackedCollisionRefusesBeforeGate(t *testing.T) {
	f := mergeRepo(t, "origin")
	marker := filepath.Join(f.base, "gate-ran")
	addMergeGateMarker(t, f, marker)
	excludes := filepath.Join(f.base, "global-excludes")
	if err := os.WriteFile(excludes, []byte("feature.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git("config", "core.excludesFile", excludes)
	f.write("feature.txt", "private ignored content\n")
	old := f.git("rev-parse", "HEAD")
	out, code := f.graft("", "merge", "feature/PROJ-42")
	if code == 0 || !strings.Contains(out, "feature.txt") {
		t.Fatalf("ignored collision was not refused: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("gate ran before ignored-path refusal: err=%v", err)
	}
	if got := f.read("feature.txt"); got != "private ignored content\n" {
		t.Errorf("ignored content was lost: %q", got)
	}
	if got := f.git("rev-parse", "HEAD"); got != old {
		t.Errorf("local trunk moved: %s -> %s", old, got)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != old {
		t.Errorf("origin moved: %s -> %s", old, got)
	}
}

func TestMergeSubmoduleIgnoreAllStillRefusesDirtyGitlink(t *testing.T) {
	f, _ := mergeRepoWithSubmodule(t)
	marker := filepath.Join(f.base, "gate-ran")
	f.git("switch", "-q", "feature/submodule")
	if err := os.WriteFile(filepath.Join(f.work, "dep", "data"), []byte("source second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.git("-C", filepath.Join(f.work, "dep"), "add", "data")
	f.git("-C", filepath.Join(f.work, "dep"), "commit", "-qm", "feat: second submodule change")
	f.git("add", "dep")
	f.mustPlainCommit("feat: second gitlink update")
	contents := strings.Replace(f.read(".graft.yaml"), "gate: []", "gate:\n  - [sh, -c, 'touch "+marker+"']", 1)
	f.write(".graft.yaml", contents)
	f.mustPlainCommit("chore: add source gate marker")
	f.git("switch", "-q", "main")
	f.git("config", "submodule.dep.ignore", "all")
	if err := os.WriteFile(filepath.Join(f.work, "dep", "data"), []byte("private submodule data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if status := f.git("status", "--porcelain", "--ignore-submodules=none"); !strings.Contains(status, "dep") {
		t.Fatalf("fixture does not expose dirty gitlink with ignore=none: %q", status)
	}
	old := f.git("rev-parse", "HEAD")
	changed := f.git("diff-tree", "--ignore-submodules=none", "--no-commit-id", "--name-only", "-r", old, "feature/submodule")
	if !strings.Contains(changed, "dep") {
		t.Fatalf("fixture result does not change gitlink: paths=%q main=%s source=%s", changed, f.git("rev-parse", "HEAD:dep"), f.git("rev-parse", "feature/submodule:dep"))
	}
	out, code := f.graft("", "merge", "feature/submodule")
	if code == 0 || !strings.Contains(out, "dep") {
		t.Fatalf("dirty gitlink hidden by ignore=all: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("gate ran before dirty-submodule refusal: err=%v", err)
	}
	if got, err := os.ReadFile(filepath.Join(f.work, "dep", "data")); err != nil || string(got) != "private submodule data\n" {
		t.Errorf("submodule dirt was lost: %q, %v", got, err)
	}
	if got := f.git("rev-parse", "HEAD"); got != old {
		t.Errorf("local trunk moved: %s -> %s", old, got)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != old {
		t.Errorf("origin moved: %s -> %s", old, got)
	}
}

func TestMergeRefusesDirtyPathChangedByResult(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.git("switch", "-q", "feature/PROJ-42")
	f.write("docs/tasks/X.md", "branch\n")
	f.mustPlainCommit("docs: add journal")
	f.git("switch", "-q", "main")
	f.write("docs/tasks/X.md", "local\n")
	if out, code := f.graft("", "merge", "feature/PROJ-42"); code == 0 || !strings.Contains(out, "docs/tasks/X.md") {
		t.Fatalf("overlapping dirty path was not named: exit %d\n%s", code, out)
	}
}

func TestMergeSkipWorktreeOverlapRefusesBeforePush(t *testing.T) {
	f := mergeRepo(t, "origin")
	marker := filepath.Join(f.base, "gate-ran")
	addMergeGateMarker(t, f, marker)
	f.git("switch", "-q", "feature/PROJ-42")
	f.write("base.txt", "source\n")
	f.mustPlainCommit("feat: change base")
	f.git("switch", "-q", "main")
	old := f.git("rev-parse", "HEAD")
	f.write("base.txt", "local hidden\n")
	f.git("update-index", "--skip-worktree", "base.txt")
	out, code := f.graft("", "merge", "feature/PROJ-42")
	if code == 0 || !strings.Contains(out, "base.txt") {
		t.Fatalf("skip-worktree overlap was not refused: exit %d\n%s", code, out)
	}
	if got := f.git("rev-parse", "HEAD"); got != old {
		t.Errorf("local trunk moved: %s -> %s", old, got)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != old {
		t.Errorf("origin moved: %s -> %s", old, got)
	}
	if got := f.read("base.txt"); got != "local hidden\n" {
		t.Errorf("hidden worktree edit = %q", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("gate ran before skip-worktree refusal: err=%v", err)
	}
}

func TestMergeAssumeUnchangedOverlapRefusesBeforePush(t *testing.T) {
	f := mergeRepo(t, "origin")
	marker := filepath.Join(f.base, "gate-ran")
	addMergeGateMarker(t, f, marker)
	f.git("switch", "-q", "feature/PROJ-42")
	f.write("base.txt", "source\n")
	f.mustPlainCommit("feat: change base")
	f.git("switch", "-q", "main")
	old := f.git("rev-parse", "HEAD")
	f.write("base.txt", "local hidden\n")
	f.git("update-index", "--assume-unchanged", "base.txt")
	out, code := f.graft("", "merge", "feature/PROJ-42")
	if code == 0 || !strings.Contains(out, "base.txt") {
		t.Fatalf("assume-unchanged overlap was not refused: exit %d\n%s", code, out)
	}
	if got := f.git("rev-parse", "HEAD"); got != old {
		t.Errorf("local trunk moved: %s -> %s", old, got)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != old {
		t.Errorf("origin moved: %s -> %s", old, got)
	}
	if got := f.read("base.txt"); got != "local hidden\n" {
		t.Errorf("hidden worktree edit = %q", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("gate ran before assume-unchanged refusal: err=%v", err)
	}
}

func TestMergeSkipWorktreeDeleteOverlapRefusesBeforePush(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.write("journal.txt", "base\n")
	f.mustPlainCommit("docs: add journal")
	f.git("push", "-q", "origin", "main")
	f.git("switch", "-q", "feature/PROJ-42")
	f.git("merge", "-q", "main")
	f.git("rm", "-q", "journal.txt")
	f.mustPlainCommit("docs: remove journal")
	f.git("switch", "-q", "main")
	old := f.git("rev-parse", "HEAD")
	f.write("journal.txt", "local hidden edit\n")
	f.git("update-index", "--skip-worktree", "journal.txt")
	out, code := f.graft("", "merge", "feature/PROJ-42")
	if code == 0 || !strings.Contains(out, "journal.txt") {
		t.Fatalf("skip-worktree delete overlap was not refused: exit %d\n%s", code, out)
	}
	if got := f.git("rev-parse", "HEAD"); got != old {
		t.Errorf("local trunk moved: %s -> %s", old, got)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != old {
		t.Errorf("origin moved: %s -> %s", old, got)
	}
	if got := f.read("journal.txt"); got != "local hidden edit\n" {
		t.Errorf("hidden deleted-file edit = %q", got)
	}
}

func TestMergeSkipWorktreeDirectoryToFileRefusesBeforePush(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.write("node/child.txt", "base\n")
	f.mustPlainCommit("chore: add node directory")
	f.git("push", "-q", "origin", "main")
	f.git("switch", "-q", "feature/PROJ-42")
	f.git("merge", "-q", "main")
	f.git("rm", "-q", "node/child.txt")
	f.write("node", "file\n")
	f.mustPlainCommit("feat: replace directory with file")
	f.git("switch", "-q", "main")
	old := f.git("rev-parse", "HEAD")
	f.write("node/child.txt", "local hidden\n")
	f.git("update-index", "--skip-worktree", "node/child.txt")
	out, code := f.graft("", "merge", "feature/PROJ-42")
	if code == 0 || !strings.Contains(out, "node/child.txt") {
		t.Fatalf("directory-to-file overlap was not refused: exit %d\n%s", code, out)
	}
	if got := f.git("rev-parse", "HEAD"); got != old {
		t.Errorf("local trunk moved: %s -> %s", old, got)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != old {
		t.Errorf("origin moved: %s -> %s", old, got)
	}
	if got := f.read("node/child.txt"); got != "local hidden\n" {
		t.Errorf("hidden child edit = %q", got)
	}
}

func TestMergePrePushIgnoredResultPathDoesNotOverwriteIgnoredFile(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.write(".gitignore", "late-ignored.txt\n")
	f.mustPlainCommit("chore: ignore late merge path")
	f.git("push", "-q", "origin", "main")
	f.git("switch", "-q", "feature/PROJ-42")
	f.git("merge", "-q", "main")
	f.write("late-ignored.txt", "merged result\n")
	f.git("add", "-f", "late-ignored.txt")
	f.mustPlainCommit("feat: add ignored path")
	f.git("switch", "-q", "main")
	old := f.git("rev-parse", "HEAD")
	hooks := f.git("rev-parse", "--path-format=absolute", "--git-path", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\nprintf 'ignored hook bytes\\n' > late-ignored.txt\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := f.graft("", "merge", "feature/PROJ-42")
	if code == 0 || !strings.Contains(out, "published ") || !strings.Contains(out, "git merge --ff-only --no-overwrite-ignore ") {
		t.Fatalf("post-push ignored collision did not defer local install with a safe recovery command: exit %d\n%s", code, out)
	}
	local := f.git("rev-parse", "main")
	remote := f.gitIn(f.remote("origin"), "rev-parse", "main")
	if local != old || remote == old {
		t.Fatalf("local/remote state after collision: old=%s local=%s origin=%s", old, local, remote)
	}
	if got := f.read("late-ignored.txt"); got != "ignored hook bytes\n" {
		t.Errorf("hook-created ignored file bytes = %q", got)
	}
}

func TestMergePushFailureLeavesLocalTrunkAndKeepsDirt(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.write("docs/tasks/X.md", "base\n")
	f.mustPlainCommit("docs: add journal")
	f.git("push", "-q", "origin", "main")
	f.git("switch", "-q", "feature/PROJ-42")
	f.git("merge", "-q", "main")
	f.git("switch", "-q", "main")
	old := f.git("rev-parse", "HEAD")
	f.write("docs/tasks/X.md", "staged\n")
	f.git("add", "docs/tasks/X.md")
	f.write("docs/tasks/X.md", "unstaged\n")
	marker := filepath.Join(f.base, "hook-head")
	hooks := f.git("rev-parse", "--path-format=absolute", "--git-path", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\ngit rev-parse HEAD > " + marker + "\necho rejected >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := f.graft("", "merge", "feature/PROJ-42")
	if code == 0 || !strings.Contains(out, "push to origin failed") {
		t.Fatalf("rejected push succeeded: exit %d\n%s", code, out)
	}
	seen, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("pre-push did not record local HEAD: %v", err)
	}
	if strings.TrimSpace(string(seen)) != old {
		t.Errorf("pre-push saw local trunk at %s, want old HEAD %s", strings.TrimSpace(string(seen)), old)
	}
	if got := f.git("rev-parse", "main"); got != old {
		t.Errorf("local trunk was not rolled back: %s -> %s", old, got)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != old {
		t.Errorf("origin moved on rejected push: %s -> %s", old, got)
	}
	if got := f.read("docs/tasks/X.md"); got != "unstaged\n" {
		t.Errorf("unstaged dirt after rollback = %q", got)
	}
	if got := f.git("diff", "--cached", "--", "docs/tasks/X.md"); !strings.Contains(got, "+staged") {
		t.Errorf("staged dirt after rollback lost: %s", got)
	}
	if got := f.git("diff", "--", "docs/tasks/X.md"); !strings.Contains(got, "+unstaged") {
		t.Errorf("unstaged dirt after rollback lost: %s", got)
	}
}

func TestMergeKeepsDirtThatAppearsDuringPushAndCanCatchUpLater(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.git("switch", "-q", "feature/PROJ-42")
	f.write("base.txt", "source change\n")
	f.mustPlainCommit("feat: change base")
	f.git("switch", "-q", "main")
	old := f.git("rev-parse", "HEAD")
	hooks := f.git("rev-parse", "--path-format=absolute", "--git-path", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\nprintf 'staged after checks\\n' > base.txt\ngit add -- base.txt\nprintf 'unstaged after checks\\n' > base.txt\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	out, code := f.graft("", "merge", "feature/PROJ-42")
	if code == 0 || !strings.Contains(out, "published ") || !strings.Contains(out, "local main stays at "+old) || !strings.Contains(out, "run `git merge --ff-only --no-overwrite-ignore ") {
		t.Fatalf("merge did not report a post-push fast-forward refusal: exit %d\n%s", code, out)
	}
	merged := f.gitIn(f.remote("origin"), "rev-parse", "main")
	if merged == old || !strings.Contains(out, "published "+merged+" to origin") {
		t.Fatalf("origin was not published before local refusal: old=%s origin=%s\n%s", old, merged, out)
	}
	if got := f.git("rev-parse", "HEAD"); got != old {
		t.Errorf("local trunk moved despite refused fast-forward: %s -> %s", old, got)
	}
	if got := f.read("base.txt"); got != "unstaged after checks\n" {
		t.Errorf("fresh unstaged bytes changed: %q", got)
	}
	if got := f.git("diff", "--cached", "--", "base.txt"); !strings.Contains(got, "+staged after checks") {
		t.Errorf("fresh staged edit was lost: %s", got)
	}
	if got := f.git("diff", "--", "base.txt"); !strings.Contains(got, "+unstaged after checks") {
		t.Errorf("fresh unstaged edit was lost: %s", got)
	}

	if err := os.Remove(filepath.Join(hooks, "pre-push")); err != nil {
		t.Fatal(err)
	}
	f.git("reset", "--hard", old)
	if out, code = f.graft("", "merge", "feature/PROJ-42"); code != 0 {
		t.Fatalf("subsequent merge could not catch up to the published trunk: exit %d\n%s", code, out)
	}
	if got := f.git("rev-parse", "HEAD"); got != merged {
		t.Errorf("subsequent merge left local trunk at %s, want %s", got, merged)
	}
}

func TestMergeAlreadyPublishedInstallsDisjointDirtOnLaggingTrunk(t *testing.T) {
	f := mergeRepo(t, "origin")
	old := f.git("rev-parse", "HEAD")
	f.mustGraft("merge", "feature/PROJ-42")
	merged := f.git("rev-parse", "HEAD")
	f.git("update-ref", "refs/heads/main", old, merged)
	f.git("reset", "--hard", old)
	f.write("base.txt", "local dirt\n")
	out, code := f.graft("", "merge", "feature/PROJ-42")
	if code != 0 {
		t.Fatalf("could not install already-published merge with disjoint dirt: exit %d\n%s", code, out)
	}
	if got := f.git("rev-parse", "HEAD"); got != merged {
		t.Errorf("local trunk = %s, want %s", got, merged)
	}
	if got := f.read("base.txt"); got != "local dirt\n" {
		t.Errorf("local dirt = %q", got)
	}
	f.git("update-ref", "refs/heads/main", old, merged)
	f.git("reset", "--hard", old)
	f.write("feature.txt", "conflicting local dirt\n")
	out, code = f.graft("", "merge", "feature/PROJ-42")
	if code == 0 || !strings.Contains(out, "git merge --ff-only --no-overwrite-ignore "+merged) {
		t.Fatalf("lagging trunk did not recommend the safe fast-forward command: exit %d\n%s", code, out)
	}
}

func TestMergeIgnoresInheritedAlternateIndex(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.write("base.txt", "alternate staged\n")
	index := filepath.Join(f.base, "alternate-index")
	indexData, err := os.ReadFile(filepath.Join(f.work, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(index, indexData, 0o600); err != nil {
		t.Fatal(err)
	}
	// Build a staged edit in a separate index while the real index stays clean.
	cmd := exec.Command("git", "add", "base.txt")
	cmd.Dir = f.work
	cmd.Env = append(append([]string(nil), testEnv...), "GIT_INDEX_FILE="+index)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("stage alternate index: %v\n%s", err, out)
	}
	f.env = append(f.env, "GIT_INDEX_FILE="+index)
	out, code := f.graft("", "merge", "feature/PROJ-42")
	f.env = nil
	if code != 0 {
		t.Fatalf("inherited alternate index blocked disjoint merge: exit %d\n%s", code, out)
	}
	if got := f.git("diff", "--cached", "--", "base.txt"); got != "" {
		t.Errorf("real index was changed by merge: %s", got)
	}
	cmd = exec.Command("git", "diff", "--cached", "--", "base.txt")
	cmd.Dir = f.work
	cmd.Env = append(append([]string(nil), testEnv...), "GIT_INDEX_FILE="+index)
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "+alternate staged") {
		t.Errorf("alternate staged edit changed: %v\n%s", err, out)
	}
}

func TestMergeAllowsDisjointDirtAddedDuringChecks(t *testing.T) {
	f := mergeRepo(t, "origin")
	addMergeTask(f, "    run: [[sh, -c, 'mkdir -p "+filepath.Join(f.work, "docs/tasks")+" && printf journal > "+filepath.Join(f.work, "docs/tasks/X.md")+"']]")
	out, code := f.graft("", "merge", "feature/PROJ-42")
	if code != 0 {
		t.Fatalf("disjoint dirt added during checks blocked merge: exit %d\n%s", code, out)
	}
	if got := f.read("docs/tasks/X.md"); got != "journal" {
		t.Errorf("check-created journal = %q", got)
	}
}

func TestMergeNoDepsNeedsReason(t *testing.T) {
	f := mergeRepo(t, "origin")
	if out, code := f.graft("", "merge", "--no-deps", "feature/PROJ-42"); code == 0 || !strings.Contains(out, "--reason") {
		t.Errorf("missing reason: exit %d\n%s", code, out)
	}
}

// A temporary tree left by a merge that is gone is named, not removed: a
// task the dead merge started may still run in it.
func TestMergeWarnsAboutDeadTemporaryWorktree(t *testing.T) {
	f := mergeRepo(t, "origin")
	kit := filepath.Join(filepath.Dir(f.work), "kit")
	f.write("go.mod", "module example.com/project\n\ngo 1.23\n")
	f.write("go.work", "go 1.23\n\nuse (\n .\n ../kit\n)\n")
	f.write("main.go", "package project\n\nimport _ \"example.com/kit\"\n")
	writeMergeNeighbour(t, kit)
	f.mustPlainCommit("chore: use neighbouring module")
	f.git("push", "-q", "origin", "main")
	root, err := os.MkdirTemp(os.TempDir(), "graft-merge-orphan-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	worktree := filepath.Join(root, filepath.Base(filepath.Dir(f.work)), filepath.Base(f.work))
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		t.Fatal(err)
	}
	f.git("worktree", "add", "--detach", "--quiet", worktree, "main")
	commonDir := f.git("rev-parse", "--path-format=absolute", "--git-common-dir")
	owner := filepath.Join(root, ".graft-merge-owner")
	if err := os.WriteFile(owner, []byte("2147483647\n"+commonDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := f.mustGraft("merge", "feature/PROJ-42")
	if !strings.Contains(out, "is left by a graft merge that is gone") || !strings.Contains(out, "git worktree remove --force '"+worktree+"' && rm -rf '"+root+"'") {
		t.Errorf("missing warning about the left temporary worktree:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(worktree, "base.txt")); err != nil {
		t.Errorf("the left temporary worktree was touched: %v", err)
	}
}

func TestMergeFindsOrphanUnderSymlinkedTMPDIR(t *testing.T) {
	f := mergeRepo(t, "origin")
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "tmp-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	f.env = append(f.env, "TMPDIR="+link)
	root, err := os.MkdirTemp(target, "graft-merge-orphan-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	worktree := filepath.Join(root, "projectA", "tree")
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		t.Fatal(err)
	}
	f.git("worktree", "add", "--detach", "--quiet", worktree, "main")
	resolvedWorktree, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		t.Fatal(err)
	}
	commonDir := f.git("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err := os.WriteFile(filepath.Join(root, ".graft-merge-owner"), []byte("2147483647\n"+commonDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := f.mustGraft("merge", "feature/PROJ-42")
	if !strings.Contains(out, "git worktree remove --force '"+resolvedWorktree+"'") {
		t.Errorf("missing registered worktree cleanup command:\n%s", out)
	}
}

func TestMergeWarnsHowToRemoveUnregisteredMergeDirectory(t *testing.T) {
	f := mergeRepo(t, "origin")
	root, err := os.MkdirTemp(os.TempDir(), "graft-merge-unregistered-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	commonDir := f.git("rev-parse", "--path-format=absolute", "--git-common-dir")
	owner := filepath.Join(root, ".graft-merge-owner")
	if err := os.WriteFile(owner, []byte("2147483647\n"+commonDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := f.mustGraft("merge", "feature/PROJ-42")
	if !strings.Contains(out, "rm -rf '"+root+"'") {
		t.Errorf("missing cleanup command for an unregistered merge directory:\n%s", out)
	}
}

// A pre-push hook may switch away from the trunk before its local fast-forward.
func TestMergePrePushSwitchDoesNotAdvanceOtherBranch(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.git("branch", "other", "main")
	before := f.git("rev-parse", "main")
	hooks := f.git("rev-parse", "--path-format=absolute", "--git-path", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\ngit switch -q other\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := f.graft("", "merge", "feature/PROJ-42")
	if code == 0 || !strings.Contains(out, "published ") || !strings.Contains(out, "checkout branch changed") {
		t.Errorf("post-push checkout change was not reported: exit %d\n%s", code, out)
	}
	if got := f.git("rev-parse", "main"); got != before {
		t.Errorf("local main moved despite checkout change: %s -> %s", before, got)
	}
	if got := f.git("rev-parse", "other"); got != before {
		t.Errorf("branch other moved to %s", got)
	}
	if got := f.git("symbolic-ref", "--short", "HEAD"); got != "other" {
		t.Errorf("hook-selected branch changed to %s", got)
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

func TestMergeChecksDirtyPathAgainstPostBumpCommitBeforeGate(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file, file: VERSION}\ngate: []\n",
		"VERSION":     "1.2.3\n", "base.txt": "base\n",
	}, "origin")
	marker := filepath.Join(f.base, "gate-ran")
	f.write(".graft.yaml", "schema: 1\nversion: {mode: file, file: VERSION}\ngate:\n  - [sh, -c, 'touch "+marker+"']\n")
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
	f.git("switch", "-q", "main")
	f.write("VERSION", "1.2.5\n")
	f.mustPlainCommit("chore: trunk version")
	f.git("push", "-q", "origin", "main")
	old := f.git("rev-parse", "HEAD")
	f.git("update-index", "--skip-worktree", "VERSION")
	out, code := f.graft("", "merge", "feature/PROJ-43")
	if code == 0 || !strings.Contains(out, "VERSION") {
		t.Fatalf("post-bump overlap was not refused: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("gate ran before post-bump overlap check: err=%v", err)
	}
	if got := f.git("rev-parse", "HEAD"); got != old {
		t.Errorf("local trunk moved: %s -> %s", old, got)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != old {
		t.Errorf("origin moved: %s -> %s", old, got)
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

func TestMergeRefusesDivergedMirrorBeforePublishing(t *testing.T) {
	f := mergeRepo(t, "origin", "mirror")
	other := filepath.Join(f.base, "mirror-writer")
	f.run(f.base, "git", "clone", "--branch", "main", f.remote("mirror"), other)
	f.gitIn(other, "config", "user.name", "Merge Test")
	f.gitIn(other, "config", "user.email", "merge@example.invalid")
	if err := os.WriteFile(filepath.Join(other, "mirror-only.txt"), []byte("mirror\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.gitIn(other, "add", "mirror-only.txt")
	f.gitIn(other, "commit", "-m", "feat: mirror only")
	f.gitIn(other, "push", "origin", "main")
	mirrorHead := f.gitIn(f.remote("mirror"), "rev-parse", "main")
	out, code := f.graft("", "merge", "feature/PROJ-42")
	if code == 0 || !strings.Contains(out, "remote mirror") {
		t.Fatalf("diverged mirror: exit %d\n%s", code, out)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != f.git("rev-parse", "main") {
		t.Errorf("primary published despite mirror preflight failure: remote=%s local=%s", got, f.git("rev-parse", "main"))
	}
	if got := f.gitIn(f.remote("mirror"), "rev-parse", "main"); got != mirrorHead {
		t.Errorf("mirror changed: %s, want %s", got, mirrorHead)
	}
}

func TestMergeAlreadyPublishedButMirrorMissingResultGivesCatchupCommand(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none, reason: test}\ngate: []\npush: {remotes: [origin]}\n",
		"base.txt":    "base\n",
	}, "origin", "mirror")
	f.mustPlainCommit("chore: base")
	f.git("branch", "-m", "main")
	f.git("push", "-q", "origin", "main")
	f.git("push", "-q", "mirror", "main")
	f.git("fetch", "--quiet", "origin")
	f.git("fetch", "--quiet", "mirror")
	f.git("branch", "--set-upstream-to", "origin/main", "main")
	f.git("remote", "set-head", "origin", "main")
	f.git("remote", "set-head", "mirror", "main")
	f.git("switch", "-q", "-c", "feature")
	f.write("feature.txt", "source\n")
	f.mustPlainCommit("feat: source")
	f.git("switch", "-q", "main")
	f.mustGraft("merge", "feature")
	f.write(".graft.yaml", "schema: 1\nversion: {mode: none, reason: test}\ngate: []\npush: {remotes: [origin, mirror]}\n")
	f.mustPlainCommit("chore: enable mirror")
	f.git("push", "-q", "origin", "main")
	out, code := f.graft("", "merge", "feature")
	if code == 0 || !strings.Contains(out, f.remote("mirror")) || !strings.Contains(out, "git push --no-follow-tags -- "+f.remote("mirror")) {
		t.Fatalf("mirror catch-up advice: exit %d\n%s", code, out)
	}
}

func TestMergeMirrorRejectsReports(t *testing.T) {
	f := mergeRepo(t, "origin", "mirror")
	addMergeTask(f, `    run: [[sh, -c, 'rival=$(git commit-tree "$(git rev-parse HEAD^{tree})" -p "$(git rev-parse HEAD)" -m rival) && git push -q mirror "$rival:refs/heads/main"']]`)
	local := f.git("rev-parse", "HEAD")
	if out, code := f.graft("", "merge", "feature/PROJ-42"); code == 0 || !strings.Contains(out, "remote mirror trunk is not an ancestor") {
		t.Fatalf("mirror refusal: exit %d\n%s", code, out)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != local {
		t.Errorf("primary was published despite mirror refusal: got %s, want %s", got, local)
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

func TestMergeResultCannotChangePublishRemote(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none, reason: test}\ngate: []\npush: {remotes: [origin]}\n",
		"base.txt":    "base\n",
	}, "origin", "upstream")
	f.mustPlainCommit("chore: base")
	f.git("branch", "-m", "main")
	f.git("push", "-q", "origin", "main")
	f.git("push", "-q", "upstream", "main")
	f.git("fetch", "--quiet", "origin")
	f.git("fetch", "--quiet", "upstream")
	f.git("branch", "--set-upstream-to", "origin/main", "main")
	f.git("remote", "set-head", "origin", "main")
	f.git("remote", "set-head", "upstream", "main")
	f.git("switch", "-q", "-c", "feature")
	f.write(".graft.yaml", "schema: 1\nversion: {mode: none, reason: test}\ngate: []\npush: {remotes: [origin]}\nenvs: {local: {}}\ndeploy:\n  remote: upstream\n  targets:\n    test: {env: local, run: [\"true\"]}\n")
	f.write("feature.txt", "source\n")
	f.mustPlainCommit("feat: change publish remote")
	f.git("switch", "-q", "main")
	out, code := f.graft("", "merge", "feature")
	if code == 0 || !strings.Contains(out, "the branch changes where trunk is published") {
		t.Fatalf("result config changed trunk remote: exit %d\n%s", code, out)
	}
	for _, remote := range []string{"origin", "upstream"} {
		if got, want := f.gitIn(f.remote(remote), "rev-parse", "main"), f.gitIn(f.remote("origin"), "rev-parse", "main"); got != want {
			t.Errorf("%s was unexpectedly updated: %s (origin %s)", remote, got, want)
		}
	}
}

func TestMergeRequiresPrimaryInPushRemotes(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none, reason: test}\ngate: []\npush: {remotes: [origin]}\ndeploy: {remote: mirror, targets: {test: {env: local, run: [\"true\"]}}}\nenvs: {local: {}}\n",
		"base.txt":    "base\n",
	}, "origin", "mirror")
	f.mustPlainCommit("chore: base")
	f.git("branch", "-m", "main")
	f.git("push", "-q", "origin", "main")
	f.git("push", "-q", "mirror", "main")
	f.git("fetch", "--quiet", "origin")
	f.git("fetch", "--quiet", "mirror")
	f.git("branch", "--set-upstream-to", "origin/main", "main")
	f.git("remote", "set-head", "origin", "main")
	f.git("remote", "set-head", "mirror", "main")
	f.git("switch", "-q", "-c", "feature")
	f.write("feature.txt", "source\n")
	f.mustPlainCommit("feat: source")
	f.git("switch", "-q", "main")
	if out, code := f.graft("", "merge", "feature"); code == 0 || !strings.Contains(out, "primary remote") {
		t.Fatalf("primary missing from push.remotes: exit %d\n%s", code, out)
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
	if !strings.Contains(out, "initial fetch offline failed") || !strings.Contains(out, "local main moved to") {
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

func TestMergeExplicitSourceFromNonPushRemote(t *testing.T) {
	f := mergeRepo(t, "origin", "fork")
	f.write(".graft.yaml", strings.TrimSuffix(f.read(".graft.yaml"), "\n")+"\npush: {remotes: [origin]}\n")
	f.mustPlainCommit("chore: restrict publication")
	f.git("push", "-q", "origin", "main")
	f.git("push", "-q", "fork", "main")
	f.git("switch", "-q", "feature/PROJ-42")
	f.git("push", "-q", "fork", "feature/PROJ-42")
	f.git("fetch", "--quiet", "fork")
	f.git("switch", "-q", "main")
	forkMain := f.gitIn(f.remote("fork"), "rev-parse", "main")
	out := f.mustGraft("merge", "--mode", "merge", "fork/feature/PROJ-42")
	if !strings.Contains(out, "pushed to origin") {
		t.Errorf("merge from fork was not published to origin:\n%s", out)
	}
	if got := f.gitIn(f.remote("fork"), "rev-parse", "main"); got != forkMain {
		t.Errorf("fork main changed: got %s, want %s", got, forkMain)
	}
}

func TestMergeRefusesLocalSourceBehindUpstream(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.git("switch", "-q", "feature/PROJ-42")
	f.git("push", "-q", "-u", "origin", "feature/PROJ-42")
	remoteWork := filepath.Join(f.base, "remote-source")
	f.run(f.base, "git", "clone", "--branch", "feature/PROJ-42", f.remote("origin"), remoteWork)
	f.gitIn(remoteWork, "config", "user.name", "Merge Test")
	f.gitIn(remoteWork, "config", "user.email", "merge@example.invalid")
	if err := os.WriteFile(filepath.Join(remoteWork, "upstream-only.txt"), []byte("remote source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.gitIn(remoteWork, "add", "upstream-only.txt")
	f.gitIn(remoteWork, "commit", "-m", "feat: remote source")
	f.gitIn(remoteWork, "push", "origin", "feature/PROJ-42")
	f.git("switch", "-q", "main")
	if out, code := f.graft("", "merge", "feature/PROJ-42"); code == 0 || !strings.Contains(out, "pull or push first") {
		t.Fatalf("local source behind upstream: exit %d\n%s", code, out)
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
	f.git("switch", "-q", "feature/PROJ-42")
	f.write("base.txt", "source change\n")
	f.mustPlainCommit("feat: change base")
	f.git("switch", "-q", "main")
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

func TestMergeRefusesCheckoutBranchChangedByDeps(t *testing.T) {
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none, reason: test}\ngate: []\n", "base.txt": "base\n"}, "origin")
	f.write(".graft.yaml", "schema: 1\nversion: {mode: none, reason: test}\ngate: []\nmerge: {deps: [switch]}\ntasks:\n  switch:\n    run: [[git, -C, "+yamlQuote(f.work)+", switch, -q, -c, other]]\n")
	f.mustPlainCommit("chore: base")
	f.git("branch", "-m", "main")
	f.git("push", "-q", "origin", "main")
	f.git("fetch", "--quiet", "origin")
	f.git("branch", "--set-upstream-to", "origin/main", "main")
	f.git("remote", "set-head", "origin", "main")
	f.git("switch", "-q", "-c", "feature")
	f.write("feature.txt", "source\n")
	f.mustPlainCommit("feat: source")
	f.git("switch", "-q", "main")
	out, code := f.graft("", "merge", "feature")
	if code == 0 || !strings.Contains(out, "checkout branch changed") {
		t.Fatalf("checkout branch changed during deps: exit %d\n%s", code, out)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != f.git("rev-parse", "main") {
		t.Errorf("checkout branch change was published: remote=%s local=%s", got, f.git("rev-parse", "main"))
	}
}

func TestMergeRefusesDepsMovingTemporaryHead(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none, reason: test}\ngate: []\nmerge: {deps: [verify]}\ntasks:\n  move:\n    run: [[git, checkout, -q, --detach, HEAD~1]]\n  verify:\n    deps: [move]\n    run: [[sh, -c, 'echo a.txt-present=$(test -f a.txt && echo yes || echo no)']]\n",
		"base.txt":    "base\n",
	}, "origin")
	f.mustPlainCommit("chore: base")
	f.git("branch", "-m", "main")
	f.git("push", "-q", "origin", "main")
	f.git("fetch", "--quiet", "origin")
	f.git("branch", "--set-upstream-to", "origin/main", "main")
	f.git("remote", "set-head", "origin", "main")
	f.git("switch", "-q", "-c", "feature")
	f.write("a.txt", "source\n")
	f.mustPlainCommit("feat: source")
	f.git("switch", "-q", "main")
	out, code := f.graft("", "merge", "feature")
	if code == 0 || !strings.Contains(out, "moved temporary HEAD") {
		t.Fatalf("deps moved temporary HEAD: exit %d\n%s", code, out)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != f.git("rev-parse", "main") {
		t.Errorf("temporary HEAD change was published: remote=%s local=%s", got, f.git("rev-parse", "main"))
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

func TestMergeModeRequiresTicketOnMergeMessage(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.write(".graft.yaml", "schema: 1\nversion: {mode: none, reason: test}\ngate: []\nticket: {pattern: 'PROJ-[0-9]+', required: true}\n")
	f.mustPlainCommit("chore: require ticket")
	f.git("push", "-q", "origin", "main")
	f.git("switch", "-q", "-c", "topic-without-key")
	f.write("source.txt", "source\n")
	f.mustPlainCommit("PROJ-42 feat: required source")
	f.git("switch", "-q", "main")
	f.write("trunk.txt", "trunk\n")
	f.mustPlainCommit("PROJ-43 feat: trunk")
	f.git("push", "-q", "origin", "main")
	if out, code := f.graft("", "merge", "--mode", "merge", "topic-without-key"); code == 0 || !strings.Contains(out, "ticket") {
		t.Fatalf("merge without ticket: exit %d\n%s", code, out)
	}
}

func TestMergeRefusesMissingTicketOnMergeCommit(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.write(".graft.yaml", "schema: 1\nversion: {mode: none, reason: test}\ngate: []\nticket: {pattern: 'PROJ-[0-9]+', required: true}\n")
	f.mustPlainCommit("chore: require ticket")
	f.git("push", "-q", "origin", "main")
	f.git("switch", "-q", "-c", "topic-without-key")
	f.write("source.txt", "source\n")
	f.mustPlainCommit("PROJ-42 feat: source")
	f.git("switch", "-q", "main")
	f.write("trunk.txt", "trunk\n")
	f.mustPlainCommit("PROJ-43 feat: trunk")
	f.git("push", "-q", "origin", "main")
	remoteHead := f.gitIn(f.remote("origin"), "rev-parse", "main")
	out, code := f.graft("", "merge", "--mode", "merge", "topic-without-key")
	if code == 0 || !strings.Contains(strings.ToLower(out), "ticket") {
		t.Fatalf("merge message without ticket: exit %d\n%s", code, out)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != remoteHead {
		t.Errorf("ticket refusal published: got %s want %s", got, remoteHead)
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

func TestMergeVersionSyncConflictsMergeMirrors(t *testing.T) {
	for _, mode := range []string{"rebase", "merge"} {
		t.Run(mode, func(t *testing.T) {
			f := versionSyncMergeRepo(t)
			f.git("switch", "-q", "-c", "feature/PROJ-91")
			f.write("web/package.json", strings.Replace(f.read("web/package.json"), `"private": false`, `"private": true`, 1))
			f.write("VERSION", "1.3.0\n")
			f.write("web/package.json", strings.Replace(f.read("web/package.json"), `"version": "1.2.3"`, `"version": "1.3.0"`, 1))
			f.write("app.yaml", strings.Replace(f.read("app.yaml"), `version: "1.2.3"`, `version: "1.3.0"`, 1))
			f.mustGraft("commit", "--minor", "-m", "feat: source version")
			f.git("switch", "-q", "main")
			f.write("web/package.json", strings.Replace(f.read("web/package.json"), `"name": "web"`, `"name": "trunk-web"`, 1))
			f.mustGraft("commit", "-m", "fix: trunk version")
			f.git("push", "-q", "origin", "main")
			args := []string{"merge"}
			if mode == "merge" {
				args = append(args, "--mode", "merge")
			}
			f.mustGraft(append(args, "feature/PROJ-91")...)
			if got := strings.TrimSpace(f.read("VERSION")); got != "1.4.0" {
				t.Errorf("VERSION = %q, want merge-bumped 1.4.0", got)
			}
			pkg := f.read("web/package.json")
			for _, want := range []string{`"version": "1.4.0"`, `"private": true`, `"name": "trunk-web"`} {
				if !strings.Contains(pkg, want) {
					t.Errorf("package.json missing %s:\n%s", want, pkg)
				}
			}
			if !strings.Contains(f.read("app.yaml"), `version: "1.4.0"`) {
				t.Errorf("yaml version not synced:\n%s", f.read("app.yaml"))
			}
		})
	}
}

func TestMergeVersionSyncConflictOnSameFieldRefuses(t *testing.T) {
	f := versionSyncMergeRepo(t)
	f.git("switch", "-q", "-c", "feature/PROJ-92")
	f.write("web/package.json", strings.Replace(f.read("web/package.json"), `"private": false`, `"private": true`, 1))
	f.write("VERSION", "1.3.0\n")
	f.write("web/package.json", strings.Replace(f.read("web/package.json"), `"version": "1.2.3"`, `"version": "1.3.0"`, 1))
	f.write("app.yaml", strings.Replace(f.read("app.yaml"), `version: "1.2.3"`, `version: "1.3.0"`, 1))
	f.mustGraft("commit", "--minor", "-m", "feat: source version")
	source := f.git("rev-parse", "HEAD")
	f.git("switch", "-q", "main")
	f.write("web/package.json", strings.Replace(f.read("web/package.json"), `"private": false`, `"private": "trunk"`, 1))
	f.mustGraft("commit", "-m", "fix: trunk version")
	f.git("push", "-q", "origin", "main")
	trunk := f.git("rev-parse", "HEAD")
	if out, code := f.graft("", "merge", "feature/PROJ-92"); code == 0 || !strings.Contains(out, "web/package.json") {
		t.Fatalf("conflicting mirror field: exit %d\n%s", code, out)
	}
	if got := f.git("rev-parse", "HEAD"); got != trunk {
		t.Errorf("trunk moved: %s", got)
	}
	if got := f.git("rev-parse", "feature/PROJ-92"); got != source {
		t.Errorf("source moved: %s", got)
	}
}

func TestMergeVersionSyncConflictWithManualVersionMismatchRefuses(t *testing.T) {
	f := versionSyncMergeRepo(t)
	f.git("switch", "-q", "-c", "feature/PROJ-93")
	f.write("web/package.json", strings.Replace(f.read("web/package.json"), `"private": false`, `"private": true`, 1))
	f.mustGraft("commit", "--minor", "-m", "feat: source version")
	sourceVersion := strings.TrimSpace(f.read("VERSION"))
	f.write("web/package.json", strings.Replace(f.read("web/package.json"), `"version": "`+sourceVersion+`"`, `"version": "9.9.9"`, 1))
	f.git("add", "web/package.json")
	f.git("commit", "--amend", "--no-edit", "--quiet")
	// graft commit set the upstream on its first push: publish the amended commit as well
	f.git("push", "-q", "--force", "origin", "feature/PROJ-93")
	source := f.git("rev-parse", "HEAD")
	f.git("switch", "-q", "main")
	f.write("web/package.json", strings.Replace(f.read("web/package.json"), `"name": "web"`, `"name": "trunk-web"`, 1))
	f.mustGraft("commit", "-m", "fix: trunk version")
	f.git("push", "-q", "origin", "main")
	trunk := f.git("rev-parse", "HEAD")
	remoteTrunk := f.gitIn(f.remote("origin"), "rev-parse", "main")
	if out, code := f.graft("", "merge", "feature/PROJ-93"); code == 0 || !strings.Contains(out, "web/package.json: mirror version 9.9.9 does not match VERSION stage "+sourceVersion) {
		t.Fatalf("inconsistent mirror version: exit %d\n%s", code, out)
	}
	if got := f.git("rev-parse", "HEAD"); got != trunk {
		t.Errorf("trunk moved: %s", got)
	}
	if got := f.git("rev-parse", "feature/PROJ-93"); got != source {
		t.Errorf("source moved: %s", got)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != remoteTrunk {
		t.Errorf("remote trunk moved: %s", got)
	}
	if got := f.git("show", "feature/PROJ-93:web/package.json"); !strings.Contains(got, `"version": "9.9.9"`) {
		t.Errorf("manual source version changed:\n%s", got)
	}
}

func TestMergeVersionSyncConflictWithDifferentRegexMatchesRefuses(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion:\n  mode: file\n  file: VERSION\n  sync:\n    - {path: app.yaml, format: regex, pattern: 'version:\\s*\"([^\"]*)\"'}\nticket: {pattern: 'PROJ-[0-9]+'}\ngate: []\n",
		"VERSION":     "1.2.3\n", "app.yaml": "name: app\nversion: \"1.2.3\"\nother-version: \"1.2.3\"\n",
	}, "origin")
	f.mustPlainCommit("chore: base PROJ-96")
	f.git("branch", "-m", "main")
	f.git("push", "-q", "origin", "main")
	f.git("fetch", "--quiet", "origin")
	f.git("branch", "--set-upstream-to", "origin/main", "main")
	f.git("remote", "set-head", "origin", "main")
	f.git("switch", "-q", "-c", "feature/PROJ-97")
	f.write("app.yaml", strings.Replace(f.read("app.yaml"), "name: app", "name: source-app", 1))
	f.mustGraft("commit", "--minor", "-m", "feat: source version")
	sourceVersion := strings.TrimSpace(f.read("VERSION"))
	f.write("app.yaml", strings.Replace(f.read("app.yaml"), `other-version: "`+sourceVersion+`"`, `other-version: "8.8.8"`, 1))
	f.git("add", "app.yaml")
	f.git("commit", "--amend", "--no-edit", "--quiet")
	// graft commit set the upstream on its first push: publish the amended commit as well
	f.git("push", "-q", "--force", "origin", "feature/PROJ-97")
	source := f.git("rev-parse", "HEAD")
	f.git("switch", "-q", "main")
	f.write("app.yaml", strings.Replace(f.read("app.yaml"), "name: app", "name: trunk-app", 1))
	f.mustGraft("commit", "-m", "fix: trunk version")
	f.git("push", "-q", "origin", "main")
	trunk := f.git("rev-parse", "HEAD")
	if out, code := f.graft("", "merge", "feature/PROJ-97"); code == 0 || !strings.Contains(out, "app.yaml: disagrees with itself") {
		t.Fatalf("different regex matches were not rejected specifically: exit %d\n%s", code, out)
	}
	if got := f.git("rev-parse", "HEAD"); got != trunk {
		t.Errorf("trunk moved: %s", got)
	}
	if got := f.git("rev-parse", "feature/PROJ-97"); got != source {
		t.Errorf("source moved: %s", got)
	}
}

func TestMergeVersionSyncMultipleTargetsInOneFile(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml":       "schema: 1\nversion:\n  mode: file\n  file: VERSION\n  sync:\n    - {path: package-lock.json, format: json, key: [version]}\n    - {path: package-lock.json, format: json, key: [packages, '', version]}\nticket: {pattern: 'PROJ-[0-9]+'}\ngate: []\n",
		"VERSION":           "1.2.3\n",
		"package-lock.json": "{\n  \"name\": \"app\",\n  \"version\": \"1.2.3\",\n  \"packages\": {\n    \"\": {\n      \"version\": \"1.2.3\",\n      \"sourceFlag\": false\n    }\n  }\n}\n",
	}, "origin")
	f.mustPlainCommit("chore: base PROJ-94")
	f.git("branch", "-m", "main")
	f.git("push", "-q", "origin", "main")
	f.git("fetch", "--quiet", "origin")
	f.git("branch", "--set-upstream-to", "origin/main", "main")
	f.git("remote", "set-head", "origin", "main")
	f.git("switch", "-q", "-c", "feature/PROJ-95")
	f.write("package-lock.json", strings.Replace(f.read("package-lock.json"), `"sourceFlag": false`, `"sourceFlag": true`, 1))
	f.mustGraft("commit", "--minor", "-m", "feat: source version")
	f.git("switch", "-q", "main")
	f.write("package-lock.json", strings.Replace(f.read("package-lock.json"), `"name": "app"`, `"name": "trunk-app"`, 1))
	f.mustGraft("commit", "-m", "fix: trunk version")
	f.git("push", "-q", "origin", "main")
	f.mustGraft("merge", "feature/PROJ-95")
	lock := f.read("package-lock.json")
	versionValue := `"version": "` + strings.TrimSpace(f.read("VERSION")) + `"`
	for _, want := range []string{versionValue, `"sourceFlag": true`, `"name": "trunk-app"`} {
		if !strings.Contains(lock, want) {
			t.Errorf("package-lock.json missing %s:\n%s", want, lock)
		}
	}
	if strings.Count(lock, versionValue) != 2 {
		t.Errorf("both package-lock version targets were not updated:\n%s", lock)
	}
}

func versionSyncMergeRepo(t *testing.T) *fixture {
	t.Helper()
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion:\n  mode: file\n  file: VERSION\n  sync:\n    - {path: web/package.json, format: json, key: [version]}\n    - {path: app.yaml, format: regex, pattern: 'version:\\s*\"([^\"]*)\"'}\nticket: {pattern: 'PROJ-[0-9]+'}\ngate: []\n",
		"VERSION":     "1.2.3\n", "web/package.json": "{\n  \"name\": \"web\",\n  \"version\": \"1.2.3\",\n  \"private\": false\n}\n",
		"app.yaml": "name: app\nversion: \"1.2.3\"\n",
	}, "origin")
	f.mustPlainCommit("chore: base PROJ-90")
	f.git("branch", "-m", "main")
	f.git("push", "-q", "origin", "main")
	f.git("fetch", "--quiet", "origin")
	f.git("branch", "--set-upstream-to", "origin/main", "main")
	f.git("remote", "set-head", "origin", "main")
	return f
}

// A replace ref shows the checks one tree and pushes another: the merge
// checks the objects the remote gets.
func TestMergeChecksReplacedObjects(t *testing.T) {
	f := mergeRepo(t, "origin")
	addMergeTask(f, "    run: [[sh, -c, '! grep -q unsafe feature.txt']]")
	trunk := f.git("rev-parse", "HEAD")
	f.git("switch", "-q", "feature/PROJ-42")
	f.write("feature.txt", "unsafe\n")
	f.mustPlainCommit("feat: PROJ-42 unsafe")
	unsafe := f.git("rev-parse", "HEAD")
	f.write("feature.txt", "safe\n")
	f.git("add", "feature.txt")
	tree := f.git("write-tree")
	replacement := f.git("commit-tree", tree, "-p", unsafe+"^", "-m", "feat: PROJ-42 safe")
	f.git("checkout", "-q", "--", ".")
	f.git("replace", unsafe, replacement)
	f.git("switch", "-q", "main")
	out, code := f.graft("", "merge", "feature/PROJ-42")
	if code == 0 || !strings.Contains(out, "replace refs") {
		t.Fatalf("merge did not refuse a replaced commit:\n%s", out)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != trunk {
		t.Errorf("remote trunk moved to %s", got)
	}
}

// push.followTags does not publish tags beside trunk.
func TestMergeDoesNotFollowTags(t *testing.T) {
	f := mergeRepo(t, "origin", "mirror")
	f.git("tag", "-a", "v0.1", "-m", "tag", "feature/PROJ-42")
	f.git("config", "push.followTags", "true")
	f.mustGraft("merge", "feature/PROJ-42")
	for _, r := range []string{"origin", "mirror"} {
		if tags := f.gitIn(f.remote(r), "tag", "-l"); tags != "" {
			t.Errorf("%s got tags %q", r, tags)
		}
	}
}

// A push to a remote with several push URLs may land on some of them: the
// failure does not say nothing was published.
func TestMergePartialPushURLsSaysSo(t *testing.T) {
	f := mergeRepo(t, "origin")
	refusing := filepath.Join(f.base, "refusing.git")
	f.git("init", "-q", "--bare", refusing)
	if err := os.WriteFile(filepath.Join(refusing, "hooks", "pre-receive"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.git("config", "--add", "remote.origin.pushurl", f.remote("origin"))
	f.git("config", "--add", "remote.origin.pushurl", refusing)
	out, code := f.graft("", "merge", "feature/PROJ-42")
	if code == 0 || strings.Contains(out, "nothing published") || !strings.Contains(out, "several push URLs") {
		t.Errorf("partial push not reported: exit %d\n%s", code, out)
	}
}

func mergeRepoWithSubmodule(t *testing.T) (*fixture, string) {
	t.Helper()
	f := mergeRepo(t, "origin")
	subremote := filepath.Join(f.base, "submodule.git")
	f.run(f.base, "git", "init", "-q", "--bare", "-b", "main", subremote)
	subseed := filepath.Join(f.base, "subseed")
	f.run(f.base, "git", "init", "-q", "-b", "main", subseed)
	f.run(subseed, "git", "config", "user.name", "Reviewer")
	f.run(subseed, "git", "config", "user.email", "review@example.com")
	if err := os.WriteFile(filepath.Join(subseed, "data"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.run(subseed, "git", "add", ".")
	f.run(subseed, "git", "commit", "-qm", "chore: submodule base")
	f.run(subseed, "git", "push", "-q", subremote, "main")
	if _, code := f.exec(f.work, "", "git", "-c", "protocol.file.allow=always", "submodule", "add", "-q", subremote, "dep"); code != 0 {
		t.Fatal("adding submodule failed")
	}
	f.git("add", "dep")
	f.mustPlainCommit("chore: add submodule")
	f.git("push", "-q", "origin", "main")
	f.git("fetch", "--quiet", "origin")
	f.git("switch", "-q", "-c", "feature/submodule")
	f.run(filepath.Join(f.work, "dep"), "git", "config", "user.name", "Reviewer")
	f.run(filepath.Join(f.work, "dep"), "git", "config", "user.email", "review@example.com")
	if err := os.WriteFile(filepath.Join(f.work, "dep", "data"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.run(filepath.Join(f.work, "dep"), "git", "add", "data")
	f.run(filepath.Join(f.work, "dep"), "git", "commit", "-qm", "feat: submodule change")
	f.git("add", "dep")
	f.mustPlainCommit("feat: update submodule")
	f.git("switch", "-q", "main")
	f.git("-C", filepath.Join(f.work, "dep"), "switch", "-q", "--detach", "origin/main")
	return f, subremote
}

func TestMergeOnDemandChecksUnpublishedSubmodule(t *testing.T) {
	f, subremote := mergeRepoWithSubmodule(t)
	subBefore := f.gitIn(subremote, "rev-parse", "main")
	mainBefore := f.gitIn(f.remote("origin"), "rev-parse", "main")
	f.git("config", "push.recurseSubmodules", "on-demand")
	out, code := f.graft("", "merge", "feature/submodule")
	if code == 0 || !strings.Contains(out, "not be found on any remote") || strings.Contains(out, "must name a ref") {
		t.Fatalf("merge did not reject the unpublished submodule through Git's check: exit %d\n%s", code, out)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != mainBefore {
		t.Errorf("superproject remote moved despite Git's check: %s", got)
	}
	if got := f.gitIn(subremote, "rev-parse", "main"); got != subBefore {
		t.Errorf("submodule remote unexpectedly moved: %s", got)
	}
	if got := f.git("config", "--get", "push.recurseSubmodules"); got != "on-demand" {
		t.Errorf("user push.recurseSubmodules changed to %q", got)
	}
}

func TestMergeOnDemandAllowsPublishedSubmodule(t *testing.T) {
	f, subremote := mergeRepoWithSubmodule(t)
	subTip := f.git("rev-parse", "feature/submodule:dep")
	f.git("-C", filepath.Join(f.work, "dep"), "push", "-q", "origin", subTip+":refs/heads/main")
	f.git("-C", filepath.Join(f.work, "dep"), "fetch", "--quiet", "origin")
	if got := f.gitIn(subremote, "rev-parse", "main"); got != subTip {
		t.Fatalf("submodule remote tip %s, want published source commit %s", got, subTip)
	}
	f.git("config", "push.recurseSubmodules", "on-demand")
	out := f.mustGraft("merge", "feature/submodule")
	if got, want := f.gitIn(f.remote("origin"), "rev-parse", "main"), f.git("rev-parse", "feature/submodule"); got != want {
		t.Fatalf("superproject remote = %s, want %s:\n%s", got, want, out)
	}
	if got := f.git("config", "--get", "push.recurseSubmodules"); got != "on-demand" {
		t.Errorf("user push.recurseSubmodules changed to %q", got)
	}
}

func TestMergeOnDemandWithoutSubmodule(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.git("config", "push.recurseSubmodules", "on-demand")
	out := f.mustGraft("merge", "feature/PROJ-42")
	if got, want := f.gitIn(f.remote("origin"), "rev-parse", "main"), f.git("rev-parse", "feature/PROJ-42"); got != want {
		t.Fatalf("remote main = %s, want %s:\n%s", got, want, out)
	}
	if got := f.git("config", "--get", "push.recurseSubmodules"); got != "on-demand" {
		t.Errorf("user push.recurseSubmodules changed to %q", got)
	}
}

func TestMergeRetryReportsLaggingPushURL(t *testing.T) {
	f := mergeRepo(t, "origin")
	refusing := filepath.Join(f.base, "refusing.git")
	f.run(f.base, "git", "init", "-q", "--bare", "-b", "main", refusing)
	if err := os.WriteFile(filepath.Join(refusing, "hooks", "pre-receive"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.git("config", "--add", "remote.origin.pushurl", f.remote("origin"))
	f.git("config", "--add", "remote.origin.pushurl", refusing)
	if _, code := f.graft("", "merge", "feature/PROJ-42"); code == 0 {
		t.Fatal("expected first multi-URL push to fail")
	}
	out, code := f.graft("", "merge", "feature/PROJ-42")
	if code != 0 || !strings.Contains(out, refusing) || !strings.Contains(out, "git push") {
		t.Fatalf("retry did not report lagging push URL and finish: exit %d\n%s", code, out)
	}
}

func TestMergeRetryFetchesAdvancedPushURLHead(t *testing.T) {
	f := mergeRepo(t, "origin")
	mirror := filepath.Join(f.base, "mirror.git")
	f.run(f.base, "git", "clone", "-q", "--bare", f.remote("origin"), mirror)
	if err := os.WriteFile(filepath.Join(mirror, "hooks", "pre-receive"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.git("config", "--add", "remote.origin.pushurl", f.remote("origin"))
	f.git("config", "--add", "remote.origin.pushurl", mirror)
	if _, code := f.graft("", "merge", "feature/PROJ-42"); code == 0 {
		t.Fatal("expected partial push while the mirror rejects publication")
	}
	if err := os.Remove(filepath.Join(mirror, "hooks", "pre-receive")); err != nil {
		t.Fatal(err)
	}
	f.git("push", "-q", mirror, "feature/PROJ-42:main")
	advance := filepath.Join(f.base, "advance")
	f.run(f.base, "git", "clone", "-q", mirror, advance)
	f.run(advance, "git", "config", "user.name", "Reviewer")
	f.run(advance, "git", "config", "user.email", "review@example.com")
	if err := os.WriteFile(filepath.Join(advance, "ahead.txt"), []byte("ahead\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.run(advance, "git", "add", "ahead.txt")
	f.run(advance, "git", "commit", "-qm", "chore: advance push URL")
	f.run(advance, "git", "push", "-q", "origin", "main")
	f.git("config", "--replace-all", "remote.origin.pushurl", mirror)
	out, code := f.graft("", "merge", "feature/PROJ-42")
	if code != 0 || !strings.Contains(out, "already merged") || strings.Contains(out, "cannot check") {
		t.Fatalf("retry could not inspect advanced push URL with its object absent locally: exit %d\n%s", code, out)
	}
	if got, want := f.gitIn(mirror, "rev-parse", "main"), f.gitIn(advance, "rev-parse", "main"); got != want {
		t.Errorf("mirror main = %s, want advanced tip %s", got, want)
	}
}

func TestMergeRetryContinuesAfterUnreachablePushURL(t *testing.T) {
	f := mergeRepo(t, "origin")
	refusing := filepath.Join(f.base, "refusing.git")
	f.run(f.base, "git", "init", "-q", "--bare", "-b", "main", refusing)
	if err := os.WriteFile(filepath.Join(refusing, "hooks", "pre-receive"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	unreachable := filepath.Join(f.base, "unreachable.git")
	f.git("config", "--add", "remote.origin.pushurl", f.remote("origin"))
	f.git("config", "--add", "remote.origin.pushurl", refusing)
	f.git("config", "--add", "remote.origin.pushurl", unreachable)
	if _, code := f.graft("", "merge", "feature/PROJ-42"); code == 0 {
		t.Fatal("expected partial push with rejecting and unreachable URLs")
	}
	out, code := f.graft("", "merge", "feature/PROJ-42")
	if code == 0 || !strings.Contains(out, "cannot check "+unreachable) || !strings.Contains(out, refusing) || !strings.Contains(out, "catch it up with: git push") {
		t.Fatalf("retry did not continue through all push URLs with address-specific diagnostics: exit %d\n%s", code, out)
	}
}

// A source already on the remote trunk while the local trunk lags behind
// fast-forwards the local trunk.
func TestMergeAlreadyMergedInstallsBehindTrunk(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.git("push", "-q", "origin", "feature/PROJ-42:main")
	out := f.mustGraft("merge", "feature/PROJ-42")
	want := f.git("rev-parse", "feature/PROJ-42")
	if got := f.git("rev-parse", "main"); got != want || !strings.Contains(out, "local main moved to") {
		t.Errorf("local main = %s, want %s:\n%s", got, want, out)
	}
}

// A grafts file rewrites history for the checks but not for the push.
func TestMergeRefusesGraftsFile(t *testing.T) {
	f := mergeRepo(t, "origin")
	grafts := f.git("rev-parse", "--path-format=absolute", "--git-path", "info/grafts")
	if err := os.MkdirAll(filepath.Dir(grafts), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(grafts, []byte(f.git("rev-parse", "main")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	trunk := f.gitIn(f.remote("origin"), "rev-parse", "main")
	if out, code := f.graft("", "merge", "feature/PROJ-42"); code == 0 || !strings.Contains(out, "grafts file") {
		t.Errorf("merge did not refuse a grafts file: exit %d\n%s", code, out)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != trunk {
		t.Errorf("remote trunk moved to %s", got)
	}
}

// The tasks read refs/replace/ whatever GIT_REPLACE_REF_BASE graft runs
// with: a replace ref there is refused under another base too.
func TestMergeRefusesDefaultReplaceRefsUnderOtherBase(t *testing.T) {
	f := mergeRepo(t, "origin")
	f.git("replace", "-f", f.git("rev-parse", "feature/PROJ-42"), f.git("rev-parse", "main"))
	f.env = append(f.env, "GIT_REPLACE_REF_BASE=refs/alternate/")
	trunk := f.gitIn(f.remote("origin"), "rev-parse", "main")
	if out, code := f.graft("", "merge", "feature/PROJ-42"); code == 0 || !strings.Contains(out, "replace refs") {
		t.Errorf("merge did not refuse replace refs under another base: exit %d\n%s", code, out)
	}
	if got := f.gitIn(f.remote("origin"), "rev-parse", "main"); got != trunk {
		t.Errorf("remote trunk moved to %s", got)
	}
}

// graft merge moves the local trunk but runs no install task: it says what
// it moved and names an installed binary the new trunk left behind.
func TestMergeWarnsForStaleInstalledGoBinary(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none, reason: 'no version'}\ngate: []\ntasks:\n  build:\n    run: [[go, build, -o, bin/tool, .]]\n  install:\n    run: [{install: bin/tool}]\n",
		".gitignore":  "bin/\n",
		"go.mod":      "module example.org/tool\n\ngo 1.22\n",
		"main.go":     "package main\n\nfunc main() { println(\"one\") }\n",
	}, "origin")
	f.mustPlainCommit("chore: base")
	f.git("branch", "-m", "main")
	f.git("push", "-q", "origin", "main")
	f.git("fetch", "--quiet", "origin")
	f.git("branch", "--set-upstream-to", "origin/main", "main")
	f.git("remote", "set-head", "origin", "main")
	userBin, cfgHome := filepath.Join(f.base, "user-bin"), filepath.Join(f.base, "user-config")
	writeUserConfig(t, cfgHome, "tools:\n  bin_dir: "+yamlQuote(userBin)+"\n")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgHome, "APPDATA="+cfgHome)
	f.mustGraft("build")
	f.mustGraft("install")
	f.git("switch", "-q", "-c", "feature/PROJ-42")
	f.write("main.go", "package main\n\nfunc main() { println(\"two\") }\n")
	f.mustPlainCommit("feat: two")
	f.git("switch", "-q", "main")
	out := f.mustGraft("merge", "feature/PROJ-42")
	if strings.Contains(out, "installed main") || !strings.Contains(out, "local main moved to") {
		t.Errorf("merge reports the trunk move as an install:\n%s", out)
	}
	if !strings.Contains(out, "tool is from ") || !strings.Contains(out, "1 commit behind") || !strings.Contains(out, "run graft install") {
		t.Errorf("merge did not name the stale install:\n%s", out)
	}
}
