package stats

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// TestReportShowsTheDatabase: the DB CPU and DB MEM columns appear when a
// task recorded its test database, the median CPU and the largest memory of
// its runs, and stay out of a report without one.
func TestReportShowsTheDatabase(t *testing.T) {
	recs := []Record{
		{Kind: KindTask, Task: "test", WallS: 10, DBCPUS: new(float64(30)), DBMemMB: new(float64(300))},
		{Kind: KindTask, Task: "test", WallS: 12, DBCPUS: new(float64(50)), DBMemMB: new(float64(700))},
		{Kind: KindTask, Task: "test", WallS: 11, DBCPUS: new(float64(40)), DBMemMB: new(float64(500))},
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

func TestReportFiltersTaskByIn(t *testing.T) {
	recs := []Record{
		{Kind: KindTask, Task: "lint", In: "smoke", Run: "r1", WallS: 10},
		{Kind: KindStep, Task: "lint", Step: "check", Run: "r1", WallS: 9},
		{Kind: KindTask, Task: "lint", In: "deploy", Run: "r2", WallS: 20},
		{Kind: KindStep, Task: "lint", Step: "check", Run: "r2", WallS: 19},
		{Kind: KindTask, Task: "lint", Run: "r3", WallS: 30},
		{Kind: KindTask, Task: "test", In: "smoke", Run: "r1", WallS: 40},
	}
	rows, _ := Summarize(recs, Filter{Task: "lint", In: "smoke"})
	got := map[string]Row{}
	for _, row := range rows {
		got[row.Label] = row
	}
	if len(rows) != 2 || got[TaskLabel].Runs != 1 || got[TaskLabel].WallMed != 10 || got["check"].Runs != 1 || got["check"].WallMed != 9 {
		t.Errorf("report rows = %+v, want the lint run in smoke and its step", rows)
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

// TestOverLimits: the gate row takes the gate's limit, the others the
// task's, both by the median without the waits, so a run that only waited
// long is not over.
func TestOverLimits(t *testing.T) {
	recs := []Record{
		{Kind: KindTask, Task: GateTask, WallS: 70},
		{Kind: KindTask, Task: GateTask, WallS: 79, WaitS: 5},
		{Kind: KindTask, Task: "test", WallS: 400, WaitS: 300},
		{Kind: KindTask, Task: "e2e", WallS: 500},
	}
	rows, _ := Summarize(recs, Filter{})
	over := OverLimits(rows, time.Minute, 8*time.Minute)
	var got []string
	for _, o := range over {
		got = append(got, FormatOverLimit(o))
	}
	// The gate is 20% over, e2e 4%: the farthest over first.
	want := []string{"over the limit: gate takes 1m12s by median without waits, above 1m0s (user config limits.gate)",
		"over the limit: e2e takes 8m20s by median without waits, above 8m0s (user config limits.task)"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}
