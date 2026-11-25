package stats

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadWindowAppliesByteAgeAndNeedleLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	now := time.Now().UTC()
	write := func(project string, at time.Time, run string) []byte {
		t.Helper()
		b, err := json.Marshal(Record{Time: at, Kind: KindStep, Task: "gate", Project: project, Profile: "repo:gate", Step: "check", Run: run})
		if err != nil {
			t.Fatal(err)
		}
		return append(b, '\n')
	}
	data := append(write("repoA", now.Add(-time.Hour), "fresh"), write("repoB", now.Add(-time.Minute), "foreign")...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	rows, _, err := LoadWindow(path, now.Add(-24*time.Hour), "repoA", int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Run != "fresh" {
		t.Fatalf("window rows = %+v, want only fresh matching row", rows)
	}
	rows, _, err = LoadWindow(path, now.Add(-time.Minute), "repoA", int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("expired row remains: %+v", rows)
	}
	rows, _, err = LoadWindow(path, now.Add(-24*time.Hour), "repoA", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("row beyond raw byte limit returned: %+v", rows)
	}
}

func TestLoadTailMatchingHandlesEscapedProfileNeedles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	rec := Record{Time: time.Now(), Kind: KindStep, Task: "gate", Project: "repo", Profile: `repo:<&"gate`, Step: "check"}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, _, err := LoadTailMatching(path, 1<<20, `repo:<&"`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Profile != rec.Profile {
		t.Fatalf("escaped needle records = %+v", rows)
	}
}

func TestDecodeRecordsAcceptsObjectDelimiterInString(t *testing.T) {
	line := objectDelimiterStepRecord(t, "projectA", "projectA:gate", "wanted")
	rows, malformed, err := decodeRecords(bytes.NewReader(line))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || malformed != 0 {
		t.Fatalf("full read returned %d rows and %d malformed lines, want 1 and 0", len(rows), malformed)
	}
}

func TestDecodeRecordsRejectsConcatenatedObjects(t *testing.T) {
	line := objectDelimiterStepRecord(t, "projectA", "projectA:gate", "wanted")
	joined := append(bytes.TrimSuffix(line, []byte{'\n'}), line...)
	rows, malformed, err := decodeRecords(bytes.NewReader(joined))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || malformed != 1 {
		t.Fatalf("full read returned %d rows and %d malformed lines, want 0 and 1", len(rows), malformed)
	}
}

func TestDecodeRecordsProjectAcceptsForeignObjectDelimiterInString(t *testing.T) {
	line := objectDelimiterStepRecord(t, "projectB", "projectB:gate", "other")
	rows, malformed, err := decodeRecordsProject(bytes.NewReader(line), "projectA")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || malformed != 0 {
		t.Fatalf("project-filtered read returned %d rows and %d malformed lines, want 0 and 0", len(rows), malformed)
	}
}

func TestDecodePackageRunsProjectAcceptsForeignObjectDelimiterInString(t *testing.T) {
	line, err := json.Marshal(PackageRun{
		Time:    time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC),
		Project: "projectB",
		Task:    "other",
		Top:     "gate",
		Runner:  "go",
		Package: "example/other",
		Result:  ResultPass,
		Reason:  "valid writer text: }{",
	})
	if err != nil {
		t.Fatal(err)
	}
	line = append(line, '\n')
	runs, malformed, err := decodePackageRunsProject(bytes.NewReader(line), "projectA")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 || malformed != 0 {
		t.Fatalf("project-filtered package read returned %d rows and %d malformed lines, want 0 and 0", len(runs), malformed)
	}
}

func TestLoadTailMatchingAcceptsForeignObjectDelimiterInString(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	line := objectDelimiterStepRecord(t, "projectB", "projectB:gate", "other")
	if err := os.WriteFile(path, line, 0o600); err != nil {
		t.Fatal(err)
	}
	rows, malformed, err := LoadTailMatching(path, int64(len(line)), "projectA:gate")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || malformed != 0 {
		t.Fatalf("tail read returned %d rows and %d malformed lines, want 0 and 0", len(rows), malformed)
	}
}

