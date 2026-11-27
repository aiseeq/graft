package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/jira"
)

type releaseVersionCheck struct {
	gate     *gateRun
	collect  bool
	failures []error
	tops     map[string]jira.Issue
	// sources are the commit keys that brought each top in; topOf, the top
	// each commit key went to.
	sources map[string][]string
	topOf   map[string]string
	// live are the commit keys without any dormant entry whose commit is live
	// work (gateRun.liveKeys): work that runs.
	live map[string]bool
	// outside are the commit keys with an outside_version entry, kept from
	// the tops until the version is chosen; returned, those put back among
	// the tops because their entry is not linked to this release.
	outside  map[string]jira.Issue
	returned map[string]bool
	// linkedKeys are the outside items whose entry is linked: it stands in
	// for their top being in the version, and for nothing else.
	linkedKeys  map[string]bool
	umbrellas   map[string]bool
	selected    jira.Version
	humanMe     string
	humanMeErr  error
	humanMeRead bool
	// liveInSelected: live work of the selected version goes out, which links
	// the outside entries naming it.
	liveInSelected bool
}

func newReleaseVersionCheck(g *gateRun, collect bool) *releaseVersionCheck {
	v := &releaseVersionCheck{gate: g, collect: collect}
	v.ensure()
	return v
}

// ensure makes the maps a check fills.
func (v *releaseVersionCheck) ensure() {
	if v.tops == nil {
		v.tops = map[string]jira.Issue{}
	}
	if v.sources == nil {
		v.sources = map[string][]string{}
	}
	if v.topOf == nil {
		v.topOf = map[string]string{}
	}
	if v.live == nil {
		v.live = map[string]bool{}
	}
	if v.outside == nil {
		v.outside = map[string]jira.Issue{}
	}
	if v.returned == nil {
		v.returned = map[string]bool{}
	}
	if v.linkedKeys == nil {
		v.linkedKeys = map[string]bool{}
	}
	if v.umbrellas == nil {
		v.umbrellas = map[string]bool{}
	}
}

func (v *releaseVersionCheck) fail(id, message string) bool {
	ref := ""
	if rule := v.gate.conf.Version.Refs[id]; rule != "" {
		ref = " (rule " + rule + ")"
	}
	v.failures = append(v.failures, fmt.Errorf("%s%s: %s", id, ref, message))
	return !v.collect
}

// failItems connects a version refusal to the shipped tasks it affects,
// including subtasks whose parent is named by the version check.
func (v *releaseVersionCheck) failItems(id, message string, keys ...string) bool {
	stop := v.fail(id, message)
	if v.gate.versionItemFailures == nil {
		v.gate.versionItemFailures = map[string][]string{}
	}
	reason := v.failures[len(v.failures)-1].Error()
	for key := range v.gate.activeKeys {
		issue := v.gate.gate.issues[key]
		if len(keys) == 0 || slices.Contains(keys, key) || issue.Parent != "" && slices.Contains(keys, issue.Parent) {
			v.gate.versionItemFailures[key] = append(v.gate.versionItemFailures[key], reason)
		}
	}
	return stop
}

// checkReleaseVersion checks the complete version and the parents of shipped subtasks.
func (g *gateRun) checkReleaseVersion(ctx context.Context, collect bool) []error {
	if g.activeCommits == 0 {
		g.app.printf("approved release: no active commits; release version checks skipped")
		return nil
	}
	v := newReleaseVersionCheck(g, collect)
	if v.readTops(ctx) || v.selectVersion() || v.linkOutside(ctx) {
		return v.failures
	}
	if v.checkTops(ctx) || (v.selected.ID != "" && (v.checkItems(ctx) || v.adviseSelected(ctx))) {
		return v.failures
	}
	if v.checkUmbrellas(ctx) {
		return v.failures
	}
	if v.selected.ID != "" {
		v.checkApproval(ctx)
	}
	return v.failures
}

