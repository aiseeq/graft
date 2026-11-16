package app

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/jira"
)

// outsideVersionServer answers every history read with an empty changelog.
func outsideVersionServer(t *testing.T) *jira.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/changelog") {
			_, _ = w.Write([]byte(`{"isLast":true,"values":[]}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return jira.New(jira.Credentials{BaseURL: server.URL})
}

func outsideEntry(key, version string) config.DormantReleaseItem {
	return config.DormantReleaseItem{Key: key, Reason: "disabled", DisabledBy: "flag", OutsideVersion: version}
}

// Adversary 1: an outside parent entry cannot excuse a live subtask.
func TestOutsideEntryOnParentDoesNotCoverSubtask(t *testing.T) {
	ar := &config.ApprovedRelease{Dormant: []config.DormantReleaseItem{outsideEntry("PROJ-120", "Product X")}}
	if _, ok := ar.DormantFor("PROJ-121", "PROJ-120"); ok {
		t.Fatal("outside parent entry covered its live subtask")
	}
	if _, ok := ar.OutsideFor("PROJ-121"); ok {
		t.Fatal("outside parent entry is the subtask's outside entry")
	}
}

// Adversary 2 (version): outside work alone, in no version, selects none.
func TestOutsideOnlyWithoutFixVersionCannotSelectRelease(t *testing.T) {
	conf := &config.ApprovedRelease{Version: &config.ApprovedReleaseVersion{Match: "^Product"}, Dormant: []config.DormantReleaseItem{outsideEntry("PROJ-110", "Product X")}}
	g := &gateRun{conf: conf, activeKeys: map[string]bool{"PROJ-110": true}, gate: &approvalGate{conf: conf, issues: map[string]jira.Issue{"PROJ-110": {Key: "PROJ-110", Status: "Testing"}}}}
	v := newReleaseVersionCheck(g, false)
	if v.readTops(t.Context()) || !v.selectVersion() || len(v.failures) != 1 || !strings.Contains(v.failures[0].Error(), "version-select") {
		t.Fatalf("outside-only selection failures = %v", v.failures)
	}
}

// Adversary 3: a stale outside subtask remains visible to the umbrella check.
func TestUnlinkedOutsideSubtaskDoesNotHideOpenUmbrellaWork(t *testing.T) {
	conf := &config.ApprovedRelease{Version: &config.ApprovedReleaseVersion{Match: "^Product"}, Dormant: []config.DormantReleaseItem{outsideEntry("PROJ-121", "Product X")}}
	parent := jira.Issue{Key: "PROJ-120", Status: "Ready for release", Subtasks: []string{"PROJ-121"}, FixVersions: []jira.Version{{ID: "y", Name: "Product Y"}}}
	sub := jira.Issue{Key: "PROJ-121", Parent: parent.Key, Status: "In Progress"}
	g := &gateRun{app: &App{}, conf: conf, gate: &approvalGate{conf: conf, issues: map[string]jira.Issue{parent.Key: parent, sub.Key: sub}}}
	v := newReleaseVersionCheck(g, false)
	v.tops[parent.Key], v.sources[parent.Key], v.topOf[parent.Key], v.live[parent.Key] = parent, []string{parent.Key}, parent.Key, true
	v.umbrellas[parent.Key] = true
	v.selected = jira.Version{Name: "Product Y", ID: "y"}
	if v.linkOutside(t.Context()) || !v.checkUmbrellas(t.Context()) || len(v.failures) != 1 || !strings.Contains(v.failures[0].Error(), "version-umbrella") {
		t.Fatalf("umbrella failures = %v", v.failures)
	}
	// Its entry naming Y, it stands in for the version check alone: the open
	// subtask still holds its parent.
	conf.Dormant = []config.DormantReleaseItem{outsideEntry("PROJ-121", "Product Y")}
	v.failures = nil
	if !v.checkUmbrellas(t.Context()) || len(v.failures) != 1 || !strings.Contains(v.failures[0].Error(), "PROJ-121 (In Progress)") {
		t.Fatalf("umbrella failures with an entry naming Y = %v", v.failures)
	}
}

// Adversary 4: freeze accepts a linked exact outside key absent from the frozen version.
func TestFreezeAcceptsMatchingOutsideEntryOnlyForItsKey(t *testing.T) {
	conf := &config.DeployTarget{ApprovedRelease: &config.ApprovedRelease{Dormant: []config.DormantReleaseItem{outsideEntry("PROJ-130", "Product X")}}}
	gate := &approvalGate{conf: conf.ApprovedRelease, issues: map[string]jira.Issue{"PROJ-130": {Key: "PROJ-130"}, "PROJ-132": {Key: "PROJ-132", Parent: "PROJ-130"}}}
	var matched []string
	if err := checkFreezeIssue(t.Context(), gate, &activeFreeze{name: "Product X", versionIDs: []string{"x"}, config: conf, anchored: true}, "PROJ-130", &matched); err != nil || len(matched) != 1 {
		t.Fatalf("freeze match = %v, %v", matched, err)
	}
	matched = nil
	if err := checkFreezeIssue(t.Context(), gate, &activeFreeze{name: "Product X", versionIDs: []string{"x"}, config: conf, anchored: true}, "PROJ-132", &matched); err != nil || len(matched) != 0 {
		t.Fatalf("the parent's outside entry let its subtask in: %v, %v", matched, err)
	}
	matched = nil
	if err := checkFreezeIssue(t.Context(), gate, &activeFreeze{name: "Product W", versionIDs: []string{"w"}, config: conf, anchored: true}, "PROJ-130", &matched); err != nil || len(matched) != 0 {
		t.Fatalf("an entry for another version let the item into this freeze: %v, %v", matched, err)
	}
}

func freezeHistoryKey(key, id string, at time.Time) string {
	return key + "\x00" + id + "\x00" + at.Format(time.RFC3339Nano)
}

func TestFreezeStillRefusesOutsideEntryAddedAfterFreeze(t *testing.T) {
	at := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	conf := &config.DeployTarget{ApprovedRelease: &config.ApprovedRelease{Dormant: []config.DormantReleaseItem{outsideEntry("PROJ-130", "Product X")}}, Freeze: &config.Freeze{}}
	gate := &approvalGate{conf: conf.ApprovedRelease, issues: map[string]jira.Issue{"PROJ-130": {Key: "PROJ-130", FixVersions: []jira.Version{{ID: "x", Name: "Product X"}}}}, history: map[string]fixVersionHistoryResult{freezeHistoryKey("PROJ-130", "x", at): {added: true, hasAddition: true}}}
	var matched []string
	err := checkFreezeIssue(t.Context(), gate, &activeFreeze{name: "Product X", versionIDs: []string{"x"}, time: at, config: conf, anchored: true}, "PROJ-130", &matched)
	if err == nil || !strings.Contains(err.Error(), "joined Product X after the freeze") || !strings.Contains(err.Error(), "take it back out of Product X") {
		t.Fatalf("freeze result = %v; want post-freeze refusal", err)
	}
}

// Review finding 2: a subtask declared outside the frozen version and added
// to it after the freeze is refused, though its parent is not in the version.
func TestReviewFreezeSubtaskJoinedAfterFreeze(t *testing.T) {
	at := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	conf := &config.DeployTarget{ApprovedRelease: &config.ApprovedRelease{Dormant: []config.DormantReleaseItem{outsideEntry("PROJ-131", "Product X")}}, Freeze: &config.Freeze{}}
	gate := &approvalGate{conf: conf.ApprovedRelease, issues: map[string]jira.Issue{
		"PROJ-130": {Key: "PROJ-130", Subtasks: []string{"PROJ-131"}},
		"PROJ-131": {Key: "PROJ-131", Parent: "PROJ-130", Subtask: true, FixVersions: []jira.Version{{ID: "x", Name: "Product X"}}},
	}, history: map[string]fixVersionHistoryResult{freezeHistoryKey("PROJ-131", "x", at): {added: true, hasAddition: true}}}
	var matched []string
	err := checkFreezeIssue(t.Context(), gate, &activeFreeze{name: "Product X", versionIDs: []string{"x"}, time: at, config: conf, anchored: true}, "PROJ-131", &matched)
	if err == nil || !strings.Contains(err.Error(), "PROJ-131 joined Product X after the freeze") || len(matched) != 0 {
		t.Fatalf("freeze result = %v, matched=%v; want the subtask's late addition refused", err, matched)
	}
	// Its fix versions are its own: in the version from before the freeze,
	// the subtask is let in by the entry.
	gate.history[freezeHistoryKey("PROJ-131", "x", at)] = fixVersionHistoryResult{added: false, hasAddition: true}
	if err := checkFreezeIssue(t.Context(), gate, &activeFreeze{name: "Product X", versionIDs: []string{"x"}, time: at, config: conf, anchored: true}, "PROJ-131", &matched); err != nil || len(matched) != 1 {
		t.Fatalf("freeze result = %v, matched=%v; want the subtask let in", err, matched)
	}
}

// Review finding 2, the parent: a subtask's entry does not let in a parent
// added to the frozen version after the freeze.
func TestFreezeOutsideSubtaskParentJoinedAfterFreeze(t *testing.T) {
	at := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	conf := &config.DeployTarget{ApprovedRelease: &config.ApprovedRelease{Dormant: []config.DormantReleaseItem{outsideEntry("PROJ-131", "Product X")}}, Freeze: &config.Freeze{}}
	gate := &approvalGate{conf: conf.ApprovedRelease, issues: map[string]jira.Issue{
		"PROJ-130": {Key: "PROJ-130", Subtasks: []string{"PROJ-131"}, FixVersions: []jira.Version{{ID: "x", Name: "Product X"}}},
		"PROJ-131": {Key: "PROJ-131", Parent: "PROJ-130", Subtask: true},
	}, history: map[string]fixVersionHistoryResult{freezeHistoryKey("PROJ-130", "x", at): {added: true, hasAddition: true}}}
	var matched []string
	err := checkFreezeIssue(t.Context(), gate, &activeFreeze{name: "Product X", versionIDs: []string{"x"}, time: at, config: conf, anchored: true}, "PROJ-131", &matched)
	if err == nil || !strings.Contains(err.Error(), "PROJ-130 joined Product X after the freeze") {
		t.Fatalf("freeze result = %v; want the parent's late addition refused", err)
	}
}

func TestFixVersionAddSkipsKeysDeclaredOutsideAnyTarget(t *testing.T) {
	var output bytes.Buffer
	f := &fixVersionRun{app: &App{Stdout: &output}, conf: &config.FixVersion{Prefix: "Product "}, outsideKeys: map[string]bool{"PROJ-150": true}}
	f.add(t.Context(), jira.Issue{Key: "PROJ-150"}, false, nil)
	if len(f.unset) != 0 || !strings.Contains(output.String(), "fix version left unchanged") {
		t.Fatalf("fix.add result unset=%v output=%q", f.unset, output.String())
	}
}

// outsideReturned is a release version check of Y with PROJ-160, whose entry
// is for X, back among the tops.
func outsideReturned(t *testing.T, issue jira.Issue) (*releaseVersionCheck, *bytes.Buffer) {
	t.Helper()
	conf := &config.ApprovedRelease{Version: &config.ApprovedReleaseVersion{Match: "^Product"}, Statuses: []string{"Ready for release"}, Dormant: []config.DormantReleaseItem{outsideEntry("PROJ-160", "Product X")}}
	var output bytes.Buffer
	g := &gateRun{app: &App{Stdout: &output}, conf: conf, activeKeys: map[string]bool{issue.Key: true}, gate: &approvalGate{conf: conf, client: outsideVersionServer(t), issues: map[string]jira.Issue{issue.Key: issue}}}
	v := newReleaseVersionCheck(g, true)
	if v.readTops(t.Context()) {
		t.Fatalf("read tops: %v", v.failures)
	}
	v.selected = jira.Version{ID: "y", Name: "Product Y"}
	return v, &output
}

// Adversary 6 of the design (the stale entry): an entry for X cannot excuse
// a task missing from Y.
func TestOutsideEntryForPreviousVersionDoesNotPassCurrentVersion(t *testing.T) {
	v, _ := outsideReturned(t, jira.Issue{Key: "PROJ-160", Status: "Ready for release"})
	v.checkTops(t.Context())
	got := strings.Join(errorsText(v.failures), " ")
	for _, want := range []string{"version-top", "not in Product Y", "PROJ-160's dormant entry is for Product X, this release is Product Y", `outside_version: "Product Y"`} {
		if !strings.Contains(got, want) {
			t.Errorf("stale entry refusal misses %q: %v", want, v.failures)
		}
	}
}

// Review finding 3, adversary 7: in Y, the task with an entry for X ships as
// the old dormant form: no status needed, and advice to drop outside_version.
func TestTaskInCurrentVersionGetsOutsideRemovalAdvice(t *testing.T) {
	v, output := outsideReturned(t, jira.Issue{Key: "PROJ-160", Status: "Testing", FixVersions: []jira.Version{{ID: "y", Name: "Product Y"}}})
	if v.checkTops(t.Context()) || len(v.failures) != 0 {
		t.Fatalf("current-version failures = %v", v.failures)
	}
	if !strings.Contains(output.String(), "PROJ-160 is in Product Y now: drop outside_version") {
		t.Fatalf("missing stale-entry advice: %s", output.String())
	}
	if len(v.gate.dormant) != 1 || v.gate.dormant[0].Line() != "ships dormant: PROJ-160 — disabled; disabled by flag" {
		t.Fatalf("dormant = %+v; want the old form", v.gate.dormant)
	}
}

// Review finding 6: no example declares a parent that has subtasks.
func TestOutsideHintNeverDeclaresParentWithSubtasks(t *testing.T) {
	g := &approvalGate{}
	parent := jira.Issue{Key: "PROJ-170", Subtasks: []string{"PROJ-171", "PROJ-172"}}
	got := strings.Join(g.declareKeys(parent, []string{"PROJ-172", "PROJ-171"}, "Product X"), "\n")
	if strings.Contains(got, "key: PROJ-170") || !strings.Contains(got, "key: PROJ-171") || !strings.Contains(got, "key: PROJ-172") {
		t.Fatalf("examples by subtask commits:\n%s", got)
	}
	got = strings.Join(g.declareKeys(parent, []string{"PROJ-170"}, "Product X"), "\n")
	if strings.Contains(got, "key: PROJ-170") || !strings.Contains(got, "declare the subtask whose code ships switched off") {
		t.Fatalf("examples by the parent's own commit:\n%s", got)
	}
	got = strings.Join(g.declareKeys(parent, []string{"PROJ-170", "PROJ-171"}, "Product X"), "\n")
	if strings.Contains(got, "key: PROJ-170") || !strings.Contains(got, "key: PROJ-171") {
		t.Fatalf("examples by the parent's and a subtask's commits:\n%s", got)
	}
}

func errorsText(errs []error) []string {
	out := make([]string, len(errs))
	for i, err := range errs {
		out[i] = err.Error()
	}
	return out
}

// Review 2, finding 2: when the parent and the subtask are both in the frozen
// version, both histories are checked: the subtask with an outside entry for
// it, added after the freeze, is refused though its parent was in before.
func TestReview2FreezeBothParentAndSubtaskMembers(t *testing.T) {
	at := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	conf := &config.DeployTarget{ApprovedRelease: &config.ApprovedRelease{Dormant: []config.DormantReleaseItem{outsideEntry("PROJ-131", "Product X")}}, Freeze: &config.Freeze{}}
	inX := []jira.Version{{ID: "x", Name: "Product X"}}
	gate := &approvalGate{conf: conf.ApprovedRelease, issues: map[string]jira.Issue{
		"PROJ-130": {Key: "PROJ-130", Subtasks: []string{"PROJ-131"}, FixVersions: inX},
		"PROJ-131": {Key: "PROJ-131", Parent: "PROJ-130", Subtask: true, FixVersions: inX},
	}, history: map[string]fixVersionHistoryResult{
		freezeHistoryKey("PROJ-130", "x", at): {added: false, hasAddition: true},
		freezeHistoryKey("PROJ-131", "x", at): {added: true, hasAddition: true},
	}}
	freeze := &activeFreeze{name: "Product X", versionIDs: []string{"x"}, time: at, config: conf, anchored: true}
	var matched []string
	err := checkFreezeIssue(t.Context(), gate, freeze, "PROJ-131", &matched)
	if err == nil || !strings.Contains(err.Error(), "PROJ-131 joined Product X after the freeze") {
		t.Fatalf("freeze result = %v, matched=%v; want the subtask's late addition refused", err, matched)
	}
	// The parent's late addition is refused the same way.
	gate.history[freezeHistoryKey("PROJ-130", "x", at)] = fixVersionHistoryResult{added: true, hasAddition: true}
	gate.history[freezeHistoryKey("PROJ-131", "x", at)] = fixVersionHistoryResult{added: false, hasAddition: true}
	if err := checkFreezeIssue(t.Context(), gate, freeze, "PROJ-131", &matched); err == nil || !strings.Contains(err.Error(), "PROJ-130 joined Product X after the freeze") {
		t.Fatalf("freeze result = %v; want the parent's late addition refused", err)
	}
	// Both in from before the freeze: let in.
	gate.history[freezeHistoryKey("PROJ-130", "x", at)] = fixVersionHistoryResult{added: false, hasAddition: true}
	matched = nil
	if err := checkFreezeIssue(t.Context(), gate, freeze, "PROJ-131", &matched); err != nil || len(matched) != 1 {
		t.Fatalf("freeze result = %v, matched=%v; want the subtask let in", err, matched)
	}
}
