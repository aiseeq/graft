package main

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// Work declared with approved_release.dormant outside_version, through the
// real graft deploy check and deploy: the review findings and the adversary
// scenarios of the design.

const liveApproved = `approved_release: {prefix: "Product v", ignore_paths: ["docs/**", "*.md"]}`

// liveOutside is deployConfig whose live target (prefix "Product v") has the
// dormant entries given.
func liveOutside(dormant string) func(state string, q func(string) string) string {
	return func(state string, q func(string) string) string {
		return strings.Replace(deployConfig(state, q), liveApproved, `approved_release: {prefix: "Product v", ignore_paths: ["docs/**", "*.md"], dormant: [`+dormant+`]}`, 1)
	}
}

// prodOutside is versionGateConfig whose prod target checks the release
// version (match ^Product) and has the dormant entries given.
func prodOutside(dormant string) func(state string, q func(string) string) string {
	return func(state string, q func(string) string) string {
		return strings.Replace(versionGateConfig(state, q), "approved_release: {statuses: [Ready for release, Done]}", `approved_release: {statuses: [Ready for release, Done], version: {match: '^Product'}, dormant: [`+dormant+`]}`, 1)
	}
}

func (d *deployFixture) setStatuses(statuses map[string]string) {
	d.jira.mu.Lock()
	defer d.jira.mu.Unlock()
	maps.Copy(d.jira.statuses, statuses)
}

func (d *deployFixture) setParents(parents map[string]string) {
	d.jira.mu.Lock()
	defer d.jira.mu.Unlock()
	d.jira.parents = parents
}

// prodVersionFixture is a prod target with a recorded commit, the released
// Product 01.11.26 (9) and the approved Product 05.11.26 (1).
func prodVersionFixture(t *testing.T, dormant string, fixVersions map[string][]string) *deployFixture {
	t.Helper()
	d := newDeployFixture(t, prodOutside(dormant))
	writeFile(t, d.state+"/gate-sha", d.git("rev-parse", "HEAD")+"\n")
	d.jira.setVersions([]map[string]any{{"id": "9", "name": "Product 01.11.26", "released": true}, {"id": "1", "name": "Product 05.11.26"}}, fixVersions)
	d.jira.setApprovers(map[string][]string{"1": {"APPROVED", "APPROVED"}})
	return d
}

// liveFixture is the live target (prefix "Product v") with a recorded commit;
// Product v1.2.10 (10010) and Product v1.3.0 (10030) are approved.
func liveFixture(t *testing.T, dormant string, fixVersions map[string][]string) *deployFixture {
	t.Helper()
	d := newDeployFixture(t, liveOutside(dormant))
	d.jira.setVersions(productVersions(), fixVersions)
	d.jira.setApprovers(map[string][]string{"10010": {"APPROVED", "APPROVED"}, "10030": {"APPROVED", "APPROVED"}})
	d.mustGraft("deploy", "--unapproved", "live")
	return d
}

func wantOut(t *testing.T, out string, code int, success bool, wants []string, unwanted ...string) {
	t.Helper()
	if (code == 0) != success {
		t.Errorf("exit %d, want success %v:\n%s", code, success, out)
	}
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	for _, bad := range unwanted {
		if strings.Contains(out, bad) {
			t.Errorf("unexpected %q:\n%s", bad, out)
		}
	}
}

const outside60 = `{key: PROJ-60, reason: new checkout, disabled_by: flag checkout.v2, outside_version: "%s"}`

func entry60(version string) string { return strings.Replace(outside60, "%s", version, 1) }

