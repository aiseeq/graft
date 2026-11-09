package stats

import (
	"cmp"
	"slices"
	"time"
)

// What makes a gate light: its last LightGateRuns runs that really ran their
// steps each stayed under these. Most gates of most projects cost a second
// from the cache and tens of seconds when they run, so a cached run says
// nothing and does not count.
const (
	LightGateRuns      = 5
	LightGateCPUSec    = 2.0
	LightGateTreeMemMB = 300.0
)

// LightGate says whether the gate of project is light by recs, counting runs
// after since only: a run that failed, ran no step, took any step from the
// cache, or did not read its memory says nothing about a whole run and is
// passed over.
func LightGate(recs []Record, project string, since time.Time) bool {
	cached := make(map[string]bool)
	for i := range recs {
		if recs[i].Kind == KindCacheHit && recs[i].Run != "" {
			cached[recs[i].Run] = true
		}
	}
	var runs []*Record
	for i := range recs {
		r := &recs[i]
		if r.Kind == KindTask && r.Task == GateTask && r.Parent == "" && r.Project == project &&
			r.Exit == 0 && r.Steps > 0 && r.TreeMemMB != nil && r.Time.After(since) && !cached[r.Run] {
			runs = append(runs, r)
		}
	}
	if len(runs) < LightGateRuns {
		return false
	}
	slices.SortFunc(runs, func(a, b *Record) int { return cmp.Compare(b.Time.UnixNano(), a.Time.UnixNano()) })
	for _, r := range runs[:LightGateRuns] {
		if r.UserS+r.SysS > LightGateCPUSec || *r.TreeMemMB > LightGateTreeMemMB {
			return false
		}
	}
	return true
}
