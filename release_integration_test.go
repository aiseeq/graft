package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// releaseConfig has a test target and a prod target with Monday and Thursday
// windows in UTC+3, a cutoff at 18:00 the day before, items in Ready for
// release or Done going out, and hotfix branches from what prod runs.
func releaseConfig(state string, q func(string) string) string {
	return `schema: 1
version: {mode: file, build_branches: ['hotfix/*', 'release/*']}
envs:
  local: {}
deploy:
  remote: origin
  hotfix: {branches: ['hotfix/*'], label: hotfix, base: prod}
  targets:
    test:
      env: local
      run: [sh, deploy.sh]
      version: 'cat ` + filepath.Join(state, "test-version") + `'
      deployed_sha: {path: ` + q("test-sha") + `}
      release_notes: {transitions: [{from: [In Progress], to: Testing, assign: qa}]}
    prod:
      env: local
      run: [sh, deploy.sh]
      requires: test
      version: 'cat ` + filepath.Join(state, "prod-version") + `'
      deployed_sha: {path: ` + q("prod-sha") + `}
      window: {days: [mon, thu], at: "15:00", until: "19:00", tz: Etc/GMT-3, cutoff: {days_before: 1, at: "18:00"}}
      approved_release: {statuses: [Ready for release, Done], ignore_paths: ["docs/**"]}
      release_notes: {transitions: [{from: [Ready for release], to: Done}, {from: [In Progress], to: Testing, assign: qa}]}
release_notes:
  jira:
    project_keys: [PROJ]
`
}

// at sets the time graft judges windows by: 2026-10-19 is a Monday.
func (d *deployFixture) at(when string) {
	d.env = append(d.env, "GRAFT_NOW="+strings.Replace(when, " ", "T", 1)+":00+03:00")
}

func (d *deployFixture) setStatus(key, status string) {
	d.jira.mu.Lock()
	defer d.jira.mu.Unlock()
	d.jira.statuses[key] = status
}

func (d *deployFixture) status(key string) string {
	d.jira.mu.Lock()
	defer d.jira.mu.Unlock()
	return d.jira.statuses[key]
}

func (d *deployFixture) commitFile(name, msg string) string {
	d.write(name, msg+"\n")
	// The build branches of the deploy fixtures.
	branch := d.git("branch", "--show-current")
	d.mustVersionedCommit(d.work, msg, strings.HasPrefix(branch, "hotfix/") || strings.HasPrefix(branch, "release/"))
	return d.git("rev-parse", "HEAD")
}

func (d *deployFixture) plainCommitFile(name, msg string) string {
	d.write(name, msg+"\n")
	d.mustPlainCommit(msg)
	d.git("push", "-q", "origin", "HEAD")
	return d.git("rev-parse", "HEAD")
}

// runCommands runs the lines of out between the line containing from and the
// line containing to that start with git or graft, as a person would paste
// them.
func (d *deployFixture) runCommands(out, from, to string) {
	d.t.Helper()
	on := false
	for line := range strings.SplitSeq(out, "\n") {
		switch {
		case strings.Contains(line, from):
			on = true
		case strings.Contains(line, to):
			on = false
		}
		cmd := strings.TrimSpace(line)
		if !on || !(strings.HasPrefix(cmd, "git ") || strings.HasPrefix(cmd, "graft ")) {
			continue
		}
		if got, code := d.exec(d.work, "", "sh", "-c", cmd); code != 0 {
			d.t.Fatalf("%s: exit %d\n%s", cmd, code, got)
		}
	}
}

func releaseRepo(t *testing.T) *deployFixture {
	d := newDeployFixture(t, releaseConfig)
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-8"] = "Ready for release"
	d.jira.statuses["PROJ-9"] = "In Progress"
	d.jira.mu.Unlock()
	d.at("2026-10-16 10:00")
	d.mustGraft("deploy", "test")
	out, code := d.graft("", "deploy", "prod")
	if code == 0 || !strings.Contains(out, "outside the release window (mon, thu 15:00-19:00 Etc/GMT-3); the next one opens Mon 19.10 15:00; a hotfix branch (hotfix/*) goes any time") {
		t.Fatalf("prod outside the window: exit %d\n%s", code, out)
	}
	d.at("2026-10-19 15:30")
	d.mustGraft("deploy", "--unapproved", "prod")
	return d
}

// TestReleaseCutoffHoldAndWithheld follows a release: the cutoff fixes the
// candidate on test, its unchecked work is reverted on a release branch,
// prod takes the branch in the window, and the trunk keeps the work, which
// the next release checks again.
func TestReleaseCutoffHoldAndWithheld(t *testing.T) {
	d := releaseRepo(t)
	trunk := d.git("branch", "--show-current")

	d.at("2026-10-20 10:00")
	d.commitFile("checked.txt", "feat: PROJ-8 checked work")
	unchecked := d.commitFile("unchecked.txt", "feat: PROJ-1 unchecked work")
	d.write("docs/notes.md", "n\n")
	d.mustFixtureCommit("docs: notes")
	d.mustGraft("deploy", "test")
	candidate := d.git("rev-parse", "HEAD")

	// After the cutoff test holds the candidate; work goes on in the trunk.
	d.at("2026-10-21 18:30")
	d.commitFile("more.txt", "feat: PROJ-1 more work")
	out, code := d.graft("", "deploy", "test")
	if code == 0 || !strings.Contains(out, "test holds the release candidate "+candidate[:12]+" of the prod 22.10.26 window") || !strings.Contains(out, "feat: PROJ-1 more work") {
		t.Fatalf("test took new work during the hold: exit %d\n%s", code, out)
	}

	out = d.mustGraft("deploy", "cutoff", "prod")
	for _, want := range []string{
		"the cutoff was Wed 21.10 18:00, test holds the candidate",
		"git switch -c release/22.10.26 " + candidate,
		"# PROJ-1 is Testing, not Ready for release or Done",
		"git diff --binary --no-renames --no-color --no-ext-diff --no-textconv --src-prefix=a/ --dst-prefix=b/ " + unchecked + "^ " + unchecked + " -- . ':(exclude,literal)VERSION' | git apply -R --3way --index",
		"graft commit -m 'revert: PROJ-1 not checked by the 22.10.26 cutoff' -m 'This reverts commit " + unchecked + ".'",
		"git merge -s ours --no-edit release/22.10.26",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cutoff: no %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "PROJ-8 checked work") || strings.Contains(out, "notes: names no item") {
		t.Errorf("cutoff lists commits that pass:\n%s", out)
	}
	d.runCommands(out, "goes out from release/22.10.26", "Then the candidate goes")
	if v := d.read("VERSION"); !strings.HasSuffix(strings.TrimSpace(v), ".1") {
		t.Errorf("the release branch build: VERSION %q", v)
	}
	if _, err := os.Stat(filepath.Join(d.work, "unchecked.txt")); err == nil {
		t.Error("the revert left unchecked.txt")
	}
	out = d.mustGraft("deploy", "test")
	if !strings.Contains(out, "takes only its reverts and hotfixes") {
		t.Errorf("the reverted candidate:\n%s", out)
	}

	d.at("2026-10-22 15:30")
	out = d.mustGraft("deploy", "prod")
	if !strings.Contains(out, "feat: PROJ-1 unchecked work: reverted by") || !strings.Contains(out, "1 commits withheld") {
		t.Errorf("the release from the branch:\n%s", out)
	}
	if got := strings.TrimSpace(d.stateFile("prod-sha.withheld")); got != unchecked {
		t.Errorf("withheld = %q, want %s", got, unchecked)
	}
	if d.status("PROJ-8") != "Done" || d.status("PROJ-1") != "Testing" {
		t.Errorf("PROJ-8 %s, PROJ-1 %s", d.status("PROJ-8"), d.status("PROJ-1"))
	}
	comments, _ := d.jira.snapshot()
	if slices.ContainsFunc(comments["PROJ-1"], func(c string) bool { return strings.Contains(c, "prod") }) {
		t.Errorf("PROJ-1 got a prod note for withheld work: %v", comments["PROJ-1"])
	}

	// Back on the trunk: the release branch is recorded, its reverts are not.
	d.git("switch", "-q", trunk)
	d.git("-c", "core.hooksPath=/dev/null", "merge", "-q", "-s", "ours", "--no-edit", "release/22.10.26")
	d.git("push", "-q", "origin", trunk)
	if _, err := os.Stat(filepath.Join(d.work, "unchecked.txt")); err != nil {
		t.Error("the trunk lost the withheld work")
	}
	d.at("2026-10-23 10:00")
	d.mustGraft("deploy", "test")

	d.at("2026-10-26 15:30")
	out, code = d.graft("", "deploy", "prod")
	if code == 0 || !strings.Contains(out, "feat: PROJ-1 unchecked work: withheld from an earlier release, goes out now") || !strings.Contains(out, "feat: PROJ-1 unchecked work: PROJ-1 is Testing") {
		t.Fatalf("the withheld commit is not checked: exit %d\n%s", code, out)
	}
	d.setStatus("PROJ-1", "Ready for release")
	d.mustGraft("deploy", "prod")
	if got := strings.TrimSpace(d.stateFile("prod-sha.withheld")); got != "" {
		t.Errorf("withheld after it passed = %q", got)
	}
}

func cutoffMigrationRepo(t *testing.T, configureMigrations bool) *deployFixture {
	t.Helper()
	if !configureMigrations {
		return releaseRepo(t)
	}
	return cutoffMigrationRepoWithPath(t, "migrations", true)
}

func cutoffMigrationRepoWithPath(t *testing.T, migrations string, configureTestDB bool) *deployFixture {
	t.Helper()
	if !configureTestDB {
		return releaseRepo(t)
	}
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := "test_db:\n" +
			"  image: postgres:16-alpine\n" +
			"  container: projecta-test\n" +
			"  port: 5432\n" +
			"  database: projecta_test\n" +
			"  user: projecta\n" +
			"  password: test\n"
		if migrations != "" {
			cfg += "  migrations: " + migrations + "\n"
		}
		return cfg + releaseConfig(state, q)
	})
	d.setStatus("PROJ-8", "Ready for release")
	d.setStatus("PROJ-9", "In Progress")
	d.at("2026-10-16 10:00")
	d.mustGraft("deploy", "test")
	out, code := d.graft("", "deploy", "prod")
	if code == 0 || !strings.Contains(out, "outside the release window") {
		t.Fatalf("prod outside the window: exit %d\n%s", code, out)
	}
	d.at("2026-10-19 15:30")
	d.mustGraft("deploy", "--unapproved", "prod")
	return d
}

func cutoffMigrationCommits(d *deployFixture, unchecked map[int]bool, numbers ...int) map[int]string {
	return cutoffMigrationCommitsAt(d, "migrations", unchecked, numbers...)
}

func cutoffMigrationCommitsAt(d *deployFixture, directory string, unchecked map[int]bool, numbers ...int) map[int]string {
	d.t.Helper()
	commits := make(map[int]string, len(numbers))
	for _, number := range numbers {
		key := "PROJ-8"
		if unchecked[number] {
			key = "PROJ-1"
		}
		name := filepath.Join(directory, fmt.Sprintf("%06d_step.up.sql", number))
		commits[number] = d.commitFile(name, fmt.Sprintf("feat: %s migration %d", key, number))
	}
	return commits
}

func cutoffOutput(t *testing.T, d *deployFixture) string {
	t.Helper()
	d.mustGraft("deploy", "test")
	d.at("2026-10-21 18:30")
	return d.mustGraft("deploy", "cutoff", "prod")
}