// Review finding 1: an old dormant entry is no live work; the outside entry
// it would anchor stays the old form, which needs an approved version.
func TestDeployOutsideOldDormantIsNoLiveAnchor(t *testing.T) {
	t.Run("prefix", func(t *testing.T) {
		d := liveFixture(t, `{key: PROJ-10, reason: old switched-off work}, `+entry60("Product v1.2.10"), map[string][]string{"PROJ-10": {"10010"}, "PROJ-1": {"10010"}})
		d.setStatuses(map[string]string{"PROJ-10": "Testing", "PROJ-60": "Testing"})
		d.commitFile("old.txt", "feat: PROJ-10 old dormant work")
		d.commitFile("new.txt", "feat: PROJ-60 new disabled work")
		out, code := d.graft("", "deploy", "check", "live")
		wantOut(t, out, code, false, []string{"PROJ-60 not in an approved", "no live work of Product v1.2.10, unreleased and approved, goes out"}, "ships dormant outside")
		// Live work of the version links it.
		d.commitFile("live.txt", "feat: PROJ-1 live work")
		out, code = d.graft("", "deploy", "check", "live")
		wantOut(t, out, code, true, []string{"ships dormant outside Product v1.2.10: PROJ-60 — new checkout; disabled by flag checkout.v2"})
	})
	t.Run("version", func(t *testing.T) {
		d := prodVersionFixture(t, `{key: PROJ-10, reason: old switched-off work}, `+entry60("Product 05.11.26"), map[string][]string{"PROJ-10": {"1"}, "PROJ-11": {"1"}})
		d.setStatuses(map[string]string{"PROJ-10": "Testing", "PROJ-60": "Testing", "PROJ-11": "Ready for release"})
		d.commitFile("old.txt", "feat: PROJ-10 old dormant work")
		d.commitFile("new.txt", "feat: PROJ-60 new disabled work")
		out, code := d.graft("", "deploy", "check", "prod")
		wantOut(t, out, code, false, []string{"version-top", "PROJ-60 is Testing, fix versions none: not in Product 05.11.26"}, "ships dormant outside")
		d.commitFile("live.txt", "feat: PROJ-11 live work")
		out, code = d.graft("", "deploy", "check", "prod")
		wantOut(t, out, code, true, []string{"ships dormant outside Product 05.11.26: PROJ-60 — new checkout; disabled by flag checkout.v2"})
	})
}

// Adversary 1: an outside entry on a parent excuses none of its subtasks.
func TestDeployOutsideParentEntryCoversNoSubtask(t *testing.T) {
	d := prodVersionFixture(t, `{key: PROJ-100, reason: feature a, disabled_by: flag feature_a, outside_version: "Product 05.11.26"}`, map[string][]string{"PROJ-10": {"1"}})
	d.setParents(map[string]string{"PROJ-101": "PROJ-100", "PROJ-102": "PROJ-100"})
	d.setStatuses(map[string]string{"PROJ-10": "Ready for release", "PROJ-100": "In Progress", "PROJ-101": "In Progress", "PROJ-102": "In Progress"})
	d.commitFile("live.txt", "feat: PROJ-10 live work")
	d.commitFile("a.txt", "feat: PROJ-101 switched off")
	d.commitFile("b.txt", "feat: PROJ-102 another feature")
	out, code := d.graft("", "deploy", "check", "prod")
	wantOut(t, out, code, false, []string{"version-top", "PROJ-100 is In Progress, fix versions none: not in Product 05.11.26", "key: PROJ-101", "key: PROJ-102"}, "key: PROJ-100", "ships dormant outside")
}

// Adversary 2: with prefix, an entry for X ships with live work of X; the
// next release takes Y while X stays unreleased and approved, and the entry
// lets nothing out.
func TestDeployOutsidePrefixEntryEndsWithItsVersion(t *testing.T) {
	d := liveFixture(t, entry60("Product v1.2.10"), map[string][]string{"PROJ-10": {"10010"}, "PROJ-11": {"10030"}})
	d.setStatuses(map[string]string{"PROJ-10": "Testing", "PROJ-11": "Testing", "PROJ-60": "Testing"})
	d.commitFile("x.txt", "feat: PROJ-10 work of X")
	d.commitFile("off.txt", "feat: PROJ-60 switched off")
	out := d.mustGraft("deploy", "live")
	wantOut(t, out, 0, true, []string{"ships dormant outside Product v1.2.10: PROJ-60"})
	d.commitFile("y.txt", "feat: PROJ-11 work of Y")
	d.commitFile("off2.txt", "feat: PROJ-60 more")
	out, code := d.graft("", "deploy", "check", "live")
	wantOut(t, out, code, false, []string{"PROJ-60 not in an approved", "its outside_version entry is for Product v1.2.10, and no live work of Product v1.2.10, unreleased and approved, goes out"}, "ships dormant outside")
}

// Adversary 3: an entry not linked to the release does not let an unfinished
// subtask out of its parent's checks.
func TestDeployOutsideUnlinkedSubtaskHoldsItsParent(t *testing.T) {
	t.Run("version", func(t *testing.T) {
		d := prodVersionFixture(t, `{key: PROJ-121, reason: feature b, disabled_by: flag feature_b, outside_version: "Product 01.11.26"}`, map[string][]string{"PROJ-120": {"1"}})
		d.setParents(map[string]string{"PROJ-121": "PROJ-120"})
		d.setStatuses(map[string]string{"PROJ-120": "Ready for release", "PROJ-121": "In Progress"})
		d.commitFile("p.txt", "feat: PROJ-120 parent work")
		out, code := d.graft("", "deploy", "check", "prod")
		wantOut(t, out, code, false, []string{"version-umbrella", "PROJ-120 is Ready for release with unfinished subtasks PROJ-121 (In Progress)"})
	})
	t.Run("prefix", func(t *testing.T) {
		d := liveFixture(t, `{key: PROJ-121, reason: feature b, disabled_by: flag feature_b, outside_version: "Product v1.2.9"}`, map[string][]string{"PROJ-120": {"10010"}})
		d.setParents(map[string]string{"PROJ-121": "PROJ-120"})
		d.setStatuses(map[string]string{"PROJ-120": "Testing", "PROJ-121": "In Progress"})
		d.commitFile("p.txt", "feat: PROJ-120 parent work")
		out, code := d.graft("", "deploy", "check", "live")
		wantOut(t, out, code, false, []string{"PROJ-120 is Testing with open subtasks PROJ-121 (In Progress)"})
	})
}

