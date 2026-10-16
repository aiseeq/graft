package stats

import (
	"strings"
	"testing"
)

func ptr(v float64) *float64 { return &v }

// TestReportShowsTheDatabase: the DB CPU and DB MEM columns appear when a
// task recorded its test database, the median CPU and the largest memory of
// its runs, and stay out of a report without one.
func TestReportShowsTheDatabase(t *testing.T) {
	recs := []Record{
		{Kind: KindTask, Task: "test", WallS: 10, DBCPUS: ptr(30), DBMemMB: ptr(300)},
		{Kind: KindTask, Task: "test", WallS: 12, DBCPUS: ptr(50), DBMemMB: ptr(700)},
		{Kind: KindTask, Task: "test", WallS: 11, DBCPUS: ptr(40), DBMemMB: ptr(500)},
		{Kind: KindTask, Task: "lint", WallS: 3},
	}
	render := func(recs []Record) string {
		t.Helper()
		rows, _ := Summarize(recs, Filter{})
		var b strings.Builder
		if err := Render(&b, rows, "TASK", false); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	got := render(recs)
	if !strings.Contains(got, "DB CPU") || !strings.Contains(got, "DB MEM") {
		t.Fatalf("no database columns:\n%s", got)
	}
	test, lint := line(got, "test"), line(got, "lint")
	if !strings.HasSuffix(test, "40s     700 MB") || !strings.HasSuffix(lint, "-       -") {
		t.Errorf("database columns:\n%s", got)
	}
	if got := render(recs[3:]); strings.Contains(got, "DB CPU") {
		t.Errorf("database columns without a database:\n%s", got)
	}
}

func line(table, label string) string {
	for l := range strings.Lines(table) {
		if strings.HasPrefix(l, label+" ") {
			return strings.TrimRight(l, " \n")
		}
	}
	return ""
}

// TestFormatSaved counts the skipped runs in words that agree with the
// number.
func TestFormatSaved(t *testing.T) {
	if got := FormatSaved(Saved{Hits: 1, WallS: 2, CPUS: 1}); !strings.HasPrefix(got, "1 run skipped ") {
		t.Errorf("one: %s", got)
	}
	if got := FormatSaved(Saved{Hits: 3, WallS: 2, CPUS: 1}); !strings.HasPrefix(got, "3 runs skipped ") {
		t.Errorf("three: %s", got)
	}
}