func TestReleaseCutoffMigrationConflict(t *testing.T) {
	d := cutoffMigrationRepo(t, true)
	d.at("2026-10-20 10:00")
	commits := cutoffMigrationCommits(d, map[int]bool{}, 1)
	d.write("app.txt", "PROJ-1 change\n")
	d.write("migrations/000002_step.up.sql", "migration 2\n")
	d.write("migrations/000002_step.down.sql", "rollback 2\n")
	d.mustFixtureCommit("feat: PROJ-1 migration 2")
	commits[2] = d.git("rev-parse", "HEAD")
	cutoffMigrationCommits(d, map[int]bool{}, 3, 4)
	// Migration 4 stays in the release; the unchecked commit edits it and adds 5.
	d.write("app.txt", "PROJ-1 second change\n")
	d.write(filepath.Join("migrations", "000004_step.up.sql"), "-- edited by unchecked task\n")
	d.write(filepath.Join("migrations", "000005_step.up.sql"), "migration 5\n")
	d.mustFixtureCommit("feat: PROJ-1 migration 5")
	out := cutoffOutput(t, d)
	if !strings.Contains(out, "migrations/000002_step.up.sql (number 2); highest remaining migration number 4") {
		t.Fatalf("cutoff did not explain migration 2's conflict:\n%s", out)
	}
	if !strings.Contains(out, "migrator applies only versions above the last applied version") || !strings.Contains(out, "skipped on the next release") {
		t.Errorf("cutoff omitted the migration risk:\n%s", out)
	}
	plainRevert2 := "git diff --binary --no-renames --no-color --no-ext-diff --no-textconv --src-prefix=a/ --dst-prefix=b/ " + commits[2] + "^ " + commits[2] + " -- . ':(exclude,literal)VERSION' | git apply -R --3way --index"
	if strings.Contains(out, plainRevert2) {
		t.Errorf("cutoff printed an unexcluded reverse patch for migration 2:\n%s", out)
	}
	wantExclude := "git diff --binary --no-renames --no-color --no-ext-diff --no-textconv --src-prefix=a/ --dst-prefix=b/ " + commits[2] + "^ " + commits[2] + " -- . ':(exclude,literal)VERSION' ':(exclude,literal)migrations/000002_step.down.sql' ':(exclude,literal)migrations/000002_step.up.sql' | git apply -R --3way --index"
	if !strings.Contains(out, wantExclude) {
		t.Errorf("cutoff omitted the ready-to-run exclude command %q:\n%s", wantExclude, out)
	}
	// The edit of migration 4, the highest staying, is lost the same way:
	// prod runs the old 4 now and never the edited one. Removing migration
	// 5 loses nothing.
	if !strings.Contains(out, "migrations/000004_step.up.sql (number 4); highest remaining migration number 4") {
		t.Errorf("cutoff did not flag reverting the edit of the highest migration 4:\n%s", out)
	}
	if strings.Count(out, "Migration cutoff conflict:") != 2 {
		t.Errorf("cutoff flagged the removed migration 5 too, or missed one:\n%s", out)
	}
	for _, option := range []string{"Keep the migration", "revert and renumber", "can be kept whole instead of reverted; skip its item: its git diff lines and its graft commit.", "#   PROJ-1: the other items' reverts stay as below.\n", "  # A migration conflict below; keeping the whole task instead is weighed above.\n"} {
		if !strings.Contains(out, option) {
			t.Errorf("cutoff omitted option %q:\n%s", option, out)
		}
	}
	// Keeping the task is said before any of its reverts: one applied
	// already would have to be undone.
	if keep, first := strings.Index(out, "can be kept whole"), strings.Index(out, "| git apply -R"); keep < 0 || keep > first {
		t.Errorf("the option to keep the task comes after a revert of it:\n%s", out)
	}
	assertCutoffConflictBlockComments(t, out)
	assertCutoffShellSyntax(t, out)
}

func TestReleaseCutoffHigherMigrationHasNoConflict(t *testing.T) {
	d := cutoffMigrationRepo(t, true)
	d.at("2026-10-20 10:00")
	cutoffMigrationCommits(d, map[int]bool{}, 1, 2, 3, 4)
	d.commitFile("migrations/000005_step.up.sql", "feat: PROJ-1 migration 5")
	out := cutoffOutput(t, d)
	if strings.Contains(out, "Migration cutoff conflict") {
		t.Fatalf("migration 5 conflicts with migrations 1..4:\n%s", out)
	}
	if !strings.Contains(out, "git diff ") || !strings.Contains(out, "| git apply -R --3way --index") {
		t.Fatalf("cutoff omitted the usual reverse patch:\n%s", out)
	}
	if strings.Contains(out, "kept whole") || strings.Contains(out, "keeping the whole task") {
		t.Errorf("cutoff offered keeping a task without a migration conflict:\n%s", out)
	}
}

// TestReleaseCutoffAddedFileAtTheHighestNumberHasNoConflict: removing a file
// added at the highest number that stays (a down migration beside the up one
// staying) loses no version: only an edit at that number is flagged.
func TestReleaseCutoffAddedFileAtTheHighestNumberHasNoConflict(t *testing.T) {
	d := cutoffMigrationRepo(t, true)
	d.at("2026-10-20 10:00")
	cutoffMigrationCommits(d, map[int]bool{}, 1, 2, 3, 4)
	d.commitFile("migrations/000004_step.down.sql", "feat: PROJ-1 down migration 4")
	d.write(filepath.Join("migrations", "000004_step.down.sql"), "-- revised rollback\n")
	d.mustFixtureCommit("feat: PROJ-1 revise down migration 4")
	out := cutoffOutput(t, d)
	if strings.Contains(out, "Migration cutoff conflict") {
		t.Fatalf("removing an added file at the highest number 4 was flagged:\n%s", out)
	}
}

func TestReleaseCutoffRenumberAndAddThenRemoveHaveNoConflict(t *testing.T) {
	t.Run("renumber above the cutoff", func(t *testing.T) {
		d := cutoffMigrationRepo(t, true)
		d.at("2026-10-20 10:00")
		cutoffMigrationCommits(d, map[int]bool{}, 1, 2, 3, 4)
		d.git("mv", "migrations/000003_step.up.sql", "migrations/000005_step.up.sql")
		d.mustFixtureCommit("feat: PROJ-1 renumber migration 3 to 5")
		out := cutoffOutput(t, d)
		if strings.Contains(out, "Migration cutoff conflict") {
			t.Fatalf("renumbering from 3 to 5 lost a migration number:\n%s", out)
		}
	})
	t.Run("add then remove", func(t *testing.T) {
		d := cutoffMigrationRepo(t, true)
		d.at("2026-10-20 10:00")
		cutoffMigrationCommits(d, map[int]bool{}, 1, 2, 4)
		d.commitFile("migrations/000003_step.up.sql", "feat: PROJ-1 add migration 3")
		if err := os.Remove(filepath.Join(d.work, "migrations", "000003_step.up.sql")); err != nil {
			t.Fatal(err)
		}
		d.mustFixtureCommit("feat: PROJ-1 remove migration 3")
		out := cutoffOutput(t, d)
		if strings.Contains(out, "Migration cutoff conflict") {
			t.Fatalf("adding and then removing migration 3 changed the release tree:\n%s", out)
		}
	})
}

func TestReleaseCutoffReportsBothTasksAgainstRestoredHighest(t *testing.T) {
	d := cutoffMigrationRepo(t, true)
	d.at("2026-10-20 10:00")
	cutoffMigrationCommits(d, map[int]bool{}, 1, 3)
	d.git("mv", "migrations/000003_step.up.sql", "migrations/000003_renamed.up.sql")
	d.write(filepath.Join("migrations", "000003_renamed.up.sql"), "feat: PROJ-8 migration 3\n-- revised\n")
	d.mustFixtureCommit("feat: PROJ-9 rename and revise migration 3")
	d.commitFile("migrations/000002_step.up.sql", "feat: PROJ-1 migration 2")
	out := cutoffOutput(t, d)
	for _, want := range []string{
		"migrations/000002_step.up.sql (number 2); highest remaining migration number 3",
		"migrations/000003_renamed.up.sql (number 3), migrations/000003_step.up.sql (number 3); highest remaining migration number 3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cutoff omitted conflict %q:\n%s", want, out)
		}
	}
}

func TestReleaseCutoffKeepTaskCanHaveUnavailableTreeCheck(t *testing.T) {
	d := cutoffMigrationRepo(t, true)
	d.at("2026-10-20 10:00")
	cutoffMigrationCommits(d, map[int]bool{}, 1, 2, 3, 4, 5, 6)
	d.write(filepath.Join("migrations", "000007_step.up.sql"), "feat: PROJ-1 migration 7\n")
	d.mustFixtureCommit("feat: PROJ-1 add migration 7")
	d.write(filepath.Join("migrations", "000007_step.up.sql"), "feat: PROJ-9 revised migration 7\n")
	d.mustFixtureCommit("feat: PROJ-9 edit migration 7")
	d.write(filepath.Join("migrations", "000002_step.up.sql"), "feat: PROJ-9 revised migration 2\n")
	d.mustFixtureCommit("feat: PROJ-9 edit migration 2")
	out := cutoffOutput(t, d)
	if strings.Contains(out, "migration check unavailable if kept whole") || (!strings.Contains(out, "Migration cutoff conflict") && !strings.Contains(out, "stays in the release")) {
		t.Fatalf("cutoff did not show the resolved migration paths when keeping PROJ-9 whole:\n%s", out)
	}
}

// TestReleaseCutoffKeepingATaskNamesRenamesAndPathsOnce: the migrations that
// keeping PROJ-1 puts below its 7 are named by every path their revert
// touches, a rename by both, each path once.
func TestReleaseCutoffKeepingATaskNamesRenamesAndPathsOnce(t *testing.T) {
	d := cutoffMigrationRepo(t, true)
	d.at("2026-10-20 10:00")
	cutoffMigrationCommits(d, map[int]bool{2: true, 7: true}, 1, 2, 3, 4, 7)
	d.git("mv", "migrations/000004_step.up.sql", "migrations/000004_renamed.up.sql")
	d.mustFixtureCommit("feat: PROJ-9 rename migration 4")
	d.commitFile("migrations/000005_step.up.sql", "feat: PROJ-9 migration 5")
	d.write(filepath.Join("migrations", "000005_step.up.sql"), "-- revised by PROJ-9\n")
	d.mustFixtureCommit("feat: PROJ-9 revise migration 5")
	out := cutoffOutput(t, d)
	start := strings.Index(out, "#   PROJ-1: its migrations raise the highest remaining number to 7, so also exclude from the other items' reverts ")
	if start < 0 {
		t.Fatalf("no keep option for PROJ-1 raising to 7:\n%s", out)
	}
	line := out[start : start+strings.Index(out[start:], "\n")]
	for _, path := range []string{"migrations/000005_step.up.sql (number 5, PROJ-9)"} {
		if n := strings.Count(line, path); n != 1 {
			t.Errorf("keep option names %q %d times: %s", path, n, line)
		}
	}
}

// TestReleaseCutoffKeepingATaskNamesAnEditOfItsHighest: kept whole, PROJ-1
// keeps its migration 7 as the highest, so reverting PROJ-9's edit of 7 loses
// that edit for good; the option names it.
func TestReleaseCutoffKeepingATaskNamesAnEditOfItsHighest(t *testing.T) {
	d := cutoffMigrationRepo(t, true)
	d.at("2026-10-20 10:00")
	cutoffMigrationCommits(d, map[int]bool{2: true, 7: true}, 1, 2, 3, 7)
	d.write(filepath.Join("migrations", "000007_extra.up.sql"), "-- PROJ-9 migration 7\n")
	d.mustFixtureCommit("feat: PROJ-9 add migration 7 file")
	d.write(filepath.Join("migrations", "000007_step.up.sql"), "-- revised by PROJ-1\n")
	d.mustFixtureCommit("feat: PROJ-1 revise migration 7")
	out := cutoffOutput(t, d)
	want := "#   PROJ-1: its migrations raise the highest remaining number to 7, so also exclude from the other items' reverts migrations/000007_extra.up.sql (number 7, PROJ-9).\n"
	if !strings.Contains(out, want) {
		t.Errorf("keeping PROJ-1 did not name PROJ-9's edit of 7 (want %q):\n%s", want, out)
	}
	lineStart := strings.Index(out, "#   PROJ-1: its migrations raise")
	line := out[lineStart : lineStart+strings.IndexByte(out[lineStart:], 10)]
	if strings.Contains(line, "number 7, PROJ-1") {
		t.Errorf("keep option lists the kept task's migration: %s", line)
	}

}