// readTops reads the commit keys: an item with an outside_version entry waits
// for the version to be chosen; the others go to their tops, a subtask to its
// parent. Without other work, the outside items choose the version as the old
// dormant form does.
func (v *releaseVersionCheck) readTops(ctx context.Context) bool {
	v.ensure()
	for _, key := range slices.Sorted(maps.Keys(v.gate.activeKeys)) {
		issue, err := v.gate.gate.issue(ctx, key)
		if err != nil {
			v.gate.collectionErr = errors.Join(v.gate.collectionErr, err)
			if v.fail("version-items", fmt.Sprintf("%s: read Jira issue: %v; retry", key, err)) {
				return true
			}
			continue
		}
		if issue.Subtask && issue.Parent == "" {
			if v.failItems("version-items", key+": subtask has no parent; fix Jira", key) {
				return true
			}
			continue
		}
		if _, outside := v.gate.conf.OutsideFor(key); outside {
			v.outside[key] = issue
			continue
		}
		if _, dormant := v.gate.conf.DormantFor(key, issue.Parent); !dormant && v.gate.liveKeys[key] {
			v.live[key] = true
		}
		if v.addTop(ctx, issue, key) {
			return true
		}
	}
	if len(v.tops) == 0 {
		return v.returnOutside(ctx)
	}
	return false
}

// returnOutside puts the waiting outside items among the tops: their entries
// are not linked to this release.
func (v *releaseVersionCheck) returnOutside(ctx context.Context) bool {
	for _, key := range slices.Sorted(maps.Keys(v.outside)) {
		v.returned[key] = true
		if v.addTop(ctx, v.outside[key], key) {
			return true
		}
	}
	v.outside = map[string]jira.Issue{}
	return false
}

func (v *releaseVersionCheck) addTop(ctx context.Context, issue jira.Issue, source string) bool {
	v.ensure()
	if issue.Parent != "" {
		v.umbrellas[issue.Parent] = true
		parent, err := v.gate.gate.issue(ctx, issue.Parent)
		if err != nil {
			v.gate.collectionErr = errors.Join(v.gate.collectionErr, err)
			return v.fail("version-items", fmt.Sprintf("%s: read parent: %v; retry", source, err))
		}
		issue = parent
	} else if len(issue.Subtasks) > 0 {
		v.umbrellas[issue.Key] = true
	}
	v.tops[issue.Key] = issue
	v.topOf[source] = issue.Key
	if !slices.Contains(v.sources[issue.Key], source) {
		v.sources[issue.Key] = append(v.sources[issue.Key], source)
	}
	v.gate.recordDormantIssue(issue)
	return false
}

func (v *releaseVersionCheck) selectVersion() bool {
	match := regexp.MustCompile(v.gate.conf.Version.Match)
	versions := map[string]jira.Version{}
	members := map[string][]string{}
	for _, key := range slices.Sorted(maps.Keys(v.tops)) {
		for _, version := range v.tops[key].FixVersions {
			if !matchingUnreleasedVersion(version, match) {
				continue
			}
			if version.ID == "" {
				if v.failItems("version-select", key+": fix version has no ID; fix Jira", key) {
					return true
				}
				continue
			}
			versions[version.ID] = version
			members[version.ID] = append(members[version.ID], key)
		}
	}
	if len(versions) != 1 {
		var details []string
		for _, id := range slices.Sorted(maps.Keys(versions)) {
			details = append(details, fmt.Sprintf("%s (%s): %s", versions[id].Name, id, strings.Join(members[id], ", ")))
		}
		msg := fmt.Sprintf("release tasks %s have no unreleased release version; what the version ships is its owner's call", strings.Join(slices.Sorted(maps.Keys(v.tops)), ", "))
		if len(details) > 0 {
			msg = "multiple release versions: " + strings.Join(details, "; ") + "; the version shipped is its owner's call"
		}
		return v.failItems("version-select", msg)
	}
	for _, selected := range versions {
		v.selected = selected
	}
	return false
}

