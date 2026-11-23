package app

import (
	"context"
	"fmt"
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

func TestCoveredVersionPortsLeavesContentOnlyMatchActiveAndAdvisesLink(t *testing.T) {
	r, base := versionPortRepo(t)
	r.git("checkout", "-q", "-b", "release", base)
	r.writePath("work.txt", new("restored\n"))
	r.writePath("VERSION", new("9\n"))
	port := r.commitAll("port PROJ-1 from prior release")
	r.git("checkout", "-q", base)
	c := versionPortCommit(r, "restored\n", "2\n", fmt.Sprintf("fix: PROJ-1 source port %s", port))
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
	g := &gateRun{conf: conf, contentOnlyPorts: map[string][]versionPortHint{"PROJ-1": {{commit: "c0ffee0000000000000000000000000000000000", port: "1234567890123456789012345678901234567890"}}}, gate: &approvalGate{conf: conf}}
	v := &releaseVersionCheck{gate: g, sources: map[string][]string{"PROJ-1": {"c0ffee0000000000000000000000000000000000"}}, selected: jira.Version{Name: "v2"}}
	msg := v.notInVersion(context.Background(), jira.Issue{Key: "PROJ-1", Status: "Done"})
	if !strings.Contains(msg, "add an empty commit in this range") || strings.Contains(msg, "approved_release.dormant") {
		t.Fatalf("content-only advice=%q", msg)
	}
}

func TestNotInVersionCombinesAssociationAndNewWorkAdvice(t *testing.T) {
	conf := &config.ApprovedRelease{}
	content := "c0ffee0000000000000000000000000000000000"
	fresh := "abcdef0000000000000000000000000000000000"
	g := &gateRun{conf: conf, contentOnlyPorts: map[string][]versionPortHint{"PROJ-1": {{commit: content, port: "1234567890123456789012345678901234567890"}}}, gate: &approvalGate{conf: conf}}
	v := &releaseVersionCheck{gate: g, sources: map[string][]string{"PROJ-1": {content, fresh}}, selected: jira.Version{Name: "v2"}}
	msg := v.notInVersion(context.Background(), jira.Issue{Key: "PROJ-1", Status: "Done"})
	if !strings.Contains(msg, "add an empty commit in this range") || !strings.Contains(msg, "approved_release.dormant") {
		t.Fatalf("mixed advice=%q", msg)
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
