package stats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadTailReadsRecordsAfterLargePrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	previous := Record{
		Time:    time.Now().Add(-time.Minute),
		Kind:    KindTask,
		Project: "projectA",
		Task:    "check",
		Exit:    1,
		Reason:  "test failed",
	}
	old := previous
	old.Task = "old-check"
	old.Time = previous.Time.Add(-time.Hour)
	oldLine, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(previous)
	if err != nil {
		t.Fatal(err)
	}
	data := string(oldLine) + "\n" + strings.Repeat("x", 4<<20) + "\n" + string(line) + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	got, malformed, err := LoadTail(path, 1<<20)
	if err != nil {
		t.Fatalf("LoadTail: %v", err)
	}
	if malformed != 0 || len(got) != 1 || got[0].Task != "check" {
		t.Fatalf("LoadTail returned %d records and %d malformed lines", len(got), malformed)
	}
	current := previous
	current.Time = time.Now()
	if _, count, same := SameFailure(got, current); !same || count != 2 {
		t.Fatalf("SameFailure(%v, current) = %d, %v; want 2, true", got, count, same)
	}
}

func TestRepeatFailureLineUsesMinutesAndSeconds(t *testing.T) {
	if got, want := RepeatFailureLine(3*time.Minute, 2), "same reason as 3 min ago, 2 times in a row"; got != want {
		t.Errorf("RepeatFailureLine(3m, 2) = %q, want %q", got, want)
	}
	if got, want := RepeatFailureLine(42*time.Second, 2), "same reason as 42 sec ago, 2 times in a row"; got != want {
		t.Errorf("RepeatFailureLine(42s, 2) = %q, want %q", got, want)
	}
}

// A run whose failed step ended before since and whose task ended after it
// shows by its task: the step outside the window does not stand for it.
func TestFailureRunsAcrossSince(t *testing.T) {
	now := time.Now()
	recs := []Record{
		{Time: now.Add(-61 * time.Minute), Kind: KindStep, Project: "projectA", Task: "gate", Run: "r1", Exit: 1},
		{Time: now.Add(-59 * time.Minute), Kind: KindTask, Project: "projectA", Task: "gate", Run: "r1", Exit: 1},
	}
	got := FailureRuns(recs, Filter{Since: now.Add(-time.Hour)})
	if len(got) != 1 || got[0].Kind != KindTask {
		t.Fatalf("want the task failure of the run, got %+v", got)
	}
}