// Adversary 6: a target without approved_release does not put back into a
// version an item some target declares outside one.
func TestDeployOutsideEntryKeepsFixVersionOff(t *testing.T) {
	d := newDeployFixture(t, liveOutside(`{key: PROJ-150, reason: feature c, disabled_by: flag feature_c, outside_version: "Product v1.2.10"}`))
	d.jira.setVersions(productVersions(), map[string][]string{})
	d.setStatuses(map[string]string{"PROJ-150": "In Progress"})
	d.mustGraft("deploy", "uat")
	d.commitFile("c.txt", "feat: PROJ-150 switched off")
	d.commitFile("d.txt", "fix: PROJ-1 live work")
	out := d.mustGraft("deploy", "uat")
	edits, _ := d.jira.editsSnapshot()
	if !slices.Contains(edits, "PROJ-1 add 10010") || slices.ContainsFunc(edits, func(e string) bool { return strings.HasPrefix(e, "PROJ-150 ") }) {
		t.Errorf("edits = %v\n%s", edits, out)
	}
	wantOut(t, out, 0, true, []string{"fix version left unchanged for PROJ-150"})
}

// Adversary 7, review finding 3: the item is in the version now while its
// entry still names the last one; it ships as the old dormant form, without a
// status, and is told to drop outside_version: with new commits and without.
func TestDeployOutsideEntryOutlivesItsRelease(t *testing.T) {
	t.Run("version, its commit alone", func(t *testing.T) {
		d := prodVersionFixture(t, `{key: PROJ-160, reason: feature d, disabled_by: flag feature_d, outside_version: "Product 01.11.26"}`, map[string][]string{"PROJ-160": {"1"}})
		d.setStatuses(map[string]string{"PROJ-160": "Testing"})
		d.commitFile("d.txt", "feat: PROJ-160 more of it")
		out, code := d.graft("", "deploy", "check", "prod")
		wantOut(t, out, code, true, []string{"PROJ-160 is in Product 05.11.26 now: drop outside_version", "ships dormant: PROJ-160 — feature d; disabled by flag feature_d"}, "ships dormant outside")
	})
	t.Run("version, a subtask without its commits", func(t *testing.T) {
		d := prodVersionFixture(t, `{key: PROJ-162, reason: feature e, disabled_by: flag feature_e, outside_version: "Product 01.11.26"}`, map[string][]string{"PROJ-160": {"1"}, "PROJ-161": {"1"}})
		d.setParents(map[string]string{"PROJ-162": "PROJ-160"})
		d.setStatuses(map[string]string{"PROJ-160": "Ready for release", "PROJ-161": "Ready for release", "PROJ-162": "Done"})
		d.commitFile("e.txt", "feat: PROJ-161 other work")
		out, code := d.graft("", "deploy", "check", "prod")
		wantOut(t, out, code, true, []string{"PROJ-162 is in Product 05.11.26 with its parent PROJ-160 now: drop outside_version"}, "ships dormant outside")
	})
	t.Run("version, without its commits", func(t *testing.T) {
		d := prodVersionFixture(t, `{key: PROJ-160, reason: feature d, disabled_by: flag feature_d, outside_version: "Product 01.11.26"}`, map[string][]string{"PROJ-160": {"1"}, "PROJ-161": {"1"}})
		d.setStatuses(map[string]string{"PROJ-160": "Testing", "PROJ-161": "Ready for release"})
		d.commitFile("e.txt", "feat: PROJ-161 other work")
		out, code := d.graft("", "deploy", "check", "prod")
		wantOut(t, out, code, true, []string{"PROJ-160 is in Product 05.11.26 now: drop outside_version", "ships dormant: PROJ-160 — feature d"}, "ships dormant outside")
	})
	t.Run("prefix, its commit alone", func(t *testing.T) {
		d := liveFixture(t, `{key: PROJ-160, reason: feature d, disabled_by: flag feature_d, outside_version: "Product v9.9.9"}`, map[string][]string{"PROJ-160": {"10030"}})
		d.setStatuses(map[string]string{"PROJ-160": "Testing"})
		d.commitFile("d.txt", "feat: PROJ-160 more of it")
		out, code := d.graft("", "deploy", "check", "live")
		wantOut(t, out, code, true, []string{"PROJ-160 is in Product v1.3.0 now: drop outside_version", "ships dormant: PROJ-160 — feature d"}, "ships dormant outside")
	})
	t.Run("prefix, without its commits", func(t *testing.T) {
		d := liveFixture(t, `{key: PROJ-160, reason: feature d, disabled_by: flag feature_d, outside_version: "Product v9.9.9"}`, map[string][]string{"PROJ-160": {"10030"}, "PROJ-1": {"10030"}})
		d.setStatuses(map[string]string{"PROJ-160": "Testing"})
		d.commitFile("e.txt", "feat: PROJ-1 other work")
		out, code := d.graft("", "deploy", "check", "live")
		wantOut(t, out, code, true, []string{"PROJ-160 is in Product v1.3.0 now: drop outside_version from its approved_release.dormant entry"}, "ships dormant outside")
	})
}

