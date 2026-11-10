package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/stats"
)

func TestStatsFailures(t *testing.T) {
	f, _ := statsRepo(t, "tasks:\n  broken:\n    run: ["+helperStep("fail")+"]\n")
	if _, code := f.graft("", "broken"); code == 0 {
		t.Fatal("broken task passed")
	}
	out := f.mustGraft("stats", "--failures", "--task", "broken", "--since", "1d")
	for _, want := range []string{"PROJECT/TREE", "broken", "failing on purpose", "1", "\n    gatehelper: failing on purpose"} {
		if !strings.Contains(out, want) {
			t.Errorf("graft stats --failures missing %q:\n%s", want, out)
		}
	}
}

func TestStatsFailuresSinceFiltersRotatedRecords(t *testing.T) {
	f, statsFile := statsRepo(t, "")
	if err := os.MkdirAll(filepath.Dir(statsFile), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	write := func(path, task, reason string, at time.Time) {
		t.Helper()
		rec := stats.Record{Time: at, Kind: stats.KindTask, Project: "projectA", Task: task, Exit: 1, Run: task, Reason: reason}
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	write(statsFile+".1", "old-failure", "old failure marker", now.Add(-2*time.Hour))
	write(statsFile, "fresh-failure", "fresh failure marker", now.Add(-10*time.Minute))
	out := f.mustGraft("stats", "--failures", "--all", "--since", "1h")
	if !strings.Contains(out, "fresh failure marker") {
		t.Errorf("fresh failure missing from the report:\n%s", out)
	}
	if strings.Contains(out, "old failure marker") {
		t.Errorf("failure older than --since appeared in the report:\n%s", out)
	}
}

func TestGateSaysTheSameReason(t *testing.T) {
	f := tasksRepo(t, "gate:\n  - task: broken\ntasks:\n  broken:\n    run: ["+helperStep("fail")+"]\n", nil)
	for attempt := 1; attempt <= 2; attempt++ {
		out, code := f.graft("", "gate")
		if code == 0 || !strings.Contains(out, "see graft stats --failures") {
			t.Fatalf("gate attempt %d: exit %d\n%s", attempt, code, out)
		}
		if attempt == 2 && (!strings.Contains(out, "same reason as") || !strings.Contains(out, "2 times in a row")) {
			t.Errorf("second gate failure did not identify the repeat:\n%s", out)
		}
	}
}

func TestReasonSkipsGraftAndWarnings(t *testing.T) {
	got := stats.CleanReason("graft: running step\nwarning: result cache off\nsource.go:8: failed check\n")
	if got != "source.go:8: failed check" {
		t.Fatalf("CleanReason() = %q", got)
	}
}
