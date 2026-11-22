package app

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/stats"
)

type statsRepoGroup struct {
	key, name, limitRoot string
}

type statsRepoPath struct {
	commonDir, profileName, worktreeName string
	lookupFailed                         bool
}

type statsRepoIdentity struct {
	commonDir, unresolvedName string
}

func summarizeStatsRows(records []stats.Record, filter stats.Filter, groupByRepo bool) ([]stats.Row, stats.Saved, error) {
	if !groupByRepo {
		rows, saved := stats.Summarize(records, filter)
		return rows, saved, nil
	}
	resolved := make(map[string]string)
	groups, err := statsRepoGroupsWithCache(records, resolved)
	if err != nil {
		return nil, stats.Saved{}, err
	}
	groupsByKey := make(map[string]statsRepoGroup, len(groups))
	for _, group := range groups {
		groupsByKey[group.key] = group
	}
	for i := range records {
		if records[i].Repo != "" {
			group := statsRepoGroupForCommonDir(resolved[records[i].Repo])
			groupsByKey[group.key] = group
		}
	}
	rows, saved := stats.SummarizeByProject(records, filter, func(record *stats.Record) string {
		if record.Repo != "" {
			return statsRepoGroupForCommonDir(resolved[record.Repo]).key
		}
		return groups[record.Project].key
	})
	for i := range rows {
		if group, ok := groupsByKey[rows[i].Project]; ok {
			rows[i].Project = group.name
			rows[i].LimitProject = group.limitRoot
		}
	}
	return rows, saved, nil
}

// statsRepoGroups gives each project path one key, preferring the repository
// identity recorded at run time, then live Git metadata, then a unique match
// for the repository name in Profile.
func statsRepoGroups(records []stats.Record) (map[string]statsRepoGroup, error) {
	return statsRepoGroupsWithCache(records, make(map[string]string))
}

func statsRepoGroupsWithCache(records []stats.Record, resolved map[string]string) (map[string]statsRepoGroup, error) {
	paths := make(map[string]*statsRepoPath)
	known := map[string]map[string]bool{}
	worktreeRoot := ""
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("locating the graft worktree cache: %w", err)
	}
	worktreeRoot = graftWorktreeRoot(cache)
	if realRoot, err := filepath.EvalSymlinks(worktreeRoot); err == nil {
		worktreeRoot = realRoot
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("resolving graft worktree cache %s: %w", worktreeRoot, err)
	}
	for i := range records {
		if err := collectStatsRepoPath(paths, known, resolved, worktreeRoot, &records[i]); err != nil {
			return nil, err
		}
	}

	identities := make(map[string]statsRepoIdentity, len(paths))
	unresolvedRoots := make(map[string]string)
	for project, path := range paths {
		identity := classifyStatsRepoPath(path, known)
		identities[project] = identity
		if identity.unresolvedName == "" {
			continue
		}
		key := "profile-unresolved:" + identity.unresolvedName
		if old, ok := unresolvedRoots[key]; !ok || project < old {
			unresolvedRoots[key] = project
		}
	}
	groups := make(map[string]statsRepoGroup, len(paths))
	for project, identity := range identities {
		groups[project] = statsRepoGroupForIdentity(project, identity, unresolvedRoots)
	}
	return groups, nil
}

func unresolvedStatsRepoGroup(name string, roots map[string]string) statsRepoGroup {
	key := "profile-unresolved:" + name
	return statsRepoGroup{key: key, name: name + " (?)", limitRoot: roots[key]}
}

// classifyStatsRepoPath is the single ordered decision for repo identity:
// recorded/live Git metadata, Profile, graft worktree name, then path.
func classifyStatsRepoPath(path *statsRepoPath, known map[string]map[string]bool) statsRepoIdentity {
	switch {
	case path.commonDir != "":
		return statsRepoIdentity{commonDir: path.commonDir}
	case path.lookupFailed && path.profileName != "":
		return statsRepoIdentity{unresolvedName: path.profileName}
	case path.profileName != "" && len(known[path.profileName]) == 1:
		return statsRepoIdentity{commonDir: onlyKnownRepo(known[path.profileName])}
	case path.worktreeName != "" && len(known[path.worktreeName]) == 1:
		return statsRepoIdentity{commonDir: onlyKnownRepo(known[path.worktreeName])}
	case path.worktreeName != "":
		return statsRepoIdentity{unresolvedName: path.worktreeName}
	case path.profileName != "":
		return statsRepoIdentity{unresolvedName: path.profileName}
	default:
		return statsRepoIdentity{}
	}
}

func onlyKnownRepo(repositories map[string]bool) string {
	keys := slices.Sorted(maps.Keys(repositories))
	if len(keys) == 1 {
		return keys[0]
	}
	return ""
}

func statsRepoGroupForIdentity(project string, identity statsRepoIdentity, unresolvedRoots map[string]string) statsRepoGroup {
	if identity.commonDir != "" {
		return statsRepoGroupForCommonDir(identity.commonDir)
	}
	if identity.unresolvedName != "" {
		return unresolvedStatsRepoGroup(identity.unresolvedName, unresolvedRoots)
	}
	return statsRepoGroup{key: "path:" + project, name: project, limitRoot: project}
}