// Adversary 8, review finding 6: the refusal gives an example per subtask
// named by the commits, never one for their parent; the prefix gate names
// the version, the example and who took the item out.
func TestDeployOutsideHintByCommitKeys(t *testing.T) {
	t.Run("version", func(t *testing.T) {
		d := prodVersionFixture(t, `{key: PROJ-10, reason: unrelated, disabled_by: x, outside_version: "Product 05.11.26"}`, map[string][]string{"PROJ-11": {"1"}})
		d.setParents(map[string]string{"PROJ-171": "PROJ-170", "PROJ-172": "PROJ-170"})
		d.setStatuses(map[string]string{"PROJ-11": "Ready for release", "PROJ-170": "In Progress", "PROJ-171": "In Progress", "PROJ-172": "In Progress"})
		d.commitFile("live.txt", "feat: PROJ-11 live work")
		d.commitFile("a.txt", "feat: PROJ-171 one")
		d.commitFile("b.txt", "feat: PROJ-172 two")
		out, code := d.graft("", "deploy", "check", "prod")
		wantOut(t, out, code, false, []string{"PROJ-170 is In Progress, fix versions none: not in Product 05.11.26", `{key: PROJ-171, reason: "...", disabled_by: "<flag or setting that keeps it off>", outside_version: "Product 05.11.26"}`, "key: PROJ-172", "Live work outside the version is reverted, not declared"}, "key: PROJ-170", "add it to")
	})
	t.Run("version, the parent's own commit", func(t *testing.T) {
		d := prodVersionFixture(t, `{key: PROJ-10, reason: unrelated, disabled_by: x, outside_version: "Product 05.11.26"}`, map[string][]string{"PROJ-11": {"1"}})
		d.setParents(map[string]string{"PROJ-171": "PROJ-170"})
		d.setStatuses(map[string]string{"PROJ-11": "Ready for release", "PROJ-170": "In Progress", "PROJ-171": "In Progress"})
		d.commitFile("live.txt", "feat: PROJ-11 live work")
		d.commitFile("a.txt", "feat: PROJ-170 parent work")
		out, code := d.graft("", "deploy", "check", "prod")
		wantOut(t, out, code, false, []string{"PROJ-170 has subtasks: declare the subtask whose code ships switched off"}, "key: PROJ-170")
	})
	t.Run("prefix", func(t *testing.T) {
		d := liveFixture(t, `{key: PROJ-10, reason: unrelated, disabled_by: x, outside_version: "Product v1.2.10"}`, map[string][]string{"PROJ-1": {"10010"}})
		d.setStatuses(map[string]string{"PROJ-60": "Testing"})
		d.jira.mu.Lock()
		d.jira.fixChanges = map[string][]fakeFixChange{"PROJ-60": {{at: time.Date(2026, 11, 16, 15, 55, 0, 0, time.UTC), id: "10010", author: "Jane Doe"}}}
		d.jira.mu.Unlock()
		d.commitFile("live.txt", "feat: PROJ-1 live work")
		d.commitFile("off.txt", "feat: PROJ-60 not in the version")
		out, code := d.graft("", "deploy", "check", "live")
		wantOut(t, out, code, false, []string{"PROJ-60 is in no approved Product v version (taken out of Product v1.2.10 by Jane Doe on 2026-11-16 15:55)", `{key: PROJ-60, reason: "...", disabled_by: "<flag or setting that keeps it off>", outside_version: "Product v1.2.10"}`}, "into the approved version")
	})
}

