package app

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/stats"
	"github.com/aiseeq/graft/v2/internal/userconfig"
)

// StatsOptions select what graft stats reports.
type StatsOptions struct {
	Since time.Duration
	// Project is a project root; empty with All unset means the current
	// project.
	Project string
	All     bool
	// Task, when set, breaks that task down into its steps, or with Tests
	// takes only its tests.
	Task string
	In   string
	// Tests reports the tests instead of the tasks.
	Tests bool
	// MaxLoad, when set, takes only the runs whose load stayed below it.
	MaxLoad  float64
	Value    string
	MinRuns  int
	Failures bool
}

// overLimitsShown is how many tasks over their limits a report names, the
// farthest over first: every worktree of a project is a project of its own.
const overLimitsShown = 10

// testsLimit is how many tests and packages the tests report ranks.
const testsLimit = 20

// Stats prints the recorded resource use of tasks, the heaviest in memory
// first.
func (a *App) Stats(o StatsOptions) error {
	path, err := stats.Path()
	if err != nil {
		return err
	}
	since := time.Now().Add(-o.Since)
	f := stats.Filter{Since: since, Task: o.Task, In: o.In, MaxLoad: o.MaxLoad}
	where := "every project"
	if !o.All {
		if f.Project, err = a.statsProject(o.Project); err != nil {
			return err
		}
		where = f.Project
	}
	if o.Tests {
		return a.statsTests(path, stats.TestFilter{Since: since, Project: f.Project, Task: o.Task, MaxLoad: o.MaxLoad}, where+calmNote(o.MaxLoad))
	}
	recs, malformed, err := stats.Load(path, since)
	if err != nil {
		return err
	}
	if malformed > 0 {
		fmt.Fprintf(a.Stderr, "graft: %s: %d malformed line(s) left out\n", path, malformed)
	}
	if o.Failures {
		return a.statsFailures(recs, f, o, where, since)
	}
	if o.Value != "" {
		return a.statsValue(recs, f, o)
	}
	what := "tasks"
	label := "TASK"
	if o.Task != "" {
		what, label = "task "+o.Task, "STEP"
	}
	a.printf("%s in %s%s since %s", what, where, calmNote(o.MaxLoad), sinceText(since, earliest(recs, func(r *stats.Record) time.Time { return r.Time })))
	rows, saved := stats.Summarize(recs, f)
	if len(rows) == 0 && saved.Hits == 0 {
		a.printf("nothing recorded (%s)", path)
		return nil
	}
	if err := stats.Render(a.Stdout, rows, label, o.All); err != nil {
		return err
	}
	if saved.Hits > 0 {
		a.printf("%s", stats.FormatSaved(saved))
	}
	if o.Task != "" {
		return nil
	}
	limits, err := userconfig.LimitSettings()
	if err != nil {
		return err
	}
	over := stats.OverLimits(rows, limits.Gate, limits.Task)
	for _, o := range over[:min(len(over), overLimitsShown)] {
		a.printf("%s", stats.FormatOverLimit(o))
	}
	if more := len(over) - overLimitsShown; more > 0 {
		a.printf("and %d more over their limits", more)
	}
	return nil
}

