package app

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/stats"
)

func statsRepoFixture(t *testing.T, root string) string {
	t.Helper()
	common := filepath.Join(root, ".git")
	if err := os.MkdirAll(common, 0o755); err != nil {
		t.Fatal(err)
	}
	return common
}

func statsTask(project, run, profile, repo string) stats.Record {
	return stats.Record{Kind: stats.KindTask, Project: project, Run: run, Profile: profile, Repo: repo,
		Task: "build", Top: "build", WallS: 4, Exit: 0}
}

func TestStatsRepoGroupsDeletedProfileCopiesToOneLiveRepository(t *testing.T) {
	base := t.TempDir()
	main := filepath.Join(base, "main", "projectA")
	common := statsRepoFixture(t, main)
	missingOne, missingTwo := filepath.Join(base, "gone", "one"), filepath.Join(base, "gone", "two")
	records := []stats.Record{
		statsTask(main, "main", "projectA:build", ""),
		statsTask(missingOne, "old-one", "projectA:build", ""),
		statsTask(missingTwo, "old-two", "projectA:build", ""),
	}
	rows := testSummarizeStatsRows(t, records)
	if len(rows) != 1 || rows[0].Runs != 3 {
		t.Fatalf("main and deleted copies did not combine: %+v", rows)
	}
	groups, err := statsRepoGroups(records)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Project != main || rows[0].LimitProject != main || groups[main].key != "git:"+common {
		t.Fatalf("group does not use primary checkout and common dir: %+v", rows[0])
	}
}

func TestStatsRepoGroupsKeepAmbiguousDeletedProfileSeparate(t *testing.T) {
	base := t.TempDir()
	mainA := filepath.Join(base, "a", "projectA")
	mainB := filepath.Join(base, "b", "projectA")
	statsRepoFixture(t, mainA)
	statsRepoFixture(t, mainB)
	deleted := filepath.Join(base, "deleted", "projectA")
	rows := testSummarizeStatsRows(t, []stats.Record{
		statsTask(mainA, "a", "projectA:build", ""),
		statsTask(mainB, "b", "projectA:build", ""),
		statsTask(deleted, "gone", "projectA:build", ""),
	})
	if len(rows) != 3 {
		t.Fatalf("two live repositories and ambiguous deleted path should remain separate: %+v", rows)
	}
	projects := map[string]bool{}
	for _, row := range rows {
		projects[row.Project] = true
	}
	for _, want := range []string{mainA, mainB, "projectA (?)"} {
		if !projects[want] {
			t.Fatalf("missing project %q in rows %+v", want, rows)
		}
	}
}

func TestStatsRepoGroupsUseRecordedCommonDirForSameNamedRepositories(t *testing.T) {
	base := t.TempDir()
	commonA := filepath.Join(base, "a", "projectA", ".git")
	commonB := filepath.Join(base, "b", "projectA", ".git")
	rows := testSummarizeStatsRows(t, []stats.Record{
		statsTask(filepath.Join(base, "gone-a"), "a", "projectA:build", commonA),
		statsTask(filepath.Join(base, "gone-b"), "b", "projectA:build", commonB),
	})
	if len(rows) != 2 {
		t.Fatalf("different recorded repositories merged: %+v", rows)
	}
	want := map[string]bool{filepath.Dir(commonA): false, filepath.Dir(commonB): false}
	for _, row := range rows {
		if _, ok := want[row.Project]; !ok {
			t.Fatalf("PROJECT is not the primary checkout path: got %q want one of %+v", row.Project, want)
		}
		want[row.Project] = true
		if row.LimitProject != row.Project {
			t.Fatalf("limit lookup %q differs from PROJECT %q", row.LimitProject, row.Project)
		}
	}
	for project, seen := range want {
		if !seen {
			t.Fatalf("missing primary project %s in %+v", project, rows)
		}
	}
}

func TestStatsRepoGroupsUseEachRecordsRecordedRepository(t *testing.T) {
	project := filepath.Join(t.TempDir(), "same-project-path")
	commonA := filepath.Join(t.TempDir(), "a", "projectA", ".git")
	commonB := filepath.Join(t.TempDir(), "b", "projectA", ".git")
	rows := testSummarizeStatsRows(t, []stats.Record{
		statsTask(project, "a", "projectA:build", commonA),
		statsTask(project, "b", "projectA:build", commonB),
	})
	if len(rows) != 2 {
		t.Fatalf("records with distinct repository identities were grouped by their shared path: %+v", rows)
	}
	want := map[string]bool{filepath.Dir(commonA): false, filepath.Dir(commonB): false}
	for _, row := range rows {
		if _, ok := want[row.Project]; !ok {
			t.Fatalf("unexpected repository label %q: %+v", row.Project, rows)
		}
		want[row.Project] = true
	}
	for project, seen := range want {
		if !seen {
			t.Errorf("missing repository label %q", project)
		}
	}
}

func TestStatsRepoGroupsResolveSymlinkedRecordedAndLiveCommonDirs(t *testing.T) {
	base := t.TempDir()
	main := filepath.Join(base, "projectA")
	common := statsRepoFixture(t, main)
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(main, alias); err != nil {
		t.Fatal(err)
	}
	rows := testSummarizeStatsRows(t, []stats.Record{
		statsTask(main, "recorded", "projectA:build", common),
		statsTask(alias, "live", "projectA:build", ""),
	})
	if len(rows) != 1 || rows[0].Runs != 2 || rows[0].Project != main {
		t.Fatalf("symlinked paths did not resolve to one repository: %+v", rows)
	}
}