// Review 2, finding 1: an outside entry on a parent covers the parent alone;
// its commit is held by an open subtask as without the entry.
func TestReview2OutsideParentDoesNotHideOpenSubtask(t *testing.T) {
	d := liveFixture(t, `{key: PROJ-120, reason: feature f, disabled_by: flag feature_f, outside_version: "Product v1.2.10"}`, map[string][]string{"PROJ-10": {"10010"}})
	d.setParents(map[string]string{"PROJ-121": "PROJ-120"})
	d.setStatuses(map[string]string{"PROJ-10": "Testing", "PROJ-120": "Testing", "PROJ-121": "In Progress"})
	d.commitFile("x.txt", "feat: PROJ-10 work of X")
	d.commitFile("p.txt", "feat: PROJ-120 parent work")
	out, code := d.graft("", "deploy", "check", "live")
	wantOut(t, out, code, false, []string{"PROJ-120 is Testing with open subtasks PROJ-121 (In Progress)"}, "ships dormant outside")
	// The subtask done, the entry lets the parent out.
	d.setStatuses(map[string]string{"PROJ-121": "Done"})
	out, code = d.graft("", "deploy", "check", "live")
	wantOut(t, out, code, true, []string{"ships dormant outside Product v1.2.10: PROJ-120 — feature f"})
}

// Review 2, finding 3: a released version anchors no outside entry, though it
// is approved and live work of it goes out; the entry is the old form.
func TestReview2ReleasedVersionCannotAnchorOutside(t *testing.T) {
	d := liveFixture(t, entry60("Product v1.2.9"), map[string][]string{"PROJ-10": {"10009"}})
	d.jira.setApprovers(map[string][]string{"10009": {"APPROVED", "APPROVED"}, "10010": {"APPROVED", "APPROVED"}, "10030": {"APPROVED", "APPROVED"}})
	d.setStatuses(map[string]string{"PROJ-10": "Testing", "PROJ-60": "Testing"})
	d.commitFile("x.txt", "feat: PROJ-10 work of the released X")
	d.commitFile("off.txt", "feat: PROJ-60 switched off")
	out, code := d.graft("", "deploy", "check", "live")
	wantOut(t, out, code, false, []string{"PROJ-60 not in an approved", "no live work of Product v1.2.9, unreleased and approved, goes out"}, "ships dormant outside")
}

// Review 2, finding 5: with prefix and version, the item has an approved
// version of its own, and the release version check links its entry to the
// selected one: the linked form is the one the release lists.
func TestReview2LinkedEntryWinsOverOldForm(t *testing.T) {
	both := func(state string, q func(string) string) string {
		return strings.Replace(versionGateConfig(state, q), "approved_release: {statuses: [Ready for release, Done]}", `approved_release: {prefix: "Product ", statuses: [Ready for release, Done], version: {match: '^Product'}, dormant: [`+entry60("Product 05.11.26")+`]}`, 1)
	}
	d := newDeployFixture(t, both)
	writeFile(t, d.state+"/gate-sha", d.git("rev-parse", "HEAD")+"\n")
	d.jira.setVersions([]map[string]any{{"id": "1", "name": "Product 05.11.26"}, {"id": "2", "name": "Product 06.11.26"}}, map[string][]string{"PROJ-10": {"1"}, "PROJ-60": {"2"}})
	d.jira.setApprovers(map[string][]string{"1": {"APPROVED", "APPROVED"}, "2": {"APPROVED", "APPROVED"}})
	d.setStatuses(map[string]string{"PROJ-10": "Ready for release", "PROJ-60": "Testing"})
	d.commitFile("live.txt", "feat: PROJ-10 live work")
	d.commitFile("off.txt", "feat: PROJ-60 switched off")
	out, code := d.graft("", "deploy", "check", "prod")
	wantOut(t, out, code, true, []string{"ships dormant outside Product 05.11.26: PROJ-60 — new checkout"}, "ships dormant: PROJ-60")
	if n := strings.Count(out, ": PROJ-60 — "); n != 1 {
		t.Errorf("PROJ-60 listed %d times, want once:\n%s", n, out)
	}
}

