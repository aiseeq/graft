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
	Failed   int
}

// Summarize groups the records f selects into rows, the heaviest in memory
// first; a broken down task leads with its whole-task row.
func Summarize(recs []Record, f Filter) []Row {
	type key struct{ project, label string }
	groups := map[key][]Record{}
	var order []key
	for _, r := range recs {
		if r.Time.Before(f.Since) || (f.Project != "" && r.Project != f.Project) {
			continue
		}
		var label string
		switch {
		case f.Task == "" && r.Kind == KindTask:
			label = r.Task
		case f.Task == "" || r.Task != f.Task:
			continue
		case r.Kind == KindTask:
			label = TaskLabel
		case r.Kind == KindStep:
			label = r.Step
		default:
			continue
		}
		k := key{label: label}
		if f.Project == "" {
			k.project = r.Project
		}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}
	rows := make([]Row, 0, len(order))
	for _, k := range order {
		rows = append(rows, summarize(k.project, k.label, groups[k]))
	}
	slices.SortStableFunc(rows, func(a, b Row) int {
		if (a.Label == TaskLabel) != (b.Label == TaskLabel) && f.Task != "" {
			if a.Label == TaskLabel {
				return -1
			}
			return 1
		}
		if c := compareRSS(b.MaxRSSMB, a.MaxRSSMB); c != 0 {
			return c
		}
		return strings.Compare(a.Project+"\x00"+a.Label, b.Project+"\x00"+b.Label)
	})
	return rows
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
	for _, r := range recs {
		walls = append(walls, r.WallS)
		cpus = append(cpus, r.UserS+r.SysS)
		row.WallMax = max(row.WallMax, r.WallS)
		if r.MaxRSSMB != nil && (row.MaxRSSMB == nil || *r.MaxRSSMB > *row.MaxRSSMB) {
			v := *r.MaxRSSMB
			row.MaxRSSMB = &v
		}
		if r.Exit != 0 {
			row.Failed++
		}
	}
	row.WallMed = median(walls)
	row.CPUMed = median(cpus)
	return row
}

func median(v []float64) float64 {
	slices.Sort(v)
	n := len(v)
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}

// Render prints rows as a table; labelHeader names the first column after
// the project, which is shown when withProject is set.
func Render(w io.Writer, rows []Row, labelHeader string, withProject bool) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	header := labelHeader + "\tRUNS\tWALL MED\tWALL MAX\tCPU MED\tMEM MAX\tFAILED\n"
	if withProject {
		header = "PROJECT\t" + header
	}
	fmt.Fprint(tw, header)
	for _, r := range rows {
		if withProject {
			fmt.Fprintf(tw, "%s\t", r.Project)
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%d%%\n", r.Label, r.Runs,
			formatSeconds(r.WallMed), formatSeconds(r.WallMax), formatSeconds(r.CPUMed),
			formatMB(r.MaxRSSMB), (r.Failed*100+r.Runs/2)/r.Runs)
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
