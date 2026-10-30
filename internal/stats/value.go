package stats

import (
	"fmt"
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
	for i := range recs {
		r := &recs[i]
		if !valueRecord(r, f) {
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
		return median(values), len(values), nil
	}
	return Percentile(values, .95), len(values), nil
}

func valueRecord(r *Record, f ValueFilter) bool {
	if r.Kind != KindTask || r.Time.Before(f.Since) || !projectRecord(r, f.ProjectRoots, f.Profile) || !calm(r.LoadMax, f.MaxLoad) {
		return false
	}
	if f.Task != "" {
		return r.Task == f.Task
	}
	return f.Stat != ValueWaitP95 || r.In == ""
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