// TestReleaseCutoffKeepingATaskNamesTheRevertsItPutsBelow: kept whole, a task
// keeps its own higher migration, so another task's reverted migration below
// it would be skipped too; the option says which, and only for that task.
func TestReleaseCutoffKeepingATaskNamesTheRevertsItPutsBelow(t *testing.T) {
	d := cutoffMigrationRepo(t, true)
	d.at("2026-10-20 10:00")
	// PROJ-1's own migration 5 is not another item's revert; PROJ-9's change
	// of migration 1 is a conflict already, not one that keeping PROJ-1 makes.
	cutoffMigrationCommits(d, map[int]bool{2: true, 5: true, 10: true}, 1, 2, 3, 5, 10)
	d.commitFile("migrations/000008_step.up.sql", "feat: PROJ-9 migration 8")
	d.write(filepath.Join("migrations", "000001_step.up.sql"), "-- edited by PROJ-9\n")
	d.mustFixtureCommit("feat: PROJ-9 edit migration 1")
	out := cutoffOutput(t, d)
	if !strings.Contains(out, "migrations/000002_step.up.sql (number 2); highest remaining migration number 3") {
		t.Fatalf("cutoff did not flag migration 2 below 3:\n%s", out)
	}
	for _, want := range []string{
		"#   PROJ-1: its migrations raise the highest remaining number to 10, so also exclude from the other items' reverts migrations/000008_step.up.sql (number 8, PROJ-9).\n",
		"#   PROJ-9: its migrations raise the highest remaining number to 8, so also exclude from the other items' reverts migrations/000005_step.up.sql (number 5, PROJ-1).\n",
	} {
		if strings.Count(out, want) != 1 {
			t.Errorf("keep option missing or repeated (want %q):\n%s", want, out)
		}
		// Said before every revert: one applied already would be undone.
		if keep, first := strings.Index(out, want), strings.Index(out, "| git apply -R"); keep > first {
			t.Errorf("the consequence of keeping a task comes after a revert:\n%s", out)
		}
	}
	keepLineStart := strings.Index(out, "#   PROJ-1:")
	keepLine := out[keepLineStart : keepLineStart+strings.Index(out[keepLineStart:], "\n")]
	if strings.Contains(keepLine, "000001_step.up.sql") {
		t.Errorf("keep option repeats a migration already lost when every item is reverted: %s", keepLine)
	}
	assertCutoffConflictBlockComments(t, out)
}

func TestReleaseCutoffRevertingDeletedMigrationKeepsOldOutput(t *testing.T) {
	d := cutoffMigrationRepo(t, true)
	d.at("2026-10-20 10:00")
	cutoffMigrationCommits(d, map[int]bool{}, 1, 2, 3, 4)
	if err := os.Remove(filepath.Join(d.work, "migrations", "000002_step.up.sql")); err != nil {
		t.Fatal(err)
	}
	d.mustFixtureCommit("feat: PROJ-1 remove old migration")
	deleted := d.git("rev-parse", "HEAD")
	out := cutoffOutput(t, d)
	want := "git diff --binary --no-renames --no-color --no-ext-diff --no-textconv --src-prefix=a/ --dst-prefix=b/ " + deleted + "^ " + deleted + " -- . ':(exclude,literal)VERSION' | git apply -R --3way --index"
	if !strings.Contains(out, want) || strings.Contains(out, "Migration cutoff conflict") {
		t.Fatalf("reverting a deleted migration changed cutoff output:\n%s", out)
	}
}

func TestReleaseCutoffChangedLowerMigrationConflicts(t *testing.T) {
	d := cutoffMigrationRepo(t, true)
	d.at("2026-10-20 10:00")
	cutoffMigrationCommits(d, map[int]bool{}, 1, 2, 3, 4)
	d.write(filepath.Join("migrations", "000002_step.up.sql"), "-- revised migration 2\n")
	d.write(filepath.Join("migrations", "000003_step.up.sql"), "-- revised migration 3\n")
	d.mustFixtureCommit("feat: PROJ-1 revise migrations 2 and 3")
	changed := d.git("rev-parse", "HEAD")
	out := cutoffOutput(t, d)
	for _, want := range []string{"migrations/000002_step.up.sql (number 2)", "migrations/000003_step.up.sql (number 3)", "highest remaining migration number 4", "nothing left to revert in this commit"} {
		if !strings.Contains(out, want) {
			t.Errorf("cutoff: no %q\n%s", want, out)
		}
	}
	if strings.Count(out, "Migration cutoff conflict:") != 1 {
		t.Errorf("cutoff repeated the explanation for multiple files in one commit:\n%s", out)
	}
	if strings.Contains(out, "git diff "+changed+"^ "+changed+" -- .") || strings.Contains(out, "-m 'This reverts commit "+changed) {
		t.Errorf("migration-only commit still has a reverse command or graft message:\n%s", out)
	}
}

func TestReleaseCutoffCleanRenamedMigrationHasNoConflict(t *testing.T) {
	d := cutoffMigrationRepo(t, true)
	d.at("2026-10-20 10:00")
	cutoffMigrationCommits(d, map[int]bool{}, 1, 2, 3, 4)
	oldPath := filepath.Join(d.work, "migrations", "000002_step.up.sql")
	newPath := filepath.Join(d.work, "migrations", "000002_renamed.up.sql")
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
	d.mustFixtureCommit("feat: PROJ-1 rename migration 2")
	out := cutoffOutput(t, d)
	if strings.Contains(out, "Migration cutoff conflict") {
		t.Fatalf("clean rename with unchanged contents is a migration loss:\n%s", out)
	}
}

func TestReleaseCutoffChangedRenameConflictsOnBothPaths(t *testing.T) {
	d := cutoffMigrationRepo(t, true)
	d.at("2026-10-20 10:00")
	cutoffMigrationCommits(d, map[int]bool{}, 1, 2, 3, 4)
	oldPath := filepath.Join(d.work, "migrations", "000002_step.up.sql")
	newPath := filepath.Join(d.work, "migrations", "000002_renamed.up.sql")
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
	d.write("app.txt", "keep the remaining patch\n")
	d.write(filepath.Join("migrations", "000002_renamed.up.sql"), "feat: PROJ-8 migration 2\n-- revised\n")
	d.mustFixtureCommit("feat: PROJ-1 revise migration 2 rename")
	sha := d.git("rev-parse", "HEAD")
	out := cutoffOutput(t, d)
	for _, want := range []string{
		"migrations/000002_renamed.up.sql (number 2)",
		"git diff --binary --no-renames --no-color --no-ext-diff --no-textconv --src-prefix=a/ --dst-prefix=b/ " + sha + "^ " + sha + " -- . ':(exclude,literal)VERSION' ':(exclude,literal)migrations/000002_renamed.up.sql' ':(exclude,literal)migrations/000002_step.up.sql' | git apply -R --3way --index",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cutoff: no %q\n%s", want, out)
		}
	}
}

func TestReleaseCutoffUnavailableWhenPlannedReversePatchDoesNotApply(t *testing.T) {
	d := cutoffMigrationRepo(t, true)
	d.at("2026-10-20 10:00")
	cutoffMigrationCommits(d, map[int]bool{}, 1, 2, 3, 4, 5, 6)
	d.write(filepath.Join("migrations", "000007_step.up.sql"), "feat: PROJ-1 migration 7\n")
	d.write(filepath.Join("migrations", "000007_step.down.sql"), "down migration 7\n")
	d.mustFixtureCommit("feat: PROJ-1 add migration 7")
	d.write(filepath.Join("migrations", "000007_step.down.sql"), "feat: PROJ-8 revise down migration 7\n")
	d.mustFixtureCommit("feat: PROJ-8 revise down migration 7")
	out := cutoffOutput(t, d)
	if !strings.Contains(out, "migrations/000007_step.down.sql stays in the release: its revert does not apply on the candidate") || strings.Contains(out, "migration check unavailable") {
		t.Fatalf("cutoff did not explain the migration path that stays in the release:\n%s", out)
	}
}

// A translated git still lets the cutoff tell a revert that does not apply.
func TestReleaseCutoffUnappliedRevertUnderTranslatedGit(t *testing.T) {
	d := cutoffMigrationRepo(t, true)
	d.at("2026-10-20 10:00")
	cutoffMigrationCommits(d, map[int]bool{}, 1, 2, 3, 4, 5, 6)
	d.write(filepath.Join("migrations", "000007_step.up.sql"), "feat: PROJ-1 migration 7\n")
	d.write(filepath.Join("migrations", "000007_step.down.sql"), "down migration 7\n")
	d.mustFixtureCommit("feat: PROJ-1 add migration 7")
	d.write(filepath.Join("migrations", "000007_step.down.sql"), "feat: PROJ-8 revise down migration 7\n")
	d.mustFixtureCommit("feat: PROJ-8 revise down migration 7")
	d.env = append(d.env, "LC_ALL=de_DE.UTF-8", "LANGUAGE=de")
	out := cutoffOutput(t, d)
	if !strings.Contains(out, "migrations/000007_step.down.sql stays in the release: its revert does not apply on the candidate") || strings.Contains(out, "migration check unavailable") {
		t.Fatalf("cutoff under a translated git did not explain the migration path that stays in the release:\n%s", out)
	}
}