// inSelected tells whether issue is in the selected version.
func (v *releaseVersionCheck) inSelected(issue jira.Issue) bool {
	return v.selected.ID != "" && len(issue.FixVersionIDs([]string{v.selected.ID})) > 0
}

// linkOutside puts the outside items among the tops, now that the version is
// chosen: an item whose entry is linked (outsideLinked) and that is not in the
// version has its entry stand in for the version check; every other check
// stays. The rest ship dormant as the old form does.
func (v *releaseVersionCheck) linkOutside(ctx context.Context) bool {
	for key := range v.live {
		if v.inSelected(v.tops[v.topOf[key]]) {
			v.liveInSelected = true
		}
	}
	for _, key := range slices.Sorted(maps.Keys(v.outside)) {
		issue := v.outside[key]
		item, _ := v.gate.conf.OutsideFor(key)
		in := v.inSelected(issue)
		if issue.Parent != "" {
			parent, err := v.gate.gate.issue(ctx, issue.Parent)
			if err != nil {
				return v.fail("version-items", fmt.Sprintf("%s: read parent: %v; retry", key, err))
			}
			in = in || v.inSelected(parent)
		}
		linked, err := v.gate.gate.outsideLinked(ctx, item, v.selected, v.liveInSelected)
		if err != nil {
			return v.fail("version-items", fmt.Sprintf("%s: read approvers of %s: %v; retry", key, v.selected.Name, err))
		}
		if linked && !in {
			v.linkedKeys[key] = true
			v.gate.dormant = config.UniqueDormant(v.gate.dormant, []config.DormantReleaseItem{item})
			v.gate.app.printf("approved release: %s ships dormant outside %s, not checked against the version", key, v.selected.Name)
		} else {
			v.returned[key] = true
		}
		if v.addTop(ctx, issue, key) {
			return true
		}
	}
	v.outside = map[string]jira.Issue{}
	return false
}

// excused tells whether top is out of the version only by linked outside
// entries: every commit key that brought it in has one.
func (v *releaseVersionCheck) excused(top string) bool {
	sources := v.sources[top]
	for _, source := range sources {
		if !v.linkedKeys[source] {
			return false
		}
	}
	return len(sources) > 0
}

// matchingUnreleasedVersion applies the shared released/archive/match rules
// when choosing Jira versions for release checks and deploy review guards.
func matchingUnreleasedVersion(version jira.Version, match *regexp.Regexp) bool {
	return !version.Released && !version.Archived && (match == nil || match.MatchString(version.Name))
}

// dormantTop tells whether a top ships dormant: by an old entry of its own,
// or by an outside entry of its own put back among the tops, which is read as
// the old form. An outside entry never covers the subtasks that brought the
// top in.
func (v *releaseVersionCheck) dormantTop(issue jira.Issue) bool {
	item, dormant := v.gate.conf.DormantFor(issue.Key, issue.Parent)
	if !dormant {
		return false
	}
	if item.OutsideVersion == "" {
		v.gate.recordDormantIssue(issue)
		return true
	}
	if v.linkedKeys[issue.Key] {
		return true // recorded linked by linkOutside.
	}
	if !v.returned[issue.Key] {
		return false
	}
	v.gate.dormant = config.UniqueDormant(v.gate.dormant, []config.DormantReleaseItem{item.Plain()})
	return true
}

// adviseReturned tells, for the outside entries put back under a top in the
// version, that the entry is stale now.
func (v *releaseVersionCheck) adviseReturned(top string) {
	for _, source := range v.sources[top] {
		if !v.returned[source] {
			continue
		}
		where := v.selected.Name
		if source != top {
			where = v.selected.Name + " with its parent " + top
		}
		v.gate.adviseOutside(source, where)
	}
}