func statsRepoGroupForCommonDir(commonDir string) statsRepoGroup {
	root := repoProjectPath(commonDir)
	return statsRepoGroup{key: "git:" + commonDir, name: root, limitRoot: root}
}

func collectStatsRepoPath(paths map[string]*statsRepoPath, known map[string]map[string]bool, resolved map[string]string, worktreeRoot string, record *stats.Record) error {
	path := paths[record.Project]
	if path == nil {
		path = &statsRepoPath{}
		commonDir, err := gitCommonDir(record.Project, resolved)
		if err == nil {
			path.commonDir = commonDir
		} else if !errors.Is(err, os.ErrNotExist) {
			path.lookupFailed = true
		}
		path.worktreeName, err = graftWorktreeName(record.Project, worktreeRoot)
		if err != nil {
			return err
		}
		paths[record.Project] = path
	}
	if name := profileRepository(record); name != "" && path.profileName == "" {
		path.profileName = name
	}
	if record.Repo != "" {
		commonDir, err := resolveStatsCommonDir(record.Repo, resolved)
		if err != nil {
			return err
		}
		addKnownRepo(known, commonDir)
	}
	addKnownRepo(known, path.commonDir)
	return nil
}

func resolveStatsCommonDir(commonDir string, cache map[string]string) (string, error) {
	if commonDir == "" {
		return "", nil
	}
	if cache != nil {
		if resolved, ok := cache[commonDir]; ok {
			return resolved, nil
		}
	}
	absolute, err := filepath.Abs(commonDir)
	if err != nil {
		return "", fmt.Errorf("resolving common Git directory %s: %w", commonDir, err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(absolute))
	if errors.Is(err, os.ErrNotExist) {
		resolved = filepath.Clean(absolute)
	} else if err != nil {
		return "", fmt.Errorf("resolving common Git directory %s: %w", absolute, err)
	}
	if cache != nil {
		cache[commonDir] = resolved
	}
	return resolved, nil
}

func graftWorktreeName(project, root string) (string, error) {
	if root == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(project)
	if err != nil {
		return "", fmt.Errorf("resolving project path %s: %w", project, err)
	}
	rel, err := filepath.Rel(root, absolute)
	if err != nil {
		return "", fmt.Errorf("finding project %s relative to graft worktree cache %s: %w", absolute, root, err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", nil
	}
	parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", nil
	}
	return parts[0], nil
}

func addKnownRepo(known map[string]map[string]bool, commonDir string) {
	if commonDir == "" {
		return
	}
	name := filepath.Base(filepath.Dir(commonDir))
	if known[name] == nil {
		known[name] = map[string]bool{}
	}
	known[name][filepath.Clean(commonDir)] = true
}

func repoProjectPath(commonDir string) string {
	if filepath.Base(commonDir) == ".git" {
		return filepath.Dir(commonDir)
	}
	return commonDir
}

func profileRepository(record *stats.Record) string {
	if record.Profile == "" {
		return ""
	}
	if record.Task != "" {
		suffix := ":" + record.Task
		if at := strings.LastIndex(record.Profile, suffix); at > 0 {
			tail := record.Profile[at+len(suffix):]
			if tail == "" || strings.HasPrefix(tail, ":") {
				return record.Profile[:at]
			}
		}
	}
	name, _, _ := strings.Cut(record.Profile, ":")
	return name
}

// gitCommonDir reads Git metadata files directly, without starting one Git
// process for every historical project path.
func gitCommonDir(root string, cache map[string]string) (string, error) {
	if root == "" {
		return "", os.ErrNotExist
	}
	dotGit := filepath.Join(root, ".git")
	info, err := os.Stat(dotGit)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", dotGit, err)
	}
	common := dotGit
	if !info.IsDir() {
		data, err := os.ReadFile(dotGit)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", dotGit, err)
		}
		line := strings.TrimSpace(string(data))
		gitDir, ok := strings.CutPrefix(line, "gitdir: ")
		if !ok || gitDir == "" {
			return "", fmt.Errorf("invalid gitdir file %s", dotGit)
		}
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(root, gitDir)
		}
		common = gitDir
		if data, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
			common = strings.TrimSpace(string(data))
			if common == "" {
				return "", fmt.Errorf("empty commondir file in %s", gitDir)
			}
			if !filepath.IsAbs(common) {
				common = filepath.Join(gitDir, common)
			}
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("read commondir in %s: %w", gitDir, err)
		}
	}
	info, err = os.Stat(common)
	if err != nil {
		return "", fmt.Errorf("stat common Git directory %s: %w", common, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("common Git directory %s is not a directory", common)
	}
	absolute, err := filepath.Abs(common)
	if err != nil {
		return "", fmt.Errorf("absolute Git common dir for %s: %w", root, err)
	}
	return resolveStatsCommonDir(filepath.Clean(absolute), cache)
}