func TestReleaseCutoffInterleavedEditsBelowDone(t *testing.T) {
	d := cutoffMigrationRepo(t, true)
	d.at("2026-10-20 10:00")
	cutoffMigrationCommits(d, map[int]bool{}, 1)
	d.commitFile("app1.txt", "feat: PROJ-1 add migration 2")
	d.write("migrations/000002_step.up.sql", "PROJ-1 v1\n")
	d.mustFixtureCommit("feat: PROJ-1 add migration 2")
	d.write("migrations/000002_step.up.sql", "PROJ-9 v2\n")
	d.mustFixtureCommit("feat: PROJ-9 edit migration 2")
	d.write("migrations/000002_step.up.sql", "PROJ-1 v3\n")
	d.mustFixtureCommit("feat: PROJ-1 edit migration 2")
	cutoffMigrationCommits(d, map[int]bool{}, 3, 4)
	candidate := d.git("rev-parse", "HEAD")
	want, err := os.ReadFile(filepath.Join(d.work, "migrations/000002_step.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	out := cutoffOutput(t, d)
	if !strings.Contains(out, "Migration cutoff conflict: migrations/000002_step.up.sql (number 2)") {
		t.Fatalf("missing migration conflict:\n%s", out)
	}
	d.runCommands(out, "goes out from release/", "Then the candidate goes")
	got, err := os.ReadFile(filepath.Join(d.work, "migrations/000002_step.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) || d.git("rev-parse", "HEAD") == candidate {
		t.Fatalf("the printed plan did not preserve migration 2")
	}
}

func TestReleaseCutoffDoneEditAfterUncheckedAdd(t *testing.T) {
	d := cutoffMigrationRepo(t, true)
	d.at("2026-10-20 10:00")
	cutoffMigrationCommits(d, map[int]bool{}, 1)
	d.write("migrations/000002_step.up.sql", "PROJ-1 v1\n")
	d.mustFixtureCommit("feat: PROJ-1 add migration 2")
	d.write("migrations/000002_step.up.sql", "PROJ-8 v2\n")
	d.mustFixtureCommit("feat: PROJ-8 fix migration 2")
	cutoffMigrationCommits(d, map[int]bool{}, 3, 4)
	want, err := os.ReadFile(filepath.Join(d.work, "migrations/000002_step.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	out := cutoffOutput(t, d)
	if !strings.Contains(out, "Migration cutoff conflict: migrations/000002_step.up.sql (number 2)") {
		t.Fatalf("missing migration conflict:\n%s", out)
	}
	d.runCommands(out, "goes out from release/", "Then the candidate goes")
	got, err := os.ReadFile(filepath.Join(d.work, "migrations/000002_step.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatal("the printed plan lost migration 2")
	}
}

func TestReleaseCutoffRootMigrations(t *testing.T) {
	d := cutoffMigrationRepoWithPath(t, ".", true)
	d.at("2026-10-20 10:00")
	cutoffMigrationCommitsAt(d, ".", map[int]bool{}, 1)
	d.write("000002_step.up.sql", "PROJ-1\n")
	d.write("000002_step.down.sql", "down\n")
	d.mustFixtureCommit("feat: PROJ-1 add migration 2")
	d.commitFile("app1.txt", "feat: PROJ-1 code")
	cutoffMigrationCommitsAt(d, ".", map[int]bool{}, 3)
	out := cutoffOutput(t, d)
	if !strings.Contains(out, "Migration cutoff conflict") || strings.Contains(out, "migration check unavailable") {
		t.Fatalf("root migration check failed:\n%s", out)
	}
}

func TestReleaseCutoffDiffNoPrefixConfig(t *testing.T) {
	d := cutoffMigrationRepo(t, true)
	d.at("2026-10-20 10:00")
	cutoffMigrationCommits(d, map[int]bool{}, 1)
	d.write("migrations/000002_step.up.sql", "feat: PROJ-1 migration 2\n")
	d.write("app1.txt", "feat: PROJ-1 code\n")
	d.mustFixtureCommit("feat: PROJ-1 migration 2")
	cutoffMigrationCommits(d, map[int]bool{}, 3)
	d.git("config", "diff.noprefix", "true")
	out := cutoffOutput(t, d)
	if !strings.Contains(out, "Migration cutoff conflict") || !strings.Contains(out, "--src-prefix=a/ --dst-prefix=b/") || strings.Contains(out, "migration check unavailable") {
		t.Fatalf("diff.noprefix changed the migration check or printed patch:\n%s", out)
	}
}

func TestReleaseCutoffComparesMigrationBlobSetSize(t *testing.T) {
	d := cutoffMigrationRepo(t, true)
	d.at("2026-10-20 10:00")
	cutoffMigrationCommits(d, map[int]bool{}, 1, 2, 3, 4)
	d.commitFile("migrations/000002_extra.up.sql", "feat: PROJ-8 extra migration 2 file")
	if err := os.Remove(filepath.Join(d.work, "migrations/000002_step.up.sql")); err != nil {
		t.Fatal(err)
	}
	d.mustFixtureCommit("feat: PROJ-1 remove migration 2 file")
	out := cutoffOutput(t, d)
	if !strings.Contains(out, "Migration cutoff conflict: migrations/000002_step.up.sql (number 2)") {
		t.Fatalf("did not compare the complete migration blob set:\n%s", out)
	}
}

func TestReleaseCutoffChecksOnlyTopLevelMigrationsAndCleansPath(t *testing.T) {
	for _, number := range []int{3, 5} {
		t.Run(fmt.Sprintf("revert-%d", number), func(t *testing.T) {
			d := cutoffMigrationRepoWithPath(t, "./db/migrations/", true)
			d.at("2026-10-20 10:00")
			cutoffMigrationCommitsAt(d, filepath.Join("db", "migrations"), map[int]bool{}, 1, 2, 3, 4)
			d.commitFile(filepath.Join("db", "migrations", "archive", "000500_archived.up.sql"), "feat: PROJ-8 archived migration")
			migration := filepath.Join("db", "migrations", fmt.Sprintf("%06d_step.up.sql", number))
			if number == 3 {
				d.write(migration, "-- changed migration 3\n")
				d.mustFixtureCommit("feat: PROJ-1 revise migration 3")
			} else {
				d.commitFile(migration, "feat: PROJ-1 migration 5")
			}
			out := cutoffOutput(t, d)
			if number == 3 {
				if !strings.Contains(out, "db/migrations/000003_step.up.sql (number 3)") || !strings.Contains(out, "highest remaining migration number 4") || !strings.Contains(out, "nothing left to revert") || strings.Contains(out, "500") {
					t.Fatalf("cutoff counted an archived migration:\n%s", out)
				}
			} else if strings.Contains(out, "Migration cutoff conflict") {
				t.Fatalf("migration 5 conflicts with top-level migrations 1..4:\n%s", out)
			}
		})
	}
}

func TestReleaseCutoffWithoutMigrationConfigKeepsOutput(t *testing.T) {
	d := cutoffMigrationRepo(t, false)
	d.at("2026-10-20 10:00")
	commits := cutoffMigrationCommits(d, map[int]bool{2: true}, 1, 2, 3, 4)
	out := cutoffOutput(t, d)
	want := "git diff --binary --no-renames --no-color --no-ext-diff --no-textconv --src-prefix=a/ --dst-prefix=b/ " + commits[2] + "^ " + commits[2] + " -- . ':(exclude,literal)VERSION' | git apply -R --3way --index"
	if !strings.Contains(out, want) || strings.Contains(out, "Migration cutoff conflict") {
		t.Fatalf("cutoff without test_db.migrations changed its output:\n%s", out)
	}
}

func TestReleaseCutoffWithTestDBButNoMigrationsKeepsOutput(t *testing.T) {
	d := cutoffMigrationRepoWithPath(t, "", true)
	d.at("2026-10-20 10:00")
	commits := cutoffMigrationCommitsAt(d, "", map[int]bool{2: true}, 1, 2, 3, 4)
	out := cutoffOutput(t, d)
	want := "git diff --binary --no-renames --no-color --no-ext-diff --no-textconv --src-prefix=a/ --dst-prefix=b/ " + commits[2] + "^ " + commits[2] + " -- . ':(exclude,literal)VERSION' | git apply -R --3way --index"
	if !strings.Contains(out, want) || strings.Contains(out, "Migration cutoff conflict") {
		t.Fatalf("cutoff with test_db but no migrations changed its output:\n%s", out)
	}
}

func TestReleaseCutoffCommitWithoutMigrationFilesKeepsRevert(t *testing.T) {
	d := cutoffMigrationRepo(t, true)
	d.at("2026-10-20 10:00")
	cutoffMigrationCommits(d, map[int]bool{}, 1, 2, 3, 4)
	d.commitFile("unchecked.txt", "feat: PROJ-1 unchecked work")
	out := cutoffOutput(t, d)
	if strings.Contains(out, "Migration cutoff conflict") || !strings.Contains(out, "git diff ") || !strings.Contains(out, "| git apply -R --3way --index") {
		t.Fatalf("non-migration commit changed cutoff output:\n%s", out)
	}
}

func assertCutoffConflictBlockComments(t *testing.T, out string) {
	t.Helper()
	inBlock := false
	sawCommit := false
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# The release goes out from ") {
			inBlock = true
		}
		if !inBlock {
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "git ") || strings.HasPrefix(line, "graft ") {
			if strings.HasPrefix(line, "graft commit ") || strings.HasPrefix(line, "graft deploy ") {
				sawCommit = true
			}
			continue
		}
		t.Errorf("cutoff block contains non-comment explanation %q", line)
	}
	if !sawCommit {
		t.Error("cutoff block has no graft command")
	}
}

func assertCutoffShellSyntax(t *testing.T, out string) {
	t.Helper()
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(out)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("cutoff plan is not valid shell syntax: %v\n%s", err, output)
	}
}

func TestWithheldRevertPersistsOnNonBuildBranch(t *testing.T) {
	d := releaseRepo(t)
	d.at("2026-10-20 10:00")
	d.commitFile("checked.txt", "feat: PROJ-8 checked work")
	unchecked := d.commitFile("unchecked.txt", "feat: PROJ-1 unchecked work")
	d.mustGraft("deploy", "test")
	d.at("2026-10-21 18:30")
	d.runCommands(d.mustGraft("deploy", "cutoff", "prod"), "goes out from release/22.10.26", "Then the candidate goes")
	d.mustGraft("deploy", "test")
	d.at("2026-10-22 15:30")
	d.mustGraft("deploy", "prod")
	if got := strings.TrimSpace(d.stateFile("prod-sha.withheld")); got != unchecked {
		t.Fatalf("initial withheld = %q, want %s", got, unchecked)
	}

	// This branch contains the release's revert but is outside build_branches.
	d.git("switch", "-q", "-c", "maintenance/PROJ-8-followup")
	d.write("followup.txt", "feat: PROJ-8 followup\n")
	d.git("-c", "core.hooksPath=/dev/null", "add", "-A")
	d.git("-c", "core.hooksPath=/dev/null", "commit", "-m", "feat: PROJ-8 followup")
	d.git("push", "-q", "-u", "origin", "maintenance/PROJ-8-followup")
	d.setStatus("PROJ-1", "Ready for release")
	d.mustGraft("deploy", "test")
	out := d.mustGraft("deploy", "prod")
	if strings.Contains(out, "withheld from an earlier release, goes out now") {
		t.Errorf("a reverted withheld commit was released on another branch:\n%s", out)
	}
	if got := strings.TrimSpace(d.stateFile("prod-sha.withheld")); got != unchecked {
		t.Errorf("withheld after deploy = %q, want %s\n%s", got, unchecked, out)
	}
	if d.status("PROJ-1") != "Ready for release" {
		t.Errorf("PROJ-1 moved to %s", d.status("PROJ-1"))
	}
}

// TestReleaseHoldOverOncePushedOnTop: once prod runs the candidate, the hold
// is over for every later deploy to test in the window, not only for the
// first one on top of the candidate; whether a deploy during the hold
// recorded the candidate or none did.
func TestReleaseHoldOverOncePushedOnTop(t *testing.T) {
	for _, recorded := range []bool{true, false} {
		t.Run(fmt.Sprintf("recorded=%v", recorded), func(t *testing.T) {
			d := releaseRepo(t)

			d.at("2026-10-20 10:00")
			d.plainCommitFile("work.txt", "feat: PROJ-8 checked work")
			d.mustGraft("deploy", "test")
			candidate := d.git("rev-parse", "HEAD")
			if recorded {
				d.at("2026-10-21 18:30")
				d.mustGraft("deploy", "--redeploy", "test")
			}

			d.at("2026-10-22 15:26")
			d.mustGraft("deploy", "--unapproved", "prod")
			for i, at := range []string{"2026-10-22 15:46", "2026-10-22 16:08"} {
				d.at(at)
				d.plainCommitFile(fmt.Sprintf("next%d.txt", i), fmt.Sprintf("feat: PROJ-1 next work %d", i))
				out, code := d.graft("", "deploy", "test")
				if code != 0 || !strings.Contains(out, "prod already runs "+candidate[:12]+", the candidate of the 22.10.26 window: the hold is over") {
					t.Fatalf("deploy %d to test after prod took the candidate: exit %d\n%s", i+1, code, out)
				}
			}
		})
	}
}

// TestReleaseHotfix: a hotfix branch from what prod runs goes through test
// during the hold and out of the window; the next release takes it only once
// the trunk has merged it.
func TestReleaseHotfix(t *testing.T) {
	d := releaseRepo(t)
	trunk := d.git("branch", "--show-current")
	prod := strings.TrimSpace(d.stateFile("prod-sha"))

	d.at("2026-10-20 10:00")
	d.commitFile("work.txt", "feat: PROJ-1 trunk work")
	d.mustGraft("deploy", "test")

	d.at("2026-10-21 18:30")
	d.git("switch", "-q", "-c", "hotfix/PROJ-9-wrong-base")
	d.commitFile("fix.txt", "fix: PROJ-9 urgent")
	// Branched from the trunk, it would take the trunk's work along.
	if out, code := d.graft("", "deploy", "test"); code == 0 || !strings.Contains(out, "feat: PROJ-1 trunk work: PROJ-1 without the label hotfix") {
		t.Errorf("a hotfix from the trunk: exit %d\n%s", code, out)
	}
	d.git("switch", "-q", "-c", "hotfix/PROJ-9", prod)
	d.commitFile("fix.txt", "fix: PROJ-9 urgent")
	if v := strings.TrimSpace(d.read("VERSION")); v != "1.0.1.1" {
		t.Errorf("hotfix build VERSION %q", v)
	}
	if out, code := d.graft("", "deploy", "test"); code == 0 || !strings.Contains(out, "PROJ-9 without the label hotfix") {
		t.Errorf("an unlabelled hotfix: exit %d\n%s", code, out)
	}
	d.jira.mu.Lock()
	d.jira.labels = map[string][]string{"PROJ-9": {"backend", "hotfix"}}
	d.jira.mu.Unlock()
	if out := d.mustGraft("deploy", "test"); !strings.Contains(out, "a hotfix passes the hold") {
		t.Errorf("hotfix to test:\n%s", out)
	}
	out := d.mustGraft("deploy", "prod")
	if !strings.Contains(out, "the release window, the hold and approved_release do not apply") || !strings.Contains(out, "git merge --no-ff origin/hotfix/PROJ-9") {
		t.Errorf("hotfix to prod:\n%s", out)
	}
	if d.status("PROJ-9") != "Testing" {
		t.Errorf("PROJ-9 is %s", d.status("PROJ-9"))
	}

	// The window: the trunk without the hotfix would take it back.
	d.at("2026-10-22 15:30")
	d.git("switch", "-q", trunk)
	d.mustGraft("deploy", "test")
	out, code := d.graft("", "deploy", "prod")
	if code == 0 || !strings.Contains(out, "git merge --no-ff origin/hotfix/PROJ-9   # a hotfix") {
		t.Fatalf("a release without the hotfix: exit %d\n%s", code, out)
	}
	if _, code := d.exec(d.work, "", "git", "-c", "core.hooksPath=/dev/null", "merge", "-q", "--no-ff", "--no-edit", "origin/hotfix/PROJ-9"); code == 0 {
		t.Fatal("the version files did not conflict")
	}
	d.git("checkout", "--ours", "--", "VERSION")
	d.git("add", "VERSION")
	d.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "--no-edit")
	d.git("push", "-q", "origin", trunk)
	if out := d.mustGraft("deploy", "test"); !strings.Contains(out, "takes only its reverts and hotfixes") {
		t.Errorf("the candidate with the hotfix merged:\n%s", out)
	}
	if out, code := d.graft("", "deploy", "prod"); code == 0 || !strings.Contains(out, "feat: PROJ-1 trunk work: PROJ-1 is Testing") {
		t.Errorf("unchecked trunk work: exit %d\n%s", code, out)
	}
	d.setStatus("PROJ-1", "Ready for release")
	d.mustGraft("deploy", "prod")
}

// versionConfig is releaseConfig with the window versions kept and the test
// target adding the window version to the items it moves to Testing.
func versionConfig(state string, q func(string) string) string {
	cfg := releaseConfig(state, q)
	cfg = strings.Replace(cfg, "release_notes: {transitions: [{from: [In Progress], to: Testing, assign: qa}]}",
		`release_notes: {transitions: [{from: [In Progress], to: Testing, assign: qa}], fix_version: {name: "Product {dd}.{mm}.{yy}"}}`, 1)
	return strings.Replace(cfg, "      approved_release:",
		`      release_version: {enabled: true, name: "Product {dd}.{mm}.{yy}", hotfix: "Product {dd}.{mm}.{yy} hotfix"}
      release_page: {parent: "500", labels: {moved: "Next window"}}
      approved_release:`, 1)
}

func (d *deployFixture) fixVersionsOf(key string) []string {
	d.jira.mu.Lock()
	defer d.jira.mu.Unlock()
	return slices.Clone(d.jira.fixVersions[key])
}

// TestReleaseVersions: the window version takes the items test moves to
// Testing, loses the ones the cutoff reverts to the next window's, and is
// released with its build; a hotfix gets a version of its own.
func TestReleaseVersions(t *testing.T) {
	d := newDeployFixture(t, versionConfig)
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-8"] = "In Progress"
	d.jira.statuses["PROJ-9"] = "In Progress"
	d.jira.types = map[string]string{"PROJ-8": "Bug"}
	d.jira.versions = []map[string]any{
		{"id": "20019", "name": "Product 19.10.26", "released": false, "archived": false, "releaseDate": "2026-10-19"},
		{"id": "20022", "name": "Product.22.10.26", "released": false, "archived": false, "releaseDate": "2026-10-22"},
		{"id": "10009", "name": "Product v1.2.9", "released": false, "archived": false},
	}
	d.jira.mu.Unlock()
	d.at("2026-10-16 10:00")
	d.mustGraft("deploy", "test")

	// Without the permission, and with a name that slips from the template,
	// the release is live but its version is not kept.
	d.at("2026-10-19 15:30")
	out, code := d.graft("", "deploy", "--unapproved", "prod")
	if code == 0 || !strings.Contains(out, "needs the Manage versions permission (MANAGE_VERSIONS); in a team-managed project it comes with the Administrator role") {
		t.Fatalf("no permission: exit %d\n%s", code, out)
	}
	d.jira.mu.Lock()
	d.jira.manageVersions = true
	d.jira.mu.Unlock()
	out, code = d.graft("", "deploy", "--unapproved", "--redeploy", "prod")
	if code == 0 || !strings.Contains(out, `versions "Product.22.10.26" look like the version name template but do not follow it`) {
		t.Fatalf("a slipped name: exit %d\n%s", code, out)
	}
	d.jira.mu.Lock()
	d.jira.versions[1]["name"] = "Product 22.10.26"
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "--unapproved", "--redeploy", "prod")
	if v := d.jira.version("20019"); v["released"] != true || v["releaseDate"] != "2026-10-19" || !strings.Contains(v["description"].(string), "build 1.0.1, commit ") {
		t.Errorf("the Monday version: %v", v)
	}
	if body := d.jira.page("Product 19.10.26: release notes"); !strings.Contains(body, `<a href="`+d.jiraURL()+`/projects/PROJ/versions/20019">Product 19.10.26</a>`) ||
		!strings.Contains(body, "<th>Date</th><td>2026-10-19</td>") || strings.Count(body, "<h2>Build 1.0.1, commit ") != 1 || !strings.Contains(body, "<p>No items.</p>") {
		t.Errorf("the Monday page, published once over three deploys:\n%s", body)
	}

	// Test moves the work to Testing into the Thursday version.
	d.at("2026-10-20 10:00")
	d.commitFile("checked.txt", "feat: PROJ-8 checked work")
	d.commitFile("unchecked.txt", "feat: PROJ-1 unchecked work")
	d.mustGraft("deploy", "test")
	for _, key := range []string{"PROJ-1", "PROJ-8"} {
		if got := d.fixVersionsOf(key); !slices.Equal(got, []string{"20022"}) {
			t.Errorf("%s fix versions = %v", key, got)
		}
	}
	d.setStatus("PROJ-8", "Ready for release")

	// The cutoff reverts PROJ-1: its version becomes the next window's.
	d.at("2026-10-21 18:30")
	d.runCommands(d.mustGraft("deploy", "cutoff", "prod"), "goes out from release/22.10.26", "Then the candidate goes")
	out = d.mustGraft("deploy", "test")
	next := d.jira.versionNamed("Product 26.10.26")
	if next == nil || next["releaseDate"] != "2026-10-26" {
		t.Fatalf("no next window version:\n%s", out)
	}
	if got := d.fixVersionsOf("PROJ-1"); !slices.Equal(got, []string{next["id"].(string)}) || !strings.Contains(out, "PROJ-1 moved to version Product 26.10.26") {
		t.Errorf("PROJ-1 fix versions = %v\n%s", got, out)
	}

	// The release refuses while the version lists work it does not take out.
	d.at("2026-10-22 15:30")
	d.jira.mu.Lock()
	d.jira.fixVersions["PROJ-5"] = []string{"20022"}
	d.jira.mu.Unlock()
	if out, code := d.graft("", "deploy", "prod"); code == 0 || !strings.Contains(out, "version Product 22.10.26 lists PROJ-5, which this release does not take out") {
		t.Errorf("a version with more than the release: exit %d\n%s", code, out)
	}
	d.jira.mu.Lock()
	delete(d.jira.fixVersions, "PROJ-5")
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "prod")
	if v := d.jira.version("20022"); v["released"] != true || v["releaseDate"] != "2026-10-22" || !strings.Contains(v["description"].(string), "build 1.0.3.1") {
		t.Errorf("the Thursday version: %v", v)
	}
	body := d.jira.page("Product 22.10.26: release notes")
	changes, moved, _ := strings.Cut(body, "<h3>Next window</h3>")
	if !strings.Contains(changes, "<h2>Build 1.0.3.1, commit ") || !strings.Contains(changes, "<h3>Fixes</h3>") ||
		!strings.Contains(changes, `<td><a href="`+d.jiraURL()+`/browse/PROJ-8">PROJ-8</a></td><td>Summary of PROJ-8</td><td>Bug</td>`) ||
		!strings.Contains(moved, ">PROJ-1</a>") || strings.Contains(changes, ">PROJ-1</a>") || strings.Contains(body, "<h3>Changes</h3>") {
		t.Errorf("the Thursday page:\n%s", body)
	}

	// A hotfix on Friday: its own version, released with it.
	d.at("2026-10-23 11:00")
	d.git("switch", "-q", "-c", "hotfix/PROJ-9", strings.TrimSpace(d.stateFile("prod-sha")))
	d.commitFile("fix.txt", "fix: PROJ-9 urgent")
	d.jira.mu.Lock()
	d.jira.labels = map[string][]string{"PROJ-9": {"hotfix"}}
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "test")
	d.mustGraft("deploy", "prod")
	hv := d.jira.versionNamed("Product 23.10.26 hotfix")
	if hv == nil || hv["released"] != true || !strings.Contains(hv["description"].(string), "build 1.0.3.2") {
		t.Fatalf("hotfix version: %v", hv)
	}
	if got := d.fixVersionsOf("PROJ-9"); !slices.Contains(got, hv["id"].(string)) {
		t.Errorf("PROJ-9 fix versions = %v", got)
	}
	if body := d.jira.page("Product 23.10.26 hotfix: release notes"); !strings.Contains(body, "<h3>Changes</h3>") || !strings.Contains(body, ">PROJ-9</a>") || !strings.Contains(body, "<th>Date</th><td>2026-10-23</td>") {
		t.Errorf("the hotfix page:\n%s", body)
	}
}

