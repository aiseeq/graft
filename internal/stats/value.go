package stats

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// ValueStat names a scalar task timing from the resource journal.
type ValueStat string

const (
	ValueWallP50 ValueStat = "wall-p50"
	ValueWallP95 ValueStat = "wall-p95"
	ValueWaitP95 ValueStat = "wait-p95"
)

// ValueFilter selects the task records used for one scalar timing.
type ValueFilter struct {
	Stat         ValueStat
	Task         string
	In           string
	Since        time.Time
	ProjectRoots map[string]bool
	Profile      string
	MaxLoad      float64
}

// Value returns the selected timing and its run count. Project records match
// by their profile's repository name, and a record without a profile by its
// project root.
func Value(recs []Record, f ValueFilter) (float64, int, error) {
	if f.Stat != ValueWallP50 && f.Stat != ValueWallP95 && f.Stat != ValueWaitP95 {
		return 0, 0, fmt.Errorf("unknown stat %q", f.Stat)
	}
	if (f.Stat == ValueWallP50 || f.Stat == ValueWallP95) && f.Task == "" {
		return 0, 0, fmt.Errorf("--task is required for %s", f.Stat)
	}
	values := make([]float64, 0)
	runs := make(map[string]*Record, len(recs))
	for i := range recs {
		if recs[i].Run != "" {
			runs[recs[i].Run] = &recs[i]
		}
	}
	for i := range recs {
		r := &recs[i]
		if !valueRecord(r, f) || f.ProjectRoots != nil && nestedInAnotherProject(r, runs) {
			continue
		}
		if f.Stat != ValueWaitP95 && r.Exit != 0 {
			continue
		}
		v := r.WaitS
		if f.Stat != ValueWaitP95 {
			v = r.WallS - r.WaitS
		}
		values = append(values, v)
	}
	if len(values) == 0 {
		return 0, 0, nil
	}
	if f.Stat == ValueWallP50 {
		return Median(values), len(values), nil
	}
	return Percentile(values, .95), len(values), nil
}

// nestedInAnotherProject reports whether a recorded ancestor belongs to a
// different project: another repository by the profiles when both records
// carry one, else another tree. A missing ancestor ends the chain, as it may
// have been rotated out of the journal.
func nestedInAnotherProject(r *Record, runs map[string]*Record) bool {
	seen := make(map[string]bool)
	for parent := r.Parent; parent != "" && !seen[parent]; {
		seen[parent] = true
		ancestor, ok := runs[parent]
		if !ok {
			return false
		}
		if !sameProject(ancestor, r) {
			return true
		}
		parent = ancestor.Parent
	}
	return false
}

// sameProject compares the repositories of two records by their profiles
// (<repository>:<kind>), so the worktrees of one repository are one project;
// a record without a profile is compared by its tree.
func sameProject(a, b *Record) bool {
	ra, _, okA := strings.Cut(a.Profile, ":")
	rb, _, okB := strings.Cut(b.Profile, ":")
	if okA && okB {
		return ra == rb
	}
	return filepath.Clean(a.Project) == filepath.Clean(b.Project)
}

func valueRecord(r *Record, f ValueFilter) bool {
	if r.Kind != KindTask || r.Time.Before(f.Since) || !projectRecord(r, f.ProjectRoots, f.Profile) || !calm(r.LoadMax, f.MaxLoad) {
		return false
	}
	if f.In != "" && !matchesIn(r, f.In) {
		return false
	}
	if f.Task != "" {
		return r.Task == f.Task
	}
	return f.Stat != ValueWaitP95 || r.In == ""
}

func matchesIn(r *Record, in string) bool {
	return in == "" || r.In == in
}

func projectRecord(r *Record, roots map[string]bool, profile string) bool {
	if roots == nil {
		return true
	}
	if profile != "" && r.Profile != "" {
		return strings.HasPrefix(r.Profile, profile+":")
	}
	return roots[r.Project]
}
