package app

import (
	"slices"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
)

func TestReviewerReminderExcludesWorkReleasedFromEarlierHold(t *testing.T) {
	repo := newWithheldRepo(t, "base\n")
	base := repo.head()
	released := repo.commitText("released.txt", "released work\n", "feat: PROJ-123 released work")
	run := &deployRun{
		repo:     repo.repo,
		cfg:      &config.Config{ReleaseNotes: &config.ReleaseNotes{Jira: &config.JiraNotes{ProjectKeys: []string{"PROJ"}}}},
		target:   &config.DeployTarget{},
		prev:     base,
		head:     base,
		released: []string{released},
	}

	keys, err := reviewerReminderExcludedKeys(run)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(keys, "PROJ-123") {
		t.Fatalf("released work excluded keys = %v, want PROJ-123", keys)
	}
}