// Review 3, finding 1: an outside entry stands in for the version check
// alone; with version, a parent with one is held by its open subtask as
// without it.
func TestReview3VersionOutsideParentOpenSubtask(t *testing.T) {
	const entry = `{key: PROJ-120, reason: feature f, disabled_by: flag feature_f, outside_version: "Product 05.11.26"}`
	both := func(state string, q func(string) string) string {
		return strings.Replace(versionGateConfig(state, q), "approved_release: {statuses: [Ready for release, Done]}", `approved_release: {prefix: "Product ", statuses: [Ready for release, Done], version: {match: '^Product'}, dormant: [`+entry+`]}`, 1)
	}
	for name, config := range map[string]func(string, func(string) string) string{"version": prodOutside(entry), "prefix and version": both} {
		t.Run(name, func(t *testing.T) {
			d := newDeployFixture(t, config)
			writeFile(t, d.state+"/gate-sha", d.git("rev-parse", "HEAD")+"\n")
			d.jira.setVersions([]map[string]any{{"id": "1", "name": "Product 05.11.26"}}, map[string][]string{"PROJ-10": {"1"}})
			d.jira.setApprovers(map[string][]string{"1": {"APPROVED", "APPROVED"}})
			d.setParents(map[string]string{"PROJ-121": "PROJ-120"})
			d.setStatuses(map[string]string{"PROJ-10": "Ready for release", "PROJ-120": "Testing", "PROJ-121": "In Progress"})
			d.commitFile("live.txt", "feat: PROJ-10 live work")
			d.commitFile("p.txt", "feat: PROJ-120 parent work")
			out, code := d.graft("", "deploy", "check", "prod")
			wantOut(t, out, code, false, []string{"PROJ-121 (In Progress)"})
			d.setStatuses(map[string]string{"PROJ-121": "Done"})
			out, code = d.graft("", "deploy", "check", "prod")
			wantOut(t, out, code, true, []string{"ships dormant outside Product 05.11.26: PROJ-120 — feature f"})
		})
	}
}

// Review 3, finding 3: a subtask's outside entry does not let its parent out
// while the subtask is open, the parent's own entry being no live anchor.
func TestReview3UnlinkedSubtaskDoesNotHoldOutsideParent(t *testing.T) {
	entries := `{key: PROJ-120, reason: feature f, disabled_by: flag feature_f, outside_version: "%s"}, {key: PROJ-121, reason: feature g, disabled_by: flag feature_g, outside_version: "%s"}`
	t.Run("prefix", func(t *testing.T) {
		d := liveFixture(t, strings.ReplaceAll(entries, "%s", "Product v1.2.10"), map[string][]string{"PROJ-120": {"10010"}})
		d.setParents(map[string]string{"PROJ-121": "PROJ-120"})
		d.setStatuses(map[string]string{"PROJ-120": "Testing", "PROJ-121": "In Progress"})
		d.commitFile("p.txt", "feat: PROJ-120 parent work")
		out, code := d.graft("", "deploy", "check", "live")
		wantOut(t, out, code, false, []string{"PROJ-120 is Testing with open subtasks PROJ-121 (In Progress)"})
	})
	t.Run("prefix, the subtask's own commit", func(t *testing.T) {
		d := liveFixture(t, `{key: PROJ-121, reason: feature g, disabled_by: flag feature_g, outside_version: "Product v1.2.10"}`, map[string][]string{"PROJ-10": {"10010"}})
		d.setParents(map[string]string{"PROJ-121": "PROJ-120", "PROJ-122": "PROJ-120"})
		d.setStatuses(map[string]string{"PROJ-10": "Testing", "PROJ-120": "Testing", "PROJ-121": "Done", "PROJ-122": "In Progress"})
		d.commitFile("x.txt", "feat: PROJ-10 work of X")
		d.commitFile("s.txt", "feat: PROJ-121 switched off")
		out, code := d.graft("", "deploy", "check", "live")
		wantOut(t, out, code, false, []string{"PROJ-120 is Testing with open subtasks PROJ-122 (In Progress)"}, "ships dormant outside")
		d.setStatuses(map[string]string{"PROJ-122": "Done"})
		out, code = d.graft("", "deploy", "check", "live")
		wantOut(t, out, code, true, []string{"ships dormant outside Product v1.2.10: PROJ-121 — feature g"})
	})
	t.Run("version", func(t *testing.T) {
		d := prodVersionFixture(t, strings.ReplaceAll(entries, "%s", "Product 05.11.26"), map[string][]string{"PROJ-120": {"1"}})
		d.setParents(map[string]string{"PROJ-121": "PROJ-120"})
		d.setStatuses(map[string]string{"PROJ-120": "Ready for release", "PROJ-121": "In Progress"})
		d.commitFile("p.txt", "feat: PROJ-120 parent work")
		out, code := d.graft("", "deploy", "check", "prod")
		wantOut(t, out, code, false, []string{"version-umbrella", "PROJ-121 (In Progress)"})
	})
}

