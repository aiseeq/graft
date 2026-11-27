package app

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/userconfig"
	"github.com/aiseeq/graft/v2/internal/version"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

type versionPortHint struct {
	commit string
	port   string
}

// coveredVersionPorts finds commits in prev..head whose complete, hash-linked
// port is already in prev. A whole port group must replay cleanly and leave
// the port tree unchanged outside version files before its tasks stop being
// active for release-version checks.
func coveredVersionPorts(repo *gitx.Repo, prev, head string, commits []deliveredCommit, targets map[string][]version.Target) (map[string]string, error) {
	prior, err := logCommits(repo, prev)
	if err != nil {
		return nil, err
	}
	return coveredVersionPortsWithPrior(repo, prev, head, prior, commits, targets)
}

func coveredVersionPortsWithPrior(repo *gitx.Repo, prev, head string, prior, commits []deliveredCommit, targets map[string][]version.Target) (map[string]string, error) {
	covered := map[string]string{}
	if len(commits) == 0 {
		return covered, nil
	}
	reverted, err := revertedInRange(repo, commits)
	if err != nil {
		return nil, err
	}
	active := make([]deliveredCommit, 0, len(commits))
	for _, c := range commits {
		if !reverted[c.sha] {
			active = append(active, c)
		}
	}
	commits = active
	if len(commits) == 0 {
		return covered, nil
	}
	groups, err := linkedVersionPortGroups(repo, prev, prior, commits)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return covered, nil
	}

	ordered, err := logCommits(repo, "--topo-order", "--reverse", prev+".."+head)
	if err != nil {
		return nil, err
	}
	commitOrder := make(map[string]int, len(commits))
	for i, c := range ordered {
		commitOrder[c.sha] = i
	}
	for i, c := range commits {
		if _, ok := commitOrder[c.sha]; !ok {
			commitOrder[c.sha] = len(ordered) + i
		}
	}
	for _, port := range slices.Sorted(maps.Keys(groups)) {
		group := make([]deliveredCommit, 0, len(groups[port]))
		for _, c := range groups[port] {
			group = append(group, c)
		}
		slices.SortFunc(group, func(a, b deliveredCommit) int { return commitOrder[a.sha] - commitOrder[b.sha] })
		unchanged, err := portGroupChangesNothing(repo, port, group, prev, head, targets)
		if err != nil {
			return nil, fmt.Errorf("checking linked port %s: %w", gitx.Short(port), err)
		}
		if unchanged {
			for _, c := range group {
				if covered[c.sha] == "" {
					covered[c.sha] = port
				}
			}
		}
	}
	return covered, nil
}

func revertedInRange(repo *gitx.Repo, commits []deliveredCommit) (map[string]bool, error) {
	reverted := map[string]bool{}
	for _, c := range commits {
		takes, err := isWorkRemovingRevert(repo, c, commits, unknownRevertTargetUsesSubject)
		if err != nil {
			return nil, err
		}
		if !takes {
			continue
		}
		for _, m := range revertsRe.FindAllStringSubmatch(c.body, -1) {
			full, err := repo.Git("rev-parse", "--verify", "--quiet", m[1]+"^{commit}")
			if err != nil {
				return nil, fmt.Errorf("resolving reverted commit %s in %s: %w", m[1], gitx.Short(c.sha), err)
			}
			reverted[strings.TrimSpace(full)] = true
		}
	}
	return reverted, nil
}

func linkedVersionPortGroups(repo *gitx.Repo, prev string, prior, commits []deliveredCommit) (map[string]map[string]deliveredCommit, error) {
	groups := map[string]map[string]deliveredCommit{}
	add := func(port string, c deliveredCommit) {
		if groups[port] == nil {
			groups[port] = map[string]deliveredCommit{}
		}
		groups[port][c.sha] = c
	}
	if err := addSourcesNamedByPorts(repo, prior, commits, add); err != nil {
		return nil, err
	}
	if err := addSourcesNamingPort(repo, prior, commits, add); err != nil {
		return nil, err
	}
	if err := addExplicitVersionPortLinks(repo, prev, commits, add); err != nil {
		return nil, err
	}
	return groups, nil
}

func addSourcesNamingPort(repo *gitx.Repo, prior, commits []deliveredCommit, add func(string, deliveredCommit)) error {
	byPrefix := map[string][]deliveredCommit{}
	for _, p := range prior {
		for n := 7; n <= len(p.sha); n++ {
			byPrefix[p.sha[:n]] = append(byPrefix[p.sha[:n]], p)
		}
	}
	for _, c := range commits {
		for _, hash := range gitx.CommitHashes(c.subject + "\n" + c.body) {
			matches := byPrefix[hash]
			if len(matches) != 1 {
				continue
			}
			resolved, ok, err := uniqueCommitPrefix(repo, hash)
			if err != nil {
				return err
			}
			if ok && resolved == matches[0].sha {
				add(matches[0].sha, c)
			}
		}
	}
	return nil
}

// unlinkedContentVersionPorts finds same-task commits whose content is on a
// prior commit but which have no hash link to it. This is advice only; it
// never suppresses a version check.
func unlinkedContentVersionPorts(repo *gitx.Repo, prev, head string, commits []deliveredCommit, targets map[string][]version.Target, keyRe *regexp.Regexp, conf *config.ApprovedRelease, covered map[string]string) (map[string][]versionPortHint, error) {
	prior, err := logCommits(repo, prev)
	if err != nil {
		return nil, err
	}
	return unlinkedContentVersionPortsWithPrior(repo, prev, head, prior, commits, targets, keyRe, conf, covered)
}

func unlinkedContentVersionPortsWithPrior(repo *gitx.Repo, prev, head string, prior, commits []deliveredCommit, targets map[string][]version.Target, keyRe *regexp.Regexp, conf *config.ApprovedRelease, covered map[string]string) (map[string][]versionPortHint, error) {
	hints := map[string][]versionPortHint{}
	groups, err := linkedVersionPortGroups(repo, prev, prior, commits)
	if err != nil {
		return nil, err
	}
	ordered, err := logCommits(repo, "--topo-order", "--reverse", prev+".."+head)
	if err != nil {
		return nil, err
	}
	order := make(map[string]int, len(ordered))
	for i, c := range ordered {
		order[c.sha] = i
	}
	byKey := map[string][]deliveredCommit{}
	for _, c := range commits {
		if covered[c.sha] != "" {
			continue
		}
		for _, key := range itemKeys(keyRe, conf, c) {
			byKey[key] = append(byKey[key], c)
		}
	}
	hinted := map[string]map[string]bool{}
	for _, port := range prior {
		portKeys := map[string]bool{}
		for _, key := range keyRe.FindAllString(port.subject+"\n"+port.body, -1) {
			portKeys[key] = true
		}
		if err := findContentOnlyHintsForPort(repo, prev, port, portKeys, byKey, groups[port.sha], order, targets, hints, hinted); err != nil {
			return nil, err
		}
	}
	for key := range hints {
		slices.SortFunc(hints[key], func(a, b versionPortHint) int {
			if n := strings.Compare(a.commit, b.commit); n != 0 {
				return n
			}
			return strings.Compare(a.port, b.port)
		})
		hints[key] = slices.CompactFunc(hints[key], func(a, b versionPortHint) bool { return a == b })
	}
	return hints, nil
}

