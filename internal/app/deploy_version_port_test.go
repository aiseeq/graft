package app

import (
	"context"
	"crypto/sha1"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/jira"
	"github.com/aiseeq/graft/v2/internal/version"
)

func versionPortTargets(files ...string) map[string][]version.Target {
	v := config.Version{File: files[0]}
	for _, f := range files[1:] {
		v.Sync = append(v.Sync, config.SyncFile{Path: f, Format: config.FormatPlain})
	}
	return versionTargetsByPath(v)
}

func versionPortRepo(t *testing.T) (*withheldRepo, string) {
	t.Helper()
	r := newEmptyWithheldRepo(t)
	r.writePath("work.txt", new("before\n"))
	r.writePath("VERSION", new("1\n"))
	r.commitAll("base")
	return r, r.head()
}

func versionPortCommit(r *withheldRepo, work, version, message string) string {
	r.t.Helper()
	r.writePath("work.txt", new(work))
	r.writePath("VERSION", new(version))
	r.commitAll(message)
	return r.head()
}

func versionPortGroupFixture(t *testing.T, partial bool) (*withheldRepo, string, string, string) {
	t.Helper()
	r, base := versionPortRepo(t)
	c1 := versionPortCommit(r, "restored\n", "2\n", "fix: PROJ-1 part one")
	c2 := versionPortCommit(r, "final\n", "3\n", "fix: PROJ-1 part two")
	r.git("checkout", "-q", "-b", "release", base)
	portWork := "final\n"
	if partial {
		portWork = "restored\n"
	}
	r.writePath("work.txt", new(portWork))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll(fmt.Sprintf("port of %s and %s", c1, c2))
	return r, port, c1, c2
}