// TestReleasePageApprovedVersions: without windows the approved versions the
// release took its items from name the page; without them nothing does.
func TestReleasePageApprovedVersions(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return strings.Replace(deployConfig(state, q), `approved_release: {prefix: "Product v", ignore_paths: ["docs/**", "*.md"]}`,
			`approved_release: {prefix: "Product v", ignore_paths: ["docs/**", "*.md"]}
      release_page: {parent: "500", title: "{release}: notes"}`, 1)
	})
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-1": {"10010"}})
	d.jira.setApprovers(map[string][]string{"10010": {"APPROVED", "APPROVED"}})
	if out, code := d.graft("", "deploy", "--unapproved", "live"); code != 1 || !strings.Contains(out, "graft: release page NOT PUBLISHED: no approved version names the release") {
		t.Errorf("an unapproved release: exit %d\n%s", code, out)
	}
	d.write("a.txt", "a\n")
	d.mustFixtureCommit("feat: PROJ-1 approved thing")
	out := d.mustGraft("deploy", "live")
	body := d.jira.page("Product v1.2.10: notes")
	if !strings.Contains(out, `release page "Product v1.2.10: notes" published: `+d.jiraURL()+"/wiki/spaces/S/pages/") ||
		!strings.Contains(body, "/projects/PROJ/versions/10010\">Product v1.2.10</a>") || !strings.Contains(body, "<th>Target</th><td>live</td>") || !strings.Contains(body, ">PROJ-1</a>") {
		t.Errorf("the page:\n%s\n%s", body, out)
	}
}

// TestReleaseNameWithoutVersions: release_version with enabled false names
// the release by its window for the release page and the notes, and leaves
// the Jira versions alone: no permission is needed.
func TestReleaseNameWithoutVersions(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := strings.Replace(releaseConfig(state, q), "      approved_release:",
			`      release_version: {enabled: false, name: "Product {dd}.{mm}.{yy}", hotfix: "Product {dd}.{mm}.{yy} hotfix"}
      release_page: {parent: "500"}
      approved_release:`, 1)
		return strings.Replace(cfg, "release_notes: {transitions: [{from: [Ready for release], to: Done}",
			"release_notes: {comment: 'Released {release} to {target}, build {version}', transitions: [{from: [Ready for release], to: Done}", 1)
	})
	d.at("2026-10-16 10:00")
	d.mustGraft("deploy", "test")
	d.at("2026-10-19 15:30")
	d.mustGraft("deploy", "--unapproved", "prod")
	d.at("2026-10-20 10:00")
	d.commitFile("checked.txt", "feat: PROJ-8 checked work")
	d.setStatus("PROJ-8", "In Progress")
	d.mustGraft("deploy", "test")
	d.setStatus("PROJ-8", "Ready for release")
	d.at("2026-10-22 15:30")
	out := d.mustGraft("deploy", "prod")
	if body := d.jira.page("Product 22.10.26: release notes"); !strings.Contains(body, ">PROJ-8</a>") || !strings.Contains(body, "<th>Release</th><td>Product 22.10.26</td>") {
		t.Errorf("the page:\n%s\n%s", body, out)
	}
	d.jira.mu.Lock()
	comments, versions := d.jira.comments["PROJ-8"], d.jira.versions
	d.jira.mu.Unlock()
	if !slices.Contains(comments, "Released Product 22.10.26 to prod, build 1.0.2") || len(versions) != 0 || strings.Contains(out, "MANAGE_VERSIONS") {
		t.Errorf("comments %q, versions %v:\n%s", comments, versions, out)
	}
}