func findContentOnlyHintsForPort(repo *gitx.Repo, prev string, port deliveredCommit, portKeys map[string]bool, byKey map[string][]deliveredCommit, linked map[string]deliveredCommit, order map[string]int, targets map[string][]version.Target, hints map[string][]versionPortHint, hinted map[string]map[string]bool) error {
	for key, candidates := range byKey {
		if !portKeys[key] {
			continue
		}
		present, err := versionPortStillPresent(repo, port.sha, prev, targets)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		unlinked := make([]deliveredCommit, 0, len(candidates))
		for _, c := range candidates {
			if _, hasLink := linked[c.sha]; !hasLink && !hinted[key][c.sha] {
				unlinked = append(unlinked, c)
			}
		}
		if len(unlinked) == 0 {
			continue
		}
		slices.SortFunc(unlinked, func(a, b deliveredCommit) int { return order[a.sha] - order[b.sha] })
		unchanged, err := replayVersionPortGroup(repo, port.sha, unlinked, targets)
		if err != nil {
			return fmt.Errorf("checking possible unlinked content on %s: %w", gitx.Short(port.sha), err)
		}
		if !unchanged {
			continue
		}
		if hinted[key] == nil {
			hinted[key] = map[string]bool{}
		}
		for _, c := range unlinked {
			hints[key] = append(hints[key], versionPortHint{commit: c.sha, port: port.sha})
			hinted[key][c.sha] = true
		}
	}
	return nil
}

func versionPortStillPresent(repo *gitx.Repo, port, prev string, targets map[string][]version.Target) (bool, error) {
	base, err := versionBlobTree(repo, port+"^1")
	if err != nil {
		return false, err
	}
	current, err := versionBlobTree(repo, prev)
	if err != nil {
		return false, err
	}
	paths, err := repo.Git("diff-tree", "--no-commit-id", "--name-only", "-r", port+"^1", port)
	if err != nil {
		return false, err
	}
	changed := 0
	for file := range strings.FieldsSeq(paths) {
		if len(targets[file]) > 0 {
			continue
		}
		changed++
		if treeEntriesEqual(base[file], base[file] != (versionTreeEntry{}), current[file], current[file] != (versionTreeEntry{})) {
			return false, nil
		}
	}
	return changed > 0, nil
}

func addSourcesNamedByPorts(repo *gitx.Repo, prior, commits []deliveredCommit, add func(string, deliveredCommit)) error {
	sourcesByPrefix := versionPortSourcesByPrefix(commits)
	for _, port := range prior {
		message := port.subject + "\n" + port.body
		for _, hash := range gitx.CommitHashes(message) {
			matches := sourcesByPrefix[hash]
			if len(matches) != 1 {
				continue
			}
			resolved, ok, err := uniqueCommitPrefix(repo, hash)
			if err != nil {
				return err
			}
			if ok && resolved == matches[0].sha {
				add(port.sha, matches[0])
			}
		}
	}
	return nil
}

// addExplicitVersionPortLinks accepts a release-range note that names both
// the source commit and its port, including an empty operator-authored link.
func addExplicitVersionPortLinks(repo *gitx.Repo, prev string, commits []deliveredCommit, add func(string, deliveredCommit)) error {
	sourcesByPrefix := versionPortSourcesByPrefix(commits)
	for _, link := range commits {
		message := link.subject + "\n" + link.body
		var ports []string
		var sources []deliveredCommit
		for _, hash := range gitx.CommitHashes(message) {
			matches := sourcesByPrefix[hash]
			resolved, ok, err := uniqueCommitPrefix(repo, hash)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if len(matches) == 1 && resolved == matches[0].sha {
				sources = append(sources, matches[0])
			}
			ancestor, err := repo.IsAncestor(resolved, prev)
			if err != nil {
				return err
			}
			if ancestor {
				ports = append(ports, resolved)
			}
		}
		if len(ports) == 0 {
			continue
		}
		for _, source := range uniqueCommits(sources) {
			for _, port := range ports {
				add(port, source)
			}
		}
		if len(sources) > 0 {
			// The hash-bearing note is part of the link: it may be a
			// version-only commit and must not be reconsidered as unlinked
			// content for the same task.
			for _, port := range slices.Compact(ports) {
				add(port, link)
			}
		}
	}
	return nil
}

// uniqueCommitPrefix resolves a hash-like message token only when it names
// exactly one commit object in the repository. rev-parse's usual abbreviation
// resolution can prefer refs, so disambiguate scans the object database.
func uniqueCommitPrefix(repo *gitx.Repo, prefix string) (string, bool, error) {
	out, err := repo.Git("rev-parse", "--disambiguate="+prefix)
	if err != nil {
		return "", false, fmt.Errorf("disambiguating commit prefix %s: %w", prefix, err)
	}
	var match string
	for object := range strings.FieldsSeq(out) {
		typeName, err := repo.Git("cat-file", "-t", object)
		if err != nil {
			return "", false, fmt.Errorf("reading object type for commit prefix %s: %w", prefix, err)
		}
		if strings.TrimSpace(typeName) != "commit" {
			continue
		}
		if match != "" {
			return "", false, nil
		}
		match = object
	}
	return match, match != "", nil
}

func versionPortSourcesByPrefix(commits []deliveredCommit) map[string][]deliveredCommit {
	byPrefix := map[string][]deliveredCommit{}
	for _, c := range commits {
		for n := 7; n <= len(c.sha); n++ {
			prefix := c.sha[:n]
			byPrefix[prefix] = append(byPrefix[prefix], c)
		}
	}
	return byPrefix
}