// Review 3, finding 4: the advice to drop outside_version looks at every fix
// version of the item, and at a subtask's own, not the first approved one.
func TestReview3AdviceSecondApprovedVersion(t *testing.T) {
	entries := `{key: PROJ-160, reason: feature d, disabled_by: flag feature_d, outside_version: "Product v9.9.9"}, {key: PROJ-163, reason: feature h, disabled_by: flag feature_h, outside_version: "Product v9.9.9"}`
	d := liveFixture(t, entries, map[string][]string{"PROJ-160": {"10010", "10030"}, "PROJ-163": {"10030"}, "PROJ-1": {"10030"}})
	d.setParents(map[string]string{"PROJ-163": "PROJ-164"})
	d.setStatuses(map[string]string{"PROJ-160": "Testing", "PROJ-163": "Testing", "PROJ-164": "In Progress"})
	d.commitFile("e.txt", "feat: PROJ-1 other work")
	out, code := d.graft("", "deploy", "check", "live")
	wantOut(t, out, code, true, []string{"PROJ-160 is in Product v1.3.0 now: drop outside_version", "PROJ-163 is in Product v1.3.0 now: drop outside_version"})
}

// freezeOutsideConfig is freezeConfig whose prod target has the dormant
// entries given.
func freezeOutsideConfig(dormant string) func(state string, q func(string) string) string {
	return func(state string, q func(string) string) string {
		return strings.Replace(freezeConfig(state, q), `approved_release: {prefix: "Product v", ignore_paths: ["docs/**"]}`, `approved_release: {prefix: "Product v", ignore_paths: ["docs/**"], dormant: [`+dormant+`]}`, 1)
	}
}

// Review 3, finding 2: the freeze counts an outside entry by the rule of the
// gates: live work of the frozen version goes out, and it is unreleased,
// unarchived and approved.
func TestReview3FreezeOutsideNeedsALiveApprovedAnchor(t *testing.T) {
	setup := func(t *testing.T, live bool) *deployFixture {
		d := freezeRepoWith(t, freezeOutsideConfig(entry60("Product v1.2.10")))
		if live {
			d.commitFile("one.txt", "feat: PROJ-1 accepted work")
		}
		d.freeze()
		d.commitFile("off.txt", "feat: PROJ-60 switched off")
		return d
	}
	refused := []string{"PROJ-60 not in Product v1.2.10", "no live work of Product v1.2.10, unreleased and approved, goes out"}
	t.Run("no live work", func(t *testing.T) {
		out, code := setup(t, false).graft("", "deploy", "acc")
		wantOut(t, out, code, false, refused, "ships dormant outside")
	})
	t.Run("live and approved", func(t *testing.T) {
		out := setup(t, true).mustGraft("deploy", "acc")
		wantOut(t, out, 0, true, []string{"ships dormant outside Product v1.2.10: PROJ-60 — new checkout"})
	})
	t.Run("not approved", func(t *testing.T) {
		d := setup(t, true)
		d.jira.setApprovers(map[string][]string{"10010": {"APPROVED", "PENDING"}})
		out, code := d.graft("", "deploy", "acc")
		wantOut(t, out, code, false, refused, "ships dormant outside")
	})
	t.Run("archived", func(t *testing.T) {
		d := setup(t, true)
		d.jira.mu.Lock()
		for _, v := range d.jira.versions {
			if v["id"] == "10010" {
				v["archived"] = true
			}
		}
		d.jira.mu.Unlock()
		out, code := d.graft("", "deploy", "acc")
		wantOut(t, out, code, false, refused, "ships dormant outside")
	})
}

// Review 3, finding 5: a subtask in the frozen version by its own fix
// versions is a member, not outside: it ships as the old dormant form.
func TestReview3FreezeSubtaskOwnMemberIsNoOutside(t *testing.T) {
	d := freezeRepoWith(t, freezeOutsideConfig(`{key: PROJ-131, reason: feature i, disabled_by: flag feature_i, outside_version: "Product v1.2.10"}`))
	d.jira.mu.Lock()
	d.jira.parents = map[string]string{"PROJ-131": "PROJ-130"}
	d.jira.fixVersions["PROJ-131"] = []string{"10010"}
	d.jira.fixChanges["PROJ-131"] = []fakeFixChange{{at: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC), id: "10010", added: true}}
	d.jira.mu.Unlock()
	d.freeze()
	d.commitFile("sub.txt", "feat: PROJ-131 switched off")
	out := d.mustGraft("deploy", "acc")
	wantOut(t, out, 0, true, []string{"ships dormant: PROJ-131 — feature i"}, "ships dormant outside")
}

