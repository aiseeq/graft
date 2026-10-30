package stats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// writeLog writes one task line at at to path, dated at too.
func writeLog(t *testing.T, path, task string, at time.Time) {
	t.Helper()
	line, err := json.Marshal(Record{Kind: KindTask, Task: task, Time: at})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// TestLoadReadsTheCopiesOldestFirst: the old copies come before the file,
// the highest number first; a copy that ends before since is left out,
// except the oldest, which tells where the log starts.
func TestLoadReadsTheCopiesOldestFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	now := time.Now()
	day := 24 * time.Hour
	writeLog(t, path+".3", "a", now.Add(-5*day))
	writeLog(t, path+".2", "b", now.Add(-3*day))
	writeLog(t, path+".1", "c", now.Add(-day))
	writeLog(t, path, "d", now)
	tasks := func(since time.Time) []string {
		recs, malformed, err := Load(path, since)
		if err != nil || malformed != 0 {
			t.Fatalf("Load: %v, %d malformed", err, malformed)
		}
		var names []string
		for _, r := range recs {
			names = append(names, r.Task)
		}
		return names
	}
	if got := tasks(time.Time{}); !slices.Equal(got, []string{"a", "b", "c", "d"}) {
		t.Errorf("every copy: %v", got)
	}
	if got := tasks(now.Add(-2 * day)); !slices.Equal(got, []string{"a", "c", "d"}) {
		t.Errorf("since 2 days ago: %v, want the oldest copy and the ones that reach it", got)
	}
}

// TestAppendWritesWhenAnOldCopyStays: an expired copy that cannot be
// removed is reported, and the line is written all the same.
func TestAppendWritesWhenAnOldCopyStays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	writeLog(t, path, "full", time.Now())
	// A directory with a file in it is not removed.
	stuck := path + ".1"
	if err := os.MkdirAll(filepath.Join(stuck, "held"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-Retention - 24*time.Hour)
	if err := os.Chtimes(stuck, old, old); err != nil {
		t.Fatal(err)
	}
	err := Append(path, Record{Kind: KindTask, Task: "next", Time: time.Now()}, 1)
	if err == nil || !strings.Contains(err.Error(), "removing the old copy") {
		t.Errorf("Append: %v, want the copy it could not remove", err)
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(data), `"task":"next"`) {
		t.Errorf("the line was not written: %q", data)
	}
	if _, serr := os.Stat(path + ".2"); serr != nil {
		t.Errorf("the stuck copy was not shifted to .2: %v", serr)
	}
}
