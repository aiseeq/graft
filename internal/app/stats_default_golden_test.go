package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/stats"
)

func TestStatsAllDefaultMatchesGolden(t *testing.T) {
	previousLocal := time.Local
	time.Local = time.FixedZone("CET", 60*60)
	t.Cleanup(func() { time.Local = previousLocal })
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	path := filepath.Join(cache, "graft", "stats.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	records := []stats.Record{{
		Time: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC), Kind: stats.KindTask,
		Project: "/tmp/projectA", Profile: "projectA:build", Task: "build", Top: "build",
		Run: "golden", WallS: 5, UserS: 2, TreeMemMB: new(float64(100)),
	}, {
		Time: time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC), Kind: stats.KindTask,
		Project: "/tmp/worktrees/projectA/topic", Profile: "projectA:build", Task: "build", Top: "build",
		Run: "golden-copy", WallS: 7, UserS: 3, TreeMemMB: new(float64(200)),
	}}
	var journal bytes.Buffer
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		journal.Write(line)
		journal.WriteByte('\n')
	}
	if err := os.WriteFile(path, journal.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	since := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	var out, stderr bytes.Buffer
	if err := (&App{Stdout: &out, Stderr: &stderr}).Stats(StatsOptions{Since: since, All: true}); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
	goldenPath := filepath.Join("testdata", "stats-all-default.golden")
	if os.Getenv("UPDATE_STATS_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("default --all output differs from its pre-grouping golden\ngot:\n%s\nwant:\n%s", out.Bytes(), want)
	}
}