func uniqueCommits(commits []deliveredCommit) []deliveredCommit {
	seen := map[string]bool{}
	unique := make([]deliveredCommit, 0, len(commits))
	for _, c := range commits {
		if !seen[c.sha] {
			seen[c.sha] = true
			unique = append(unique, c)
		}
	}
	return unique
}

// portGroupChangesNothing cherry-picks a linked source group onto its port in
// memory and compares the final tree outside version files. merge-tree writes
// its temporary trees only to a private object directory under TMPDIR.
func portGroupChangesNothing(repo *gitx.Repo, port string, group []deliveredCommit, prev, head string, targets map[string][]version.Target) (bool, error) {
	if prev == "" {
		return replayVersionPortGroup(repo, port, group, targets)
	}
	unchanged, err := replayVersionPortGroup(repo, port, group, targets)
	if err != nil || !unchanged {
		return unchanged, err
	}
	return versionPortGroupStillPresent(repo, group, prev, head, targets)
}

func replayVersionPortGroup(repo *gitx.Repo, port string, group []deliveredCommit, targets map[string][]version.Target) (bool, error) {
	tmp, err := os.MkdirTemp("", "graft-version-port-")
	if err != nil {
		return false, fmt.Errorf("creating temporary git object directory: %w", err)
	}
	defer os.RemoveAll(tmp)
	objects := filepath.Join(tmp, "objects")
	if err := os.Mkdir(objects, 0o700); err != nil {
		return false, fmt.Errorf("creating temporary git objects path: %w", err)
	}
	baseObjects, err := repo.Git("rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return false, err
	}
	env := []string{"GIT_OBJECT_DIRECTORY=" + objects, "GIT_ALTERNATE_OBJECT_DIRECTORIES=" + strings.TrimSpace(baseObjects)}
	tree, err := repo.Git("rev-parse", "--verify", port+"^{tree}")
	if err != nil {
		return false, err
	}
	tree = strings.TrimSpace(tree)
	firstParent, err := repo.Git("rev-parse", "--verify", group[0].sha+"^1")
	if err != nil {
		return false, err
	}
	firstParent = strings.TrimSpace(firstParent)
	commit := group[len(group)-1].sha
	linear := true
	for i := 1; i < len(group); i++ {
		parent, err := repo.Git("rev-parse", "--verify", group[i].sha+"^1")
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(parent) != group[i-1].sha {
			linear = false
			break
		}
	}
	if !linear {
		for _, c := range group {
			nextTree, ok, err := mergePortCommit(repo, env, tree, c.sha, c.sha+"^1", targets)
			if err != nil || !ok {
				return false, err
			}
			tree = nextTree
		}
	} else {
		nextTree, ok, err := mergePortCommit(repo, env, tree, commit, firstParent, targets)
		if err != nil || !ok {
			return false, err
		}
		tree = nextTree
	}
	portTree, err := versionBlobTree(repo, port)
	if err != nil {
		return false, err
	}
	mergedTree, err := versionBlobTreeWithEnv(repo, env, tree)
	if err != nil {
		return false, err
	}
	return treesEqualExceptVersionEnv(repo, portTree, mergedTree, targets, env)
}

func versionPortGroupStillPresent(repo *gitx.Repo, group []deliveredCommit, prev, head string, targets map[string][]version.Target) (bool, error) {
	active, err := versionPortGroupHasActiveDelta(repo, group, prev, head, targets)
	return !active, err
}

type versionPortLine struct {
	number int
	text   string
}
type versionPortHunk struct{ added, deleted []versionPortLine }

var versionPortHunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)
var versionPortBlameHeader = regexp.MustCompile(`^(?:\^)?([0-9a-f]{40}|[0-9a-f]{64}) (\d+) (\d+)(?: \d+)?$`)

type versionPortOrigin struct {
	commit string
	line   int
}

const versionPortRewriteDepth = 32
const versionPortRewriteBudget = 256

func versionPortGroupHasActiveDelta(repo *gitx.Repo, group []deliveredCommit, prev, head string, targets map[string][]version.Target) (bool, error) {
	groupFiles, removed, err := versionPortSourceDiff(repo, group, targets)
	if err != nil {
		return false, err
	}
	linked, mergeRange, revertRange, inRange, err := versionPortRangeLinks(repo, group, prev, head)
	if err != nil {
		return false, err
	}
	for file := range groupFiles {
		active, err := versionPortFileHasTextDelta(repo, file, prev, head, removed[file], linked, mergeRange, revertRange, inRange, targets)
		if err != nil {
			return false, err
		}
		if active {
			return true, nil
		}
	}
	return versionPortHasBinaryDelta(repo, groupFiles, prev, head)
}

func versionPortRangeLinks(repo *gitx.Repo, group []deliveredCommit, prev, head string) (map[string]bool, map[string]bool, map[string]bool, map[string]bool, error) {
	linked := map[string]bool{}
	for _, c := range group {
		linked[c.sha] = true
	}
	rangeCommits, err := logCommits(repo, prev+".."+head)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	mergeRange := map[string]bool{}
	revertRange := map[string]bool{}
	inRange := map[string]bool{}
	for _, c := range rangeCommits {
		inRange[c.sha] = true
		if c.merge {
			mergeRange[c.sha] = true
		}
		reverted, err := isWorkRemovingRevert(repo, c, rangeCommits, unknownRevertTargetUsesSubject)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("checking revert %s in release range: %w", gitx.Short(c.sha), err)
		}
		if reverted {
			revertRange[c.sha] = true
		}
	}
	return linked, mergeRange, revertRange, inRange, nil
}

func versionPortFileHasTextDelta(repo *gitx.Repo, file, prev, head string, removed, linked, mergeRange, revertRange, inRange map[string]bool, targets map[string][]version.Target) (bool, error) {
	hunks, err := versionPortDiff(repo, prev, head, file)
	if err != nil {
		return false, err
	}
	before, err := normalizedVersionLines(repo, prev, file, targets[file])
	if err != nil {
		return false, err
	}
	after, err := normalizedVersionLines(repo, head, file, targets[file])
	if err != nil {
		return false, err
	}
	var blame map[int]versionPortOrigin
	rewrite := newVersionPortRewrite(repo, file, prev, linked, mergeRange, revertRange, inRange)
	for _, h := range hunks {
		for _, added := range h.added {
			if versionPortLineOnlyBump(added, h.deleted, before, after, targets[file]) {
				continue
			}
			if blame == nil {
				blame, err = versionPortFileBlame(repo, file, prev, head)
				if err != nil {
					return false, err
				}
			}
			// A line blame did not name stays active: an unread line must not
			// hide the group's work from the version checks.
			origin, named := blame[added.number]
			if !named {
				return true, nil
			}
			budget := versionPortRewriteBudget
			active, err := rewrite.walk(origin, 0, &budget)
			if err != nil || active {
				return active, err
			}
		}
		for _, deleted := range h.deleted {
			if versionPortLineOnlyBump(deleted, h.added, after, before, targets[file]) {
				continue
			}
			if removed[deleted.text] {
				return true, nil
			}
		}
	}
	return false, nil
}