// TestReleaseAttribute: a commit that names no item, attributed to an
// unchecked item, is reverted by the cutoff under that item, withheld by the
// release and listed as the item's on the release page; the next release
// checks it again and takes it out as the item's.
func TestReleaseAttribute(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return strings.Replace(releaseConfig(state, q), "      approved_release:",
			`      release_version: {enabled: false, name: "Product {dd}.{mm}.{yy}"}
      release_page: {parent: "500"}
      approved_release:`, 1)
	})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-8"] = "Ready for release"
	d.jira.mu.Unlock()
	trunk := d.git("branch", "--show-current")
	d.at("2026-10-16 10:00")
	d.mustGraft("deploy", "test")
	d.at("2026-10-19 15:30")
	d.mustGraft("deploy", "--unapproved", "prod")

	d.at("2026-10-20 10:00")
	d.commitFile("checked.txt", "feat: PROJ-8 checked work")
	keyless := d.commitFile("keyless.txt", "feat: forgot the key")
	d.write(".graft.yaml", strings.Replace(d.read(".graft.yaml"), `ignore_paths: ["docs/**"]}`,
		`ignore_paths: ["docs/**"], attribute: [{commit: `+keyless+`, item: PROJ-1, reason: key left out}]}`, 1))
	d.mustFixtureCommit("chore: PROJ-8 attribute the keyless commit")
	d.mustGraft("deploy", "test")

	d.at("2026-10-21 18:30")
	out := d.mustGraft("deploy", "cutoff", "prod")
	for _, want := range []string{
		"# PROJ-1 is In Progress, not Ready for release or Done",
		"git diff --binary --no-renames --no-color --no-ext-diff --no-textconv --src-prefix=a/ --dst-prefix=b/ " + keyless + "^ " + keyless + " -- . ':(exclude,literal)VERSION' | git apply -R --3way --index",
		"graft commit -m 'revert: PROJ-1 not checked by the 22.10.26 cutoff' -m 'This reverts commit " + keyless + ".'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cutoff: no %q\n%s", want, out)
		}
	}
	d.runCommands(out, "goes out from release/22.10.26", "Then the candidate goes")
	d.mustGraft("deploy", "test")

	d.at("2026-10-22 15:30")
	out = d.mustGraft("deploy", "prod")
	if got := strings.TrimSpace(d.stateFile("prod-sha.withheld")); got != keyless || !strings.Contains(out, "feat: forgot the key: reverted by") {
		t.Errorf("withheld = %q, want %s\n%s", got, keyless, out)
	}
	changes, moved, _ := strings.Cut(d.jira.page("Product 22.10.26: release notes"), "<h3>Moved to the next release</h3>")
	if !strings.Contains(changes, ">PROJ-8</a>") || strings.Contains(changes, ">PROJ-1</a>") || !strings.Contains(moved, ">PROJ-1</a>") {
		t.Errorf("the page: changes\n%s\nmoved\n%s", changes, moved)
	}
	comments, _ := d.jira.snapshot()
	if slices.ContainsFunc(comments["PROJ-1"], func(c string) bool { return strings.Contains(c, "prod") }) {
		t.Errorf("PROJ-1 got a prod note for withheld work: %v", comments["PROJ-1"])
	}

	d.git("switch", "-q", trunk)
	d.git("-c", "core.hooksPath=/dev/null", "merge", "-q", "-s", "ours", "--no-edit", "release/22.10.26")
	d.git("push", "-q", "origin", trunk)
	d.at("2026-10-23 10:00")
	d.mustGraft("deploy", "test")
	d.setStatus("PROJ-1", "Ready for release")
	d.at("2026-10-26 15:30")
	out = d.mustGraft("deploy", "prod")
	if !strings.Contains(out, "feat: forgot the key: withheld from an earlier release, goes out now") || !strings.Contains(out, "feat: forgot the key: attributed to PROJ-1: key left out") {
		t.Errorf("the withheld commit:\n%s", out)
	}
	// It went out now, outside the deployed range: its item gets the notes.
	comments, _ = d.jira.snapshot()
	if got := strings.TrimSpace(d.stateFile("prod-sha.withheld")); got != "" || d.status("PROJ-1") != "Done" ||
		!slices.ContainsFunc(comments["PROJ-1"], func(c string) bool { return strings.Contains(c, "Deployed to prod") }) {
		t.Errorf("withheld = %q, PROJ-1 %s, comments %v", got, d.status("PROJ-1"), comments["PROJ-1"])
	}
	if body := d.jira.page("Product 26.10.26: release notes"); !strings.Contains(body, ">PROJ-1</a>") {
		t.Errorf("the next page:\n%s", body)
	}
}

// TestReleaseMergeIntoTrunk: with merge_into_trunk, the release from a
// release branch is recorded on the trunk by the deploy itself, keeping the
// trunk's tree, the withheld work with it; a fix made on the release branch
// that the trunk lacks stops the merge, which would drop it.
func TestReleaseMergeIntoTrunk(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return strings.Replace(releaseConfig(state, q), "      deployed_sha: {path: "+q("prod-sha")+"}\n",
			"      deployed_sha: {path: "+q("prod-sha")+"}\n      merge_into_trunk: true\n", 1)
	})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-8"] = "Ready for release"
	d.jira.mu.Unlock()
	trunk := d.git("branch", "--show-current")
	d.git("remote", "set-head", "origin", trunk)
	d.at("2026-10-16 10:00")
	d.mustGraft("deploy", "test")
	d.at("2026-10-19 15:30")
	d.mustGraft("deploy", "--unapproved", "prod")

	d.at("2026-10-20 10:00")
	d.commitFile("checked.txt", "feat: PROJ-8 checked work")
	d.commitFile("unchecked.txt", "feat: PROJ-1 unchecked work")
	d.mustGraft("deploy", "test")
	d.at("2026-10-21 18:30")
	d.runCommands(d.mustGraft("deploy", "cutoff", "prod"), "goes out from release/22.10.26", "Then the candidate goes")
	d.mustGraft("deploy", "test")
	release := d.git("rev-parse", "HEAD")
	before := d.git("rev-parse", "origin/"+trunk+"^{tree}")

	d.at("2026-10-22 15:30")
	out := d.mustGraft("deploy", "prod")
	if !strings.Contains(out, "merged release/22.10.26 into "+trunk) {
		t.Errorf("no merge into the trunk:\n%s", out)
	}
	d.git("fetch", "-q", "origin")
	if got := d.git("rev-parse", "origin/"+trunk+"^{tree}"); got != before {
		t.Errorf("the merge changed the trunk's tree: %s, was %s", got, before)
	}
	if got := d.git("rev-parse", "origin/"+trunk+"^2"); got != release {
		t.Errorf("the trunk's merge has %s as its second parent, want %s", got, release)
	}
	if got := d.git("rev-parse", trunk); got != d.git("rev-parse", "origin/"+trunk) {
		t.Errorf("the local trunk %s did not follow", got)
	}

}

// TestDeployMergeIntoTrunkKeepsFixes: a fix made on a release branch that
// the trunk lacks stops the merge into the trunk, which would drop it; once
// the trunk has it by content, a commit of its own, the next deploy merges,
// and so does a port of trunk work that names the item the trunk worked on.
func TestDeployMergeIntoTrunkKeepsFixes(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return `schema: 1
version: {mode: file, build_branches: ['release/*']}
ticket: {pattern: 'PROJ-[0-9]+'}
envs:
  local: {}
deploy:
  remote: origin
  targets:
    prod:
      env: local
      run: [sh, deploy.sh]
      deployed_sha: {path: ` + q("prod-sha") + `}
      merge_into_trunk: true
`
	})
	trunk := d.git("branch", "--show-current")
	d.git("remote", "set-head", "origin", trunk)
	d.git("switch", "-q", "-c", "release/1")
	d.git("push", "-q", "-u", "origin", "release/1")
	fix := d.commitFile("fix.txt", "fix: found in the release")
	out, code := d.graft("", "deploy", "prod")
	if code == 0 || !strings.Contains(out, "deployed "+fix[:12]+" to prod") ||
		!strings.Contains(out, "release/1 is not merged into "+trunk+": "+trunk+" lacks these of its commits, by content and by the items they name") ||
		!strings.Contains(out, fix[:12]+" fix: found in the release") {
		t.Errorf("a fix the trunk lacks: exit %d\n%s", code, out)
	}
	if got := strings.TrimSpace(d.stateFile("prod-sha")); got != fix {
		t.Errorf("the deploy is not recorded: prod-sha %q", got)
	}
	d.git("fetch", "-q", "origin")
	if _, code := d.exec(d.work, "", "git", "merge-base", "--is-ancestor", fix, "origin/"+trunk); code == 0 {
		t.Fatal("the release branch with the fix was merged into the trunk")
	}

	// The trunk takes the fix as a commit of its own; the branch builds again.
	d.git("switch", "-q", trunk)
	d.commitFile("fix.txt", "fix: found in the release")
	d.commitFile("b.txt", "feat: b")
	d.git("switch", "-q", "release/1")
	head := d.commitFile("b.txt", "feat: b")
	if out := d.mustGraft("deploy", "prod"); !strings.Contains(out, "merged release/1 into "+trunk) {
		t.Errorf("no merge once the trunk has the fix:\n%s", out)
	}
	d.git("fetch", "-q", "origin")
	if got := d.git("rev-parse", "origin/"+trunk+"^2"); got != head {
		t.Errorf("the trunk's merge has %s as its second parent, want %s", got, head)
	}

	// A port of trunk work the trunk has since reworked: by content it
	// conflicts, by its item it is in the trunk, and the merge says so.
	d.git("switch", "-q", trunk)
	d.git("pull", "-q", "--ff-only", "origin", trunk)
	d.commitFile("c.txt", "feat: PROJ-7 first take")
	d.git("switch", "-q", "-c", "release/2")
	d.git("push", "-q", "-u", "origin", "release/2")
	d.git("switch", "-q", trunk)
	d.commitFile("c.txt", "feat: PROJ-7 reworked")
	d.git("switch", "-q", "release/2")
	port := d.commitFile("c.txt", "fix: PROJ-7 ported from the trunk")
	out = d.mustGraft("deploy", "prod")
	if !strings.Contains(out, "merged release/2 into "+trunk) ||
		!strings.Contains(out, "1 commits of release/2 count as ports of "+trunk+"'s work by the items they name, not by content") ||
		!strings.Contains(out, port[:12]+" fix: PROJ-7 ported from the trunk") {
		t.Errorf("a port by its item:\n%s", out)
	}
}

type rv2Tree map[uint64]map[string]bool

func rv2Migrations(d *deployFixture, rev, dir string) rv2Tree {
	d.t.Helper()
	out := d.git("ls-tree", "-r", rev)
	t := rv2Tree{}
	for line := range strings.SplitSeq(out, "\n") {
		meta, path, ok := strings.Cut(line, "\t")
		if !ok || filepath.ToSlash(filepath.Dir(path)) != dir {
			continue
		}
		base := filepath.Base(path)
		if strings.Contains(base, ".down.") {
			continue
		}
		i := 0
		for i < len(base) && base[i] >= '0' && base[i] <= '9' {
			i++
		}
		if i == 0 {
			continue
		}
		n, _ := strconv.ParseUint(base[:i], 10, 64)
		if t[n] == nil {
			t[n] = map[string]bool{}
		}
		t[n][strings.Fields(meta)[2]] = true
	}
	return t
}

func rv2Max(t rv2Tree) uint64 {
	var m uint64
	for n := range t {
		m = max(m, n)
	}
	return m
}

// rv2Losses: candidate numbers at or below the release's highest that the
// release lacks or has different.
func rv2Losses(cand, rel rv2Tree) []uint64 {
	hi := rv2Max(rel)
	var lost []uint64
	for n, blobs := range cand {
		if n > hi {
			continue
		}
		r := rel[n]
		same := len(r) == len(blobs)
		for b := range blobs {
			if !r[b] {
				same = false
			}
		}
		if !same {
			lost = append(lost, n)
		}
	}
	slices.Sort(lost)
	return lost
}

// rv2Extra: release numbers the candidate does not have at all (a revert
// brought back a migration the trunk renamed or removed).
func rv2Extra(cand, rel rv2Tree) []uint64 {
	var extra []uint64
	for n := range rel {
		if cand[n] == nil {
			extra = append(extra, n)
		}
	}
	slices.Sort(extra)
	return extra
}

func rv2Commit(d *deployFixture, msg string, files map[string]string) string {
	d.t.Helper()
	for p, c := range files {
		if c == "" {
			if err := os.Remove(filepath.Join(d.work, filepath.FromSlash(p))); err != nil {
				d.t.Fatal(err)
			}
			continue
		}
		d.write(p, c)
	}
	d.mustFixtureCommit(msg)
	return d.git("rev-parse", "HEAD")
}

func rv2Repo(t *testing.T, dir string) *deployFixture {
	d := cutoffMigrationRepoWithPath(t, dir, true)
	d.at("2026-10-20 10:00")
	return d
}

var rv2ItemRe = regexp.MustCompile(`PROJ-\d+`)
var rv2DiffRe = regexp.MustCompile(`^git diff .* ([0-9a-f]{40})\^ ([0-9a-f]{40}) -- `)