func (a *App) statsFailures(records []stats.Record, filter stats.Filter, o StatsOptions, where string, since time.Time) error {
	failures := stats.FailureRuns(records, filter.Project, o.Task, o.In)
	start := sinceText(since, earliest(records, func(r *stats.Record) time.Time { return r.Time }))
	if len(failures) == 0 {
		a.printf("no failures since %s", start)
		return nil
	}
	a.printf("failures in %s since %s", where, start)
	var headers bytes.Buffer
	tw := tabwriter.NewWriter(&headers, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tPROJECT/TREE\tTASK\tSTEP\tEXIT")
	for _, r := range failures {
		step := r.Step
		if step == "" {
			step = "(before step)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n", r.Time.Local().Format("2006-01-02 15:04:05"), r.Project, r.Task, step, r.Exit)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("formatting failure report: %w", err)
	}
	rows := strings.Split(strings.TrimSuffix(headers.String(), "\n"), "\n")
	a.printf("%s", rows[0])
	for i, r := range failures {
		reason := r.Reason
		if reason == "" {
			reason = "-"
		}
		lines := strings.Split(reason, "\n")
		var block strings.Builder
		block.WriteString(rows[i+1])
		block.WriteByte('\n')
		for _, line := range lines {
			fmt.Fprintf(&block, "    %s\n", line)
		}
		a.printf("%s", strings.TrimSuffix(block.String(), "\n"))
	}
	return nil
}

func (a *App) statsValue(recs []stats.Record, f stats.Filter, o StatsOptions) error {
	var roots map[string]bool
	profile := ""
	if !o.All {
		var err error
		roots, profile, err = a.statsProjectTrees(f.Project)
		if err != nil {
			return err
		}
	}
	value, runs, err := stats.Value(recs, stats.ValueFilter{Stat: stats.ValueStat(o.Value), Task: o.Task,
		In: o.In, Since: f.Since, ProjectRoots: roots, Profile: profile, MaxLoad: o.MaxLoad})
	if err != nil {
		return err
	}
	if runs < o.MinRuns {
		return &ExitCodeError{Code: 3, Err: fmt.Errorf("--value %s: %d runs, need %d", o.Value, runs, o.MinRuns)}
	}
	formatted := fmt.Sprintf("%.3f", value)
	formatted = strings.TrimRight(strings.TrimRight(formatted, "0"), ".")
	if formatted == "" || formatted == "-0" {
		formatted = "0"
	}
	fmt.Fprintln(a.Stdout, formatted)
	return nil
}

// statsProjectTrees returns every worktree root and the profile prefix shared
// by the repository's records.
func (a *App) statsProjectTrees(root string) (map[string]bool, string, error) {
	roots := map[string]bool{root: true}
	repo, err := gitx.Open(root)
	if err != nil {
		if errors.Is(err, gitx.ErrNotRepo) {
			return roots, "", nil
		}
		return nil, "", err
	}
	out, err := repo.Git("worktree", "list", "--porcelain")
	if err != nil {
		return nil, "", fmt.Errorf("listing project worktrees: %w", err)
	}
	for line := range strings.SplitSeq(out, "\n") {
		if path, ok := strings.CutPrefix(line, "worktree "); ok {
			roots[filepath.Clean(path)] = true
		}
	}
	name := filepath.Base(filepath.Dir(repo.CommonDir))
	return roots, name, nil
}

// calmNote tells, in a report's heading, which runs --max-load kept.
func calmNote(maxLoad float64) string {
	if maxLoad == 0 {
		return ""
	}
	return fmt.Sprintf(", runs with the load below %v", maxLoad)
}

// statsTests prints the slowest tests and packages, the flaky tests and the
// tasks a graft reran to a pass; statsPath is the stats file, the tests
// file beside it.
func (a *App) statsTests(statsPath string, f stats.TestFilter, where string) error {
	oldGC := debug.SetGCPercent(30) // runtime-gc-tuning: safe — tests journal acceptance measured below 200 MB RSS at this setting
	defer debug.SetGCPercent(oldGC)
	path := stats.TestsPath(statsPath)
	rep, firstTest, malformed, err := stats.SummarizeTestFile(path, f, testsLimit)
	if err != nil {
		return err
	}
	if malformed > 0 {
		fmt.Fprintf(a.Stderr, "graft: %s: %d malformed line(s) left out\n", path, malformed)
	}
	recs, malformed, err := stats.Load(statsPath, f.Since)
	if err != nil {
		return err
	}
	if malformed > 0 {
		fmt.Fprintf(a.Stderr, "graft: %s: %d malformed line(s) left out\n", statsPath, malformed)
	}
	what := "tests"
	if f.Task != "" {
		what = "tests of task " + f.Task
	}
	a.printf("%s in %s since %s, the %d slowest by median", what, where, sinceText(f.Since, firstTest), testsLimit)
	rep.Reruns = stats.Reruns(recs, f)
	if len(rep.Packages) == 0 && len(rep.Reruns) == 0 {
		a.printf("nothing recorded (%s); a task records its tests with tests: go", path)
		return nil
	}
	return stats.RenderTests(a.Stdout, rep, f.Project == "")
}

// statsProject is the root the records of a project carry: the given path
// made absolute, or the current repository's root.
func (a *App) statsProject(path string) (string, error) {
	if path == "" {
		repo, err := gitx.Open(a.Dir)
		if err != nil {
			return "", fmt.Errorf("%w; pass --project <path> or --all", err)
		}
		return repo.Root, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("--project %s: %w", path, err)
	}
	// Git reports roots with symlinks resolved; a project that no longer
	// exists keeps the path as given.
	real, err := filepath.EvalSymlinks(abs)
	if errors.Is(err, os.ErrNotExist) {
		return abs, nil
	}
	if err != nil {
		return "", fmt.Errorf("--project %s: %w", path, err)
	}
	return real, nil
}

// sinceText names where a report starts: the cut asked for, or the first
// line of the log when the log starts after it.
func sinceText(since, first time.Time) string {
	const layout = "2006-01-02 15:04"
	if !first.IsZero() && first.After(since) {
		return fmt.Sprintf("%s (the log starts there, after the %s asked)", first.Local().Format(layout), since.Format(layout))
	}
	return since.Format(layout)
}

// earliest is the time of the oldest of the lines; zero for none.
func earliest[T any](lines []T, at func(*T) time.Time) time.Time {
	var first time.Time
	for i := range lines {
		if t := at(&lines[i]); first.IsZero() || t.Before(first) {
			first = t
		}
	}
	return first
}
