package app

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/jira"
)

type releaseVersionCheck struct {
	gate      *gateRun
	collect   bool
	failures  []error
	tops      map[string]jira.Issue
	umbrellas map[string]bool
	selected  jira.Version
}

func (v *releaseVersionCheck) fail(id, message string) bool {
	ref := ""
	if rule := v.gate.conf.Version.Refs[id]; rule != "" {
		ref = " (rule " + rule + ")"
	}
	v.failures = append(v.failures, fmt.Errorf("%s%s: %s", id, ref, message))
	return !v.collect
}

// checkReleaseVersion checks the complete version and the parents of shipped subtasks.
func (g *gateRun) checkReleaseVersion(ctx context.Context, collect bool) []error {
	if g.activeCommits == 0 {
		g.app.printf("approved release: no active commits; release version checks skipped")
		return nil
	}
	v := &releaseVersionCheck{gate: g, collect: collect, tops: map[string]jira.Issue{}, umbrellas: map[string]bool{}}
	if v.readTops(ctx) || v.selectVersion() {
		return v.failures
	}
	if v.checkTops() || (v.selected.ID != "" && v.checkItems(ctx)) {
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

func (v *releaseVersionCheck) readTops(ctx context.Context) bool {
	for _, key := range slices.Sorted(maps.Keys(v.gate.activeKeys)) {
		issue, err := v.gate.gate.issue(ctx, key)
		if err != nil { //nolint:silent-error-handling // err goes into the failure message below; glint ticket silent-error-call-in-if-cond
			if v.fail("version-items", fmt.Sprintf("%s: read Jira issue: %v; retry", key, err)) {
				return true
			}
			continue
		}
		if issue.Subtask && issue.Parent == "" {
			if v.fail("version-items", key+": subtask has no parent; fix Jira") {
				return true
			}
			continue
		}
		if issue.Parent != "" {
			v.umbrellas[issue.Parent] = true
			issue, err = v.gate.gate.issue(ctx, issue.Parent)
			if err != nil { //nolint:silent-error-handling // err goes into the failure message below; glint ticket silent-error-call-in-if-cond
				if v.fail("version-items", fmt.Sprintf("%s: read parent: %v; retry", key, err)) {
					return true
				}
				continue
			}
		} else if len(issue.Subtasks) > 0 {
			v.umbrellas[issue.Key] = true
		}
		v.tops[issue.Key] = issue
		v.gate.recordDormantIssue(issue)
	}
	return false
}

func (v *releaseVersionCheck) selectVersion() bool {
	match := regexp.MustCompile(v.gate.conf.Version.Match)
	versions := map[string]jira.Version{}
	members := map[string][]string{}
	for _, key := range slices.Sorted(maps.Keys(v.tops)) {
		for _, version := range v.tops[key].FixVersions {
			if version.Released || version.Archived || !match.MatchString(version.Name) {
				continue
			}
			if version.ID == "" {
				if v.fail("version-select", key+": fix version has no ID; fix Jira") {
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
		msg := fmt.Sprintf("release tasks %s have no unreleased release version; add one to their fix versions", strings.Join(slices.Sorted(maps.Keys(v.tops)), ", "))
		if len(details) > 0 {
			msg = "multiple release versions: " + strings.Join(details, "; ") + "; choose one for the release tasks"
		}
		return v.fail("version-select", msg)
	}
	for _, selected := range versions {
		v.selected = selected
	}
	return false
}

func (v *releaseVersionCheck) checkTops() bool {
	for _, key := range slices.Sorted(maps.Keys(v.tops)) {
		issue := v.tops[key]
		inVersion := v.selected.ID == "" || slices.Contains(issue.FixVersionIDs([]string{v.selected.ID}), v.selected.ID)
		statuses := v.gate.conf.StatusesFor(issue.Labels)
		v.gate.recordDormantIssue(issue)
		_, dormant := v.gate.conf.DormantFor(issue.Key, issue.Parent)
		if !inVersion || !dormant && !slices.Contains(statuses, issue.Status) {
			var names []string
			for _, version := range issue.FixVersions {
				names = append(names, version.Name)
			}
			if len(names) == 0 {
				names = append(names, "none")
			}
			var todo []string
			if !inVersion {
				todo = append(todo, "add it to "+v.selected.Name)
			}
			if !dormant && !slices.Contains(statuses, issue.Status) {
				todo = append(todo, "move it to "+strings.Join(statuses, " or "))
			}
			msg := fmt.Sprintf("%s is %s, fix versions %s: %s", key, issue.Status, strings.Join(names, ", "), strings.Join(todo, " and "))
			if v.fail("version-top", msg) {
				return true
			}
		}
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
		return v.fail("version-items", fmt.Sprintf("%s: read version items: %v; retry", v.selected.Name, err))
	}
	var bad, subtasks, missing []string
	for _, key := range keys {
		issue, err := v.gate.gate.issue(ctx, key)
		if err != nil { //nolint:silent-error-handling // err goes into the failure message below; glint ticket silent-error-call-in-if-cond
			if v.fail("version-items", fmt.Sprintf("%s: read Jira issue: %v; retry", key, err)) {
				return true
			}
			continue
		}
		v.gate.recordDormantIssue(issue)
		_, dormant := v.gate.conf.DormantFor(issue.Key, issue.Parent)
		if !dormant {
			if detail := versionStatusDetail(issue, v.gate.conf.StatusesFor(issue.Labels), len(v.gate.conf.LabelStatuses) > 0); detail != "" {
				bad = append(bad, detail)
			}
		}
		if issue.Subtask || issue.Parent != "" {
			subtasks = append(subtasks, key+" ("+issue.Status+")")
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
		if v.fail("version-status", message) {
			return true
		}
	}
	if len(subtasks) > 0 && v.fail("version-subtasks", fmt.Sprintf("%s: %s; remove subtasks from the version and add their parents", v.selected.Name, strings.Join(subtasks, ", "))) {
		return true
	}
	if len(missing) > 0 {
		v.gate.app.warn("release version %s has items without code in the range: %s", v.selected.Name, strings.Join(missing, ", "))
	}
	return false
}

func (v *releaseVersionCheck) checkUmbrellas(ctx context.Context) bool {
	for _, key := range slices.Sorted(maps.Keys(v.umbrellas)) {
		if slices.Contains(v.gate.gate.skipKeys, key) {
			continue
		}
		parent, ok := v.tops[key]
		if !ok {
			continue
		}
		if _, dormant := v.gate.conf.DormantFor(parent.Key, parent.Parent); dormant {
			v.gate.recordDormantIssue(parent)
			continue
		}
		var open []string
		for _, subkey := range parent.Subtasks {
			if slices.Contains(v.gate.gate.skipKeys, subkey) {
				continue
			}
			sub, err := v.gate.gate.issue(ctx, subkey)
			if err != nil { //nolint:silent-error-handling // err goes into the failure message below; glint ticket silent-error-call-in-if-cond
				if v.fail("version-umbrella", fmt.Sprintf("%s: read subtask %s: %v; retry", key, subkey, err)) {
					return true
				}
				continue
			}
			if _, dormant := v.gate.conf.DormantFor(subkey, parent.Key); dormant {
				v.gate.recordDormantIssue(sub)
				continue
			}
			if !sub.Done {
				open = append(open, subkey+" ("+sub.Status+")")
			}
		}
		if len(open) > 0 && v.fail("version-umbrella", fmt.Sprintf("%s is %s with unfinished subtasks %s; move them to Done", key, parent.Status, strings.Join(open, ", "))) {
			return true
		}
	}
	return false
}

func (v *releaseVersionCheck) checkApproval(ctx context.Context) {
	approvers, err := v.gate.gate.client.VersionApprovers(ctx, v.selected.ID)
	if err != nil {
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
	v.fail("version-approval", fmt.Sprintf("%s: %s; add approvers and obtain all approvals", v.selected.Name, strings.Join(pending, ", ")))
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
