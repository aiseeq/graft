package stats

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

// Load reads the old copy of the stats file and the file itself, oldest
// first. Lines that are not records (a torn write of a crashed graft) are
// counted in malformed and left out.
func Load(path string) (recs []Record, malformed int, err error) {
	for _, p := range []string{path + ".1", path} {
		r, m, err := loadFile(p)
		if err != nil {
			return nil, 0, err
		}
		recs = append(recs, r...)
		malformed += m
	}
	return recs, malformed, nil
}

func loadFile(path string) (recs []Record, malformed int, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("reading the stats: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.Kind == "" || r.Task == "" {
			malformed++
			continue
		}
		recs = append(recs, r)
	}
	if err := sc.Err(); err != nil {
		return nil, 0, fmt.Errorf("reading %s: %w", path, err)
	}
	return recs, malformed, nil
}

// Filter selects the records a report covers.
type Filter struct {
	Since time.Time
	// Project is the project root; empty takes every project, one row
	// group each.
	Project string
	// Task, when set, breaks that task down into its steps.
	Task string
	// MaxLoad, when set, takes only the runs whose highest load stayed
	// below it; runs without a load are left out.
	MaxLoad float64
}

// calm tells whether a run with peak load stays under maxLoad, 0 taking
// every run.
func calm(peak *float64, maxLoad float64) bool {
	return maxLoad == 0 || (peak != nil && *peak < maxLoad)
}

// TaskLabel is the label of the whole-task row of a task broken down into
// its steps.
const TaskLabel = "(task)"

// Row is one line of a report: the runs of a task, or of a step.
type Row struct {
	Project string // set when the report covers every project
	Label   string // the task, or the step of the broken down task
	Runs    int
	WallMed float64
	WallMax float64
	CPUMed  float64
	// MaxRSSMB is the largest peak of a run; nil when none was reported.
	MaxRSSMB *float64
	// TreeMemMB is the largest peak of a run's process tree as a whole;
	// nil when none was reported.
	TreeMemMB *float64
	// DBCPUMed and DBMemMB are the median CPU time and the largest memory
	// of the test database over the runs that recorded it; nil when none
	// did.
	DBCPUMed *float64
	DBMemMB  *float64
	Failed   int
	// Hits are the runs the cache skipped; SavedS is the wall time they
	// saved.
	Hits   int
	SavedS float64
}

// Saved is what the cache skipped in a report: runs, wall and CPU time.
type Saved struct {
	Hits        int
	WallS, CPUS float64
}

// rowKey names a row: the project is empty when the report covers one.
type rowKey struct{ project, label string }

func (f Filter) rowKey(r *Record, label string) rowKey {
	if f.Project != "" {
		return rowKey{label: label}
	}
	return rowKey{project: r.Project, label: label}
}

// Summarize groups the records f selects into rows, the heaviest in memory
// first; a broken down task leads with its whole-task row. Saved sums the
// cache hits, which also show on their task's row.
func Summarize(recs []Record, f Filter) ([]Row, Saved) {
	groups := map[rowKey][]Record{}
	hits := map[rowKey]Saved{}
	var saved Saved
	var order []rowKey
	for i := range recs {
		r := &recs[i]
		if r.Time.Before(f.Since) || (f.Project != "" && r.Project != f.Project) {
			continue
		}
		if r.Kind == KindCacheHit {
			if f.Task == "" || r.Task == f.Task {
				k := f.rowKey(r, hitLabel(r, f))
				hits[k] = hits[k].add(r)
				saved = saved.add(r)
			}
			continue
		}
		label, ok := runLabel(r, f)
		if !ok || !calm(r.LoadMax, f.MaxLoad) {
			continue
		}
		k := f.rowKey(r, label)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], *r)
	}
	rows := make([]Row, 0, len(order))
	for _, k := range order {
		row := summarize(k.project, k.label, groups[k])
		row.Hits, row.SavedS = hits[k].Hits, hits[k].WallS
		rows = append(rows, row)
	}
	sortRows(rows, f.Task != "")
	return rows, saved
}

// runLabel is the row a task or step record belongs to; false leaves it out
// of the report.
func runLabel(r *Record, f Filter) (string, bool) {
	switch {
	case f.Task == "" && r.Kind == KindTask:
		return r.Task, true
	case f.Task == "" || r.Task != f.Task:
		return "", false
	case r.Kind == KindTask:
		return TaskLabel, true
	case r.Kind == KindStep:
		return r.Step, true
	}
	return "", false
}

