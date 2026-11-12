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
	cached := runsNotCostedHere(recs)
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

// SplitLightGate says whether the local remainder of a split gate is light.
// Its cost comes from the outer graft's local step records only: task records
// include descendants and nested grafts have their own Run.
func SplitLightGate(recs []Record, project string, since time.Time, localSteps []string) bool {
	if len(localSteps) == 0 {
		return false
	}
	want := slices.Clone(localSteps)
	slices.Sort(want)
	want = slices.Compact(want)
	cached := runsWithCacheHit(recs)
	type runCost struct {
		gate  *Record
		steps []Record
	}
	byRun := map[string]*runCost{}
	for i := range recs {
		r := &recs[i]
		if r.Kind == KindTask && r.Task == GateTask && r.Parent == "" && r.Project == project && r.Run != "" &&
			r.Steps > 0 && r.Time.After(since) && !cached[r.Run] {
			byRun[r.Run] = &runCost{gate: r}
		}
	}
	for i := range recs {
		r := &recs[i]
		if r.Kind == KindStep && r.Host == "" {
			if run := byRun[r.Run]; run != nil {
				run.steps = append(run.steps, *r)
			}
		}
	}
	var runs []*runCost
	for _, run := range byRun {
		runs = append(runs, run)
	}
	slices.SortFunc(runs, func(a, b *runCost) int {
		return cmp.Compare(b.gate.Time.UnixNano(), a.gate.Time.UnixNano())
	})
	passed := 0
	for _, run := range runs {
		got := make([]string, len(run.steps))
		for i, step := range run.steps {
			got[i] = step.Step
		}
		slices.Sort(got)
		got = slices.Compact(got)
		if !slices.Equal(got, want) {
			continue
		}
		var cpu, memory float64
		builtin := false
		for _, step := range run.steps {
			if step.Builtin {
				builtin = true
				continue
			}
			if step.TreeMemMB == nil || *step.TreeMemMB > LightGateTreeMemMB {
				return false
			}
			cpu += step.UserS + step.SysS
			memory = max(memory, *step.TreeMemMB)
		}
		if builtin {
			if run.gate.TreeMemMB == nil || *run.gate.TreeMemMB > LightGateTreeMemMB {
				return false
			}
			cpu = run.gate.UserS + run.gate.SysS
			memory = max(memory, *run.gate.TreeMemMB)
		}
		if cpu > LightGateCPUSec || memory > LightGateTreeMemMB {
			return false
		}
		if run.gate.Exit == 0 {
			passed++
		}
		if passed == LightGateRuns {
			return true
		}
	}
	return false
}

func runsWithCacheHit(recs []Record) map[string]bool {
	parents := make(map[string]string)
	for i := range recs {
		if recs[i].Run != "" && recs[i].Parent != "" {
			parents[recs[i].Run] = recs[i].Parent
		}
	}
	marked := make(map[string]bool)
	for i := range recs {
		if recs[i].Kind == KindCacheHit {
			for run, hops := recs[i].Run, 0; run != "" && hops < len(recs); run, hops = parents[run], hops+1 {
				marked[run] = true
			}
		}
	}
	return marked
}

// runsNotCostedHere are the runs with a step taken from the cache or sent to
// a host, with the runs above them: a graft a gate step started has a run of
// its own.
func runsNotCostedHere(recs []Record) map[string]bool {
	parents := make(map[string]string)
	for i := range recs {
		if recs[i].Run != "" && recs[i].Parent != "" {
			parents[recs[i].Run] = recs[i].Parent
		}
	}
	marked := make(map[string]bool)
	for i := range recs {
		if recs[i].Kind == KindCacheHit || recs[i].Host != "" {
			for run, hops := recs[i].Run, 0; run != "" && hops < len(recs); run, hops = parents[run], hops+1 {
				marked[run] = true
			}
		}
	}
	return marked
}