// rv2Exec runs the plan lines; keep names an item kept whole: its git diff
// lines and graft commit are skipped, and the paths its "Decide first" line
// names get excluded from the other items' git diff lines. It returns the
// first failing command, or "".
func rv2Exec(t *testing.T, d *deployFixture, out, keep string) string {
	t.Helper()
	extra := map[string][]string{} // item -> paths
	empty := map[string]bool{}
	if keep != "" {
		for line := range strings.SplitSeq(out, "\n") {
			if strings.HasPrefix(line, "#   if keeping "+keep+": nothing left to revert") {
				if m := regexp.MustCompile(`commit ([0-9a-f]{12})`).FindStringSubmatch(line); m != nil {
					empty[m[1]] = true
				}
			}
			if !strings.HasPrefix(line, "#   "+keep+":") {
				continue
			}
			_, list, ok := strings.Cut(line, "other items' reverts ")
			if !ok {
				continue
			}
			for _, m := range regexp.MustCompile(`(\S+) \(number \d+, (PROJ-\d+)\)`).FindAllStringSubmatch(list, -1) {
				extra[m[2]] = append(extra[m[2]], m[1])
			}
		}
		t.Logf("keep %s: extra excludes %v", keep, extra)
	}
	itemOf := func(sha string) string {
		return rv2ItemRe.FindString(d.git("log", "-1", "--format=%s", sha))
	}
	on := false
	for line := range strings.SplitSeq(out, "\n") {
		switch {
		case strings.Contains(line, "goes out from release/"):
			on = true
		case strings.Contains(line, "Then the candidate goes"):
			on = false
		}
		cmd := strings.TrimSpace(line)
		if !on || !(strings.HasPrefix(cmd, "git ") || strings.HasPrefix(cmd, "graft ")) {
			continue
		}
		if keep != "" {
			if m := rv2DiffRe.FindStringSubmatch(cmd); m != nil {
				if empty[m[2][:12]] {
					continue
				}
				item := itemOf(m[2])
				if item == keep {
					continue
				}
				for _, p := range extra[item] {
					cmd = strings.Replace(cmd, " | git apply", " ':(exclude)"+p+"' | git apply", 1)
				}
			}
			if strings.HasPrefix(cmd, "graft commit") && strings.Contains(cmd, "revert: "+keep+" ") {
				continue
			}
			if strings.HasPrefix(cmd, "graft commit") {
				for _, m := range regexp.MustCompile(`-m 'This reverts commit ([0-9a-f]{40})\.'`).FindAllStringSubmatch(cmd, -1) {
					if empty[m[1][:12]] {
						cmd = strings.Replace(cmd, m[0], "", 1)
					}
				}
				if !strings.Contains(cmd, "This reverts commit ") {
					continue
				}
			}
		}
		if got, code := d.exec(d.work, "", "sh", "-c", cmd); code != 0 {
			t.Logf("FAILED: %s: exit %d\n%s", cmd, code, got)
			return cmd
		}
	}
	return ""
}

// rv2Check runs the plan (or the keep variant on a fresh branch) and reports
// failing commands, losses and extras.
func rv2Check(t *testing.T, d *deployFixture, out, dir, keep string) {
	t.Helper()
	cand := strings.TrimSpace(d.stateFile("test-sha"))
	trunk := d.git("branch", "--show-current")
	if failed := rv2Exec(t, d, out, keep); failed != "" {
		t.Errorf("FINDING (keep=%q): a printed command fails: %s", keep, failed)
	}
	c, r := rv2Migrations(d, cand, dir), rv2Migrations(d, "HEAD", dir)
	if lost := rv2Losses(c, r); len(lost) > 0 {
		t.Errorf("FINDING (keep=%q): release lost migrations %v (release highest %d)", keep, lost, rv2Max(r))
	}
	if extra := rv2Extra(c, r); len(extra) > 0 {
		t.Logf("NOTE (keep=%q): release ships numbers %v the candidate does not have", keep, extra)
	}
	// back to the trunk for a second variant
	d.exec(d.work, "", "sh", "-c", "git reset -q --hard && git switch -q "+trunk+" && for b in $(git for-each-ref --format='%(refname:short)' refs/heads/release/); do git branch -D -q $b; git push -q origin --delete $b 2>/dev/null; done; true")
}

func rv2Plan(t *testing.T, d *deployFixture) string {
	t.Helper()
	out := cutoffOutput(t, d)
	t.Logf("plan:\n%s", out)
	assertCutoffShellSyntax(t, out)
	if strings.Contains(out, "migration check unavailable") {
		t.Errorf("FINDING: migration check unavailable")
	}
	return out
}

// 1. migrations: . with VERSION, a migration-only commit and a code commit.
func TestReleaseCutoffRootMigrationOnlyCommit(t *testing.T) {
	d := rv2Repo(t, ".")
	cutoffMigrationCommitsAt(d, ".", map[int]bool{}, 1)
	rv2Commit(d, "feat: PROJ-1 add 2", map[string]string{"000002_step.up.sql": "PROJ-1\n", "000002_step.down.sql": "down\n"})
	rv2Commit(d, "feat: PROJ-1 code", map[string]string{"app1.txt": "1\n"})
	cutoffMigrationCommitsAt(d, ".", map[int]bool{}, 3)
	out := rv2Plan(t, d)
	if !strings.Contains(out, "nothing left to revert") {
		t.Errorf("want nothing left to revert for the migration-only commit")
	}
	rv2Check(t, d, out, ".", "")
}

// 2. Subdirectory with nested dir: three items edit one migration below the
// highest in interleaved order.
func TestReleaseCutoffThreeItemsInterleavedBelow(t *testing.T) {
	d := rv2Repo(t, "db/migrations")
	cutoffMigrationCommitsAt(d, "db/migrations", map[int]bool{}, 1)
	rv2Commit(d, "feat: PROJ-3 add 2", map[string]string{"db/migrations/000002_x.up.sql": "v1\n", "app3.txt": "1\n"})
	rv2Commit(d, "feat: PROJ-1 edit 2", map[string]string{"db/migrations/000002_x.up.sql": "v2\n", "app1.txt": "1\n"})
	rv2Commit(d, "feat: PROJ-9 edit 2", map[string]string{"db/migrations/000002_x.up.sql": "v3\n", "app9.txt": "1\n"})
	rv2Commit(d, "feat: PROJ-3 edit 2", map[string]string{"db/migrations/000002_x.up.sql": "v4\n", "app3.txt": "2\n"})
	cutoffMigrationCommitsAt(d, "db/migrations", map[int]bool{}, 3)
	out := rv2Plan(t, d)
	rv2Check(t, d, out, "db/migrations", "")
	for _, k := range []string{"PROJ-1", "PROJ-3", "PROJ-9"} {
		rv2Check(t, d, out, "db/migrations", k)
	}
}

// 3. Interleaved edits of a migration ABOVE every checked one: all reverts
// are possible in history order, but the per-item order fails; the plan
// should not ship unchecked migration 5 if it can be reverted.
func TestReleaseCutoffInterleavedAboveHighest(t *testing.T) {
	d := rv2Repo(t, "migrations")
	cutoffMigrationCommits(d, map[int]bool{}, 1, 2, 3, 4)
	rv2Commit(d, "feat: PROJ-3 add 5", map[string]string{"migrations/000005_x.up.sql": "v1\n", "app3.txt": "1\n"})
	rv2Commit(d, "feat: PROJ-1 edit 5", map[string]string{"migrations/000005_x.up.sql": "v2\n", "app1.txt": "1\n"})
	rv2Commit(d, "feat: PROJ-3 edit 5", map[string]string{"migrations/000005_x.up.sql": "v3\n", "app3.txt": "2\n"})
	out := rv2Plan(t, d)
	if strings.Contains(out, "Migration cutoff conflict") {
		t.Errorf("FINDING: conflict named for migration 5 above every checked migration; reverting it loses nothing")
	}
	rv2Check(t, d, out, "migrations", "")
	r := rv2Migrations(d, "HEAD", "migrations")
	_ = r
}

// 4. Revert does not apply because of a later edit: own item (applies),
// other unchecked item, checked item; numbers above the highest.
func TestReleaseCutoffLateEditAbove(t *testing.T) {
	for _, later := range []string{"PROJ-1", "PROJ-9", "PROJ-8"} {
		t.Run(later, func(t *testing.T) {
			d := rv2Repo(t, "migrations")
			cutoffMigrationCommits(d, map[int]bool{}, 1, 2, 3, 4)
			rv2Commit(d, "feat: PROJ-1 add 5", map[string]string{"migrations/000005_x.up.sql": "v1\n", "migrations/000005_x.down.sql": "d1\n", "app1.txt": "1\n"})
			rv2Commit(d, "feat: "+later+" edit 5", map[string]string{"migrations/000005_x.up.sql": "v2\n", "migrations/000005_x.down.sql": "d2\n", "app" + later + ".txt": "1\n"})
			out := rv2Plan(t, d)
			rv2Check(t, d, out, "migrations", "")
		})
	}
}

// 5. A non-conflicting commit whose only change is a migration path that
// stays (its revert does not apply): the printed exclude leaves an empty
// patch, and git apply refuses an empty patch.
func TestReleaseCutoffStaysOnlyPathEmptyPatch(t *testing.T) {
	d := rv2Repo(t, "migrations")
	cutoffMigrationCommits(d, map[int]bool{}, 1, 2, 3)
	rv2Commit(d, "feat: PROJ-1 add 7", map[string]string{"migrations/000007_x.up.sql": "up\n", "migrations/000007_x.down.sql": "d1\n", "app1.txt": "1\n"})
	rv2Commit(d, "feat: PROJ-1 down 7", map[string]string{"migrations/000007_x.down.sql": "d2\n"})
	rv2Commit(d, "feat: PROJ-8 down 7", map[string]string{"migrations/000007_x.down.sql": "d3\n"})
	out := rv2Plan(t, d)
	rv2Check(t, d, out, "migrations", "")
}

// 6. Rename with edit of a checked migration to a higher number by an
// unchecked item: the release gets the old number back, the trunk's new
// number repeats it on the next release.
func TestReleaseCutoffRenameSameNumberWithEdit(t *testing.T) {
	d := rv2Repo(t, "migrations")
	cutoffMigrationCommits(d, map[int]bool{}, 1, 2, 3)
	d.git("mv", "migrations/000002_step.up.sql", "migrations/000002_new name[1].up.sql")
	rv2Commit(d, "feat: PROJ-1 rename 2", map[string]string{"migrations/000002_new name[1].up.sql": "feat: PROJ-8 migration 2\nedited\n", "app1.txt": "1\n"})
	cutoffMigrationCommits(d, map[int]bool{}, 4)
	out := rv2Plan(t, d)
	if !strings.Contains(out, "Migration cutoff conflict") {
		t.Errorf("FINDING: no conflict for renamed+edited 2")
	}
	rv2Check(t, d, out, "migrations", "")
	rv2Check(t, d, out, "migrations", "PROJ-1")
}

// 8. .down. only below highest, and .down. with up below highest.
func TestReleaseCutoffDownFiles(t *testing.T) {
	d := rv2Repo(t, "migrations")
	cutoffMigrationCommits(d, map[int]bool{}, 1, 2)
	rv2Commit(d, "feat: PROJ-1 down 1", map[string]string{"migrations/000001_step.down.sql": "d\n", "app1.txt": "1\n"})
	rv2Commit(d, "feat: PROJ-9 add 3", map[string]string{"migrations/000003_x.up.sql": "u\n", "migrations/000003_x.down.sql": "d\n", "app9.txt": "1\n"})
	cutoffMigrationCommits(d, map[int]bool{}, 4)
	out := rv2Plan(t, d)
	if strings.Contains(out, "000001_step.down.sql (number 1)") {
		t.Errorf("FINDING: false conflict on a down-only change")
	}
	if !strings.Contains(out, "migrations/000003_x.down.sql") {
		t.Errorf("FINDING: down 3 not excluded with up 3")
	}
	rv2Check(t, d, out, "migrations", "")
}

// 9. Keep whole: two other items touch the same path that keeping raises
// below the highest; the Decide first line must name both.
func TestReleaseCutoffKeepNamesEveryItemOfAPath(t *testing.T) {
	d := rv2Repo(t, "migrations")
	cutoffMigrationCommits(d, map[int]bool{}, 1, 2, 4)
	rv2Commit(d, "feat: PROJ-3 add 3", map[string]string{"migrations/000003_x.up.sql": "v1\n", "app3.txt": "1\n"})
	rv2Commit(d, "feat: PROJ-9 edit 3", map[string]string{"migrations/000003_x.up.sql": "v2\n", "app9.txt": "1\n"})
	rv2Commit(d, "feat: PROJ-1 edit 1 add 5", map[string]string{"migrations/000001_step.up.sql": "edited\n", "migrations/000005_x.up.sql": "p1\n", "app1.txt": "1\n"})
	out := rv2Plan(t, d)
	rv2Check(t, d, out, "migrations", "")
	rv2Check(t, d, out, "migrations", "PROJ-1")
	rv2Check(t, d, out, "migrations", "PROJ-9")
	if !strings.Contains(out, "migrations/000003_x.up.sql (number 3, PROJ-3)") {
		t.Errorf("keeping PROJ-9 must exclude PROJ-3's revert of the shared path:\n%s", out)
	}
}