// Follow deleted lines from the whole file: a commit can move a line to a
// different hunk while changing it. Cache the diff and blame per commit.
type versionPortRewrite struct {
	repo                                     *gitx.Repo
	file, prev                               string
	linked, mergeRange, revertRange, inRange map[string]bool
	commits                                  map[string]*versionPortRewriteCommit
	moduleLines                              map[string]map[int]bool
	latest                                   userconfig.Modules
	latestLoaded                             bool
	prevModules                              map[string]bool
	prevModulesLoaded                        bool
	invalidPrevModules                       bool
	invalidModules                           map[string]bool
}

type versionPortRewriteCommit struct {
	deleted       map[int][]versionPortLine
	changed       map[int]bool
	blame         map[int]versionPortOrigin
	parent        string
	lines         []int
	loaded        bool
	parentMissing bool
}

func versionPortRewriteActive(repo *gitx.Repo, file, prev string, origin versionPortOrigin, linked, mergeRange, revertRange, inRange map[string]bool, depth int) (bool, error) {
	state := newVersionPortRewrite(repo, file, prev, linked, mergeRange, revertRange, inRange)
	budget := versionPortRewriteBudget
	return state.walk(origin, depth, &budget)
}

func newVersionPortRewrite(repo *gitx.Repo, file, prev string, linked, mergeRange, revertRange, inRange map[string]bool) *versionPortRewrite {
	return &versionPortRewrite{repo: repo, file: file, prev: prev, linked: linked, mergeRange: mergeRange, revertRange: revertRange, inRange: inRange, commits: map[string]*versionPortRewriteCommit{}, moduleLines: map[string]map[int]bool{}, invalidModules: map[string]bool{}}
}

func (s *versionPortRewrite) walk(origin versionPortOrigin, depth int, budget *int) (bool, error) {
	if s.linked[origin.commit] || s.mergeRange[origin.commit] || s.revertRange[origin.commit] {
		return true, nil
	}
	if !s.inRange[origin.commit] {
		return false, nil
	}
	if depth >= versionPortRewriteDepth || *budget == 0 {
		return true, nil
	}
	*budget--
	mechanical, uncertain, err := s.mechanical(origin)
	if err != nil || uncertain {
		return uncertain, err
	}
	if mechanical {
		return false, nil
	}
	data, err := s.commit(origin.commit)
	if err != nil {
		return false, err
	}
	deleted, ok := data.deleted[origin.line]
	if !ok {
		return true, nil
	}
	if len(deleted) == 0 {
		return data.parentMissing, nil
	}
	if err := s.loadPrior(data); err != nil {
		return false, err
	}
	for _, line := range deleted {
		if !data.changed[line.number] {
			continue
		}
		prior, ok := data.blame[line.number]
		if !ok {
			return true, nil
		}
		active, err := s.walk(prior, depth+1, budget)
		if err != nil || active {
			return active, err
		}
	}
	return false, nil
}

func (s *versionPortRewrite) mechanical(origin versionPortOrigin) (bool, bool, error) {
	switch filepath.Base(s.file) {
	case "go.sum":
		return true, false, nil
	case "go.mod":
	default:
		return false, false, nil
	}
	lines, ok := s.moduleLines[origin.commit]
	if !ok {
		var err error
		lines, err = s.loadModuleHistory(origin.commit)
		if err != nil {
			return false, false, err
		}
	}
	return lines[origin.line], s.invalidPrevModules || s.invalidModules[origin.commit], nil
}

func (s *versionPortRewrite) loadModuleHistory(commit string) (map[int]bool, error) {
	if !s.latestLoaded {
		var err error
		s.latest, err = userconfig.ModulesSettings()
		if err != nil {
			return nil, fmt.Errorf("reading modules.latest: %w", err)
		}
		s.latestLoaded = true
	}
	lines := map[int]bool{}
	if len(s.latest.Latest) == 0 {
		s.moduleLines[commit] = lines
		return lines, nil
	}
	s.loadPrevModules()
	if s.invalidPrevModules {
		s.moduleLines[commit] = lines
		return lines, nil
	}
	text, err := s.repo.Git("show", commit+":"+s.file)
	if err != nil {
		return nil, err
	}
	parent, parentErr := s.repo.Git("show", commit+"^1:"+s.file)
	if parentErr == nil {
		lines, err = versionPortModuleRaises(parent, text, func(path string) bool { return s.latest.IsLatest(path) && s.prevModules[path] })
		// A parse failure means attribution is uncertain, so walk marks it active.
		s.invalidModules[commit] = err != nil
	}
	s.moduleLines[commit] = lines
	return lines, nil
}

func (s *versionPortRewrite) loadPrevModules() {
	if s.prevModulesLoaded {
		return
	}
	s.prevModules = map[string]bool{}
	previous, err := s.repo.Git("show", s.prev+":"+s.file)
	// Without a readable target go.mod we cannot prove the module is present.
	s.invalidPrevModules = err != nil
	if !s.invalidPrevModules {
		var parseErr error
		s.prevModules, parseErr = versionPortRequiredModules(s.file, previous)
		// Unreadable history is active rather than treated as a successful match.
		s.invalidPrevModules = parseErr != nil
	}
	s.prevModulesLoaded = true
}

func versionPortRequiredModules(file, text string) (map[string]bool, error) {
	mod, err := modfile.Parse(file, []byte(text), nil)
	if err != nil {
		return nil, fmt.Errorf("parsing previous go.mod: %w", err)
	}
	modules := map[string]bool{}
	for _, req := range mod.Require {
		modules[req.Mod.Path] = true
	}
	for _, rep := range mod.Replace {
		delete(modules, rep.Old.Path)
	}
	return modules, nil
}

