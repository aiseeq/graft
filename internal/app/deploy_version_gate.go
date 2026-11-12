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
	if v.selected.ID != "" && (v.checkTops() || v.checkItems(ctx)) {
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
		if err != nil {
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
			if err != nil {
				if v.fail("version-items", fmt.Sprintf("%s: read parent: %v; retry", key, err)) {
					return true
				}
				continue
			}
		}
		v.tops[issue.Key] = issue
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
		if !slices.Contains(issue.FixVersionIDs([]string{v.selected.ID}), v.selected.ID) || !slices.Contains(v.gate.conf.Statuses, issue.Status) {
			var names []string
			for _, version := range issue.FixVersions {
				names = append(names, version.Name)
			}
			if len(names) == 0 {
				names = append(names, "none")
			}
			if v.fail("version-top", fmt.Sprintf("%s is %s, fix versions %s: add it to %s and move it to %s", key, issue.Status, strings.Join(names, ", "), v.selected.Name, strings.Join(v.gate.conf.Statuses, " or "))) {
				return true
			}
		}
	}
	return false
}

func (v *releaseVersionCheck) checkItems(ctx context.Context) bool {
	keys, err := v.gate.gate.client.VersionItems(ctx, v.selected.ID)
	if err != nil {
		return v.fail("version-items", fmt.Sprintf("%s: read version items: %v; retry", v.selected.Name, err))
	}
	var bad, subtasks, missing []string
	for _, key := range keys {
		issue, err := v.gate.gate.issue(ctx, key)
		if err != nil {
			if v.fail("version-items", fmt.Sprintf("%s: read Jira issue: %v; retry", key, err)) {
				return true
			}
			continue
		}
		if !slices.Contains(v.gate.conf.Statuses, issue.Status) {
			bad = append(bad, key+" ("+issue.Status+")")
		}
		if issue.Subtask || issue.Parent != "" {
			subtasks = append(subtasks, key+" ("+issue.Status+")")
		}
		if _, ok := v.tops[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(bad) > 0 && v.fail("version-status", fmt.Sprintf("%s: %s; move to %s", v.selected.Name, strings.Join(bad, ", "), strings.Join(v.gate.conf.Statuses, " or "))) {
		return true
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
		var open []string
		for _, subkey := range parent.Subtasks {
			sub, err := v.gate.gate.issue(ctx, subkey)
			if err != nil {
				if v.fail("version-umbrella", fmt.Sprintf("%s: read subtask %s: %v; retry", key, subkey, err)) {
					return true
				}
				continue
			}
			if !sub.Done && !slices.Contains(v.gate.conf.Statuses, sub.Status) {
				open = append(open, subkey+" ("+sub.Status+")")
			}
		}
		if len(open) > 0 && v.fail("version-umbrella", fmt.Sprintf("%s is %s with unfinished subtasks %s; finish or move them to an approved status", key, parent.Status, strings.Join(open, ", "))) {
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
	if !jira.FullyApproved(approvers) {
		var pending []string
		for _, a := range approvers {
			if a.Status != jira.ApprovedStatus {
				pending = append(pending, a.AccountID+" ("+a.Status+")")
			}
		}
		if len(pending) == 0 {
			pending = append(pending, "no approvers")
		}
		v.fail("version-approval", fmt.Sprintf("%s: %s; add approvers and obtain all approvals", v.selected.Name, strings.Join(pending, ", ")))
	}
}