func TestStatsRepoGroupsResolveDeletedGraftWorktreeWithoutProfile(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "user-cache")
	t.Setenv("XDG_CACHE_HOME", cache)
	worktreeRoot := graftWorktreeRoot(cache)
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(t.TempDir(), "projectA")
	statsRepoFixture(t, main)
	deleted := filepath.Join(worktreeRoot, "projectA", "gone-branch")
	rows := testSummarizeStatsRows(t, []stats.Record{
		statsTask(main, "main", "projectA:build", ""),
		statsTask(deleted, "old", "", ""),
	})
	if len(rows) != 1 || rows[0].Runs != 2 || rows[0].Project != main {
		t.Fatalf("deleted graft worktree did not resolve by its repository directory: %+v", rows)
	}
}

func TestStatsRepoGroupsIgnorePrunedGitdirAndContinueWithProfile(t *testing.T) {
	for _, missing := range []string{"gitdir", "commondir"} {
		t.Run(missing, func(t *testing.T) {
			cache := filepath.Join(t.TempDir(), "cache")
			t.Setenv("XDG_CACHE_HOME", cache)
			worktreeRoot := graftWorktreeRoot(cache)
			main := filepath.Join(t.TempDir(), "projectA")
			statsRepoFixture(t, main)
			stale := filepath.Join(worktreeRoot, "projectA", "branch")
			if err := os.MkdirAll(stale, 0o755); err != nil {
				t.Fatal(err)
			}
			gitDir := filepath.Join(t.TempDir(), "pruned-gitdir")
			if missing == "commondir" {
				if err := os.MkdirAll(gitDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte(filepath.Join(t.TempDir(), "gone-common")), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(stale, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			rows := testSummarizeStatsRows(t, []stats.Record{
				statsTask(main, "main", "projectA:build", ""),
				statsTask(stale, "pruned", "projectA:build", ""),
			})
			if len(rows) != 1 || rows[0].Runs != 2 || rows[0].Project != main {
				t.Fatalf("pruned Git metadata prevented the Profile match: %+v", rows)
			}
		})
	}
}

func TestStatsRepoUnresolvedLimitRootIsDeterministic(t *testing.T) {
	base := t.TempDir()
	pathA := filepath.Join(base, "a")
	pathB := filepath.Join(base, "b")
	rows := testSummarizeStatsRows(t, []stats.Record{
		statsTask(pathB, "b", "unknown:build", ""),
		statsTask(pathA, "a", "unknown:build", ""),
	})
	if len(rows) != 1 || rows[0].Project != "unknown (?)" || rows[0].LimitProject != pathA {
		t.Fatalf("unresolved profile group selected a nondeterministic limit path: %+v", rows)
	}
}

func TestStatsRepoCollapsedRowUsesLimitProject(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "user-config"))
	if err := os.WriteFile(filepath.Join(root, ".graft.yaml"), []byte("schema: 1\nversion: {mode: none}\nlimits:\n  task: 1ms\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := statsTask(root, "run", "unknown:build", "")
	record.UserS = 3
	rows := testSummarizeStatsRows(t, []stats.Record{record})
	if len(rows) != 1 || rows[0].Project != "unknown (?)" || rows[0].LimitProject != root {
		t.Fatalf("collapsed row has the wrong display or limit path: %+v", rows)
	}
	var stderr bytes.Buffer
	over, err := (&App{Stderr: &stderr}).overLimits(rows, stats.Filter{}, StatsOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("project limit config was not read: %s", stderr.String())
	}
	if len(over) != 1 || over[0].Limit != time.Millisecond || over[0].Key != "project .graft.yaml limits.task" {
		t.Fatalf("project limit was not applied through LimitProject: %+v", over)
	}
}

func TestStatsRepoGroupsPreferRepoAcrossProfilelessRecordsAtSamePath(t *testing.T) {
	base := t.TempDir()
	main := filepath.Join(base, "main", "projectA")
	common := filepath.Join(main, ".git")
	missing := filepath.Join(base, "gone", "same-path")
	records := []stats.Record{
		statsTask(missing, "old", "", ""),
		statsTask(missing, "new", "projectA:build", common),
	}
	rows := testSummarizeStatsRows(t, records)
	if len(rows) != 1 || rows[0].Runs != 2 || rows[0].Project != main || rows[0].LimitProject != main {
		t.Fatalf("same-path records were split or lost the repo's primary path: %+v", rows)
	}
}

func TestStatsRepoGroupsFallbackByPathWithoutIdentity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plain")
	groups, err := statsRepoGroups([]stats.Record{statsTask(root, "one", "", "")})
	if err != nil {
		t.Fatal(err)
	}
	want := statsRepoGroup{key: "path:" + root, name: root, limitRoot: root}
	if got := groups[root]; got != want {
		t.Fatalf("fallback group = %+v, want %+v", got, want)
	}
}

func testSummarizeStatsRows(t *testing.T, records []stats.Record) []stats.Row {
	t.Helper()
	rows, _, err := summarizeStatsRows(records, stats.Filter{}, true)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestRepoProjectPathForNonDotGitCommonDirectory(t *testing.T) {
	common := filepath.Join(t.TempDir(), "metadata")
	if got := repoProjectPath(common); got != common {
		t.Fatalf("non-.git common dir path = %q, want %q", got, common)
	}
	if got, want := repoProjectPath(fmt.Sprintf("%s/.git", common)), common; got != want {
		t.Fatalf(".git common dir path = %q, want %q", got, want)
	}
}