// Only version increases are mechanical; changes to a directive's other
// tokens or comments remain task work. Invalid module files stay conservative.
func versionPortModuleRaises(before, after string, latest func(string) bool) (map[int]bool, error) {
	lines := map[int]bool{}
	base, err := modfile.Parse("go.mod", []byte(before), nil)
	if err != nil {
		return nil, fmt.Errorf("parsing parent go.mod: %w", err)
	}
	next, err := modfile.Parse("go.mod", []byte(after), nil)
	if err != nil {
		return nil, fmt.Errorf("parsing rewritten go.mod: %w", err)
	}
	oldLines, newLines := strings.Split(before, "\n"), strings.Split(after, "\n")
	replaced := map[string]bool{}
	for _, rep := range append(slices.Clone(base.Replace), next.Replace...) {
		replaced[rep.Old.Path] = true
	}
	for _, req := range next.Require {
		for _, old := range base.Require {
			if req.Mod.Path == old.Mod.Path && !replaced[req.Mod.Path] && worktreeSyncLatestRequireUpgrade(req.Mod.Path, old.Mod.Version, req.Mod.Version, latest) && versionPortOnlyRaises(oldLines[old.Syntax.Start.Line-1], newLines[req.Syntax.Start.Line-1], old.Mod.Version, req.Mod.Version) {
				lines[req.Syntax.Start.Line] = true
			}
		}
	}
	return lines, nil
}

func versionPortOnlyRaises(before, after, oldVersion, newVersion string) bool {
	oldCode, oldComment, oldHasComment := strings.Cut(before, "//")
	newCode, newComment, newHasComment := strings.Cut(after, "//")
	if oldHasComment != newHasComment || oldComment != newComment {
		return false
	}
	old, next := strings.Fields(oldCode), strings.Fields(newCode)
	if len(old) != len(next) {
		return false
	}
	changed := false
	for i := range old {
		if old[i] == next[i] {
			continue
		}
		if old[i] != oldVersion || next[i] != newVersion || changed {
			return false
		}
		changed = true
	}
	return changed
}

func (s *versionPortRewrite) commit(commit string) (*versionPortRewriteCommit, error) {
	if data := s.commits[commit]; data != nil {
		return data, nil
	}
	parent := commit + "^1"
	hunks, err := versionPortDiff(s.repo, parent, commit, s.file)
	if err != nil {
		return nil, err
	}
	data := &versionPortRewriteCommit{parent: parent, deleted: map[int][]versionPortLine{}}
	var allDeleted []versionPortLine
	for _, h := range hunks {
		allDeleted = append(allDeleted, h.deleted...)
		for _, line := range h.deleted {
			data.lines = append(data.lines, line.number)
		}
	}
	for _, h := range hunks {
		for _, line := range h.added {
			data.deleted[line.number] = allDeleted
		}
	}
	if len(allDeleted) == 0 {
		_, err := s.repo.Git("cat-file", "-e", parent+":"+s.file)
		data.parentMissing = err != nil
	}
	s.commits[commit] = data
	return data, nil
}

func (s *versionPortRewrite) loadPrior(data *versionPortRewriteCommit) error {
	if data.loaded {
		return nil
	}
	var err error
	data.changed, err = versionPortAddedLines(s.repo, s.prev, data.parent, s.file)
	if err != nil {
		return err
	}
	var lines []int
	for _, line := range data.lines {
		if data.changed[line] {
			lines = append(lines, line)
		}
	}
	if len(lines) > 0 {
		data.blame, err = versionPortFileBlameLines(s.repo, s.file, s.prev, data.parent, lines)
		if err != nil {
			return err
		}
	}
	data.loaded = true
	return nil
}

func versionPortAddedLines(repo *gitx.Repo, prev, head, file string) (map[int]bool, error) {
	hunks, err := versionPortDiff(repo, prev, head, file)
	if err != nil {
		return nil, err
	}
	added := map[int]bool{}
	for _, h := range hunks {
		for _, line := range h.added {
			added[line.number] = true
		}
	}
	return added, nil
}

func versionPortFileBlame(repo *gitx.Repo, file, prev, head string) (map[int]versionPortOrigin, error) {
	return versionPortFileBlameLines(repo, file, prev, head, nil)
}

func versionPortFileBlameLines(repo *gitx.Repo, file, prev, head string, lines []int) (map[int]versionPortOrigin, error) {
	args := versionPortFileBlameArgs(file, prev, head, lines)
	output, err := repo.Git(args...)
	if err != nil {
		return nil, fmt.Errorf("blaming %s in release range: %w", file, err)
	}
	origins := map[int]versionPortOrigin{}
	for line := range strings.SplitSeq(output, "\n") {
		match := versionPortBlameHeader.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		original, err := strconv.Atoi(match[2])
		if err != nil {
			return nil, fmt.Errorf("parsing original line number %q in %s: %w", match[2], file, err)
		}
		lineNumber, err := strconv.Atoi(match[3])
		if err != nil {
			return nil, fmt.Errorf("parsing blamed line number %q in %s: %w", match[3], file, err)
		}
		origins[lineNumber] = versionPortOrigin{match[1], original}
	}
	return origins, nil
}

func versionPortFileBlameArgs(file, prev, head string, lines []int) []string {
	args := []string{"blame", "--line-porcelain", "--no-follow"}
	slices.Sort(lines)
	lines = slices.Compact(lines)
	// A very fragmented file can exceed the operating system's argument limit;
	// in that case one full-file blame remains bounded to a single Git process.
	var ranges []string
	for i := 0; i < len(lines); {
		start, end := lines[i], lines[i]
		i++
		for i < len(lines) && lines[i] == end+1 {
			end = lines[i]
			i++
		}
		ranges = append(ranges, strconv.Itoa(start)+","+strconv.Itoa(end))
	}
	if len(ranges) <= 128 {
		for _, r := range ranges {
			args = append(args, "-L", r)
		}
	}
	args = append(args, prev+".."+head, "--", file)
	return args
}

func versionPortHasBinaryDelta(repo *gitx.Repo, groupFiles map[string]bool, prev, head string) (bool, error) {
	prevTree, err := versionBlobTree(repo, prev)
	if err != nil {
		return false, err
	}
	headTree, err := versionBlobTree(repo, head)
	if err != nil {
		return false, err
	}
	for file := range groupFiles {
		before, beforeOK := prevTree[file]
		after, afterOK := headTree[file]
		if beforeOK != afterOK {
			return true, nil
		}
		if !beforeOK {
			continue
		}
		if before.mode != after.mode {
			return true, nil
		}
		if before.oid == after.oid {
			continue
		}
		for _, entry := range []versionTreeEntry{before, after} {
			if entry.kind != "blob" || entry.oid == "" {
				continue
			}
			blob, err := repo.Git("cat-file", "blob", entry.oid)
			if err != nil {
				return false, err
			}
			if bytes.IndexByte([]byte(blob), 0) >= 0 {
				return true, nil
			}
		}
	}
	return false, nil
}