// Review 4: a commit that ships nothing (ignore_paths alone) is no live work
// of its version; the same commit with a shipped file is.
func TestReview4PrefixIgnoredCommitIsNoLiveAnchor(t *testing.T) {
	for _, c := range []struct {
		file    string
		success bool
	}{{"docs/ignored.txt", false}, {"work.txt", true}} {
		t.Run(c.file, func(t *testing.T) {
			d := liveFixture(t, entry60("Product v1.2.10"), map[string][]string{"PROJ-1": {"10010"}})
			d.setStatuses(map[string]string{"PROJ-1": "Testing", "PROJ-60": "Testing"})
			d.commitFile(c.file, "docs: PROJ-1 work of X")
			d.commitFile("off.txt", "feat: PROJ-60 switched off")
			out, code := d.graft("", "deploy", "check", "live")
			if c.success {
				wantOut(t, out, code, true, []string{"ships dormant outside Product v1.2.10: PROJ-60"})
				return
			}
			wantOut(t, out, code, false, []string{"PROJ-60 not in an approved", "no live work of Product v1.2.10"}, "ships dormant outside")
		})
	}
}

func TestReview4FreezeIgnoredCommitIsNoLiveAnchor(t *testing.T) {
	for _, c := range []struct {
		file    string
		success bool
	}{{"docs/ignored.txt", false}, {"work.txt", true}} {
		t.Run(c.file, func(t *testing.T) {
			d := freezeRepoWith(t, freezeOutsideConfig(entry60("Product v1.2.10")))
			d.commitFile(c.file, "docs: PROJ-1 work of X")
			d.freeze()
			d.commitFile("off.txt", "feat: PROJ-60 switched off")
			out, code := d.graft("", "deploy", "acc")
			if c.success {
				wantOut(t, out, code, true, []string{"ships dormant outside Product v1.2.10: PROJ-60"})
				return
			}
			wantOut(t, out, code, false, []string{"PROJ-60 not in Product v1.2.10", "no live work of Product v1.2.10"}, "ships dormant outside")
		})
	}
}

// emptyCommit commits nothing, pushed like the other fixture commits.
func (d *deployFixture) emptyCommit(msg string) {
	d.t.Helper()
	d.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "--allow-empty", "-m", msg)
	d.git("push", "-q", "-u", "origin", "HEAD")
}

// versionOnlyCommit raises VERSION alone, as graft commit would.
func (d *deployFixture) versionOnlyCommit(msg string) {
	d.t.Helper()
	branch := d.git("branch", "--show-current")
	d.mustVersionedCommit(d.work, msg, strings.HasPrefix(branch, "hotfix/") || strings.HasPrefix(branch, "release/"))
}

// Review 5: an empty commit, or one that only raises the version, is no live
// work of its version, in each mode.
func TestReview5EmptyOrVersionOnlyCommitIsNoLiveAnchor(t *testing.T) {
	kinds := map[string]func(d *deployFixture, msg string){
		"empty":        (*deployFixture).emptyCommit,
		"version only": (*deployFixture).versionOnlyCommit,
	}
	for kind, anchor := range kinds {
		t.Run("prefix, "+kind, func(t *testing.T) {
			d := liveFixture(t, entry60("Product v1.2.10"), map[string][]string{"PROJ-1": {"10010"}})
			d.setStatuses(map[string]string{"PROJ-1": "Testing", "PROJ-60": "Testing"})
			anchor(d, "feat: PROJ-1 work of X")
			d.commitFile("off.txt", "feat: PROJ-60 switched off")
			out, code := d.graft("", "deploy", "check", "live")
			wantOut(t, out, code, false, []string{"PROJ-60 not in an approved", "no live work of Product v1.2.10"}, "ships dormant outside")
		})
		t.Run("version, "+kind, func(t *testing.T) {
			d := prodVersionFixture(t, entry60("Product 05.11.26"), map[string][]string{"PROJ-11": {"1"}})
			d.setStatuses(map[string]string{"PROJ-11": "Ready for release", "PROJ-60": "Testing"})
			anchor(d, "feat: PROJ-11 work of X")
			d.commitFile("off.txt", "feat: PROJ-60 switched off")
			out, code := d.graft("", "deploy", "check", "prod")
			// A version-only commit leaves the release with no item of its own
			// (main bf9049f), so the refusal comes at version selection instead.
			want := []string{"version-top", "PROJ-60 is Testing, fix versions none: not in Product 05.11.26"}
			if kind == "version only" {
				want = []string{"version-select", "release tasks PROJ-60 have no unreleased release version"}
			}
			wantOut(t, out, code, false, want, "ships dormant outside")
		})
		t.Run("freeze, "+kind, func(t *testing.T) {
			d := freezeRepoWith(t, freezeOutsideConfig(entry60("Product v1.2.10")))
			d.freeze()
			anchor(d, "feat: PROJ-1 work of X")
			d.commitFile("off.txt", "feat: PROJ-60 switched off")
			out, code := d.graft("", "deploy", "acc")
			wantOut(t, out, code, false, []string{"PROJ-60 not in Product v1.2.10", "no live work of Product v1.2.10"}, "ships dormant outside")
		})
	}
}