func (v *releaseVersionCheck) checkTops(ctx context.Context) bool {
	for _, key := range slices.Sorted(maps.Keys(v.tops)) {
		issue := v.tops[key]
		inVersion := v.selected.ID == "" || v.inSelected(issue) || v.excused(key)
		statuses := v.gate.conf.StatusesFor(issue.Labels)
		dormant := v.dormantTop(issue)
		if v.selected.ID != "" && inVersion {
			v.adviseReturned(key)
		}
		if !inVersion {
			if v.failItems("version-top", v.notInVersion(ctx, issue), key) {
				return true
			}
			continue
		}
		if !dormant && !slices.Contains(statuses, issue.Status) {
			msg := fmt.Sprintf("%s is %s, fix versions %s: move it to %s", key, issue.Status, fixVersionNames(issue), strings.Join(statuses, " or "))
			if v.failItems("version-top", msg, key) {
				return true
			}
		}
	}
	return false
}

func fixVersionNames(issue jira.Issue) string {
	var names []string
	for _, version := range issue.FixVersions {
		names = append(names, version.Name)
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// notInVersion is the refusal of a top outside the selected version: who
// took it out, and the way to ship its switched-off code, by the commit keys
// that brought it in.
func (v *releaseVersionCheck) notInVersion(ctx context.Context, issue jira.Issue) string {
	var msg strings.Builder
	fmt.Fprintf(&msg, "%s is %s, fix versions %s: not in %s", issue.Key, issue.Status, fixVersionNames(issue), v.selected.Name)
	msg.WriteString(removalTail(ctx, v.gate.gate.client, issue.Key, "it", func(id, _ string) bool { return id == v.selected.ID }))
	var stale []string
	for _, source := range v.sources[issue.Key] {
		if item, ok := v.gate.conf.OutsideFor(source); ok && v.returned[source] {
			stale = append(stale, source+"'s dormant entry is for "+item.OutsideVersion+", this release is "+v.selected.Name)
		}
	}
	if len(stale) > 0 {
		msg.WriteString(" (" + strings.Join(stale, "; ") + ")")
	}
	var contentOnly []versionPortHint
	contentOnly = append(contentOnly, v.gate.contentOnlyPorts[issue.Key]...)
	contentOnlyTasks := map[string]bool{}
	if len(v.gate.contentOnlyPorts[issue.Key]) > 0 {
		contentOnlyTasks[issue.Key] = true
	}
	for _, source := range v.sources[issue.Key] {
		if source != issue.Key {
			hints := v.gate.contentOnlyPorts[source]
			contentOnly = append(contentOnly, hints...)
			if len(hints) > 0 {
				contentOnlyTasks[source] = true
			}
		}
	}
	if len(contentOnly) == 0 {
		msg.WriteString("; " + declareHint(v.gate.gate.declareKeys(issue, v.sources[issue.Key], v.selected.Name)))
	} else {
		slices.SortFunc(contentOnly, func(a, b versionPortHint) int {
			if n := strings.Compare(a.commit, b.commit); n != 0 {
				return n
			}
			return strings.Compare(a.port, b.port)
		})
		contentOnly = slices.CompactFunc(contentOnly, func(a, b versionPortHint) bool { return a == b })
		var remaining []string
		for _, source := range v.sources[issue.Key] {
			if !contentOnlyTasks[source] {
				remaining = append(remaining, source)
			}
		}
		if len(remaining) > 0 {
			msg.WriteString("; " + declareHint(v.gate.gate.declareKeys(issue, remaining, v.selected.Name)))
		}
		for _, hint := range contentOnly {
			fmt.Fprintf(&msg, "; %s has matching content on %s but no hash link; run graft commit --allow-empty -m %q in this range to link both hashes (%s and %s)", gitx.Short(hint.commit), gitx.Short(hint.port), "chore: link "+issue.Key+" "+gitx.Short(hint.commit)+" "+gitx.Short(hint.port), gitx.Short(hint.commit), gitx.Short(hint.port))
		}
	}
	return msg.String()
}

// adviseSelected tells, for every outside_version entry whose item or its
// parent is in the selected version, that the entry outlived its release,
// whether or not the item has commits in the range.
func (v *releaseVersionCheck) adviseSelected(ctx context.Context) bool {
	for _, item := range v.gate.conf.Dormant {
		if _, ok := v.gate.conf.OutsideFor(item.Key); !ok || v.gate.advised[item.Key] {
			continue
		}
		issue, err := v.gate.gate.issue(ctx, item.Key)
		if err != nil {
			if v.fail("version-items", fmt.Sprintf("%s: read Jira issue: %v; retry", item.Key, err)) {
				return true
			}
			continue
		}
		where := v.selected.Name
		if !v.inSelected(issue) {
			if issue.Parent == "" {
				continue
			}
			parent, err := v.gate.gate.issue(ctx, issue.Parent)
			if err != nil {
				if v.fail("version-items", fmt.Sprintf("%s: read parent: %v; retry", item.Key, err)) {
					return true
				}
				continue
			}
			if !v.inSelected(parent) {
				continue
			}
			where += " with its parent " + parent.Key
		}
		v.gate.adviseOutside(item.Key, where)
	}
	return false
}

func versionStatusDetail(issue jira.Issue, statuses []string, labelStatuses bool) string {
	if issue.Parent != "" || issue.Subtask || slices.Contains(statuses, issue.Status) {
		return ""
	}
	detail := issue.Key + " (" + issue.Status + ")"
	if labelStatuses {
		detail += "; move to " + strings.Join(statuses, " or ")
	}
	return detail
}

func (v *releaseVersionCheck) checkItems(ctx context.Context) bool {
	keys, err := v.gate.gate.client.VersionItems(ctx, v.selected.ID)
	if err != nil {
		v.gate.collectionErr = errors.Join(v.gate.collectionErr, err)
		return v.fail("version-items", fmt.Sprintf("%s: read version items: %v; retry", v.selected.Name, err))
	}
	var bad, badKeys, subtasks, subtaskKeys, missing []string
	for _, key := range keys {
		issue, err := v.gate.gate.issue(ctx, key)
		if err != nil {
			v.gate.collectionErr = errors.Join(v.gate.collectionErr, err)
			if v.fail("version-items", fmt.Sprintf("%s: read Jira issue: %v; retry", key, err)) {
				return true
			}
			continue
		}
		v.gate.recordDormantIssue(issue)
		item, dormant := v.gate.conf.DormantFor(issue.Key, issue.Parent)
		if dormant && item.OutsideVersion != "" {
			// In the version, an outside entry ships dormant as the old form.
			v.gate.dormant = config.UniqueDormant(v.gate.dormant, []config.DormantReleaseItem{item.Plain()})
			v.gate.adviseOutside(issue.Key, v.selected.Name)
		}
		if !dormant {
			if detail := versionStatusDetail(issue, v.gate.conf.StatusesFor(issue.Labels), len(v.gate.conf.LabelStatuses) > 0); detail != "" {
				bad = append(bad, detail)
				badKeys = append(badKeys, key)
			}
		}
		if v.checkTopLevelHumanStatus(ctx, issue) {
			return true
		}
		if issue.Subtask || issue.Parent != "" {
			subtasks = append(subtasks, key+" ("+issue.Status+")")
			subtaskKeys = append(subtaskKeys, key)
		}
		if _, ok := v.tops[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(bad) > 0 {
		message := fmt.Sprintf("%s: %s", v.selected.Name, strings.Join(bad, ", "))
		if len(v.gate.conf.LabelStatuses) == 0 {
			// As before label_statuses: one remedy for the whole list.
			message += "; move to " + strings.Join(v.gate.conf.Statuses, " or ")
		}
		if v.failItems("version-status", message, badKeys...) {
			return true
		}
	}
	if len(subtasks) > 0 && v.failItems("version-subtasks", fmt.Sprintf("%s: %s; remove subtasks from the version and add their parents", v.selected.Name, strings.Join(subtasks, ", ")), subtaskKeys...) {
		return true
	}
	if len(missing) > 0 {
		v.gate.app.warn("release version %s has items without code in the range: %s", v.selected.Name, strings.Join(missing, ", "))
	}
	return false
}

func (v *releaseVersionCheck) checkTopLevelHumanStatus(ctx context.Context, issue jira.Issue) bool {
	if issue.Subtask || issue.Parent != "" || v.gate.conf.Version.HumanCheck == nil {
		return false
	}
	return v.checkHumanStatus(ctx, issue)
}

func statusInSet(status string, statuses []string) bool {
	for _, configured := range statuses {
		if strings.EqualFold(configured, status) {
			return true
		}
	}
	return false
}

func latestStatusEntry(changes []jira.StatusChange, approved []string) *jira.StatusChange {
	var latest *jira.StatusChange
	for i := range changes {
		change := changes[i]
		if statusInSet(change.To, approved) {
			if !statusInSet(change.From, approved) {
				latest = &change
			}
		} else {
			latest = nil
		}
	}
	return latest
}

func latestHumanStatusEntry(issue jira.Issue, changes []jira.StatusChange, approved []string) *jira.StatusChange {
	history := changes
	if len(history) > 0 && !statusInSet(issue.Status, approved) && issue.Done {
		// Jira's done category can be outside the configured approved set.
		// The final transition explains the current status, but does not
		// replace the human check recorded before the task was completed.
		history = history[:len(history)-1]
	}
	return latestStatusEntry(history, approved)
}

func (v *releaseVersionCheck) checkHumanStatus(ctx context.Context, issue jira.Issue) bool {
	h := v.gate.conf.Version.HumanCheck
	if h == nil {
		return false
	}
	for _, label := range issue.Labels {
		if slices.Contains(h.Labels, label) {
			return false
		}
	}
	approved := v.gate.conf.StatusesFor(issue.Labels)
	remedy := fmt.Sprintf("put label %s (from the setting) on it or have a reviewer move it through %s", strings.Join(h.Labels, " or "), strings.Join(h.From, " or "))
	if !v.humanMeRead {
		v.humanMeRead = true
		v.humanMe, v.humanMeErr = v.gate.gate.client.Myself(ctx)
	}
	if v.humanMeErr != nil {
		return v.failItems("version-human-check", fmt.Sprintf("%s: read Jira /myself: %v; %s", issue.Key, v.humanMeErr, remedy), issue.Key)
	}
	changes, err := v.gate.gate.client.StatusChanges(ctx, issue.Key)
	if err != nil {
		return v.failItems("version-human-check", fmt.Sprintf("%s: read Jira changelog: %v; %s", issue.Key, err, remedy), issue.Key)
	}
	if len(changes) == 0 || !strings.EqualFold(changes[len(changes)-1].To, issue.Status) {
		return v.failItems("version-human-check", fmt.Sprintf("%s: history does not explain the current status %s; %s", issue.Key, issue.Status, remedy), issue.Key)
	}
	latest := latestHumanStatusEntry(issue, changes, approved)
	currentStatus := describeCurrentStatusEntry(issue.Status, changes)
	if latest != nil && statusInSet(latest.From, h.From) && latest.AuthorID != "" && latest.AuthorID != v.humanMe {
		return false
	}
	if latest == nil {
		return v.failItems("version-human-check", fmt.Sprintf("%s: %s; no latest entry into an approved status from outside the approved statuses was found; %s", issue.Key, currentStatus, remedy), issue.Key)
	}
	if !statusInSet(latest.From, h.From) {
		return v.failItems("version-human-check", fmt.Sprintf("%s: %s; latest entry into an approved status is not from %s; %s", issue.Key, currentStatus, strings.Join(h.From, " or "), remedy), issue.Key)
	}
	if latest.AuthorID == "" {
		return v.failItems("version-human-check", fmt.Sprintf("%s: %s; latest approved-status entry has no Jira author account ID to compare with the automation used by graft; %s", issue.Key, currentStatus, remedy), issue.Key)
	}
	return v.failItems("version-human-check", fmt.Sprintf("%s: %s; latest approved-status entry was made by the automation used by graft; %s", issue.Key, currentStatus, remedy), issue.Key)
}

func describeCurrentStatusEntry(status string, changes []jira.StatusChange) string {
	if len(changes) == 0 {
		return fmt.Sprintf("current status %s has no status history entry", status)
	}
	change := changes[len(changes)-1]
	who := change.AuthorName
	if who == "" {
		who = change.AuthorID
	}
	if who == "" {
		who = "unknown account"
	}
	when := change.When.Format("2006-01-02 15:04")
	if strings.EqualFold(change.To, status) {
		return fmt.Sprintf("current status %s was entered from %s by %s on %s", status, change.From, who, when)
	}
	return fmt.Sprintf("current status %s; latest history entry entered %s from %s by %s on %s", status, change.To, change.From, who, when)
}

// checkUmbrellas holds a parent while a subtask is not Done. An old dormant
// entry of the parent or the subtask lets it be; an outside entry only when
// it is linked to this release.
func (v *releaseVersionCheck) checkUmbrellas(ctx context.Context) bool {
	for _, key := range slices.Sorted(maps.Keys(v.umbrellas)) {
		if slices.Contains(v.gate.gate.skipKeys, key) {
			continue
		}
		parent, ok := v.tops[key]
		if !ok {
			continue
		}
		if item, dormant := v.gate.conf.DormantFor(parent.Key, parent.Parent); dormant && item.OutsideVersion == "" {
			v.gate.recordDormantIssue(parent)
			continue
		}
		var open []string
		for _, subkey := range parent.Subtasks {
			if slices.Contains(v.gate.gate.skipKeys, subkey) {
				continue
			}
			sub, err := v.gate.gate.issue(ctx, subkey)
			if err != nil {
				v.gate.collectionErr = errors.Join(v.gate.collectionErr, err)
				if v.fail("version-umbrella", fmt.Sprintf("%s: read subtask %s: %v; retry", key, subkey, err)) {
					return true
				}
				continue
			}
			// An outside_version entry stands in for the version check
			// alone: the subtask holds its parent as without the entry.
			if item, dormant := v.gate.conf.DormantFor(subkey, parent.Key); dormant && item.OutsideVersion == "" {
				v.gate.recordDormantIssue(sub)
				continue
			}
			if !sub.Done {
				open = append(open, subkey+" ("+sub.Status+")")
			}
		}
		if len(open) > 0 && v.failItems("version-umbrella", fmt.Sprintf("%s is %s with unfinished subtasks %s; move them to Done", key, parent.Status, strings.Join(open, ", ")), key) {
			return true
		}
	}
	return false
}

func (v *releaseVersionCheck) checkApproval(ctx context.Context) {
	approvers, err := v.gate.gate.client.VersionApprovers(ctx, v.selected.ID)
	if err != nil {
		v.gate.collectionErr = errors.Join(v.gate.collectionErr, err)
		v.fail("version-approval", fmt.Sprintf("%s: read approvers: %v; retry", v.selected.Name, err))
		return
	}
	if jira.FullyApproved(approvers) {
		// The pass names its source: who approved which version.
		var who []string
		for _, a := range approvers {
			who = append(who, v.approverName(ctx, a.AccountID))
		}
		v.gate.app.printf("approved release: version %s (%s) approved by %s", v.selected.Name, v.selected.ID, strings.Join(who, ", "))
		return
	}
	var pending []string
	for _, a := range approvers {
		if a.Status != jira.ApprovedStatus {
			pending = append(pending, v.approverName(ctx, a.AccountID)+" ("+a.Status+")")
		}
	}
	if len(pending) == 0 {
		pending = append(pending, "no approvers")
	}
	v.failItems("version-approval", fmt.Sprintf("%s: %s; add approvers and obtain all approvals", v.selected.Name, strings.Join(pending, ", ")))
}

// approverName reads the approver's name; without it the account still says who.
func (v *releaseVersionCheck) approverName(ctx context.Context, accountID string) string {
	name, err := v.gate.gate.client.UserName(ctx, accountID)
	if err != nil {
		v.gate.app.warn("approver %s: name not read, named by the account: %v", accountID, err)
		return accountID
	}
	return name
}