func versionPortSourceDiff(repo *gitx.Repo, group []deliveredCommit, targets map[string][]version.Target) (map[string]bool, map[string]map[string]bool, error) {
	files, removed := map[string]bool{}, map[string]map[string]bool{}
	for _, c := range group {
		parent, err := repo.Git("rev-parse", "--verify", c.sha+"^1")
		if err != nil {
			return nil, nil, err
		}
		parent = strings.TrimSpace(parent)
		out, err := repo.Git("diff-tree", "--no-commit-id", "--name-only", "-r", "-z", "--no-renames", parent, c.sha)
		if err != nil {
			return nil, nil, err
		}
		for file := range strings.SplitSeq(out, "\x00") {
			if file != "" {
				files[file] = true
			}
		}
		for file := range strings.SplitSeq(out, "\x00") {
			if file == "" {
				continue
			}
			if err := versionPortRecordRemoved(repo, file, parent, c.sha, targets[file], removed); err != nil {
				return nil, nil, err
			}
		}
	}
	return files, removed, nil
}

func versionPortRecordRemoved(repo *gitx.Repo, file, parent, commit string, targets []version.Target, removed map[string]map[string]bool) error {
	hunks, err := versionPortDiff(repo, parent, commit, file)
	if err != nil {
		return err
	}
	before, err := normalizedVersionLines(repo, parent, file, targets)
	if err != nil {
		return err
	}
	after, err := normalizedVersionLines(repo, commit, file, targets)
	if err != nil {
		return err
	}
	for _, h := range hunks {
		for _, line := range h.deleted {
			if versionPortLineOnlyBump(line, h.added, after, before, targets) {
				continue
			}
			if removed[file] == nil {
				removed[file] = map[string]bool{}
			}
			removed[file][line.text] = true
		}
	}
	return nil
}

func versionPortDiff(repo *gitx.Repo, from, to, file string) ([]versionPortHunk, error) {
	args := versionPortDiffArgs(from, to, file)
	out, err := repo.Git(args...)
	if err != nil {
		return nil, err
	}
	var hunks []versionPortHunk
	var h *versionPortHunk
	oldLine, newLine := 0, 0
	for line := range strings.SplitSeq(out, "\n") {
		if m := versionPortHunkHeader.FindStringSubmatch(line); m != nil {
			var err error
			oldLine, err = strconv.Atoi(m[1])
			if err != nil {
				return nil, fmt.Errorf("parsing old line number in diff hunk %q: %w", line, err)
			}
			newLine, err = strconv.Atoi(m[3])
			if err != nil {
				return nil, fmt.Errorf("parsing new line number in diff hunk %q: %w", line, err)
			}
			hunks = append(hunks, versionPortHunk{})
			h = &hunks[len(hunks)-1]
			continue
		}
		if h == nil || line == "" || strings.HasPrefix(line, "\\") {
			continue
		}
		if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
			continue
		}
		switch line[0] {
		case '+':
			h.added = append(h.added, versionPortLine{newLine, line[1:]})
			newLine++
		case '-':
			h.deleted = append(h.deleted, versionPortLine{oldLine, line[1:]})
			oldLine++
		case ' ':
			oldLine++
			newLine++
		}
	}
	return hunks, nil
}

func versionPortDiffArgs(from, to, file string) []string {
	return []string{"diff", "--no-renames", "--no-ext-diff", "--no-color", "--unified=0", from, to, "--", file}
}

func normalizedVersionLines(repo *gitx.Repo, rev, file string, targets []version.Target) ([]string, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	tree, err := versionBlobTree(repo, rev)
	if err != nil {
		return nil, err
	}
	entry, ok := tree[file]
	if !ok || entry.kind != "blob" {
		return nil, nil
	}
	content, err := repo.Git("cat-file", "blob", entry.oid)
	if err != nil {
		return nil, err
	}
	normalized := []byte(content)
	for _, target := range targets {
		normalized, err = target.Replace(normalized, "__GRAFT_VERSION_VALUE__")
		if err != nil {
			return nil, err
		}
	}
	return strings.Split(string(normalized), "\n"), nil
}

func versionPortLineOnlyBump(line versionPortLine, opposite []versionPortLine, before, after []string, targets []version.Target) bool {
	if len(targets) == 0 {
		return false
	}
	if line.number < 1 || line.number > len(after) {
		return false
	}
	normalized := after[line.number-1]
	for _, other := range opposite {
		if other.number >= 1 && other.number <= len(before) && before[other.number-1] == normalized {
			return true
		}
	}
	return false
}

func mergePortCommit(repo *gitx.Repo, env []string, ontoTree, commit, mergeBase string, targets map[string][]version.Target) (string, bool, error) {
	out, err := repo.GitEnv(env, "merge-tree", "--write-tree", "--name-only", "--no-messages", "--merge-base="+mergeBase, ontoTree, commit)
	conflict := err != nil && gitx.ExitCode(err) == 1
	if err != nil && !conflict {
		return "", false, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return "", false, fmt.Errorf("git merge-tree returned no tree for %s", gitx.Short(commit))
	}
	if conflict {
		return mergeVersionPortConflict(repo, env, ontoTree, commit, mergeBase, lines, targets)
	}
	merged, err := versionBlobTreeWithEnv(repo, env, lines[0])
	if err != nil {
		return "", false, err
	}
	onto, err := versionBlobTree(repo, ontoTree)
	if err != nil {
		return "", false, err
	}
	return mergeVersionPortClean(repo, env, commit, mergeBase, lines[0], merged, onto, targets)
}

