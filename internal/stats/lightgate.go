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
// after since only. A run that ran no step, took any step from the cache or
// sent any to a host says nothing about what the gate costs here and is
// passed over; a failed run may have stopped early, so it counts towards
// LightGateRuns only as a bar: one over the limits, or whose memory was not
// read, keeps the gate in the queue.
func LightGate(recs []Record, project string, since time.Time) bool {
	cached := make(map[string]bool)
	for i := range recs {
		if recs[i].Run != "" && (recs[i].Kind == KindCacheHit || recs[i].Host != "") {
			cached[recs[i].Run] = true
		}
	}
	var runs []*Record
	for i := range recs {
		r := &recs[i]
		if r.Kind == KindTask && r.Task == GateTask && r.Parent == "" && r.Project == project &&
			r.Steps > 0 && r.Time.After(since) && !cached[r.Run] {
			runs = append(runs, r)
		}
	}
	slices.SortFunc(runs, func(a, b *Record) int { return cmp.Compare(b.Time.UnixNano(), a.Time.UnixNano()) })
	passed := 0
	for _, r := range runs {
		if r.TreeMemMB == nil || r.UserS+r.SysS > LightGateCPUSec || *r.TreeMemMB > LightGateTreeMemMB {
			return false
		}
		if r.Exit == 0 {
			passed++
		}
		if passed == LightGateRuns {
			return true
		}
	}
	return false
}