// sortRows puts the heaviest in memory first; broken down, the whole-task
// row leads.
func sortRows(rows []Row, brokenDown bool) {
	slices.SortStableFunc(rows, func(a, b Row) int {
		if (a.Label == TaskLabel) != (b.Label == TaskLabel) && brokenDown {
			if a.Label == TaskLabel {
				return -1
			}
			return 1
		}
		if c := compareRSS(b.TreeMemMB, a.TreeMemMB); c != 0 {
			return c
		}
		if c := compareRSS(b.MaxRSSMB, a.MaxRSSMB); c != 0 {
			return c
		}
		return strings.Compare(a.Project+"\x00"+a.Label, b.Project+"\x00"+b.Label)
	})
}

func compareRSS(a, b *float64) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	case *a < *b:
		return -1
	case *a > *b:
		return 1
	}
	return 0
}

func summarize(project, label string, recs []Record) Row {
	row := Row{Project: project, Label: label, Runs: len(recs)}
	walls := make([]float64, 0, len(recs))
	cpus := make([]float64, 0, len(recs))
	var dbCPUs []float64
	for i := range recs {
		r := &recs[i]
		walls = append(walls, r.WallS)
		cpus = append(cpus, r.UserS+r.SysS)
		row.WallMax = max(row.WallMax, r.WallS)
		row.MaxRSSMB = larger(row.MaxRSSMB, r.MaxRSSMB)
		row.TreeMemMB = larger(row.TreeMemMB, r.TreeMemMB)
		row.DBMemMB = larger(row.DBMemMB, r.DBMemMB)
		if r.DBCPUS != nil {
			dbCPUs = append(dbCPUs, *r.DBCPUS)
		}
		if r.Exit != 0 {
			row.Failed++
		}
	}
	row.WallMed = median(walls)
	row.CPUMed = median(cpus)
	if len(dbCPUs) > 0 {
		m := median(dbCPUs)
		row.DBCPUMed = &m
	}
	return row
}

// larger is the larger of two optional values, a copy.
func larger(a, b *float64) *float64 {
	if b == nil || (a != nil && *a >= *b) {
		return a
	}
	v := *b
	return &v
}

func median(v []float64) float64 {
	slices.Sort(v)
	n := len(v)
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}

// hitLabel is the row a cache hit belongs to: its task's, or the whole-task
// row of a broken down task.
func hitLabel(r *Record, f Filter) string {
	if f.Task != "" {
		return TaskLabel
	}
	return r.Task
}

func (s Saved) add(r *Record) Saved {
	s.Hits++
	s.WallS += r.WallS
	s.CPUS += r.UserS + r.SysS
	return s
}

// FormatSaved is the line a report ends with when the cache skipped runs.
func FormatSaved(s Saved) string {
	runs := "runs"
	if s.Hits == 1 {
		runs = "run"
	}
	return fmt.Sprintf("%d %s skipped by the cache, saving %s of wall time and %s of CPU time", s.Hits, runs, formatSeconds(s.WallS), formatSeconds(s.CPUS))
}

// Render prints rows as a table; labelHeader names the first column after
// the project, which is shown when withProject is set. The test database's
// columns come last, when a row has them.
func Render(w io.Writer, rows []Row, labelHeader string, withProject bool) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	withDB := slices.ContainsFunc(rows, func(r Row) bool { return r.DBCPUMed != nil })
	header := labelHeader + "\tRUNS\tWALL MED\tWALL MAX\tCPU MED\tTREE MEM\tMEM MAX\tFAILED\tSAVED"
	if withProject {
		header = "PROJECT\t" + header
	}
	if withDB {
		header += "\tDB CPU\tDB MEM"
	}
	fmt.Fprintln(tw, header)
	for _, r := range rows {
		if withProject {
			fmt.Fprintf(tw, "%s\t", r.Project)
		}
		saved := "-"
		if r.Hits > 0 {
			saved = fmt.Sprintf("%s in %d", formatSeconds(r.SavedS), r.Hits)
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%d%%\t%s", r.Label, r.Runs,
			formatSeconds(r.WallMed), formatSeconds(r.WallMax), formatSeconds(r.CPUMed),
			formatMB(r.TreeMemMB), formatMB(r.MaxRSSMB), (r.Failed*100+r.Runs/2)/r.Runs, saved)
		if withDB {
			dbCPU := "-"
			if r.DBCPUMed != nil {
				dbCPU = formatSeconds(*r.DBCPUMed)
			}
			fmt.Fprintf(tw, "\t%s\t%s", dbCPU, formatMB(r.DBMemMB))
		}
		fmt.Fprintln(tw)
	}
	return tw.Flush()
}

func formatSeconds(s float64) string {
	d := time.Duration(s * float64(time.Second))
	switch {
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

func formatMB(mb *float64) string {
	switch {
	case mb == nil:
		return "-"
	case *mb < 1024:
		return fmt.Sprintf("%.0f MB", *mb)
	}
	return fmt.Sprintf("%.1f GB", *mb/1024)
}