func mergeVersionPortConflict(repo *gitx.Repo, env []string, ontoTree, commit, mergeBase string, lines []string, targets map[string][]version.Target) (string, bool, error) {
	for _, file := range lines[1:] {
		if file != "" && len(targets[file]) == 0 && file != "go.mod" && file != "go.sum" {
			return lines[0], false, nil
		}
	}
	merged, err := versionBlobTreeWithEnv(repo, env, lines[0])
	if err != nil {
		return "", false, err
	}
	onto, err := versionBlobTree(repo, ontoTree)
	if err != nil {
		return "", false, err
	}
	base, err := versionBlobTree(repo, mergeBase)
	if err != nil {
		return "", false, err
	}
	source, err := versionBlobTree(repo, commit)
	if err != nil {
		return "", false, err
	}
	onBase, err := versionTargetsEqual(repo, base, onto, targets)
	if err != nil {
		return "", false, err
	}
	sourceOnBase, err := versionTargetsEqual(repo, base, source, targets)
	if err != nil {
		return "", false, err
	}
	equal, equalErr := treesEqualOutsideVersionFiles(repo, env, onto, merged, targets)
	if equalErr != nil {
		return "", false, equalErr
	}
	if !allVersionTargetsExist(onto, targets) || !allVersionTargetsExist(source, targets) || !onBase || !sourceOnBase || !equal {
		return lines[0], false, nil
	}
	return ontoTree, true, nil
}

func mergeVersionPortClean(repo *gitx.Repo, env []string, commit, mergeBase, mergedID string, merged, onto map[string]versionTreeEntry, targets map[string][]version.Target) (string, bool, error) {
	equal, err := treesEqualExceptVersionEnv(repo, onto, merged, targets, env)
	if err != nil {
		return "", false, err
	}
	if equal {
		if !allVersionTargetsExist(merged, targets) {
			return mergedID, false, nil
		}
		return mergedID, true, nil
	}
	base, err := versionBlobTree(repo, mergeBase)
	if err != nil {
		return "", false, err
	}
	source, err := versionBlobTree(repo, commit)
	if err != nil {
		return "", false, err
	}
	ontoOnBase, err := treesEqualExceptVersion(repo, base, onto, targets)
	if err != nil {
		return "", false, err
	}
	sourceOnBase, err := treesEqualExceptVersion(repo, base, source, targets)
	if err != nil {
		return "", false, err
	}
	if ontoOnBase && sourceOnBase && allVersionTargetsExist(onto, targets) && allVersionTargetsExist(source, targets) {
		return mergedID, true, nil
	}
	return mergedID, false, nil
}

func treesEqualOutsideVersionFiles(repo *gitx.Repo, env []string, a, b map[string]versionTreeEntry, targets map[string][]version.Target) (bool, error) {
	moduleFiles, err := versionPortModuleFilesCanDiffer(repo, env, a, b)
	if err != nil || !moduleFiles {
		return false, err
	}
	for file := range unionTreePaths(a, b) {
		if len(targets[file]) > 0 || moduleFiles && (file == "go.mod" || file == "go.sum") {
			continue
		}
		if !treeEntriesEqual(a[file], a[file] != (versionTreeEntry{}), b[file], b[file] != (versionTreeEntry{})) {
			return false, nil
		}
	}
	return true, nil
}

func allVersionTargetsExist(tree map[string]versionTreeEntry, targets map[string][]version.Target) bool {
	for file := range targets {
		entry, ok := tree[file]
		if !ok || entry.kind != "blob" {
			return false
		}
	}
	return true
}

func versionTargetsEqual(repo *gitx.Repo, a, b map[string]versionTreeEntry, targets map[string][]version.Target) (bool, error) {
	for file, ts := range targets {
		aa, aok := a[file]
		bb, bok := b[file]
		if !aok || !bok || aa.kind != "blob" || bb.kind != "blob" || aa.mode != bb.mode {
			return false, nil
		}
		ac, err := repo.Git("cat-file", "blob", aa.oid)
		if err != nil {
			return false, fmt.Errorf("reading version target %q: %w", file, err)
		}
		bc, err := repo.Git("cat-file", "blob", bb.oid)
		if err != nil {
			return false, fmt.Errorf("reading version target %q: %w", file, err)
		}
		for _, target := range ts {
			x, e := target.Replace([]byte(ac), "__GRAFT_VERSION_VALUE__")
			if e != nil {
				return false, e
			}
			y, e := target.Replace([]byte(bc), "__GRAFT_VERSION_VALUE__")
			if e != nil {
				return false, e
			}
			ac, bc = string(x), string(y)
		}
		if ac != bc {
			return false, nil
		}
	}
	return true, nil
}

func unionTreePaths(a, b map[string]versionTreeEntry) map[string]bool {
	out := map[string]bool{}
	for p := range a {
		out[p] = true
	}
	for p := range b {
		out[p] = true
	}
	return out
}

func treeEntriesEqual(a versionTreeEntry, aok bool, b versionTreeEntry, bok bool) bool {
	return aok == bok && (!aok || a == b)
}

func treesEqualExceptVersion(repo *gitx.Repo, a, b map[string]versionTreeEntry, targets map[string][]version.Target) (bool, error) {
	return treesEqualExceptVersionEnv(repo, a, b, targets, nil)
}

func treesEqualExceptVersionEnv(repo *gitx.Repo, a, b map[string]versionTreeEntry, targets map[string][]version.Target, env []string) (bool, error) {
	git := repo.Git
	if len(env) > 0 {
		git = func(args ...string) (string, error) { return repo.GitEnv(env, args...) }
	}
	moduleFiles, err := versionPortModuleFilesCanDifferWithGit(git, a, b)
	if err != nil || !moduleFiles {
		return false, err
	}
	for file := range unionTreePaths(a, b) {
		aa, aok := a[file]
		bb, bok := b[file]
		if treeEntriesEqual(aa, aok, bb, bok) {
			continue
		}
		if !aok || !bok || aa.kind != "blob" || bb.kind != "blob" || aa.mode != bb.mode || (len(targets[file]) == 0 && !(moduleFiles && (file == "go.mod" || file == "go.sum"))) {
			return false, nil
		}
		if len(targets[file]) == 0 {
			continue
		}
		equal, err := versionTargetBlobsEqual(git, file, aa, bb, targets[file])
		if err != nil {
			return false, err
		}
		if !equal {
			return false, nil
		}
	}
	return true, nil
}

func versionTargetBlobsEqual(git func(...string) (string, error), file string, a, b versionTreeEntry, targets []version.Target) (bool, error) {
	ac, err := git("cat-file", "blob", a.oid)
	if err != nil {
		return false, fmt.Errorf("reading version-target blob %q: %w", file, err)
	}
	bc, err := git("cat-file", "blob", b.oid)
	if err != nil {
		return false, fmt.Errorf("reading version-target blob %q: %w", file, err)
	}
	for _, target := range targets {
		acb, err := target.Replace([]byte(ac), "__GRAFT_VERSION_VALUE__")
		if err != nil {
			return false, err
		}
		bcb, err := target.Replace([]byte(bc), "__GRAFT_VERSION_VALUE__")
		if err != nil {
			return false, err
		}
		ac = string(acb)
		bc = string(bcb)
	}
	return ac == bc, nil
}