func TestCoveredVersionPortsReplaysLinkedGroupAsAGroup(t *testing.T) {
	r, port, c1, head := versionPortGroupFixture(t, false)
	commits, err := deliveredCommits(r.repo, port, head)
	if err != nil {
		t.Fatal(err)
	}
	covered, err := coveredVersionPorts(r.repo, port, head, commits, versionPortTargets("VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if covered[c1] != port || covered[head] != port {
		t.Fatalf("covered = %#v, want both commits linked to %s", covered, port)
	}
}

func TestCoveredVersionPortsReplaysNonLinearGroupSequentially(t *testing.T) {
	r, base := versionPortRepo(t)
	c1 := versionPortCommit(r, "one\n", "2\n", "fix: PROJ-1 part one")
	r.git("checkout", "-q", "-b", "side", base)
	r.writePath("feat.txt", new("F\n"))
	c2 := r.commitAll("fix: PROJ-1 part two")
	r.git("checkout", "-q", "-")
	r.git("merge", "-q", "--no-ff", "-m", "merge side", "side")
	head := r.head()
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("one\n"))
	r.writePath("feat.txt", new("F\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll(fmt.Sprintf("port %s and %s", c1, c2))
	if got := versionPortCovered(t, r, port, head); got[c1] != port || got[c2] != port {
		t.Fatalf("non-linear group coverage=%v", got)
	}
}

func TestCoveredVersionPortsReplaysNonLinearGroupRejectsMissingMember(t *testing.T) {
	r, base := versionPortRepo(t)
	r.git("checkout", "-q", "-b", "side", base)
	r.writePath("feat.txt", new("F\n"))
	c2 := r.commitAll("fix: PROJ-1 side feature")
	r.git("checkout", "-q", "-")
	c1 := versionPortCommit(r, "one\n", "2\n", "fix: PROJ-1 main fix")
	r.git("merge", "-q", "--no-ff", "-m", "merge side", "side")
	head := r.head()
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("one\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll(fmt.Sprintf("port %s and %s", c1, c2))
	commits, err := deliveredCommits(r.repo, port, head)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := logCommits(r.repo, port)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := linkedVersionPortGroups(r.repo, port, prior, commits)
	if err != nil {
		t.Fatal(err)
	}
	var group []deliveredCommit
	for _, c := range groups[port] {
		group = append(group, c)
	}
	ordered, err := logCommits(r.repo, "--topo-order", "--reverse", port+".."+head)
	if err != nil {
		t.Fatal(err)
	}
	order := map[string]int{}
	for i, c := range ordered {
		order[c.sha] = i
	}
	slices.SortFunc(group, func(a, b deliveredCommit) int { return order[a.sha] - order[b.sha] })
	unchanged, err := replayVersionPortGroup(r.repo, port, group, versionPortTargets("VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if unchanged {
		t.Fatalf("non-linear replay hid missing side member: order=%v", []string{group[0].sha, group[len(group)-1].sha})
	}
}

func TestReplayVersionPortGroupRejectsSideFirstNonLinearGroup(t *testing.T) {
	r, base := versionPortRepo(t)
	r.git("checkout", "-q", "-b", "side", base)
	r.writePath("side.txt", new("side\n"))
	side := r.commitAll("fix: PROJ-1 side")
	r.git("checkout", "-q", base)
	main := versionPortCommit(r, "main\n", "2\n", "fix: PROJ-1 main")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("main\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll("port main")
	group := []deliveredCommit{{sha: side}, {sha: main}}
	unchanged, err := replayVersionPortGroup(r.repo, port, group, versionPortTargets("VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if unchanged {
		t.Fatal("replay accepted port missing the first side-branch commit")
	}
}

func TestReplayVersionPortGroupRejectsModifyDeleteConflictOutsideVersion(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	r.writePath("work.txt", new("before\n"))
	r.writePath("VERSION", new("1\n"))
	r.writePath("other.txt", new("base\n"))
	base := r.commitAll("base")
	r.git("checkout", "-q", "-b", "side", base)
	r.git("rm", "-q", "other.txt")
	source := r.commitAll("fix: PROJ-1 delete")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("other.txt", new("port edit\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll("port edit")
	unchanged, err := replayVersionPortGroup(r.repo, port, []deliveredCommit{{sha: source}}, versionPortTargets("VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if unchanged {
		t.Fatal("replay accepted modify/delete conflict outside version files")
	}
}

func TestLinkedVersionPortGroupsIgnoresExplicitLinkOutsidePreviousRange(t *testing.T) {
	r, base := versionPortRepo(t)
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("release.txt", new("release\n"))
	prev := r.commitAll("release point")
	r.git("checkout", "-q", base)
	c := versionPortCommit(r, "source\n", "2\n", "fix: PROJ-1 source")
	r.git("checkout", "-q", "-b", "unreleased-port", base)
	r.writePath("work.txt", new("source\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll("unreleased port")
	r.git("checkout", "-q", c)
	r.git("commit", "--allow-empty", "-qm", fmt.Sprintf("link %s to %s", c, port))
	link := r.head()
	commits, err := deliveredCommits(r.repo, prev, link)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := logCommits(r.repo, prev)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := linkedVersionPortGroups(r.repo, prev, prior, commits)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups[port]) != 0 {
		t.Fatalf("non-ancestor explicit port was linked: %#v", groups)
	}
}

func TestCoveredVersionPortsAcceptsSingleTransferWithVersionConflict(t *testing.T) {
	r, base := versionPortRepo(t)
	c := versionPortCommit(r, "restored\n", "2\n", "fix: PROJ-1 source")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("restored\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll(fmt.Sprintf("port %s", c))
	commits, err := deliveredCommits(r.repo, port, c)
	if err != nil {
		t.Fatal(err)
	}
	covered, err := coveredVersionPorts(r.repo, port, c, commits, versionPortTargets("VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if covered[c] != port {
		t.Fatalf("covered = %#v, want %s covered despite VERSION conflict", covered, c)
	}
}

func TestLinkedVersionPortGroupsAcceptsShortHashMention(t *testing.T) {
	r, base := versionPortRepo(t)
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll("port source")
	r.git("checkout", "-q", "-b", "source", base)
	r.writePath("work.txt", new("fixed\n"))
	source := r.commitAll("fix: PROJ-1 source references port " + port[:7])
	r.git("checkout", "-q", "release")
	r.git("merge", "-q", "--no-ff", "-m", "merge source", "source")
	head := r.head()
	commits, err := deliveredCommits(r.repo, port, head)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := logCommits(r.repo, port)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := linkedVersionPortGroups(r.repo, port, prior, commits)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := groups[port][source]; !ok {
		t.Fatalf("short hash mention did not link source to port: %#v", groups)
	}
}

func TestLinkedVersionPortGroupsRejectsAmbiguousShortHashMention(t *testing.T) {
	const prefix = "abcdef0"
	r, _ := versionPortRepo(t)
	prior := []deliveredCommit{
		{sha: prefix + strings.Repeat("1", 33)},
		{sha: prefix + strings.Repeat("2", 33)},
	}
	commits := []deliveredCommit{{sha: strings.Repeat("3", 40), subject: "port mentions " + prefix}}
	linked := false
	err := addSourcesNamingPort(r.repo, prior, commits, func(string, deliveredCommit) { linked = true })
	if err != nil {
		t.Fatal(err)
	}
	if linked {
		t.Fatal("ambiguous seven-character hash mention linked a source")
	}
}

func TestExplicitVersionPortLinkRejectsPrefixAmbiguousInReleaseRange(t *testing.T) {
	r, base := versionPortRepo(t)
	source := versionPortCommit(r, "fixed\n", "2\n", "fix: PROJ-123 source")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll("port source")
	r.git("checkout", "-q", source)
	r.git("commit", "--allow-empty", "-qm", "link "+source[:7]+" to "+port)
	link := r.head()
	fake := deliveredCommit{sha: source[:7] + strings.Repeat("f", len(source)-7)}
	commits := []deliveredCommit{{sha: source}, fake, {sha: link, subject: "link " + source[:7] + " to " + port}}
	groups := map[string]map[string]deliveredCommit{}
	add := func(port string, c deliveredCommit) {
		if groups[port] == nil {
			groups[port] = map[string]deliveredCommit{}
		}
		groups[port][c.sha] = c
	}
	if err := addExplicitVersionPortLinks(r.repo, port, commits, add); err != nil {
		t.Fatal(err)
	}
	if len(groups[port]) != 0 {
		t.Fatalf("ambiguous sample prefix linked commits: %#v", groups[port])
	}
}

func TestExplicitVersionPortLinkRejectsSelfReference(t *testing.T) {
	r, _ := versionPortRepo(t)
	self := versionPortCommit(r, "synthetic work\n", "2\n", "fix: PROJ-1 synthetic source")
	prev := versionPortCommit(r, "synthetic work\n", "3\n", "chore: later synthetic commit")
	commits := []deliveredCommit{{sha: self, subject: "chore: link " + self}}
	groups := map[string]map[string]deliveredCommit{}
	add := func(port string, c deliveredCommit) {
		if groups[port] == nil {
			groups[port] = map[string]deliveredCommit{}
		}
		groups[port][c.sha] = c
	}
	if err := addExplicitVersionPortLinks(r.repo, prev, commits, add); err != nil {
		t.Fatal(err)
	}
	if len(groups) != 0 {
		t.Fatalf("explicit self-reference created a port group: %#v", groups)
	}
}

func TestUniqueCommitPrefixRejectsAmbiguousRepositoryObjects(t *testing.T) {
	r, _ := versionPortRepo(t)
	tree, err := r.repo.Git("rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	tree = strings.TrimSpace(tree)
	seen := map[string]string{}
	var first, second, prefix string
	for i := range 100_000 {
		content := fmt.Sprintf("tree %s\nauthor Test <test@example.invalid> 0 +0000\ncommitter Test <test@example.invalid> 0 +0000\n\nsynthetic %06d\n", tree, i)
		header := fmt.Sprintf("commit %d\x00%s", len(content), content)
		sum := sha1.Sum([]byte(header))
		objectID := fmt.Sprintf("%x", sum)
		if prior, ok := seen[objectID[:7]]; ok {
			first, second, prefix = prior, content, objectID[:7]
			break
		}
		seen[objectID[:7]] = content
	}
	if prefix == "" {
		t.Fatal("did not find a synthetic seven-character object prefix collision")
	}
	var objectIDs []string
	for _, content := range []string{first, second} {
		out, err := r.repo.GitInput(strings.NewReader(content), nil, "hash-object", "-t", "commit", "-w", "--stdin")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(strings.TrimSpace(out), prefix) {
			t.Fatalf("stored object %s does not match synthetic prefix %s", strings.TrimSpace(out), prefix)
		}
		objectIDs = append(objectIDs, strings.TrimSpace(out))
	}
	if resolved, ok, err := uniqueCommitPrefix(r.repo, prefix); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatalf("ambiguous repository prefix resolved to %s", resolved)
	}
	link := deliveredCommit{sha: strings.Repeat("e", 40), subject: "link " + prefix + " to " + strings.TrimSpace(strings.TrimSpace(r.head()))}
	groups := map[string]map[string]deliveredCommit{}
	add := func(port string, c deliveredCommit) {
		if groups[port] == nil {
			groups[port] = map[string]deliveredCommit{}
		}
		groups[port][c.sha] = c
	}
	if err := addExplicitVersionPortLinks(r.repo, r.head(), []deliveredCommit{{sha: objectIDs[0]}, link}, add); err != nil {
		t.Fatal(err)
	}
	if len(groups) != 0 {
		t.Fatalf("repository-ambiguous source prefix created a link: %#v", groups)
	}
}

func TestExplicitVersionPortLinkAddsVersionOnlyLinkCommitToGroup(t *testing.T) {
	r, base := versionPortRepo(t)
	source := versionPortCommit(r, "fixed\n", "2\n", "fix: PROJ-123 source")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll("port source")
	r.git("checkout", "-q", source)
	r.git("commit", "--allow-empty", "-qm", "link "+source[:12]+" to "+port[:12])
	link := r.head()
	groups := map[string]map[string]deliveredCommit{}
	add := func(port string, c deliveredCommit) {
		if groups[port] == nil {
			groups[port] = map[string]deliveredCommit{}
		}
		groups[port][c.sha] = c
	}
	linkCommit := deliveredCommit{sha: link, subject: "link " + source[:12] + " to " + port[:12]}
	if err := addExplicitVersionPortLinks(r.repo, port, []deliveredCommit{{sha: source}, linkCommit}, add); err != nil {
		t.Fatal(err)
	}
	if _, ok := groups[port][link]; !ok {
		t.Fatalf("version-only link commit was not added to its port group: %#v", groups[port])
	}
}

func TestCoveredVersionPortIgnoresModuleFilesDuringReplay(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	if err := os.MkdirAll(filepath.Join(configHome, "graft"), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := "modules:\n  latest: [example.com/own/]\n"
	if err := os.WriteFile(filepath.Join(configHome, "graft", "config.yaml"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newEmptyWithheldRepo(t)
	mod := func(v string) string {
		return "module example.com/test\n\ngo 1.23\n\nrequire example.com/own/mod " + v + "\n"
	}
	r.writePath("work.txt", new("before\n"))
	r.writePath("go.mod", new(mod("v1.0.0")))
	r.writePath("go.sum", new("example.com/own/mod v1.0.0 h1:old\n"))
	r.writePath("VERSION", new("1\n"))
	base := r.commitAll("base")
	r.git("checkout", "-q", "-b", "source", base)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("go.mod", new(mod("v1.1.0")))
	r.writePath("go.sum", new("example.com/own/mod v1.1.0 h1:new\n"))
	r.writePath("VERSION", new("2\n"))
	source := r.commitAll("fix: PROJ-1 source")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll("port source " + source)
	r.git("checkout", "-q", "-b", "deploy", port)
	r.writePath("go.mod", new(mod("v1.1.0")))
	r.writePath("go.sum", new("example.com/own/mod v1.1.0 h1:new\n"))
	prev := r.commitAll("chore: module upgrade")
	portCommits, err := deliveredCommits(r.repo, port, source)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := logCommits(r.repo, prev)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := linkedVersionPortGroups(r.repo, prev, prior, portCommits)
	if err != nil {
		t.Fatal(err)
	}
	group := []deliveredCommit{groups[port][source]}
	replayed, err := replayVersionPortGroup(r.repo, port, group, versionPortTargets("VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if !replayed {
		t.Fatal("module-only replay differences rejected")
	}
	covered := versionPortCovered(t, r, prev, source)
	if covered[source] != port {
		t.Fatalf("module-only replay differences prevented coverage: %#v", covered)
	}
}

// A replay whose go.mod conflicts leaves conflict markers in the merged
// go.mod. That go.mod shows no module raise: the port is not linked, and the
// check goes on instead of failing on the markers.
func TestReplayWithConflictedGoModIsNotLinked(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	if err := os.MkdirAll(filepath.Join(configHome, "graft"), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := "modules:\n  latest: [example.com/own/]\n"
	if err := os.WriteFile(filepath.Join(configHome, "graft", "config.yaml"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newEmptyWithheldRepo(t)
	mod := func(v string) string {
		return "module example.com/test\n\ngo 1.23\n\nrequire example.com/own/mod " + v + "\n"
	}
	r.writePath("work.txt", new("before\n"))
	r.writePath("go.mod", new(mod("v1.0.0")))
	r.writePath("go.sum", new("example.com/own/mod v1.0.0 h1:old\n"))
	r.writePath("VERSION", new("1\n"))
	base := r.commitAll("base")
	r.git("checkout", "-q", "-b", "source", base)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("go.mod", new(mod("v1.1.0")))
	r.writePath("go.sum", new("example.com/own/mod v1.1.0 h1:new\n"))
	r.writePath("VERSION", new("2\n"))
	source := r.commitAll("fix: PROJ-1 source")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("go.mod", new(mod("v1.0.5")))
	r.writePath("go.sum", new("example.com/own/mod v1.0.5 h1:mid\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll("port source " + source)
	group := []deliveredCommit{{sha: source}}
	replayed, err := replayVersionPortGroup(r.repo, port, group, versionPortTargets("VERSION"))
	if err != nil {
		t.Fatalf("replay with a conflicted go.mod failed the check: %v", err)
	}
	if replayed {
		t.Fatal("replay with a conflicted go.mod linked the port")
	}
}

func TestReplayRejectsNewOrLoweredGoModuleRequirement(t *testing.T) {
	for _, tc := range []struct {
		name, before, after, beforeSum, afterSum string
	}{
		{
			name:      "new require",
			before:    "module example.com/test\n\ngo 1.23\n\nrequire example.com/own/mod v1.0.0\n",
			after:     "module example.com/test\n\ngo 1.23\n\nrequire example.com/own/mod v1.0.0\nrequire example.com/new/mod v1.0.0\n",
			beforeSum: "example.com/own/mod v1.0.0 h1:old\n",
			afterSum:  "example.com/own/mod v1.0.0 h1:old\n",
		},
		{
			name:      "downgrade",
			before:    "module example.com/test\n\ngo 1.23\n\nrequire example.com/own/mod v1.2.0\n",
			after:     "module example.com/test\n\ngo 1.23\n\nrequire example.com/own/mod v1.1.0\n",
			beforeSum: "example.com/own/mod v1.2.0 h1:old\n",
			afterSum:  "example.com/own/mod v1.2.0 h1:old\n",
		},
		{
			name:      "require directive comment changed",
			before:    "module example.com/test\n\ngo 1.23\n\nrequire example.com/own/mod v1.0.0 // indirect\n",
			after:     "module example.com/test\n\ngo 1.23\n\nrequire example.com/own/mod v1.1.0 // direct\n",
			beforeSum: "example.com/own/mod v1.0.0 h1:old\n",
			afterSum:  "example.com/own/mod v1.1.0 h1:new\n",
		},
		{
			name:      "replaced require",
			before:    "module example.com/test\n\ngo 1.23\n\nrequire example.com/own/mod v1.0.0\n\nreplace example.com/own/mod => example.com/fork/mod v1.0.0\n",
			after:     "module example.com/test\n\ngo 1.23\n\nrequire example.com/own/mod v1.1.0\n\nreplace example.com/own/mod => example.com/fork/mod v1.0.0\n",
			beforeSum: "example.com/own/mod v1.0.0 h1:old\n",
			afterSum:  "example.com/own/mod v1.1.0 h1:new\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newEmptyWithheldRepo(t)
			r.writePath("go.mod", new(tc.before))
			r.writePath("go.sum", new(tc.beforeSum))
			r.writePath("work.txt", new("before\n"))
			r.writePath("VERSION", new("1\n"))
			base := r.commitAll("base")
			r.git("checkout", "-q", "-b", "source", base)
			r.writePath("go.mod", new(tc.after))
			r.writePath("go.sum", new(tc.afterSum))
			r.writePath("work.txt", new("fixed\n"))
			r.writePath("VERSION", new("2\n"))
			source := r.commitAll("fix: PROJ-123 change modules")
			beforeTree, err := versionBlobTree(r.repo, base)
			if err != nil {
				t.Fatal(err)
			}
			afterTree, err := versionBlobTree(r.repo, source)
			if err != nil {
				t.Fatal(err)
			}
			moduleFilesDiffer, err := versionPortModuleFilesCanDiffer(r.repo, nil, beforeTree, afterTree)
			if err != nil {
				t.Fatal(err)
			}
			if moduleFilesDiffer {
				t.Fatal("non-mechanical module change was accepted as a version raise")
			}
			r.git("checkout", "-q", "-b", "release", base)
			r.writePath("work.txt", new("fixed\n"))
			r.writePath("VERSION", new("9\n"))
			port := r.commitAll("port source " + source)
			replayed, err := replayVersionPortGroup(r.repo, port, []deliveredCommit{{sha: source}}, versionPortTargets("VERSION"))
			if err != nil {
				t.Fatal(err)
			}
			if replayed {
				t.Fatal("non-mechanical module change was ignored during replay")
			}
		})
	}
}

func TestReplayVersionConflictReadsAutoMergedModulesFromReplayEnvironment(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	mod := func(a, b string) string {
		return "module example.com/test\n\ngo 1.23\n\nrequire example.com/a " + a + "\n\n// independent requirement\nrequire example.com/b " + b + "\n"
	}
	r.writePath("go.mod", new(mod("v1.0.0", "v1.0.0")))
	r.writePath("go.sum", new("example.com/a v1.0.0 h1:old-a\n\nexample.com/b v1.0.0 h1:old-b\n"))
	r.writePath("VERSION", new("1\n"))
	base := r.commitAll("base")
	r.git("checkout", "-q", "-b", "source", base)
	r.writePath("go.mod", new(mod("v1.1.0", "v1.0.0")))
	r.writePath("go.sum", new("example.com/a v1.1.0 h1:new-a\n\nexample.com/b v1.0.0 h1:old-b\n"))
	r.writePath("VERSION", new("2\n"))
	source := r.commitAll("fix: PROJ-123 module a")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("go.mod", new(mod("v1.0.0", "v1.1.0")))
	r.writePath("go.sum", new("example.com/a v1.0.0 h1:old-a\n\nexample.com/b v1.1.0 h1:new-b\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll("port module b")
	unchanged, err := replayVersionPortGroup(r.repo, port, []deliveredCommit{{sha: source}}, versionPortTargets("VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if !unchanged {
		t.Fatal("VERSION-only conflict with an auto-merged module upgrade should replay cleanly")
	}
}

func TestCoveredVersionPortAcceptsTwelveCharacterExplicitLink(t *testing.T) {
	r, base := versionPortRepo(t)
	r.git("checkout", "-q", "-b", "source", base)
	r.writePath("work.txt", new("fixed\n"))
	source := r.commitAll("fix: PROJ-1 source")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll("port")
	r.git("merge", "-q", "--no-ff", "-m", "merge source", "source")
	r.writePath("VERSION", new("10\n"))
	r.git("commit", "-qam", "chore: link source "+source[:12]+" port "+port[:12])
	head := r.head()
	commits, err := deliveredCommits(r.repo, port, head)
	if err != nil {
		t.Fatal(err)
	}
	covered, err := coveredVersionPorts(r.repo, port, head, commits, versionPortTargets("VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if covered[source] != port {
		t.Fatalf("explicit twelve-character link did not cover source: %#v", covered)
	}
	if covered[head] != port {
		t.Fatalf("version-only link note was not treated as covered metadata: %#v", covered)
	}
}

func TestCoveredVersionPortsRejectsConflictWithNonVersionSyncChange(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	r.writePath("work.txt", new("before\n"))
	r.writePath("VERSION", new("1\n"))
	r.writePath("web/package.json", new("{\n  \"version\": \"1\",\n  \"dependencies\": {}\n}\n"))
	base := r.commitAll("base")
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("VERSION", new("2\n"))
	r.writePath("web/package.json", new("{\n  \"version\": \"2\",\n  \"dependencies\": {\"evil\": \"1.0\"}\n}\n"))
	c := r.commitAll("fix: PROJ-1 source")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("VERSION", new("9\n"))
	r.writePath("web/package.json", new("{\n  \"version\": \"9\",\n  \"dependencies\": {}\n}\n"))
	port := r.commitAll(fmt.Sprintf("port %s", c))
	commits, err := deliveredCommits(r.repo, port, c)
	if err != nil {
		t.Fatal(err)
	}
	targets := versionTargetsByPath(config.Version{File: "VERSION", Sync: []config.SyncFile{{Path: "web/package.json", Format: config.FormatJSON, Key: []string{"version"}}}})
	replayed, err := replayVersionPortGroup(r.repo, port, commits, targets)
	if err != nil {
		t.Fatal(err)
	}
	if replayed {
		t.Fatal("conflicted replay with an independent sync-file edit was accepted")
	}
}

func TestCoveredVersionPortsLeavesContentOnlyMatchActiveAndAdvisesLink(t *testing.T) {
	r, base := versionPortRepo(t)
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("restored\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll("port PROJ-1 from prior release")
	r.git("checkout", "-q", base)
	c := versionPortCommit(r, "restored\n", "2\n", "fix: PROJ-1 source")
	commits, err := deliveredCommits(r.repo, port, c)
	if err != nil {
		t.Fatal(err)
	}
	covered, err := coveredVersionPorts(r.repo, port, c, commits, versionPortTargets("VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if len(covered) != 0 {
		t.Fatalf("covered = %#v, want content-only source to remain active", covered)
	}
	hints, err := unlinkedContentVersionPorts(r.repo, port, c, commits, versionPortTargets("VERSION"), regexp.MustCompile(`PROJ-[0-9]+`), &config.ApprovedRelease{}, covered)
	if err != nil {
		t.Fatal(err)
	}
	if len(hints["PROJ-1"]) != 1 || hints["PROJ-1"][0] != (versionPortHint{commit: c, port: port}) {
		t.Fatalf("content-only hints = %#v, want source and port", hints)
	}
}

func TestUnlinkedContentVersionPortsDoesNotAdviseForALinkedCommit(t *testing.T) {
	r, base := versionPortRepo(t)
	r.writePath("VERSION", new("2\n"))
	c := r.commitAll("fix: PROJ-1 version only")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("port-only\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll(fmt.Sprintf("port: PROJ-1 source %s", c))
	prior, err := logCommits(r.repo, port)
	if err != nil {
		t.Fatal(err)
	}
	commits, err := deliveredCommits(r.repo, port, c)
	if err != nil {
		t.Fatal(err)
	}
	hints, err := unlinkedContentVersionPortsWithPrior(r.repo, port, c, prior, commits, versionPortTargets("VERSION"), regexp.MustCompile(`PROJ-[0-9]+`), &config.ApprovedRelease{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hints["PROJ-1"]) != 0 {
		t.Fatalf("hash-linked source received content-only advice: %#v", hints)
	}
}

func TestUnlinkedContentVersionPortsDoesNotTreatCommitAsItsOwnPort(t *testing.T) {
	r, base := versionPortRepo(t)
	self := versionPortCommit(r, "restored\n", "2\n", "fix: PROJ-1 source")
	prior, err := logCommits(r.repo, self)
	if err != nil {
		t.Fatal(err)
	}
	commits, err := deliveredCommits(r.repo, base, self)
	if err != nil {
		t.Fatal(err)
	}
	hints, err := unlinkedContentVersionPortsWithPrior(r.repo, self, self, prior, commits, versionPortTargets("VERSION"), regexp.MustCompile(`PROJ-[0-9]+`), &config.ApprovedRelease{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hints) != 0 {
		t.Fatalf("self-port hints = %#v, want none", hints)
	}
}

func TestUnlinkedContentVersionPortsRejectsUnmatchedPortContent(t *testing.T) {
	r, base := versionPortRepo(t)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("extra.txt", new("source-only\n"))
	r.writePath("VERSION", new("2\n"))
	head := r.commitAll("fix: PROJ-1 source")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("VERSION", new("9\n"))
	prev := r.commitAll("port PROJ-1 without a hash link")
	commits, err := deliveredCommits(r.repo, prev, head)
	if err != nil {
		t.Fatal(err)
	}
	hints, err := unlinkedContentVersionPorts(r.repo, prev, head, commits, versionPortTargets("VERSION"), regexp.MustCompile(`PROJ-[0-9]+`), &config.ApprovedRelease{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hints["PROJ-1"]) != 0 {
		t.Fatalf("port that omits source content received advice: %#v", hints)
	}
}

func TestUnlinkedContentVersionPortsSkipsAlreadyCoveredSource(t *testing.T) {
	r, base := versionPortRepo(t)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("VERSION", new("2\n"))
	c := r.commitAll("fix: PROJ-1 source")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll(fmt.Sprintf("port %s", c))
	r.writePath("extra.txt", new("second port remains\n"))
	r.writePath("VERSION", new("10\n"))
	prev := r.commitAll("port PROJ-1 independently")
	commits, err := deliveredCommits(r.repo, prev, c)
	if err != nil {
		t.Fatal(err)
	}
	covered, err := coveredVersionPorts(r.repo, prev, c, commits, versionPortTargets("VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if covered[c] != port {
		t.Fatalf("source was not covered by linked port: %v", covered)
	}
	prior, err := logCommits(r.repo, prev)
	if err != nil {
		t.Fatal(err)
	}
	hints, err := unlinkedContentVersionPortsWithPrior(r.repo, prev, c, prior, commits, versionPortTargets("VERSION"), regexp.MustCompile(`PROJ-[0-9]+`), &config.ApprovedRelease{}, covered)
	if err != nil {
		t.Fatal(err)
	}
	if len(hints["PROJ-1"]) != 0 {
		t.Fatalf("already-covered source got a second-port hint: %#v", hints)
	}
}

func TestUnlinkedContentVersionPortsOnlySuggestsUnlinkedCandidates(t *testing.T) {
	r, base := versionPortRepo(t)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("VERSION", new("2\n"))
	c := r.commitAll("fix: PROJ-1 source")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll(fmt.Sprintf("port %s", c))
	commits, err := deliveredCommits(r.repo, port, c)
	if err != nil {
		t.Fatal(err)
	}
	hints, err := unlinkedContentVersionPorts(r.repo, port, c, commits, versionPortTargets("VERSION"), regexp.MustCompile(`PROJ-[0-9]+`), &config.ApprovedRelease{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hints["PROJ-1"]) != 0 {
		t.Fatalf("linked source got content-only advice: %#v", hints)
	}
}

func TestUnlinkedContentVersionPortsIgnoresRevertedSource(t *testing.T) {
	r, base := versionPortRepo(t)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("VERSION", new("2\n"))
	source := r.commitAll("fix: PROJ-1 source")
	r.revert(source)
	head := r.head()
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("VERSION", new("9\n"))
	prev := r.commitAll("port PROJ-1 without a hash link")
	commits, err := deliveredCommits(r.repo, prev, head)
	if err != nil {
		t.Fatal(err)
	}
	reverted, err := revertedInRange(r.repo, commits)
	if err != nil {
		t.Fatal(err)
	}
	revertedBy := map[string]string{}
	for sha := range reverted {
		revertedBy[sha] = "revert"
	}
	commits = notRevertedVersionPortCommits(commits, revertedBy)
	hints, err := unlinkedContentVersionPorts(r.repo, prev, head, commits, versionPortTargets("VERSION"), regexp.MustCompile(`PROJ-[0-9]+`), &config.ApprovedRelease{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hints["PROJ-1"]) != 0 {
		t.Fatalf("reverted source or removed port received content-only advice: %#v", hints)
	}
}

func TestGateRunFiltersRevertedCommitsBeforePortAdvice(t *testing.T) {
	r, base := versionPortRepo(t)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("VERSION", new("2\n"))
	source := r.commitAll("fix: PROJ-1 source")
	r.revert(source)
	head := r.head()
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("VERSION", new("9\n"))
	prev := r.commitAll("port PROJ-1 without a hash link")
	g, _ := versionPortGateRun(r)
	if _, err := g.run(context.Background(), prev, head, nil, false); err != nil {
		t.Fatal(err)
	}
	if len(g.contentOnlyPorts) != 0 {
		t.Fatalf("gate advised content already removed by a revert: %#v", g.contentOnlyPorts)
	}
}

func TestUnlinkedContentVersionPortsIgnoresRemovedPort(t *testing.T) {
	r, base := versionPortRepo(t)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("VERSION", new("2\n"))
	_ = r.commitAll("fix: PROJ-1 source")
	head := r.head()
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll("port PROJ-1 without a hash link")
	r.revert(port)
	prev := r.head()
	commits, err := deliveredCommits(r.repo, prev, head)
	if err != nil {
		t.Fatal(err)
	}
	hints, err := unlinkedContentVersionPorts(r.repo, prev, head, commits, versionPortTargets("VERSION"), regexp.MustCompile(`PROJ-[0-9]+`), &config.ApprovedRelease{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hints["PROJ-1"]) != 0 {
		t.Fatalf("removed port received content-only advice: %#v", hints)
	}
}

func TestCoveredVersionPortsAcceptsExplicitRangeLinkForHashlessPort(t *testing.T) {
	r, base := versionPortRepo(t)
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("restored\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll("port PROJ-1 from prior release")
	r.git("checkout", "-q", base)
	c := versionPortCommit(r, "restored\n", "2\n", fmt.Sprintf("fix: PROJ-1 source port %s", port))
	r.git("checkout", "-q", c)
	r.git("commit", "--allow-empty", "-qm", fmt.Sprintf("link port %s to source %s", port, c))
	head := r.head()
	commits, err := deliveredCommits(r.repo, port, head)
	if err != nil {
		t.Fatal(err)
	}
	covered, err := coveredVersionPorts(r.repo, port, head, commits, versionPortTargets("VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if covered[c] != port {
		t.Fatalf("covered = %#v, want explicit link to cover %s", covered, c)
	}
}

func TestCoveredVersionPortsKeepsPartialGroupActive(t *testing.T) {
	r, port, c1, head := versionPortGroupFixture(t, true)
	commits, err := deliveredCommits(r.repo, port, head)
	if err != nil {
		t.Fatal(err)
	}
	covered, err := coveredVersionPorts(r.repo, port, head, commits, versionPortTargets("VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if len(covered) != 0 {
		t.Fatalf("covered = %#v, want partial port to leave group active (%s)", covered, c1)
	}
}

func TestCoveredVersionPortsRequiresPortToBeInPrev(t *testing.T) {
	r, base := versionPortRepo(t)
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("restored\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll("port PROJ-1")
	r.git("checkout", "-q", base)
	c := versionPortCommit(r, "restored\n", "2\n", "fix PROJ-1 source")
	r.git("commit", "--allow-empty", "-qm", fmt.Sprintf("link port %s to source %s", port, c))
	head := r.head()
	commits, err := deliveredCommits(r.repo, base, head)
	if err != nil {
		t.Fatal(err)
	}
	covered, err := coveredVersionPorts(r.repo, base, head, commits, versionPortTargets("VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if len(covered) != 0 {
		t.Fatalf("covered = %#v, want non-ancestor port to leave commit active", covered)
	}
}

func TestCoveredVersionPortsRejectsRevertedPortAtPrev(t *testing.T) {
	r, base := versionPortRepo(t)
	c := versionPortCommit(r, "fix\n", "2\n", "fix: PROJ-1 source")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("fix\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll(fmt.Sprintf("fix: PROJ-1 port %s", c))
	r.revert(port)
	prev := r.head()
	if got := r.git("show", prev+":work.txt"); got != "before" {
		t.Fatalf("prev work=%q", got)
	}
	if got := versionPortCovered(t, r, prev, c); got[c] != "" {
		t.Fatalf("reverted port still covers: %v", got)
	}
}

func TestCoveredVersionPortsRejectsExplicitLinkToDeletedContent(t *testing.T) {
	r, _ := versionPortRepo(t)
	old := versionPortCommit(r, "feature\n", "2\n", "feat: PROJ-5 old feature")
	versionPortCommit(r, "before\n", "3\n", "chore: PROJ-6 drop feature")
	prev := r.head()
	c := versionPortCommit(r, "feature\n", "4\n", "feat: PROJ-7 new feature")
	r.git("commit", "--allow-empty", "-qm", fmt.Sprintf("link %s to %s", c, old))
	head := r.head()
	if got := versionPortCovered(t, r, prev, head); got[c] != "" {
		t.Fatalf("deleted old content still covers: %v", got)
	}
}

func TestCoveredVersionPortsChecksSyncFileOutsideVersion(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	r.writePath("work.txt", new("before\n"))
	r.writePath("VERSION", new("1\n"))
	r.writePath("web/package.json", new("{\n  \"version\": \"1\",\n  \"dependencies\": {}\n}\n"))
	base := r.commitAll("base")
	r.writePath("work.txt", new("fix\n"))
	r.writePath("VERSION", new("2\n"))
	r.writePath("web/package.json", new("{\n  \"version\": \"2\",\n  \"dependencies\": {\"evil\": \"1.0\"}\n}\n"))
	c := r.commitAll("fix: PROJ-1 source")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("fix\n"))
	r.writePath("VERSION", new("9\n"))
	r.writePath("web/package.json", new("{\n  \"version\": \"9\",\n  \"dependencies\": {}\n}\n"))
	p := r.commitAll(fmt.Sprintf("port %s", c))
	commits, err := deliveredCommits(r.repo, p, c)
	if err != nil {
		t.Fatal(err)
	}
	targets := versionTargetsByPath(config.Version{File: "VERSION", Sync: []config.SyncFile{{Path: "web/package.json", Format: config.FormatJSON, Key: []string{"version"}}}})
	covered, err := coveredVersionPorts(r.repo, p, c, commits, targets)
	if err != nil {
		t.Fatal(err)
	}
	if covered[c] != "" {
		t.Fatalf("sync dependency addition covered: %v", covered)
	}
}

func TestCoveredVersionPortsRejectsDeletedVersionFile(t *testing.T) {
	r, base := versionPortRepo(t)
	r.writePath("VERSION", nil)
	r.writePath("work.txt", new("fix\n"))
	c := r.commitAll("fix: PROJ-1 delete version")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("fix\n"))
	r.writePath("VERSION", new("9\n"))
	p := r.commitAll(fmt.Sprintf("port %s", c))
	if got := versionPortCovered(t, r, p, c); got[c] != "" {
		t.Fatalf("deleted VERSION covered: %v", got)
	}
}

func TestCoveredVersionPortsReplayRejectsMissingVersionTarget(t *testing.T) {
	r, base := versionPortRepo(t)
	r.writePath("VERSION", nil)
	r.writePath("work.txt", new("fixed\n"))
	c := r.commitAll("fix: PROJ-1 delete version")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("VERSION", nil)
	port := r.commitAll(fmt.Sprintf("port %s", c))
	commits, err := deliveredCommits(r.repo, port, c)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := replayVersionPortGroup(r.repo, port, commits, versionPortTargets("VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if replayed {
		t.Fatal("replay without a required version target was accepted")
	}
}

func TestCoveredVersionPortsAcceptsSourceCherryPickTrailer(t *testing.T) {
	r, base := versionPortRepo(t)
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("fix\n"))
	r.writePath("VERSION", new("9\n"))
	p := r.commitAll("fix: PROJ-1 hotfix")
	r.git("checkout", "-q", "-")
	c := versionPortCommit(r, "fix\n", "2\n", fmt.Sprintf("fix: PROJ-1 hotfix\n\n(cherry picked from commit %s)", p))
	if got := versionPortCovered(t, r, p, c); got[c] != p {
		t.Fatalf("source trailer coverage=%v", got)
	}
}

func TestCoveredVersionPortsFiltersRevertedGroupMember(t *testing.T) {
	r, base := versionPortRepo(t)
	r.writePath("feat.txt", new("F\n"))
	c1 := versionPortCommit(r, "fix\n", "2\n", "feat: PROJ-1 fix and feature")
	r.writePath("feat.txt", nil)
	c2 := versionPortCommit(r, "fix\n", "3\n", "chore: PROJ-1 drop feature")
	r.revert(c2)
	head := r.head()
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("fix\n"))
	r.writePath("VERSION", new("9\n"))
	p := r.commitAll(fmt.Sprintf("port %s %s", c1, c2))
	if got := versionPortCovered(t, r, p, head); got[c1] != "" {
		t.Fatalf("group covered after member revert: %v", got)
	}
}

func TestCoveredVersionPortsDoesNotAttributeRevertedGroupMember(t *testing.T) {
	r, base := versionPortRepo(t)
	r.writePath("first.txt", new("first\n"))
	c1 := r.commitAll("fix: PROJ-1 first work")
	r.writePath("second.txt", new("second\n"))
	c2 := r.commitAll("fix: PROJ-1 second work")
	r.revert(c1)
	head := r.head()
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("second.txt", new("second\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll(fmt.Sprintf("port %s and %s", c1, c2))
	covered := versionPortCovered(t, r, port, head)
	if covered[c2] != port || covered[c1] != "" {
		t.Fatalf("coverage attribution for reverted member: %v", covered)
	}
}

func TestCoveredVersionPortsRejectsRenameWithExtraEdit(t *testing.T) {
	r, _ := versionPortRepo(t)
	r.writePath("a.txt", new(strings.Repeat("line\n", 30)))
	base := r.commitAll("add a")
	r.writePath("a.txt", nil)
	r.writePath("b.txt", new(strings.Repeat("line\n", 30)+"evil\n"))
	c := versionPortCommit(r, "before\n", "2\n", "refactor: PROJ-1 rename with edit")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("a.txt", nil)
	r.writePath("b.txt", new(strings.Repeat("line\n", 30)))
	r.writePath("VERSION", new("9\n"))
	p := r.commitAll(fmt.Sprintf("port %s", c))
	if got := versionPortCovered(t, r, p, c); got[c] != "" {
		t.Fatalf("rename with extra edit covered: %v", got)
	}
}

func TestCoveredVersionPortOnlySkipsVersionActivity(t *testing.T) {
	var output strings.Builder
	g := &gateRun{app: &App{Stdout: &output}, conf: &config.ApprovedRelease{}, keyRe: regexp.MustCompile(`PROJ-[0-9]+`), activeKeys: map[string]bool{}}
	c := deliveredCommit{sha: "abcdef0123456789", subject: "fix: PROJ-1"}
	if err := g.recordVersionActivity(c, c.subject, "1234567890abcdef"); err != nil {
		t.Fatal(err)
	}
	if g.activeKeys["PROJ-1"] || g.activeCommits != 0 {
		t.Fatalf("covered work became version-active: keys=%v commits=%d", g.activeKeys, g.activeCommits)
	}
	if !strings.Contains(output.String(), "already on the approved target") {
		t.Fatalf("missing gate explanation: %s", output.String())
	}
}

func TestNotInVersionUsesAssociationForContentOnlyPort(t *testing.T) {
	conf := &config.ApprovedRelease{}
	g := &gateRun{conf: conf, contentOnlyPorts: map[string][]versionPortHint{"PROJ-2": {{commit: "c0ffee0000000000000000000000000000000000", port: "1234567890123456789012345678901234567890"}}}, gate: &approvalGate{conf: conf}}
	v := &releaseVersionCheck{gate: g, sources: map[string][]string{"PROJ-1": {"PROJ-2"}}, selected: jira.Version{Name: "v2"}}
	msg := v.notInVersion(context.Background(), jira.Issue{Key: "PROJ-1", Status: "Done"})
	if !strings.Contains(msg, "graft commit --allow-empty -m") || strings.Contains(msg, "approved_release.dormant") {
		t.Fatalf("content-only advice=%q", msg)
	}
}

func TestNotInVersionCombinesAssociationAndNewWorkAdvice(t *testing.T) {
	conf := &config.ApprovedRelease{}
	content := "c0ffee0000000000000000000000000000000000"
	g := &gateRun{conf: conf, contentOnlyPorts: map[string][]versionPortHint{"PROJ-2": {{commit: content, port: "1234567890123456789012345678901234567890"}}}, gate: &approvalGate{conf: conf}}
	v := &releaseVersionCheck{gate: g, sources: map[string][]string{"PROJ-1": {"PROJ-2", "PROJ-3"}}, selected: jira.Version{Name: "v2"}}
	msg := v.notInVersion(context.Background(), jira.Issue{Key: "PROJ-1", Status: "Done"})
	if !strings.Contains(msg, "graft commit --allow-empty -m") || !strings.Contains(msg, "approved_release.dormant: [{key: PROJ-3") || strings.Contains(msg, "key: PROJ-2") {
		t.Fatalf("mixed advice=%q", msg)
	}
}

func TestNotInVersionCombinesSubtaskContentPortAndNewWorkAdvice(t *testing.T) {
	conf := &config.ApprovedRelease{}
	content := "c0ffee0000000000000000000000000000000000"
	g := &gateRun{conf: conf, contentOnlyPorts: map[string][]versionPortHint{"PROJ-2": {{commit: content, port: "1234567890123456789012345678901234567890"}}}, gate: &approvalGate{conf: conf}}
	v := &releaseVersionCheck{gate: g, sources: map[string][]string{"PROJ-1": {"PROJ-2", "PROJ-3"}}, selected: jira.Version{Name: "v2"}}
	msg := v.notInVersion(context.Background(), jira.Issue{Key: "PROJ-1", Status: "Done"})
	if !strings.Contains(msg, "graft commit --allow-empty -m") || !strings.Contains(msg, "approved_release.dormant: [{key: PROJ-3") || strings.Contains(msg, "key: PROJ-2") {
		t.Fatalf("mixed subtask advice=%q", msg)
	}
}

func TestNotInVersionKeepsParentAdviceBesideSubtaskContentPort(t *testing.T) {
	conf := &config.ApprovedRelease{}
	content := "c0ffee0000000000000000000000000000000000"
	g := &gateRun{conf: conf, contentOnlyPorts: map[string][]versionPortHint{"PROJ-2": {{commit: content, port: "1234567890123456789012345678901234567890"}}}, gate: &approvalGate{conf: conf}}
	v := &releaseVersionCheck{gate: g, sources: map[string][]string{"PROJ-1": {"PROJ-1", "PROJ-2"}}, selected: jira.Version{Name: "v2"}}
	msg := v.notInVersion(context.Background(), jira.Issue{Key: "PROJ-1", Status: "Done"})
	if !strings.Contains(msg, "approved_release.dormant: [{key: PROJ-1") || strings.Contains(msg, "key: PROJ-2") {
		t.Fatalf("parent and subtask advice=%q", msg)
	}
}

func versionPortCovered(t *testing.T, r *withheldRepo, prev, head string) map[string]string {
	t.Helper()
	commits, err := deliveredCommits(r.repo, prev, head)
	if err != nil {
		t.Fatal(err)
	}
	covered, err := coveredVersionPorts(r.repo, prev, head, commits, versionPortTargets("VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	return covered
}

func portCovered(t *testing.T, r *withheldRepo, prev, head string, files ...string) map[string]string {
	t.Helper()
	commits, err := deliveredCommits(r.repo, prev, head)
	if err != nil {
		t.Fatal(err)
	}
	covered, err := coveredVersionPorts(r.repo, prev, head, commits, versionPortTargets(files...))
	if err != nil {
		t.Fatal(err)
	}
	return covered
}

func portLines(first, last string) string {
	var b strings.Builder
	b.WriteString(first + "\n")
	for i := range 20 {
		fmt.Fprintf(&b, "mid %d\n", i)
	}
	b.WriteString(last + "\n")
	return b.String()
}

func portBase(t *testing.T) (*withheldRepo, string) {
	r := newEmptyWithheldRepo(t)
	r.writePath("work.txt", new(portLines("a0", "z0")))
	r.writePath("VERSION", new("1\n"))
	return r, r.commitAll("base")
}

func TestCoveredVersionPortsRejectsPartialRevertSameFile(t *testing.T) {
	r, base := portBase(t)
	r.writePath("work.txt", new(portLines("a1", "z1")))
	r.writePath("VERSION", new("2\n"))
	c := r.commitAll("fix: PROJ-1 source")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new(portLines("a1", "z1")))
	r.writePath("VERSION", new("9\n"))
	r.commitAll(fmt.Sprintf("port %s", c))
	r.writePath("work.txt", new(portLines("a0", "z1")))
	prev := r.commitAll("roll back first hunk")
	if got := portCovered(t, r, prev, c, "VERSION"); got[c] != "" {
		t.Fatalf("partially reverted port covered: %v", got)
	}
}

func TestCoveredVersionPortsRejectsRevertThenOtherSamePathEdit(t *testing.T) {
	r, base := portBase(t)
	r.writePath("work.txt", new(portLines("a1", "z0")))
	r.writePath("VERSION", new("2\n"))
	c := r.commitAll("fix: PROJ-1 source")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new(portLines("a1", "z0")))
	r.writePath("VERSION", new("9\n"))
	p := r.commitAll(fmt.Sprintf("port %s", c))
	r.revert(p)
	r.writePath("work.txt", new(portLines("a0", "zz")))
	prev := r.commitAll("other same-file change")
	if got := portCovered(t, r, prev, c, "VERSION"); got[c] != "" {
		t.Fatalf("reverted port covered after unrelated edit: %v", got)
	}
}

func TestCoveredVersionPortsRejectsPortFileDeletedAtPrev(t *testing.T) {
	r, base := portBase(t)
	r.writePath("work.txt", new(portLines("a1", "z0")))
	r.writePath("VERSION", new("2\n"))
	c := r.commitAll("fix: PROJ-1 source")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new(portLines("a1", "z0")))
	r.writePath("VERSION", new("9\n"))
	r.commitAll(fmt.Sprintf("port %s", c))
	r.writePath("work.txt", nil)
	prev := r.commitAll("drop file")
	if got := portCovered(t, r, prev, c, "VERSION"); got[c] != "" {
		t.Fatalf("deleted port file still covered: %v", got)
	}
}

func TestCoveredVersionPortsMatchesDeletedLinesFromGroup(t *testing.T) {
	r, _ := versionPortRepo(t)
	r.writePath("work.txt", new("before\nkeep\n"))
	base := r.commitAll("fixture base")
	r.writePath("work.txt", new("keep\n"))
	r.writePath("VERSION", new("2\n"))
	c := r.commitAll("fix: PROJ-1 remove old line")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("keep\n"))
	r.writePath("VERSION", new("9\n"))
	r.commitAll(fmt.Sprintf("port %s", c))
	r.writePath("work.txt", new("before\nkeep\n"))
	prev := r.commitAll("restore old line")
	if got := portCovered(t, r, prev, c, "VERSION"); got[c] != "" {
		t.Fatalf("deleted group line failed to keep task active: %v", got)
	}
}

func TestCoveredVersionPortsChecksBinaryPathExistence(t *testing.T) {
	r, base := versionPortRepo(t)
	cbin := "\x00source\x01"
	r.writePath("asset.bin", &cbin)
	r.writePath("VERSION", new("2\n"))
	c := r.commitAll("fix: PROJ-1 add binary")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("asset.bin", &cbin)
	r.writePath("VERSION", new("9\n"))
	r.commitAll(fmt.Sprintf("port %s", c))
	r.writePath("asset.bin", nil)
	prev := r.commitAll("remove binary")
	if got := portCovered(t, r, prev, c, "VERSION"); got[c] != "" {
		t.Fatalf("binary path change was covered: %v", got)
	}
}

func TestCoveredVersionPortsRejectsChangedBinaryBlobAfterPortRevert(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	r.writePath("work.txt", new("before\n"))
	r.writePath("asset.bin", new("\x00\x01old\x00"))
	r.writePath("VERSION", new("1\n"))
	base := r.commitAll("base")
	prev, c := versionPortThenRevert(t, r, base, func() { r.writePath("asset.bin", new("\x00\x02new\x00")) })
	if r.git("rev-parse", prev+":asset.bin") == r.git("rev-parse", c+":asset.bin") {
		t.Fatal("fixture: binary blob did not change")
	}
	if got := versionPortCovered(t, r, prev, c); got[c] != "" {
		t.Fatalf("changed binary blob covered after port revert: %v", got)
	}
}

func TestCoveredVersionPortsRejectsModeChangeAfterPortRevert(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	r.writePath("run.sh", new("echo hi\n"))
	r.writePath("work.txt", new("before\n"))
	r.writePath("VERSION", new("1\n"))
	base := r.commitAll("base")
	prev, c := versionPortThenRevert(t, r, base, func() {
		if err := os.Chmod(filepath.Join(r.dir, "run.sh"), 0o755); err != nil {
			t.Fatal(err)
		}
	})
	if got := versionPortCovered(t, r, prev, c); got[c] != "" {
		t.Fatalf("mode-only change covered after port revert: %v", got)
	}
}

func TestCoveredVersionPortsCountsMergeResolutionBlame(t *testing.T) {
	r, base := versionPortRepo(t)
	trunk := r.git("symbolic-ref", "--short", "HEAD")
	c := versionPortCommit(r, "source\n", "2\n", "fix: PROJ-1 source")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("source\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll(fmt.Sprintf("port %s", c))
	r.git("checkout", "-q", trunk)
	r.writePath("work.txt", new("other\n"))
	r.commitAll("chore: PROJ-2 conflicting edit")
	r.git("checkout", "-q", "-b", "merge-test")
	mergeCmd := exec.Command("git", "merge", "--no-commit", "release")
	mergeCmd.Dir = r.dir
	if output, err := mergeCmd.CombinedOutput(); err == nil {
		t.Fatalf("fixture merge unexpectedly clean: %s", output)
	}
	r.writePath("work.txt", new("merge-resolution\n"))
	r.writePath("VERSION", new("9\n"))
	r.git("add", "-A")
	tree := r.git("write-tree")
	firstParent := r.head()
	merge := r.git("commit-tree", tree, "-p", firstParent, "-p", port, "-m", "merge release")
	r.git("checkout", "-q", "--detach", merge)
	if got := r.git("blame", "--line-porcelain", "--no-follow", "-L", "1,1", port+".."+merge, "--", "work.txt"); !strings.HasPrefix(got, merge) {
		t.Fatalf("fixture blame = %q, want merge %s", got, merge)
	}
	if got := versionPortCovered(t, r, port, merge); got[c] != "" {
		t.Fatalf("merge-attributed group line covered: %v", got)
	}
}

func TestCoveredVersionPortDiffKeepsRenameDetectionDisabled(t *testing.T) {
	r, _ := versionPortRepo(t)
	r.writePath("rename/a.txt", new(strings.Repeat("same\n", 30)))
	base := r.commitAll("add rename source")
	r.writePath("rename/a.txt", nil)
	r.writePath("rename/b.txt", new(strings.Repeat("same\n", 30)))
	commit := r.commitAll("rename file")
	hunks, err := versionPortDiff(r.repo, base, commit, "rename")
	if err != nil {
		t.Fatal(err)
	}
	if len(hunks) != 2 || len(hunks[0].deleted) == 0 || len(hunks[1].added) == 0 {
		t.Fatalf("rename diff hunks = %#v, want separate deletion and addition", hunks)
	}
	args := versionPortDiffArgs(base, commit, "rename")
	if !slices.Contains(args, "--no-renames") {
		t.Fatalf("rename diff args = %v, want --no-renames", args)
	}
}

func TestVersionPortBlameDisablesRenameFollowing(t *testing.T) {
	args := versionPortFileBlameArgs("work.txt", "prev", "head", []int{4, 2, 3})
	if !slices.Contains(args, "--no-follow") {
		t.Fatalf("blame args = %v, want --no-follow", args)
	}
	if !slices.Contains(args, "2,4") {
		t.Fatalf("blame args = %v, want the coalesced line range", args)
	}
}

func TestCoveredVersionPortsKeepsSyncFileNonVersionChangeActive(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	r.writePath("work.txt", new("before\n"))
	r.writePath("VERSION", new("1\n"))
	r.writePath("web/package.json", new("{\n  \"version\": \"1\",\n  \"dependencies\": {}\n}\n"))
	base := r.commitAll("base")
	r.writePath("work.txt", new("fix\n"))
	r.writePath("VERSION", new("2\n"))
	r.writePath("web/package.json", new("{\n  \"version\": \"2\",\n  \"dependencies\": {\"evil\": \"1.0\"}\n}\n"))
	c := r.commitAll("fix: PROJ-1 source")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("fix\n"))
	r.writePath("VERSION", new("9\n"))
	r.writePath("web/package.json", new("{\n  \"version\": \"9\",\n  \"dependencies\": {}\n}\n"))
	port := r.commitAll(fmt.Sprintf("port %s", c))
	targets := versionTargetsByPath(config.Version{File: "VERSION", Sync: []config.SyncFile{{Path: "web/package.json", Format: config.FormatJSON, Key: []string{"version"}}}})
	commits, err := deliveredCommits(r.repo, port, c)
	if err != nil {
		t.Fatal(err)
	}
	covered, err := coveredVersionPorts(r.repo, port, c, commits, targets)
	if err != nil {
		t.Fatal(err)
	}
	if covered[c] != "" {
		t.Fatalf("sync-file non-version change covered: %v", covered)
	}
}

func TestCoveredVersionPortsIgnoresOnlyConfiguredSyncVersionLine(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	r.writePath("work.txt", new("before\n"))
	r.writePath("VERSION", new("1\n"))
	baseJSON := "{\n  \"version\": \"1\",\n  \"dependencies\": {}\n}\n"
	r.writePath("web/package.json", &baseJSON)
	base := r.commitAll("base")
	r.writePath("VERSION", new("2\n"))
	c1JSON := "{\n  \"version\": \"2\",\n  \"dependencies\": {}\n}\n"
	r.writePath("web/package.json", &c1JSON)
	c1 := r.commitAll("fix: PROJ-1 version")
	sourceJSON := "{\n  \"version\": \"2\",\n  \"dependencies\": {\"added\": \"1.0\"}\n}\n"
	r.writePath("web/package.json", &sourceJSON)
	c2 := r.commitAll("fix: PROJ-1 dependency")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("VERSION", new("9\n"))
	portJSON := "{\n  \"version\": \"9\",\n  \"dependencies\": {\"added\": \"1.0\"}\n}\n"
	r.writePath("web/package.json", &portJSON)
	port := r.commitAll(fmt.Sprintf("port %s and %s", c1, c2))
	prevJSON := "{\n  \"version\": \"9\",\n  \"dependencies\": {}\n}\n"
	r.writePath("web/package.json", &prevJSON)
	prev := r.commitAll("rollback sync dependency on release")
	targets := versionTargetsByPath(config.Version{File: "VERSION", Sync: []config.SyncFile{{Path: "web/package.json", Format: config.FormatJSON, Key: []string{"version"}}}})
	rangeCommits, err := deliveredCommits(r.repo, prev, c2)
	if err != nil {
		t.Fatal(err)
	}
	covered, err := coveredVersionPorts(r.repo, prev, c2, rangeCommits, targets)
	if err != nil {
		t.Fatal(err)
	}
	if covered[c1] != "" || covered[c2] != "" {
		t.Fatalf("non-version sync-field addition hidden by version target: %v (port %s)", covered, port)
	}
}

func TestVersionPortFileActivityDoesNotIgnoreOtherSyncFileLines(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	r.writePath("VERSION", new("1\n"))
	before := "{\n  \"version\": \"1\",\n  \"dependencies\": {}\n}\n"
	r.writePath("web/package.json", &before)
	prev := r.commitAll("base")
	r.writePath("VERSION", new("2\n"))
	after := "{\n  \"version\": \"2\",\n  \"dependencies\": {\"added\": \"1.0\"}\n}\n"
	r.writePath("web/package.json", &after)
	commit := r.commitAll("fix: PROJ-1 version and dependency")
	targets := versionTargetsByPath(config.Version{File: "VERSION", Sync: []config.SyncFile{{Path: "web/package.json", Format: config.FormatJSON, Key: []string{"version"}}}})
	active, err := versionPortFileHasTextDelta(r.repo, "web/package.json", prev, commit, nil, map[string]bool{commit: true}, nil, nil, nil, targets)
	if err != nil {
		t.Fatal(err)
	}
	if !active {
		t.Fatal("non-version line in sync file was ignored with its version line")
	}
}

func TestCoveredVersionPortsRejectsRewrittenAddedLine(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	r.writePath("work.txt", new(portLines("a0", "z0")))
	r.writePath("VERSION", new("1\n"))
	base := r.commitAll("base")
	prev, c := versionPortThenRevert(t, r, base, func() { r.writePath("work.txt", new(portLines("a0\nadded", "z0"))) })
	r.writePath("work.txt", new(portLines("a0\nadded // tweaked", "z0")))
	head := r.commitAll("fix: PROJ-8 tweak")
	if got := versionPortCovered(t, r, prev, head); got[c] != "" {
		t.Fatalf("rewritten addition covered: %v", got)
	}
}

func TestCoveredVersionPortsRejectsTwiceRewrittenAddedLine(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	r.writePath("work.txt", new(portLines("a0", "z0")))
	r.writePath("VERSION", new("1\n"))
	base := r.commitAll("base")
	prev, c := versionPortThenRevert(t, r, base, func() { r.writePath("work.txt", new(portLines("a0\nadded", "z0"))) })
	r.writePath("work.txt", new(portLines("a0\nfirst rewrite", "z0")))
	r.commitAll("fix: PROJ-8 first")
	r.writePath("work.txt", new(portLines("a0\nsecond rewrite", "z0")))
	head := r.commitAll("fix: PROJ-9 second")
	if got := versionPortCovered(t, r, prev, head); got[c] != "" {
		t.Fatalf("twice rewritten addition covered: %v", got)
	}
}

func TestCoveredVersionPortsAllowsUnrelatedRewrite(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	r.writePath("work.txt", new(portLines("a0", "z0")))
	r.writePath("VERSION", new("1\n"))
	base := r.commitAll("base")
	r.writePath("work.txt", new(portLines("a0\nadded", "z0")))
	r.writePath("VERSION", new("2\n"))
	c := r.commitAll("fix: PROJ-1 source")
	r.writePath("work.txt", new(portLines("a0\nadded", "rewritten unrelated")))
	head := r.commitAll("fix: PROJ-8 unrelated")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new(portLines("a0\nadded", "z0")))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll(fmt.Sprintf("port %s", c))
	if got := versionPortCovered(t, r, port, head); got[c] != port {
		t.Fatalf("unrelated rewrite prevented coverage: %v", got)
	}
}

func TestCoveredVersionPortsStopsRewriteHistoryAtTarget(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	r.writePath("work.txt", new(portLines("a0", "z0")))
	r.writePath("VERSION", new("1\n"))
	base := r.commitAll("base")
	r.writePath("work.txt", new(portLines("a0\nadded", "z0")))
	r.writePath("VERSION", new("2\n"))
	c := r.commitAll("fix: PROJ-1 source")
	r.writePath("work.txt", new(portLines("a0\nfirst rewrite", "z0")))
	r.commitAll("fix: PROJ-8 first")
	r.writePath("work.txt", new(portLines("a0\nsecond rewrite", "z0")))
	head := r.commitAll("fix: PROJ-9 second")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new(portLines("a0\nadded", "z0")))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll(fmt.Sprintf("port %s", c))
	r.writePath("work.txt", new(portLines("a0\nfirst rewrite", "z0")))
	prev := r.commitAll("fix: PROJ-8 on release")
	if got := versionPortCovered(t, r, prev, head); got[c] != port {
		t.Fatalf("rewrite already on target prevented coverage: %v", got)
	}
}

func TestVersionPortRewriteDepthLimitStaysActive(t *testing.T) {
	origin := versionPortOrigin{commit: "in-range", line: 1}
	active, err := versionPortRewriteActive(nil, "work.txt", "prev", origin, nil, nil, nil, map[string]bool{origin.commit: true}, versionPortRewriteDepth)
	if err != nil || !active {
		t.Fatalf("depth limit: active=%v err=%v", active, err)
	}
}

func TestCoveredVersionPortsAllowsGroupRewrite(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	r.writePath("work.txt", new(portLines("a0", "z0")))
	r.writePath("VERSION", new("1\n"))
	base := r.commitAll("base")
	r.writePath("work.txt", new(portLines("a0\nadded", "z0")))
	r.writePath("VERSION", new("2\n"))
	c := r.commitAll("fix: PROJ-1 source")
	r.writePath("work.txt", new(portLines("a0\nrewritten", "z0")))
	head := r.commitAll("fix: PROJ-1 rewrite")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new(portLines("a0\nrewritten", "z0")))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll(fmt.Sprintf("port %s and %s", c, head))
	if got := versionPortCovered(t, r, port, head); got[c] != port || got[head] != port {
		t.Fatalf("group rewrite not covered: %v", got)
	}
}

func TestCoveredVersionPortsCountsRevertAttributedAddedLine(t *testing.T) {
	r, base := versionPortRepo(t)
	prev, c := versionPortThenRevert(t, r, base, func() { r.writePath("work.txt", new("before\nadded\n")) })
	r.writePath("work.txt", new("before\n"))
	x := r.commitAll("chore: PROJ-9 drop added")
	r.revert(x)
	covered := versionPortCovered(t, r, prev, r.head())
	if covered[c] != "" {
		t.Fatalf("revert-attributed added line covered: %v", covered)
	}
}

func TestCoveredVersionPortsRejectsNewTextFileDeletedAfterPort(t *testing.T) {
	r, base := versionPortRepo(t)
	r.writePath("new.txt", new("n1\nn2\n"))
	r.writePath("VERSION", new("2\n"))
	c := r.commitAll("feat: PROJ-1 new file")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("new.txt", new("n1\nn2\n"))
	r.writePath("VERSION", new("9\n"))
	r.commitAll(fmt.Sprintf("feat: PROJ-1 port %s", c))
	r.writePath("new.txt", nil)
	prev := r.commitAll("chore: PROJ-2 drop new.txt")
	if got := versionPortCovered(t, r, prev, c); got[c] != "" {
		t.Fatalf("new text file deletion covered: %v", got)
	}
}

func versionPortThenRevert(t *testing.T, r *withheldRepo, base string, edit func()) (string, string) {
	t.Helper()
	edit()
	r.writePath("VERSION", new("2\n"))
	c := r.commitAll("fix: PROJ-1 source")
	r.git("checkout", "-q", "-b", "release", base)
	edit()
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll(fmt.Sprintf("fix: PROJ-1 port %s", c))
	r.revert(port)
	prev := r.head()
	r.git("checkout", "-q", c)
	return prev, c
}

func TestNotInVersionUsesTaskKeyForContentOnlyAdvice(t *testing.T) {
	conf := &config.ApprovedRelease{}
	c := "c0ffee0000000000000000000000000000000000"
	g := &gateRun{conf: conf, contentOnlyPorts: map[string][]versionPortHint{"PROJ-1": {{commit: c, port: "1234567890123456789012345678901234567890"}}}, gate: &approvalGate{conf: conf}}
	v := &releaseVersionCheck{gate: g, sources: map[string][]string{"PROJ-1": {"PROJ-1"}}, selected: jira.Version{Name: "v2"}}
	msg := v.notInVersion(context.Background(), jira.Issue{Key: "PROJ-1", Status: "Done"})
	if strings.Contains(msg, "Revert its commits") {
		t.Fatalf("key-shaped content-only advice suggests revert: %s", msg)
	}
}

func TestGateRunPortCoverageSkipsVersionActivityOnlyWhilePortContentRemains(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	r.writePath("work.txt", new("before\n"))
	r.writePath("VERSION", new("1.0.0\n"))
	base := r.commitAll("base")
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("VERSION", new("1.0.1\n"))
	c := r.commitAll("fix: PROJ-1 source")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("VERSION", new("9.0.0\n"))
	p := r.commitAll(fmt.Sprintf("port %s", c))
	prev := p
	t.Run("port stays on prev", func(t *testing.T) {
		g, out := versionPortGateRun(r)
		if _, err := g.run(context.Background(), prev, c, nil, false); err != nil {
			t.Fatal(err)
		}
		if len(g.activeKeys) != 0 || g.activeCommits != 0 {
			t.Fatalf("covered task active: keys=%v commits=%d; output=%s", g.activeKeys, g.activeCommits, out.String())
		}
	})
	r.revert(p)
	prev = r.head()
	t.Run("port reverted on release", func(t *testing.T) {
		g, out := versionPortGateRun(r)
		if _, err := g.run(context.Background(), prev, c, nil, false); err != nil {
			t.Fatal(err)
		}
		if !g.activeKeys["PROJ-1"] || g.activeCommits == 0 {
			t.Fatalf("reverted port hid task from version gate: keys=%v commits=%d; output=%s", g.activeKeys, g.activeCommits, out.String())
		}
	})
}

func TestGateRunCoversEarlierWithheldSourceByPortAlreadyInTarget(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	r.writePath("work.txt", new("before\n"))
	r.writePath("VERSION", new("1.0.0\n"))
	base := r.commitAll("base")
	r.git("checkout", "-q", "-b", "source", base)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("VERSION", new("1.0.1\n"))
	source := r.commitAll("fix: PROJ-1 source")
	r.git("checkout", "-q", "-b", "target", base)
	r.writePath("work.txt", new("fixed\n"))
	r.writePath("VERSION", new("9.0.0\n"))
	prev := r.commitAll(fmt.Sprintf("port %s", source))
	g, output := versionPortGateRun(r)
	if _, err := g.run(context.Background(), prev, prev, []string{source}, false); err != nil {
		t.Fatal(err)
	}
	if g.activeKeys["PROJ-1"] || g.activeCommits != 0 {
		t.Fatalf("earlier covered source remained active: keys=%v commits=%d; output=%s", g.activeKeys, g.activeCommits, output.String())
	}
}

func versionPortGateRun(r *withheldRepo) (*gateRun, *strings.Builder) {
	var out strings.Builder
	conf := &config.ApprovedRelease{Version: &config.ApprovedReleaseVersion{}, Statuses: []string{"Done"}}
	cfg := &config.Config{Version: config.Version{File: "VERSION"}}
	gate := &approvalGate{conf: conf, issues: map[string]jira.Issue{"PROJ-1": {Key: "PROJ-1", Status: "Done"}}}
	return &gateRun{app: &App{Stdout: &out}, repo: r.repo, cfg: cfg, conf: conf, gate: gate, keyRe: regexp.MustCompile(`PROJ-[0-9]+`), activeKeys: map[string]bool{}}, &out
}

func TestCoveredVersionPortsModuleRewrites(t *testing.T) {
	for _, tc := range []struct {
		name, path, directive string
		covered               bool
	}{
		{"latest require", "example.com/own/mod", "require", true},
		{"other require", "example.com/other/mod", "require", false},
		{"latest replace", "example.com/own/mod", "replace", false},
		{"other replace", "example.com/other/mod", "replace", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", cfg)
			if err := os.MkdirAll(filepath.Join(cfg, "graft"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cfg, "graft", "config.yaml"), []byte("modules:\n  latest: [example.com/own/]\n"), 0600); err != nil {
				t.Fatal(err)
			}
			mod := func(v string) string {
				if tc.directive == "replace" {
					return fmt.Sprintf("module example.com/test\n\ngo 1.23\n\nreplace %s => example.com/replacement %s\n", tc.path, v)
				}
				return fmt.Sprintf("module example.com/test\n\ngo 1.23\n\nrequire %s %s\n", tc.path, v)
			}
			sum := func(v string) string { return fmt.Sprintf("%s %s h1:checksum\n", tc.path, v) }
			r := newEmptyWithheldRepo(t)
			r.writePath("go.mod", new(mod("v1.0.0")))
			r.writePath("go.sum", new(sum("v1.0.0")))
			r.writePath("VERSION", new("1\n"))
			base := r.commitAll("base")
			r.writePath("go.mod", new(mod("v1.1.0")))
			r.writePath("go.sum", new(sum("v1.1.0")))
			r.writePath("VERSION", new("2\n"))
			c := r.commitAll("fix: PROJ-1 source")
			r.writePath("go.mod", new(mod("v1.2.0")))
			r.writePath("go.sum", new(sum("v1.2.0")))
			head := r.commitAll("fix: PROJ-8 module update")
			r.git("checkout", "-q", "-b", "release", base)
			r.writePath("go.mod", new(mod("v1.1.0")))
			r.writePath("go.sum", new(sum("v1.1.0")))
			r.writePath("VERSION", new("9\n"))
			r.commitAll(fmt.Sprintf("port %s", c))
			r.writePath("go.mod", new(mod("v1.0.0")))
			r.writePath("go.sum", new(sum("v1.0.0")))
			r.writePath("VERSION", new("1\n"))
			r.commitAll("chore: manual rollback")
			// Release independently raised the same dependency after the rollback.
			// This leaves the group's deleted v1.0.0 line absent on the target.
			r.writePath("go.mod", new(mod("v1.0.1")))
			r.writePath("go.sum", new(sum("v1.0.1")))
			prev := r.commitAll("chore: independent release update")
			got := versionPortCovered(t, r, prev, head)
			if (got[c] != "") != tc.covered {
				t.Fatalf("covered=%v want %v: %v", got[c] != "", tc.covered, got)
			}
			direct := versionPortCovered(t, r, prev, c)
			if direct[c] != "" {
				t.Fatalf("group's own line should stay active: %v", direct)
			}
		})
	}
}

func TestVersionPortModuleRaisesOnlyVersion(t *testing.T) {
	latest := func(path string) bool { return strings.HasPrefix(path, "example.com/own/") }
	for _, tc := range []struct {
		name, before, after string
		want                bool
	}{
		{"require upgrade", "require example.com/own/mod v1.1.0", "require example.com/own/mod v1.2.0", true},
		{"require downgrade", "require example.com/own/mod v1.1.0", "require example.com/own/mod v1.0.0", false},
		{"require adds indirect", "require example.com/own/mod v1.1.0", "require example.com/own/mod v1.2.0 // indirect", false},
		{"require changes comment", "require example.com/own/mod v1.1.0 // v1.1.0", "require example.com/own/mod v1.2.0 // v1.2.0", false},
		{"outside latest", "require example.com/other/mod v1.1.0", "require example.com/other/mod v1.2.0", false},
		{"replace upgrade", "replace example.com/own/mod => example.com/replacement v1.1.0", "replace example.com/own/mod => example.com/replacement v1.2.0", false},
		{"replace changes path", "replace example.com/own/mod => example.com/replacement v1.1.0", "replace example.com/own/mod => example.com/other v1.2.0", false},
		{"replaced requirement", "require example.com/own/mod v1.1.0\nreplace example.com/own/mod => example.com/replacement v1.0.0", "require example.com/own/mod v1.2.0\nreplace example.com/own/mod => example.com/replacement v1.0.0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix := "module example.com/test\n\ngo 1.23\n\n"
			lines, err := versionPortModuleRaises(prefix+tc.before+"\n", prefix+tc.after+"\n", latest)
			if err != nil {
				t.Fatal(err)
			}
			if got := lines[5]; got != tc.want {
				t.Fatalf("mechanical=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestCoveredVersionPortsModuleRewriteAfterManualRollback(t *testing.T) {
	for _, tc := range []struct {
		path    string
		covered bool
	}{
		{"example.com/own/mod", false}, {"example.com/other/mod", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			cfg := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", cfg)
			if err := os.MkdirAll(filepath.Join(cfg, "graft"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cfg, "graft", "config.yaml"), []byte("modules:\n  latest: [example.com/own/]\n"), 0600); err != nil {
				t.Fatal(err)
			}
			baseMod := "module example.com/test\n\ngo 1.23\n\n"
			mod := func(v string) string { return fmt.Sprintf("%srequire %s %s\n", baseMod, tc.path, v) }
			r := newEmptyWithheldRepo(t)
			r.writePath("go.mod", new(baseMod))
			r.writePath("VERSION", new("1\n"))
			base := r.commitAll("base")
			r.writePath("go.mod", new(mod("v1.1.0")))
			r.writePath("VERSION", new("2\n"))
			c := r.commitAll("fix: PROJ-1 dependency")
			r.writePath("go.mod", new(mod("v1.2.0")))
			head := r.commitAll("fix: PROJ-8 dependency update")
			r.git("checkout", "-q", "-b", "release", base)
			r.writePath("go.mod", new(mod("v1.1.0")))
			r.writePath("VERSION", new("9\n"))
			r.commitAll(fmt.Sprintf("port %s", c))
			r.writePath("go.mod", new(baseMod))
			r.writePath("VERSION", new("1\n"))
			prev := r.commitAll("manual rollback without link")
			got := versionPortCovered(t, r, prev, head)
			if (got[c] != "") != tc.covered {
				t.Fatalf("covered=%v want %v: %v", got[c] != "", tc.covered, got)
			}
		})
	}
}

func TestCoveredVersionPortsRejectsMovedGroupLine(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed_%t", changed), func(t *testing.T) {
			r := newEmptyWithheldRepo(t)
			r.writePath("work.txt", new(portLines("a0", "z0")))
			r.writePath("VERSION", new("1\n"))
			base := r.commitAll("base")
			prev, c := versionPortThenRevert(t, r, base, func() { r.writePath("work.txt", new(portLines("a0\nadded", "z0"))) })
			moved := "added"
			if changed {
				moved = "added rewritten"
			}
			r.writePath("work.txt", new(portLines("a0", "z0\n"+moved)))
			head := r.commitAll("fix: PROJ-8 move")
			if got := versionPortCovered(t, r, prev, head); got[c] != "" {
				t.Fatalf("moved group line covered: %v", got)
			}
		})
	}
}

func TestCoveredVersionPortsRejectsMovedFileBackWithoutParent(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	r.writePath("work.txt", new(portLines("a0", "z0")))
	r.writePath("VERSION", new("1\n"))
	base := r.commitAll("base")
	prev, c := versionPortThenRevert(t, r, base, func() { r.writePath("work.txt", new(portLines("a0\nadded", "z0"))) })
	r.git("mv", "work.txt", "elsewhere.txt")
	r.commitAll("chore: PROJ-8 move away")
	r.git("mv", "elsewhere.txt", "work.txt")
	r.writePath("work.txt", new(portLines("a0\nrewritten", "z0")))
	head := r.commitAll("fix: PROJ-8 move back")
	if got := versionPortCovered(t, r, prev, head); got[c] != "" {
		t.Fatalf("line with missing parent file covered: %v", got)
	}
}

func TestCoveredVersionPortsAllowsNewLineAndUnrelatedRangeRewrites(t *testing.T) {
	for _, n := range []int{0, 1, 4} {
		t.Run(fmt.Sprintf("rewrites_%d", n), func(t *testing.T) {
			r := newEmptyWithheldRepo(t)
			r.writePath("work.txt", new(portLines("a0", "z0")))
			r.writePath("VERSION", new("1\n"))
			base := r.commitAll("base")
			r.writePath("work.txt", new(portLines("a0\ngroup", "z0")))
			r.writePath("VERSION", new("2\n"))
			c := r.commitAll("fix: PROJ-1 source")
			r.writePath("work.txt", new(portLines("a0\ngroup", "z0\nother 0")))
			head := r.commitAll("fix: PROJ-8 addition")
			for i := 1; i <= n; i++ {
				r.writePath("work.txt", new(portLines("a0\ngroup", fmt.Sprintf("z0\nother %d", i))))
				head = r.commitAll(fmt.Sprintf("fix: PROJ-8 rewrite %d", i))
			}
			r.git("checkout", "-q", "-b", "release", base)
			r.writePath("work.txt", new(portLines("a0\ngroup", "z0")))
			r.writePath("VERSION", new("9\n"))
			port := r.commitAll(fmt.Sprintf("port %s", c))
			if got := versionPortCovered(t, r, port, head); got[c] != port {
				t.Fatalf("unrelated range work prevents coverage: %v", got)
			}
		})
	}
}

func TestCoveredVersionPortsUsesOriginalBlameLineAfterShift(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	r.writePath("work.txt", new(portLines("a0", "z0")))
	r.writePath("VERSION", new("1\n"))
	base := r.commitAll("base")
	r.writePath("work.txt", new(portLines("a0\ngroup", "z0")))
	r.writePath("VERSION", new("2\n"))
	c := r.commitAll("fix: PROJ-1 source")
	r.writePath("work.txt", new(portLines("a0\ngroup", "z0\nother")))
	r.commitAll("fix: PROJ-8 addition")
	r.writePath("work.txt", new(portLines("a0\ngroup", "z0\nother rewritten")))
	r.commitAll("fix: PROJ-8 rewrite")
	r.writePath("work.txt", new("inserted one\ninserted two\n"+portLines("a0\ngroup", "z0\nother rewritten")))
	head := r.commitAll("chore: PROJ-9 shift")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new(portLines("a0\ngroup", "z0")))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll(fmt.Sprintf("port %s", c))
	if got := versionPortCovered(t, r, port, head); got[c] != port {
		t.Fatalf("shifted unrelated rewrite prevents coverage: %v", got)
	}
}

func TestVersionPortRewriteConservativeMissingEvidence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    *versionPortRewriteCommit
		budget  int
		want    bool
		inRange bool
	}{
		{"missing hunk", &versionPortRewriteCommit{deleted: map[int][]versionPortLine{}, loaded: true}, 10, true, true},
		{"missing blame", &versionPortRewriteCommit{deleted: map[int][]versionPortLine{1: {{number: 2}}}, changed: map[int]bool{2: true}, blame: map[int]versionPortOrigin{}, loaded: true}, 10, true, true},
		{"budget exhausted", &versionPortRewriteCommit{deleted: map[int][]versionPortLine{1: nil}, loaded: true}, 0, true, true},
		{"outside range", nil, 10, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newVersionPortRewrite(nil, "work.txt", "prev", nil, nil, nil, map[string]bool{"commit": tc.inRange})
			if tc.data != nil {
				s.commits["commit"] = tc.data
			}
			budget := tc.budget
			active, err := s.walk(versionPortOrigin{commit: "commit", line: 1}, 0, &budget)
			if err != nil || active != tc.want {
				t.Fatalf("active=%v err=%v want=%v", active, err, tc.want)
			}
		})
	}
}

func TestVersionPortRewriteInvalidGoModStaysActive(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, "graft"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "graft", "config.yaml"), []byte("modules:\n  latest: [example.com/own/]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := newEmptyWithheldRepo(t)
	r.writePath("go.mod", new("module example.com/test\n\nrequire example.com/own/mod v1.0.0\n"))
	prev := r.commitAll("base")
	r.writePath("go.mod", new("not a valid go.mod\n"))
	d := r.commitAll("fix: PROJ-8 invalid")
	s := newVersionPortRewrite(r.repo, "go.mod", prev, nil, nil, nil, map[string]bool{d: true})
	budget := versionPortRewriteBudget
	active, err := s.walk(versionPortOrigin{commit: d, line: 1}, 0, &budget)
	if err != nil || !active {
		t.Fatalf("invalid history: active=%v err=%v", active, err)
	}
	r.writePath("go.mod", new("module example.com/test\n\nrequire example.com/own/mod v1.1.0\n"))
	valid := r.commitAll("fix: PROJ-8 repair")
	s = newVersionPortRewrite(r.repo, "go.mod", prev, nil, nil, nil, map[string]bool{d: true, valid: true})
	budget = versionPortRewriteBudget
	active, err = s.walk(versionPortOrigin{commit: valid, line: 3}, 0, &budget)
	if err != nil || !active {
		t.Fatalf("invalid parent: active=%v err=%v", active, err)
	}
}

func TestVersionPortRequiredModulesExcludesReplaced(t *testing.T) {
	text := "module example.com/test\n\nrequire example.com/own/mod v1.0.0\nreplace example.com/own/mod => example.com/other v1.0.0\n"
	modules, err := versionPortRequiredModules("go.mod", text)
	if err != nil {
		t.Fatal(err)
	}
	if modules["example.com/own/mod"] {
		t.Fatal("replaced module was treated as present")
	}
}

// Source commits that are not a chain replay one by one, each onto the tree
// the one before left. That tree lives only in the replay's private object
// directory, so reading it must use the replay's environment.
func TestReplayOfUnchainedSourcesReadsTheIntermediateTree(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	r.writePath("a.txt", new("before\n"))
	r.writePath("b.txt", new("before\n"))
	r.writePath("VERSION", new("1\n"))
	base := r.commitAll("base")
	r.git("checkout", "-q", "-b", "first", base)
	r.writePath("a.txt", new("fixed\n"))
	r.writePath("VERSION", new("2\n"))
	first := r.commitAll("fix: PROJ-1 first")
	r.git("checkout", "-q", "-b", "second", base)
	r.writePath("b.txt", new("fixed\n"))
	r.writePath("VERSION", new("3\n"))
	second := r.commitAll("fix: PROJ-2 second")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("a.txt", new("fixed\n"))
	r.writePath("b.txt", new("fixed\n"))
	port := r.commitAll("port " + first + " " + second)
	group := []deliveredCommit{{sha: first}, {sha: second}}
	replayed, err := replayVersionPortGroup(r.repo, port, group, versionPortTargets("VERSION"))
	if err != nil {
		t.Fatalf("replay of unchained sources failed: %v", err)
	}
	if !replayed {
		t.Fatal("replay of unchained sources whose changes are the port's was not linked")
	}
}

// The second source merges onto the intermediate tree without a conflict:
// the clean path reads that tree with the replay's environment too.
func TestReplayOfUnchainedSourcesMergesCleanlyOntoTheIntermediateTree(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	r.writePath("a.txt", new("before\n"))
	r.writePath("b.txt", new("before\n"))
	r.writePath("VERSION", new("1\n"))
	base := r.commitAll("base")
	r.git("checkout", "-q", "-b", "first", base)
	r.writePath("a.txt", new("fixed\n"))
	r.writePath("VERSION", new("2\n"))
	first := r.commitAll("fix: PROJ-1 first")
	r.git("checkout", "-q", "-b", "second", base)
	r.writePath("b.txt", new("fixed\n"))
	second := r.commitAll("fix: PROJ-2 second")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("a.txt", new("fixed\n"))
	r.writePath("b.txt", new("fixed\n"))
	port := r.commitAll("port " + first + " " + second)
	group := []deliveredCommit{{sha: first}, {sha: second}}
	replayed, err := replayVersionPortGroup(r.repo, port, group, versionPortTargets("VERSION"))
	if err != nil {
		t.Fatalf("replay of unchained sources failed: %v", err)
	}
	if !replayed {
		t.Fatal("replay of unchained sources whose changes are the port's was not linked")
	}
}

func TestReplayOfUnchainedSourcesConflictReadsIntermediateVersionBlob(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	r.writePath("VERSION", new("base\nmiddle\nlast\n"))
	base := r.commitAll("base")
	r.git("checkout", "-q", "-b", "first", base)
	r.writePath("VERSION", new("source one\nmiddle\nlast\n"))
	first := r.commitAll("fix: PROJ-1 first")
	r.git("checkout", "-q", "-b", "second", base)
	r.writePath("VERSION", new("source two\nmiddle\nlast\n"))
	second := r.commitAll("fix: PROJ-2 second")
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("VERSION", new("base\nmiddle\nport last\n"))
	port := r.commitAll("port both sources")

	replayed, err := replayVersionPortGroup(r.repo, port, []deliveredCommit{{sha: first}, {sha: second}}, versionPortTargets("VERSION"))
	if err != nil {
		t.Fatalf("non-linear conflict replay could not read its intermediate version blob: %v", err)
	}
	if !replayed {
		t.Fatal("non-linear version-only conflict was not linked")
	}
}

func TestReplayVersionPortCleanReadsVersionBlobFromReplayObjects(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	r.writePath("VERSION", new("1\n"))
	r.writePath("go.mod", new("module example.com/test\n"))
	r.writePath("go.sum", new(""))
	base := r.commitAll("base")
	r.writePath("VERSION", new("2\n"))
	source := r.commitAll("fix: PROJ-2 source")
	env, objects := newVersionPortTestObjectEnv(t, r)
	blob := versionPortTestObject(t, r, env, "hash-object", "-w", "--stdin", "9\n")
	onto := map[string]versionTreeEntry{
		"VERSION": {mode: "100644", kind: "blob", oid: blob},
		"go.mod":  {mode: "100644", kind: "blob", oid: r.git("rev-parse", base+":go.mod")},
		"go.sum":  {mode: "100644", kind: "blob", oid: r.git("rev-parse", base+":go.sum")},
	}
	merged := mapsCloneVersionTree(onto)
	merged["other.txt"] = versionTreeEntry{mode: "100644", kind: "blob", oid: r.git("rev-parse", base+":VERSION")}

	_, linked, err := mergeVersionPortClean(r.repo, env, source, base, "unused", merged, onto, versionPortTargets("VERSION"))
	if err != nil {
		t.Fatalf("clean replay could not read temporary version blob (%s): %v", objects, err)
	}
	if !linked {
		t.Fatal("clean replay of a version-only change was not linked")
	}
	baseTree, err := versionBlobTree(r.repo, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := treesEqualExceptVersionEnv(r.repo, baseTree, onto, versionPortTargets("VERSION"), nil); err == nil {
		t.Fatal("reading the replay-only version blob without env unexpectedly succeeded")
	} else {
		t.Logf("without replay env: %v", err)
	}
}

func newVersionPortTestObjectEnv(t *testing.T, r *withheldRepo) ([]string, string) {
	t.Helper()
	objects := filepath.Join(t.TempDir(), "objects")
	if err := os.Mkdir(objects, 0700); err != nil {
		t.Fatal(err)
	}
	baseObjects := r.git("rev-parse", "--path-format=absolute", "--git-path", "objects")
	return []string{"GIT_OBJECT_DIRECTORY=" + objects, "GIT_ALTERNATE_OBJECT_DIRECTORIES=" + baseObjects}, objects
}

func versionPortTestObject(t *testing.T, r *withheldRepo, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args[:len(args)-1]...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(args[len(args)-1])
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args[:len(args)-1], " "), err)
	}
	return strings.TrimSpace(string(out))
}

func mapsCloneVersionTree(tree map[string]versionTreeEntry) map[string]versionTreeEntry {
	clone := make(map[string]versionTreeEntry, len(tree))
	maps.Copy(clone, tree)
	return clone
}