func TestLoadWindowDeduplicatesInodeAndRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	rec := Record{Time: time.Now().UTC(), Kind: KindStep, Task: "gate", Project: "repo", Profile: "repo:gate", Step: "check", Run: "same"}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	data := append(append(append([]byte{}, b...), '\n'), append(b, '\n')...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	rows, _, err := LoadWindow(path, time.Time{}, "repo:", int64(len(data))*2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("duplicate rows = %d, want one", len(rows))
	}
}

func TestLoadWindowKeepsOpenedFilesAcrossRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	now := time.Now().UTC()
	row := func(run string, wall float64) []byte {
		t.Helper()
		b, err := json.Marshal(Record{Time: now, Kind: KindStep, Task: "gate", Project: "repo", Profile: "repo:gate", Step: run, Run: run, WallS: wall})
		if err != nil {
			t.Fatal(err)
		}
		return append(b, '\n')
	}
	lows := append(append(row("one", 1), row("two-low", 1)...), row("three-low", 1)...)
	if err := os.WriteFile(path+".1", lows, 0o600); err != nil {
		t.Fatal(err)
	}
	current := append(append(append([]byte{}, lows...), row("four", 20)...), row("five", 20)...)
	if err := os.WriteFile(path, current, 0o600); err != nil {
		t.Fatal(err)
	}
	loadBoundedAfterOpen = func() {
		if err := shift(path); err != nil {
			t.Errorf("rotate during read: %v", err)
		}
	}
	defer func() { loadBoundedAfterOpen = nil }()
	rows, _, err := LoadWindow(path, time.Time{}, "repo:", int64(len(current)+len(lows)))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("records across rotation = %d, want 5 unique rows: %+v", len(rows), rows)
	}
	var walls []float64
	for _, rec := range rows {
		walls = append(walls, rec.WallS)
	}
	if got := Median(walls); got != 1 {
		t.Fatalf("median after rotation = %v, want 1 from [1 1 1 20 20]", got)
	}
}

func TestLoadWindowReportsTruncationAfterOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	makeRow := func(step string, wall float64) []byte {
		b, err := json.Marshal(Record{Time: time.Now(), Kind: KindStep, Task: "gate", Project: "repo", Profile: "repo:gate", Step: step, Run: step, WallS: wall})
		if err != nil {
			t.Fatal(err)
		}
		return append(b, '\n')
	}
	data := append(append([]byte(nil), makeRow("one", 1)...), makeRow("two", 2)...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loadBoundedAfterOpen = func() {
		if err := os.Truncate(path, 0); err != nil {
			t.Errorf("truncate: %v", err)
		}
	}
	defer func() { loadBoundedAfterOpen = nil }()
	_, _, err := LoadWindow(path, time.Time{}, "repo", int64(len(data)))
	if err == nil || !strings.Contains(err.Error(), "short read") {
		t.Fatalf("truncated journal error = %v, want explicit short read", err)
	}
}

func TestLoadWindowMatchingCountsMalformedJournalShapedRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	line := `{"time":"2026-11-24T12:00:00Z","kind":"step","task":"other","profile":"foreign:gate","step":"other","wall_s":oops}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, malformed, err := LoadWindowMatching(path, time.Time{}, []RecordMatch{{Profile: "repo:gate", Step: "wanted"}}, int64(len(line)))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || malformed != 1 {
		t.Fatalf("matching read returned %d rows and %d malformed lines, want 0 and 1", len(rows), malformed)
	}
}

func TestLoadWindowMatchingCountsMalformedForeignRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	data := "{\"time\":\"2026-11-24T12:00:00Z\",\"kind\":\"\",\"task\":\"other\",\"profile\":\"foreign:gate\"}\n" +
		"{\"time\":\"not-a-time\",\"kind\":\"step\",\"task\":\"other\",\"profile\":\"foreign:gate\"}\n" +
		"{\"time\":\"2026-11-24T12:00:00Z\",\"kind\":null,\"task\":\"other\",\"profile\":\"foreign:gate\"}\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, malformed, err := LoadWindowMatching(path, time.Time{}, []RecordMatch{{Profile: "repo:gate", Step: "wanted"}}, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || malformed != 3 {
		t.Fatalf("foreign rows = %d records and %d malformed lines, want 0 and 3", len(rows), malformed)
	}
}

func TestLoadWindowMatchingRejectsForeignRecordWithOnlyNestedTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	line := `{"time":123,"kind":"step","task":"other","profile":"foreign:gate","meta":{"time":"2026-11-24T12:00:00Z"}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, malformed, err := LoadWindowMatching(path, time.Time{}, []RecordMatch{{Profile: "repo:gate", Step: "wanted"}}, int64(len(line)))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || malformed != 1 {
		t.Fatalf("matching read returned %d rows and %d malformed lines, want 0 and 1", len(rows), malformed)
	}
}

