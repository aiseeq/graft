package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// Tests reports the tests instead of the tasks.
	Tests bool
	// MaxLoad, when set, takes only the runs whose load stayed below it.
	MaxLoad float64
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
	f := stats.Filter{Since: since, Task: o.Task, MaxLoad: o.MaxLoad}
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
	recs, malformed, err := stats.Load(path)
	if err != nil {
		return err
	}
	if malformed > 0 {
		fmt.Fprintf(a.Stderr, "graft: %s: %d malformed line(s) left out\n", path, malformed)
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
	path := stats.TestsPath(statsPath)
	runs, malformed, err := stats.LoadTests(path)
	if err != nil {
		return err
	}
	if malformed > 0 {
		fmt.Fprintf(a.Stderr, "graft: %s: %d malformed line(s) left out\n", path, malformed)
	}
	recs, malformed, err := stats.Load(statsPath)
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
	a.printf("%s in %s since %s, the %d slowest by median", what, where, sinceText(f.Since, earliest(runs, func(r *stats.PackageRun) time.Time { return r.Time })), testsLimit)
	rep := stats.SummarizeTests(runs, f, testsLimit)
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
