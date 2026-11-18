package app

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

func TestCutoffRenameReapplyWarning(t *testing.T) {
	for _, edit := range []bool{true, false} {
		name := "rename with edit"
		if !edit {
			name = "rename without edit"
		}
		t.Run(name, func(t *testing.T) {
			r, sha, candidate, cfg := cutoffRenameFixture(t, "rename", edit)
			output := cutoffRenamePlanOutput(t, r.repo, cfg, sha, candidate)
			for _, want := range []string{"db/migrations/003_init.up.sql returns in the release", "db/migrations/006_init.up.sql", "PROJ-123", "prod would apply it twice"} {
				if !strings.Contains(output, want) {
					t.Errorf("cutoff plan missing %q:\n%s", want, output)
				}
			}
		})
	}
}

func TestCutoffNewMigrationDoesNotWarnRenameReapply(t *testing.T) {
	r, sha, candidate, cfg := cutoffRenameFixture(t, "add", false)
	output := cutoffRenamePlanOutput(t, r.repo, cfg, sha, candidate)
	if strings.Contains(output, "prod would apply it twice") {
		t.Fatalf("cutoff plan warned for a new migration without a rename:\n%s", output)
	}
}

func TestCutoffDeleteMigrationDoesNotWarnRenameReapply(t *testing.T) {
	r, sha, candidate, cfg := cutoffRenameFixture(t, "delete", false)
	output := cutoffRenamePlanOutput(t, r.repo, cfg, sha, candidate)
	if strings.Contains(output, "prod would apply it twice") {
		t.Fatalf("cutoff plan warned for a deletion without a replacement:\n%s", output)
	}
}

func TestCutoffRenameRevertedWithinTaskDoesNotWarnReapply(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	body := cutoffMigrationBody("old")
	r.commitText("db/migrations/003_init.up.sql", body, "accepted migration 3")
	r.writePath("db/migrations/003_init.up.sql", nil)
	r.writePath("db/migrations/006_init.up.sql", &body)
	renameSHA := r.commitAll("PROJ-123 rename migration")
	r.writePath("db/migrations/006_init.up.sql", nil)
	r.writePath("db/migrations/003_init.up.sql", &body)
	undoSHA := r.commitAll("PROJ-123 restore migration name")
	cfg := &config.Config{TestDB: &config.TestDB{Migrations: "db/migrations"}}
	failures := []gateFailure{
		{commit: deliveredCommit{sha: undoSHA, subject: "restore migration name"}, keys: []string{"PROJ-123"}, why: "not checked"},
		{commit: deliveredCommit{sha: renameSHA, subject: "rename migration"}, keys: []string{"PROJ-123"}, why: "not checked"},
	}
	output := cutoffRenamePlanOutputFailures(t, r.repo, cfg, failures, undoSHA)
	if strings.Contains(output, "prod would apply it twice") {
		t.Fatalf("cutoff plan warned after the rename was undone in trunk:\n%s", output)
	}
}

func cutoffRenameFixture(t *testing.T, change string, edit bool) (*withheldRepo, string, string, *config.Config) {
	t.Helper()
	r := newEmptyWithheldRepo(t)
	r.commitText("db/migrations/003_init.up.sql", cutoffMigrationBody("old"), "accepted migration 3")
	switch change {
	case "rename":
		body := cutoffMigrationBody("old")
		r.writePath("db/migrations/003_init.up.sql", nil)
		if edit {
			body = strings.Replace(body, "-- migration statement 00 old", "-- migration statement 00 edited", 1)
		}
		r.writePath("db/migrations/006_init.up.sql", &body)
	case "add":
		body := cutoffMigrationBody("old")
		r.writePath("db/migrations/006_init.up.sql", &body)
	case "delete":
		r.writePath("db/migrations/003_init.up.sql", nil)
	default:
		t.Fatalf("unknown change %q", change)
	}
	sha := r.commitAll("PROJ-123 migration change")
	cfg := &config.Config{TestDB: &config.TestDB{Migrations: "db/migrations"}}
	return r, sha, sha, cfg
}

func cutoffMigrationBody(marker string) string {
	var b strings.Builder
	for i := range 30 {
		if i == 0 {
			fmt.Fprintf(&b, "-- migration statement %02d %s\n", i, marker)
		} else {
			fmt.Fprintf(&b, "-- migration statement %02d stable\n", i)
		}
	}
	return b.String()
}

func cutoffRenamePlanOutput(t *testing.T, repo *gitx.Repo, cfg *config.Config, sha, candidate string) string {
	t.Helper()
	failure := gateFailure{commit: deliveredCommit{sha: sha, subject: "migration change"}, keys: []string{"PROJ-123"}, why: "not checked"}
	return cutoffRenamePlanOutputFailures(t, repo, cfg, []gateFailure{failure}, candidate)
}

func cutoffRenamePlanOutputFailures(t *testing.T, repo *gitx.Repo, cfg *config.Config, failures []gateFailure, candidate string) string {
	t.Helper()
	plan, err := cutoffMigrationConflicts(repo, cfg, candidate, failures)
	if err != nil {
		t.Fatal(err)
	}
	if plan.unavailable != "" {
		t.Fatalf("migration plan unavailable: %s", plan.unavailable)
	}
	var out bytes.Buffer
	a := &App{Stdout: &out}
	for _, group := range groupFailures(failures) {
		a.printCutoffGroup(&deployRun{repo: repo, cfg: cfg}, cutoff{}, plan, group, map[string]bool{})
	}
	return out.String()
}
