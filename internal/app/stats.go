package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/stats"
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
}

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
	f := stats.Filter{Since: since, Task: o.Task}
	where := "every project"
	if !o.All {
		if f.Project, err = a.statsProject(o.Project); err != nil {
			return err
		}
		where = f.Project
	}
	if o.Tests {
		return a.statsTests(stats.TestsPath(path), stats.TestFilter{Since: since, Project: f.Project, Task: o.Task}, where)
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
	a.printf("%s in %s since %s", what, where, since.Format("2006-01-02 15:04"))
	rows := stats.Summarize(recs, f)
	if len(rows) == 0 {
		a.printf("nothing recorded (%s)", path)
		return nil
	}
	return stats.Render(a.Stdout, rows, label, o.All)
}

// statsTests prints the slowest tests and packages and the flaky tests.
func (a *App) statsTests(path string, f stats.TestFilter, where string) error {
	runs, malformed, err := stats.LoadTests(path)
	if err != nil {
		return err
	}
	if malformed > 0 {
		fmt.Fprintf(a.Stderr, "graft: %s: %d malformed line(s) left out\n", path, malformed)
	}
	what := "tests"
	if f.Task != "" {
		what = "tests of task " + f.Task
	}
	a.printf("%s in %s since %s, the %d slowest by median", what, where, f.Since.Format("2006-01-02 15:04"), testsLimit)
	rep := stats.SummarizeTests(runs, f, testsLimit)
	if len(rep.Packages) == 0 {
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
