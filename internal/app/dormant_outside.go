package app

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/jira"
)

// shipsLiveWork tells whether a commit can be live work that anchors an
// outside_version entry: it changes a file outside ignore_paths and the
// version files, and shipsNothing does not leave it out. An empty commit and
// one that only raises the version are no live work. The prefix gate, the
// release version check and the freeze all ask it; it decides nothing else.
func shipsLiveWork(repo *gitx.Repo, cfg *config.Config, conf *config.ApprovedRelease, sha string) (bool, error) {
	out, err := repo.Git("diff-tree", "--no-commit-id", "--name-only", "-r", "--root", "-z", sha)
	if err != nil {
		return false, err
	}
	if files, _ := classifyShipChanges(out, cfg.VersionFiles(), conf); len(files) == 0 {
		return false, nil
	}
	ignored, _, err := shipsNothing(repo, cfg, conf, sha)
	return !ignored, err
}

// outsideAnchor tells whether versions anchor the outside_version entries
// naming them: live work of them (items without any dormant entry, named by
// commits that ship something: shipsLiveWork) goes out in this release, and
// each is unreleased, unarchived and approved by every approver. The version
// gate, the prefix gate and the freeze all ask it.
func (g *approvalGate) outsideAnchor(ctx context.Context, versions []jira.Version, live bool) (bool, error) {
	if !live || len(versions) == 0 {
		return false, nil
	}
	for _, v := range versions {
		if v.ID == "" || v.Released || v.Archived {
			return false, nil
		}
		full, err := g.fullyApproved(ctx, v.ID)
		if err != nil || !full {
			return false, err
		}
	}
	return true, nil
}

// outsideLinked tells whether item's outside_version entry is linked to the
// release: it names version, which anchors it.
func (g *approvalGate) outsideLinked(ctx context.Context, item config.DormantReleaseItem, version jira.Version, live bool) (bool, error) {
	if item.OutsideVersion == "" || item.OutsideVersion != version.Name {
		return false, nil
	}
	return g.outsideAnchor(ctx, []jira.Version{version}, live)
}

// declareHint is the remedy for work outside the version that ships: the
// version is its owner's call, so nothing is added back; the commits are
// reverted, or switched-off code is declared by the examples.
func declareHint(examples []string) string {
	var b strings.Builder
	b.WriteString("what the version ships is its owner's call, do not add it back. Revert its commits on this branch, or, only if its code ships switched off, declare it:")
	for _, example := range examples {
		b.WriteString("\n  " + example)
	}
	b.WriteString("\n  Live work outside the version is reverted, not declared.")
	return b.String()
}

// declareKeys are the examples of declarations for a top and the commit keys
// that brought it in: one entry per commit key, never one for a parent that
// has subtasks, whose entry would cover none of them.
func (g *approvalGate) declareKeys(top jira.Issue, sources []string, version string) []string {
	var out []string
	sub := false
	for _, source := range sources {
		if source != top.Key {
			sub = true
		}
	}
	for _, source := range slices.Sorted(slices.Values(sources)) {
		if source == top.Key && len(top.Subtasks) > 0 {
			if !sub {
				out = append(out, top.Key+" has subtasks: declare the subtask whose code ships switched off, not "+top.Key)
			}
			continue
		}
		out = append(out, `approved_release.dormant: [{key: `+source+`, reason: "...", disabled_by: "<flag or setting that keeps it off>", outside_version: "`+version+`"}]`)
	}
	return out
}

// removalTail names who took key out of a version removed accepts, and when,
// from the item's history; a history that cannot be read is said so, and the
// refusal stays. what names the version in the text.
func removalTail(ctx context.Context, client *jira.Client, key, what string, removed func(id, name string) bool) string {
	if client == nil {
		return ""
	}
	got, found, err := client.LastFixVersionRemoval(ctx, key, removed)
	switch {
	case err != nil:
		return " (Jira history not read: " + err.Error() + ")"
	case !found:
		return ""
	}
	if what == "" {
		what = got.Version
	}
	return " (taken out of " + what + " by " + got.Who + " on " + got.When + ")"
}

// adviseOutside tells once that an item with an outside_version entry is in
// the version now: the entry outlived its release.
func (g *gateRun) adviseOutside(key, where string) {
	if g.advised == nil {
		g.advised = map[string]bool{}
	}
	if g.advised[key] {
		return
	}
	g.advised[key] = true
	g.app.printf("%s is in %s now: drop outside_version from its approved_release.dormant entry; drop the entry once the feature is on and checked", key, where)
}

// outsideHints are the remedies of a prefix gate's refusal for the items in
// no approved version: the version this release ships, an example per
// commit key, and who took the item out of a version of the prefix.
func (g *gateRun) outsideHints(ctx context.Context, failures []gateFailure) []string {
	version := "<the approved " + g.conf.Prefix + " version this release ships>"
	if live := slices.Sorted(maps.Keys(g.liveVersions)); len(live) == 1 {
		version = live[0]
	}
	var lines []string
	seen := map[string]bool{}
	for _, f := range failures {
		for _, key := range f.keys {
			if seen[key] {
				continue
			}
			seen[key] = true
			issue, ok := g.gate.issues[key]
			if !ok {
				continue
			}
			approved, err := g.gate.approvedVersion(ctx, issue)
			if err != nil {
				lines = append(lines, key+": approved version not read: "+err.Error())
				continue
			}
			if approved != "" {
				continue
			}
			top := issue
			if issue.Parent != "" {
				if parent, ok := g.gate.issues[issue.Parent]; ok {
					top = parent
				}
			}
			line := key + " is in no approved " + g.conf.Prefix + " version" + removalTail(ctx, g.gate.client, top.Key, "", func(_, name string) bool { return strings.HasPrefix(name, g.conf.Prefix) })
			if item, ok := g.conf.OutsideFor(key); ok {
				line += " (its dormant entry is for " + item.OutsideVersion + ", and no live work of " + item.OutsideVersion + ", unreleased and approved, goes out in this release)"
			}
			lines = append(lines, line+": "+declareHint(g.gate.declareKeys(top, []string{key}, version)))
		}
	}
	return lines
}