func TestLoadWindowMatchingAcceptsForeignRecordWithoutPackage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	line := `{"time":"2026-11-24T12:00:00Z","kind":"step","task":"other","profile":"foreign:gate"}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, malformed, err := LoadWindowMatching(path, time.Time{}, []RecordMatch{{Profile: "repo:gate", Step: "wanted"}}, int64(len(line)))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || malformed != 0 {
		t.Fatalf("matching read returned %d rows and %d malformed lines, want 0 and 0", len(rows), malformed)
	}
}

func TestLoadWindowMatchingAcceptsForeignRecordWithObjectDelimiterInString(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	line := objectDelimiterStepRecord(t, "projectB", "projectB:gate", "other")
	if err := os.WriteFile(path, line, 0o600); err != nil {
		t.Fatal(err)
	}
	rows, malformed, err := LoadWindowMatching(path, time.Time{}, []RecordMatch{{Profile: "projectA:gate", Step: "wanted"}}, int64(len(line)))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || malformed != 0 {
		t.Fatalf("matching read returned %d rows and %d malformed lines, want 0 and 0", len(rows), malformed)
	}
}

func TestLoadWindowMatchingRejectsConcatenatedForeignObjects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	line := objectDelimiterStepRecord(t, "projectB", "projectB:gate", "other")
	joined := append(bytes.TrimSuffix(line, []byte{'\n'}), line...)
	if err := os.WriteFile(path, joined, 0o600); err != nil {
		t.Fatal(err)
	}
	rows, malformed, err := LoadWindowMatching(path, time.Time{}, []RecordMatch{{Profile: "projectA:gate", Step: "wanted"}}, int64(len(joined)))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || malformed != 1 {
		t.Fatalf("matching read returned %d rows and %d malformed lines, want 0 and 1", len(rows), malformed)
	}
}

func TestLoadWindowMatchingAcceptsMatchedRecordWithObjectDelimiterInString(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	line := objectDelimiterStepRecord(t, "projectA", "projectA:gate", "wanted")
	if err := os.WriteFile(path, line, 0o600); err != nil {
		t.Fatal(err)
	}
	rows, malformed, err := LoadWindowMatching(path, time.Time{}, []RecordMatch{{Profile: "projectA:gate", Step: "wanted"}}, int64(len(line)))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Step != "wanted" || malformed != 0 {
		t.Fatalf("matching read returned %d rows and %d malformed lines, want 1 and 0", len(rows), malformed)
	}
}

func objectDelimiterStepRecord(t *testing.T, project, profile, step string) []byte {
	t.Helper()
	line, err := json.Marshal(Record{
		Time:    time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC),
		Kind:    KindStep,
		Task:    "gate",
		Project: project,
		Profile: profile,
		Step:    step,
		Here:    "valid writer text: }{",
		WallS:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return append(line, '\n')
}

func TestLoadWindowMatchingStopsAfterFiveLocalRowsAcrossProjects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	now := time.Now().UTC()
	var current []byte
	for i := range 5 {
		project := "/repo/a"
		if i%2 != 0 {
			project = "/repo/b"
		}
		row, err := json.Marshal(Record{Time: now.Add(time.Duration(i) * time.Second), Kind: KindStep, Task: "gate", Project: project, Profile: "repo:gate", Step: "check", Run: string(rune('a' + i)), Exit: 0, WallS: 1})
		if err != nil {
			t.Fatal(err)
		}
		current = append(append(current, row...), '\n')
	}
	if err := os.WriteFile(path, current, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".1", []byte("torn older row\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, malformed, err := LoadWindowMatching(path, time.Time{}, []RecordMatch{{Profile: "repo:gate", Step: "check"}}, int64(len(current)+len("torn older row\n")))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 || malformed != 0 {
		t.Fatalf("window returned %d rows and %d malformed lines, want 5 rows and no older read", len(rows), malformed)
	}
}