func TestReleaseCutoffIgnoresMigrationNamedGitlink(t *testing.T) {
	d := rv2Repo(t, "migrations")
	cutoffMigrationCommits(d, map[int]bool{}, 1, 3)
	linkPath := filepath.Join(d.work, "migrations", "000002_link.up.sql")
	if err := os.MkdirAll(linkPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linkPath, "placeholder"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := d.exec(linkPath, "", "git", "init", "-q"); code != 0 {
		t.Fatalf("init migration path gitlink: exit %d\n%s", code, out)
	}
	for _, args := range [][]string{{"git", "add", "placeholder"}, {"git", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "-q", "-m", "placeholder"}} {
		if out, code := d.exec(linkPath, "", args[0], args[1:]...); code != 0 {
			t.Fatalf("gitlink %s: exit %d\n%s", args[1], code, out)
		}
	}
	shaOut, code := d.exec(linkPath, "", "git", "rev-parse", "HEAD")
	if code != 0 {
		t.Fatalf("read migration gitlink commit: exit %d\n%s", code, shaOut)
	}
	sha := strings.TrimSpace(shaOut)
	rv2Commit(d, "feat: PROJ-1 link path", map[string]string{"app1.txt": "1\n"})
	d.git("update-index", "--add", "--cacheinfo", "160000,"+sha+",migrations/000002_link.up.sql")
	if out, code := d.exec(d.work, "", "git", "-c", "core.hooksPath=/dev/null", "commit", "-q", "--amend", "--no-edit"); code != 0 {
		t.Fatalf("commit migration path gitlink: exit %d\n%s", code, out)
	}
	if entry := d.git("ls-tree", "HEAD", "--", "migrations/000002_link.up.sql"); !strings.HasPrefix(entry, "160000 commit ") {
		t.Fatalf("fixture did not commit a gitlink migration path: %q", entry)
	}
	out := rv2Plan(t, d)
	if strings.Contains(out, "Migration cutoff conflict") {
		t.Fatalf("a gitlink is not a migration file:\n%s", out)
	}
	if failed := rv2Exec(t, d, out, ""); failed != "" {
		t.Fatalf("printed command failed: %s", failed)
	}
}

// 10. Keep whole: the kept item edited another item's migration above the
// highest; the other item's printed revert no longer applies.
func TestReleaseCutoffKeepMakesOtherRevertFail(t *testing.T) {
	d := rv2Repo(t, "migrations")
	cutoffMigrationCommits(d, map[int]bool{}, 1, 2, 3)
	rv2Commit(d, "feat: PROJ-9 add 6", map[string]string{"migrations/000006_x.up.sql": "v1\n", "app9.txt": "1\n"})
	rv2Commit(d, "feat: PROJ-1 edit 2 and 6", map[string]string{"migrations/000002_step.up.sql": "edited\n", "migrations/000006_x.up.sql": "v2\n", "app1.txt": "1\n"})
	out := rv2Plan(t, d)
	if !strings.Contains(out, "and the revert of migrations/000006_x.up.sql (PROJ-9) does not apply then: it stays as in the candidate") {
		t.Errorf("keep option omitted the other item's non-applying revert:\n%s", out)
	}
	rv2Check(t, d, out, "migrations", "")
	rv2Check(t, d, out, "migrations", "PROJ-1")
}

func TestReleaseCutoffKeepNamesOtherPathAtSameNumber(t *testing.T) {
	d := rv2Repo(t, "migrations")
	cutoffMigrationCommits(d, map[int]bool{}, 1, 2, 4)
	rv2Commit(d, "feat: PROJ-9 add 3", map[string]string{"migrations/000003_a.up.sql": "a\n", "app9.txt": "1\n"})
	rv2Commit(d, "feat: PROJ-1 add another 3", map[string]string{"migrations/000003_b.up.sql": "b\n", "migrations/000005_x.up.sql": "p1\n", "app1.txt": "1\n"})
	out := rv2Plan(t, d)
	if !strings.Contains(out, "migrations/000003_b.up.sql (number 3, PROJ-1)") {
		t.Fatalf("keeping PROJ-9 must name PROJ-1's other file at the same migration number:\n%s", out)
	}
	rv2Check(t, d, out, "migrations", "PROJ-9")
}

// 11. Personal git config.
func TestReleaseCutoffPersonalConfig(t *testing.T) {
	d := rv2Repo(t, "db/my migrations")
	cutoffMigrationCommitsAt(d, "db/my migrations", map[int]bool{}, 1)
	rv2Commit(d, "feat: PROJ-1 add 2", map[string]string{"db/my migrations/000002_a[1].up.sql": "PROJ-1\n", "app1.txt": "1\n"})
	rv2Commit(d, "feat: PROJ-9 add 5", map[string]string{"db/my migrations/000005_b.up.sql": "PROJ-9\n", "app9.txt": "1\n"})
	cutoffMigrationCommitsAt(d, "db/my migrations", map[int]bool{}, 3, 4)
	for _, kv := range [][2]string{{"diff.noprefix", "true"}, {"diff.renames", "copies"}, {"color.ui", "always"}, {"diff.relative", "true"}, {"diff.mnemonicPrefix", "true"}, {"diff.external", "false"}, {"core.quotePath", "true"}} {
		d.git("config", kv[0], kv[1])
	}
	out := rv2Plan(t, d)
	if !strings.Contains(out, "Migration cutoff conflict") {
		t.Errorf("FINDING: no conflict with personal config")
	}
	rv2Check(t, d, out, "db/my migrations", "")
}

// 12. Keep whole of an item with a migration-only commit.
func TestReleaseCutoffKeepWholeRun(t *testing.T) {
	d := rv2Repo(t, "migrations")
	cutoffMigrationCommits(d, map[int]bool{}, 1, 2)
	rv2Commit(d, "feat: PROJ-9 add 3", map[string]string{"migrations/000003_step.up.sql": "PROJ-9\n", "app9.txt": "1\n"})
	cutoffMigrationCommits(d, map[int]bool{}, 4)
	rv2Commit(d, "feat: PROJ-1 edit 1, add 5", map[string]string{"migrations/000001_step.up.sql": "edited\n", "migrations/000005_step.up.sql": "PROJ-1\n"})
	rv2Commit(d, "feat: PROJ-3 add 6", map[string]string{"migrations/000006_step.up.sql": "PROJ-3\n", "app3.txt": "1\n"})
	rv2Commit(d, "feat: PROJ-9 add 7 code", map[string]string{"migrations/000007_step.up.sql": "PROJ-9\n", "app9.txt": "2\n"})
	out := rv2Plan(t, d)
	rv2Check(t, d, out, "migrations", "")
	rv2Check(t, d, out, "migrations", "PROJ-1")
	rv2Check(t, d, out, "migrations", "PROJ-9")
}

// 13. Fixed point needs a second pass: a revert that does not apply keeps
// migration 9 in the release only after it is excluded, which raises the
// highest above 5, which PROJ-9 adds. (Kills the mutant that stops the loop
// after the first pass with an exclusion.)
func TestReleaseCutoffFixedPointSecondPass(t *testing.T) {
	d := rv2Repo(t, "migrations")
	cutoffMigrationCommits(d, map[int]bool{}, 1, 2, 3, 4)
	rv2Commit(d, "feat: PROJ-8 add 9", map[string]string{"migrations/000009_q.up.sql": "v0\n"})
	rv2Commit(d, "feat: PROJ-1 drop 9", map[string]string{"migrations/000009_q.up.sql": "", "app1.txt": "1\n"})
	rv2Commit(d, "feat: PROJ-9 add 5", map[string]string{"migrations/000005_x.up.sql": "p9\n", "app9.txt": "1\n"})
	rv2Commit(d, "feat: PROJ-3 re-add 9", map[string]string{"migrations/000009_q.up.sql": "v5\n", "app3.txt": "1\n"})
	rv2Commit(d, "feat: PROJ-1 code", map[string]string{"app1.txt": "2\n"})
	out := rv2Plan(t, d)
	if !strings.Contains(out, "migrations/000005_x.up.sql (number 5)") {
		t.Errorf("FINDING: migration 5 not flagged although 9 stays")
	}
	rv2Check(t, d, out, "migrations", "")
}

func TestReleaseCutoffBinaryMigration(t *testing.T) {
	d := rv2Repo(t, "migrations")
	cutoffMigrationCommits(d, map[int]bool{}, 1)
	rv2Commit(d, "feat: PROJ-1 add binary migration", map[string]string{
		"migrations/000002_binary.up.sql": "select 2;\x00\n",
		"app1.txt":                        "feature\n",
	})
	cutoffMigrationCommits(d, map[int]bool{}, 3)
	out := cutoffOutput(t, d)
	t.Logf("plan:\n%s", out)
	if strings.Contains(out, "migration check unavailable") || !strings.Contains(out, "Migration cutoff conflict") {
		t.Errorf("binary migration below 3 was not protected")
	}
	rv2Check(t, d, out, "migrations", "")
}

func TestReleaseCutoffZeroContextConfig(t *testing.T) {
	d := rv2Repo(t, "migrations")
	cutoffMigrationCommits(d, map[int]bool{}, 1, 2, 3)
	rv2Commit(d, "feat: PROJ-1 edit migration", map[string]string{
		"migrations/000002_step.up.sql": "different\n",
		"app1.txt":                      "feature\n",
	})
	d.git("config", "diff.context", "0")
	out := cutoffOutput(t, d)
	t.Logf("plan:\n%s", out)
	if strings.Contains(out, "migration check unavailable") {
		t.Errorf("zero context made migration check unavailable")
	}
	rv2Check(t, d, out, "migrations", "")
}

func TestReleaseCutoffBinaryAssetInRevert(t *testing.T) {
	d := rv2Repo(t, "migrations")
	cutoffMigrationCommits(d, map[int]bool{}, 1)
	rv2Commit(d, "feat: PROJ-1 add migration and asset", map[string]string{
		"migrations/000002_binary.up.sql": "select 2;\n",
		"assets/icon.bin":                 "binary\x00content\n",
	})
	cutoffMigrationCommits(d, map[int]bool{}, 3)
	out := cutoffOutput(t, d)
	t.Logf("plan:\n%s", out)
	rv2Check(t, d, out, "migrations", "")
}

func TestReleaseCutoffBinaryHighestStays(t *testing.T) {
	d := rv2Repo(t, "migrations")
	cutoffMigrationCommits(d, map[int]bool{}, 1)
	rv2Commit(d, "feat: PROJ-8 mark SQL binary", map[string]string{".gitattributes": "migrations/*.sql -diff\n"})
	rv2Commit(d, "feat: PROJ-1 add migration", map[string]string{
		"migrations/000002_binary.up.sql": "select 2;\n",
		"app1.txt":                        "feature\n",
	})
	out := cutoffOutput(t, d)
	t.Logf("plan:\n%s", out)
	if strings.Contains(out, "stays in the release") {
		t.Errorf("binary migration is unnecessarily shipped: no later commit touches it")
	}
	rv2Check(t, d, out, "migrations", "")
}

func TestReleaseCutoffDuplicateMigrationBlobs(t *testing.T) {
	d := rv2Repo(t, "migrations")
	cutoffMigrationCommits(d, map[int]bool{}, 1)
	rv2Commit(d, "feat: PROJ-8 add 2a", map[string]string{"migrations/000002_a.up.sql": "select 2;\n"})
	rv2Commit(d, "feat: PROJ-1 add 2b", map[string]string{"migrations/000002_b.up.sql": "select 2;\n", "app1.txt": "feature\n"})
	cutoffMigrationCommits(d, map[int]bool{}, 3)
	out := cutoffOutput(t, d)
	t.Logf("plan:\n%s", out)
	if failed := rv2Exec(t, d, out, ""); failed != "" {
		t.Fatalf("plan failed: %s", failed)
	}
	paths := d.git("ls-tree", "-r", "--name-only", "HEAD", "--", "migrations")
	if !strings.Contains(paths, "migrations/000002_b.up.sql") {
		t.Errorf("plan silently deleted candidate migration 2b below remaining 3: %s", paths)
	}
}