type versionPortModuleUpgrade struct{ before, after string }

func versionPortModuleFilesCanDiffer(repo *gitx.Repo, env []string, a, b map[string]versionTreeEntry) (bool, error) {
	git := repo.Git
	if len(env) > 0 {
		git = func(args ...string) (string, error) { return repo.GitEnv(env, args...) }
	}
	return versionPortModuleFilesCanDifferWithGit(git, a, b)
}

func versionPortModuleFilesCanDifferWithGit(git func(...string) (string, error), a, b map[string]versionTreeEntry) (bool, error) {
	upgrades, ok, err := versionPortGoModUpgrades(git, a, b)
	if err != nil || !ok {
		return false, err
	}
	return versionPortGoSumMatches(git, a, b, upgrades)
}

func versionPortGoModUpgrades(git func(...string) (string, error), a, b map[string]versionTreeEntry) (map[string]versionPortModuleUpgrade, bool, error) {
	modA, modAOK := a["go.mod"]
	modB, modBOK := b["go.mod"]
	if treeEntriesEqual(modA, modAOK, modB, modBOK) {
		return map[string]versionPortModuleUpgrade{}, true, nil
	}
	if !modAOK || !modBOK || modA.kind != "blob" || modB.kind != "blob" || modA.mode != modB.mode {
		return nil, false, nil
	}
	before, err := git("cat-file", "blob", modA.oid)
	if err != nil {
		return nil, false, fmt.Errorf("reading replay go.mod: %w", err)
	}
	after, err := git("cat-file", "blob", modB.oid)
	if err != nil {
		return nil, false, fmt.Errorf("reading replay go.mod: %w", err)
	}
	upgrades, ok, err := versionPortModuleRaisesOnly(before, after)
	if err != nil {
		return nil, false, fmt.Errorf("parsing replay go.mod: %w", err)
	}
	return upgrades, ok && len(upgrades) > 0, nil
}

func versionPortGoSumMatches(git func(...string) (string, error), a, b map[string]versionTreeEntry, upgrades map[string]versionPortModuleUpgrade) (bool, error) {
	sumA, sumAOK := a["go.sum"]
	sumB, sumBOK := b["go.sum"]
	if !treeEntriesEqual(sumA, sumAOK, sumB, sumBOK) {
		if !sumAOK || !sumBOK || sumA.kind != "blob" || sumB.kind != "blob" || sumA.mode != sumB.mode || len(upgrades) == 0 {
			return false, nil
		}
		before, err := git("cat-file", "blob", sumA.oid)
		if err != nil {
			return false, fmt.Errorf("reading replay go.sum: %w", err)
		}
		after, err := git("cat-file", "blob", sumB.oid)
		if err != nil {
			return false, fmt.Errorf("reading replay go.sum: %w", err)
		}
		matches, err := versionPortGoSumChangesOnlyForUpgrades(before, after, upgrades)
		if err != nil {
			return false, fmt.Errorf("checking replay go.sum: %w", err)
		}
		if !matches {
			return false, nil
		}
	}
	return true, nil
}

func versionPortModuleRaisesOnly(before, after string) (map[string]versionPortModuleUpgrade, bool, error) {
	base, err := modfile.Parse("go.mod", []byte(before), nil)
	if err != nil {
		return nil, false, fmt.Errorf("parsing previous go.mod: %w", err)
	}
	next, err := modfile.Parse("go.mod", []byte(after), nil)
	if err != nil {
		return nil, false, fmt.Errorf("parsing replayed go.mod: %w", err)
	}
	baseLines, nextLines := strings.Split(before, "\n"), strings.Split(after, "\n")
	if len(baseLines) != len(nextLines) {
		return nil, false, nil
	}
	replaced := map[string]bool{}
	for _, rep := range append(slices.Clone(base.Replace), next.Replace...) {
		replaced[rep.Old.Path] = true
	}
	oldReqs, newReqs := map[string]modfile.Require{}, map[string]modfile.Require{}
	for _, req := range base.Require {
		oldReqs[req.Mod.Path] = *req
	}
	for _, req := range next.Require {
		newReqs[req.Mod.Path] = *req
	}
	upgrades := map[string]versionPortModuleUpgrade{}
	for i := range baseLines {
		if baseLines[i] == nextLines[i] {
			continue
		}
		line := i + 1
		path, oldReq, newReq, found := versionPortRequirementAtLine(oldReqs, newReqs, line)
		if !found || replaced[path] || !versionPortValidRaise(oldReq, newReq) || !versionPortOnlyRaises(baseLines[i], nextLines[i], oldReq, newReq) {
			return nil, false, nil
		}
		upgrades[path] = versionPortModuleUpgrade{before: oldReq, after: newReq}
	}
	if len(upgrades) == 0 {
		return nil, false, nil
	}
	return upgrades, true, nil
}

func versionPortRequirementAtLine(oldReqs, newReqs map[string]modfile.Require, line int) (string, string, string, bool) {
	for _, path := range slices.Sorted(maps.Keys(oldReqs)) {
		req := oldReqs[path]
		if req.Syntax.Start.Line != line {
			continue
		}
		new, ok := newReqs[path]
		if !ok || new.Syntax.Start.Line != line {
			return "", "", "", false
		}
		return path, req.Mod.Version, new.Mod.Version, true
	}
	return "", "", "", false
}

func versionPortValidRaise(oldVersion, newVersion string) bool {
	return semver.IsValid(oldVersion) && semver.IsValid(newVersion) && semver.Compare(newVersion, oldVersion) > 0
}

func versionPortGoSumChangesOnlyForUpgrades(before, after string, upgrades map[string]versionPortModuleUpgrade) (bool, error) {
	counts := map[string]int{}
	for line := range strings.SplitSeq(before, "\n") {
		if line != "" {
			counts[line]++
		}
	}
	for line := range strings.SplitSeq(after, "\n") {
		if line != "" {
			counts[line]--
		}
	}
	for line, count := range counts {
		if count == 0 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return false, nil
		}
		path, err := module.UnescapePath(fields[0])
		if err != nil {
			return false, fmt.Errorf("unescaping go.sum module path: %w", err)
		}
		change, ok := upgrades[path]
		if !ok {
			return false, nil
		}
		version := strings.TrimSuffix(fields[1], "/go.mod")
		if version != change.before && version != change.after {
			return false, nil
		}
	}
	return true, nil
}
