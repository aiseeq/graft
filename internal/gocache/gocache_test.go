package gocache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cacheDir makes a Go build cache with entries of the given ages and the
// cache's own files, all as old as the oldest entry.
func cacheDir(t *testing.T, now time.Time, ages map[string]time.Duration) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, data string, age time.Duration) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	for rel, age := range ages {
		write(rel, "entry", age)
	}
	week := 7 * 24 * time.Hour
	write("README", readme+"\nRun \"go clean -cache\" if the directory is getting too large.\n", week)
	write("trim.txt", "1700000000", week)
	write("testexpire.txt", "1700000000", week)
	write("fuzz/example.com/m/FuzzX/0123", "go test fuzz v1\n", week)
	write("0a/notes", "not an entry", week)
	return dir
}

func TestTrimRemovesTheEntriesUnusedForLongerThanTheAge(t *testing.T) {
	now := time.Date(2026, 10, 28, 17, 0, 0, 0, time.UTC)
	ages := map[string]time.Duration{
		"00/" + strings.Repeat("0", 64) + "-a": 30 * time.Hour,
		"00/" + strings.Repeat("1", 64) + "-d": 30 * time.Hour,
		"ff/" + strings.Repeat("f", 64) + "-d": 26 * time.Hour,
		"0a/" + strings.Repeat("a", 64) + "-a": 2 * time.Hour,
		"0a/" + strings.Repeat("b", 64) + "-d": 24 * time.Hour,
	}
	old := []string{"00/" + strings.Repeat("0", 64) + "-a", "00/" + strings.Repeat("1", 64) + "-d", "ff/" + strings.Repeat("f", 64) + "-d"}
	kept := []string{"0a/" + strings.Repeat("a", 64) + "-a", "0a/" + strings.Repeat("b", 64) + "-d",
		"README", "trim.txt", "testexpire.txt", "fuzz/example.com/m/FuzzX/0123", "0a/notes"}
	want := Result{Removed: 3, Kept: 2, RemovedBytes: 3 * 5, KeptBytes: 2 * 5}

	dir := cacheDir(t, now, ages)
	r, err := Trim(dir, 25*time.Hour, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if r != want {
		t.Errorf("report: %+v, want %+v", r, want)
	}
	for _, rel := range append(old, kept...) {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Errorf("without apply %s is gone: %v", rel, err)
		}
	}

	r, err = Trim(dir, 25*time.Hour, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if r != want {
		t.Errorf("apply: %+v, want %+v", r, want)
	}
	for _, rel := range old {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("%s not removed: %v", rel, err)
		}
	}
	for _, rel := range kept {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s removed: %v", rel, err)
		}
	}
	if r, err := Trim(dir, 25*time.Hour, now, true); err != nil || r != (Result{Kept: 2, KeptBytes: 10}) {
		t.Errorf("second trim: %+v, %v", r, err)
	}
}

func TestTrimRefusesADirectoryThatIsNotAGoCache(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	entry := filepath.Join(dir, "00", strings.Repeat("0", 64)+"-a")
	if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(entry, now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"", "some other README\n"} {
		if data != "" {
			if err := os.WriteFile(filepath.Join(dir, "README"), []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := Trim(dir, 25*time.Hour, now, true); err == nil || !strings.Contains(err.Error(), "not a Go build cache") {
			t.Errorf("README %q: %v", data, err)
		}
		if _, err := os.Stat(entry); err != nil {
			t.Errorf("README %q: the entry removed: %v", data, err)
		}
	}
}
