package stats

import (
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"
)

// RerunRow is a task a graft ran again after it failed and that then
// passed: a retry of a script around graft (a test runner that reruns a
// failed stage), which hides a flake the way a test retry does.
type RerunRow struct {
	Project, Task string
	// Runs is how many runs of the parent graft reran the task to a pass;
	// Failed counts the failed attempts in them.
	Runs, Failed int
}

// Reruns finds, among the task lines f selects, the tasks that failed and
// then passed under the same parent graft run, the most runs first. Lines
// without a parent (a graft started by hand, or before graft 2.19) are
// left out: two runs of a task by hand are not one run retrying it.
func Reruns(recs []Record, f TestFilter) []RerunRow {
	type attempts struct{ project, task, parent string }
	byParent := map[attempts][]*Record{}
	for i := range recs {
		r := &recs[i]
		if r.Kind != KindTask || r.Parent == "" || r.Time.Before(f.Since) || (f.Project != "" && r.Project != f.Project) ||
			(f.Task != "" && r.Task != f.Task) || !calm(r.LoadMax, f.MaxLoad) {
			continue
		}
		k := attempts{r.Project, r.Task, r.Parent}
		byParent[k] = append(byParent[k], r)
	}
	type rowKey struct{ project, task string }
	rows := map[rowKey]*RerunRow{}
	for k, rs := range byParent {
		slices.SortStableFunc(rs, func(a, b *Record) int { return a.Time.Compare(b.Time) })
		failed := 0
		for _, r := range rs {
			if r.Exit != 0 {
				failed++
				continue
			}
			if failed == 0 {
				continue
			}
			project := ""
			if f.Project == "" {
				project = k.project
			}
			row := rows[rowKey{project, k.task}]
			if row == nil {
				row = &RerunRow{Project: project, Task: k.task}
				rows[rowKey{project, k.task}] = row
			}
			row.Runs++
			row.Failed += failed
			break
		}
	}
	out := make([]RerunRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b RerunRow) int {
		if a.Runs != b.Runs {
			return b.Runs - a.Runs
		}
		if a.Failed != b.Failed {
			return b.Failed - a.Failed
		}
		return strings.Compare(a.Project+"\x00"+a.Task, b.Project+"\x00"+b.Task)
	})
	return out
}

// renderReruns prints the rerun tasks as a table under the flaky tests.
func renderReruns(tw *tabwriter.Writer, rows []RerunRow, withProject bool) {
	fmt.Fprintln(tw, "\nflaky tasks: failed, then passed when the graft that ran them ran them again")
	if len(rows) == 0 {
		fmt.Fprintln(tw, "none")
		return
	}
	header := "TASK\tRUNS\tFAILED"
	if withProject {
		header = "PROJECT\t" + header
	}
	fmt.Fprintln(tw, header)
	for _, r := range rows {
		if withProject {
			fmt.Fprintf(tw, "%s\t", r.Project)
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\n", r.Task, r.Runs, r.Failed)
	}
}
