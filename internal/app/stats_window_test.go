package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/stats"
)

func TestStatsReportsKeepLogStartAcrossHistoryGap(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	dir := filepath.Join(cache, "graft")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	statsPath := filepath.Join(dir, "stats.jsonl")
	testsPath := stats.TestsPath(statsPath)
	since := time.Now().Add(-2 * 24 * time.Hour).Truncate(time.Minute)
	old := since.Add(-3 * 24 * time.Hour)
	recent := since.Add(24 * time.Hour)

	write := func(path string, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(statsPath+".2", stats.Record{Time: old, Kind: stats.KindTask, Project: "/project", Task: "old", Top: "gate", Steps: 1, WallS: 1})
	write(statsPath+".1", stats.Record{Time: recent, Kind: stats.KindTask, Project: "/project", Task: "recent", Top: "gate", Steps: 1, WallS: 1})
	write(testsPath+".2", stats.PackageRun{Time: old, Project: "/project", Task: "gate", Runner: "go", Package: "example.com/old", Result: stats.ResultPass, WallS: 1})
	write(testsPath+".1", stats.PackageRun{Time: recent, Project: "/project", Task: "gate", Runner: "go", Package: "example.com/recent", Result: stats.ResultPass, WallS: 1})
	for _, path := range []string{statsPath + ".2", testsPath + ".2"} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}

	reports := []struct {
		name string
		opt  StatsOptions
		line string
	}{
		{name: "stats", opt: StatsOptions{Since: since, All: true}, line: "graft: tasks in every project since "},
		{name: "failures", opt: StatsOptions{Since: since, All: true, Failures: true}, line: "graft: no failures since "},
		{name: "tests", opt: StatsOptions{Since: since, All: true, Tests: true}, line: "graft: tests in every project since "},
	}
	wantSince := since.Format("2006-01-02 15:04")
	want := make([]string, len(reports))
	run := func(i int) string {
		t.Helper()
		var out, stderr bytes.Buffer
		if err := (&App{Stdout: &out, Stderr: &stderr}).Stats(reports[i].opt); err != nil {
			t.Fatalf("%s: %v", reports[i].name, err)
		}
		if !strings.Contains(out.String(), reports[i].line+wantSince) || strings.Contains(out.String(), "the log starts there") {
			t.Fatalf("%s output has wrong log start:\n%s", reports[i].name, out.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("%s reported unread stale data: %s", reports[i].name, stderr.String())
		}
		return out.String()
	}
	for i := range reports {
		want[i] = run(i)
	}

	for _, path := range []string{statsPath + ".2", testsPath + ".2"} {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := f.WriteString(strings.Repeat("not-json\n", 4096))
		closeErr := f.Close()
		if writeErr != nil {
			t.Fatal(writeErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	for i := range reports {
		if got := run(i); got != want[i] {
			t.Errorf("%s changed after junk was appended to the stale copy:\nwant:\n%s\ngot:\n%s", reports[i].name, want[i], got)
		}
	}
}
