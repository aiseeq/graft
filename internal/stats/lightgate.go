package stats

import (
	"cmp"
	"slices"
	"time"
)

// What makes a gate light: its last LightGateRuns runs that really ran their
// steps each stayed under these. Most gates of most projects cost a second
// from the cache and tens of seconds when they run, so a cached run does not
// count; it is still a bar by the steps it ran here.
const (
	LightGateRuns      = 5
	LightGateCPUSec    = 2.0
	LightGateTreeMemMB = 300.0
)

// LightGate says whether the gate of project is light by recs, counting runs
// after since only. A run that ran no step is passed over. A run that took
// any step from the cache or sent any to a host is a bar by the steps it ran
// here alone, and passed over when it ran none here; it never counts towards
// LightGateRuns. A failed run may have stopped early, so it counts only as a
// bar too: one over the limits, or whose memory was not read, keeps the gate
// in the queue.
func LightGate(recs []Record, project string, since time.Time) bool {
	cached := runsNotCostedHere(recs)
	var runs []*Record
	for i := range recs {
		r := &recs[i]
		if r.Kind == KindTask && r.Task == GateTask && r.Parent == "" && r.Project == project &&
			r.Steps > 0 && r.Time.After(since) {
			runs = append(runs, r)
		}
	}
	localSteps := localStepsByRun(recs)
	slices.SortFunc(runs, func(a, b *Record) int { return cmp.Compare(b.Time.UnixNano(), a.Time.UnixNano()) })
	passed := 0
	for _, r := range runs {
		if cached[r.Run] {
			if len(localSteps[r.Run]) == 0 {
				continue
			}
			if !stepsAreLight(localSteps[r.Run]) {
				return false
			}
		} else if r.TreeMemMB == nil || r.UserS+r.SysS > LightGateCPUSec || *r.TreeMemMB > LightGateTreeMemMB {
			return false
		}
		if r.Exit == 0 && !cached[r.Run] {
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
// include descendants and nested grafts have their own Run. The steps of the
// remainder in every recent run are a bar, as a failed gate is in LightGate:
// a run that stopped early or ran another set still says what those steps
// cost, and so does a run that took a step from the cache. Only a run that
// ran exactly the remainder here, none of it from the cache, and passed
// counts towards LightGateRuns.
func SplitLightGate(recs []Record, project string, since time.Time, localSteps []string) bool {
	if len(localSteps) == 0 {
		return false
	}
	want := slices.Clone(localSteps)
	slices.Sort(want)
	runs := splitGateRuns(recs, project, since)
	passed := 0
	for _, run := range runs {
		if !splitGateStepsAreLight(run.steps, want) {
			return false
		}
		if run.cached || !splitGateRunMatches(run, want) {
			continue
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

type splitGateRun struct {
	gate  *Record
	steps []*Record
	// cached is a run that took a step from the cache: the steps it ran
	// still bar the remainder, but it does not count as a light run.
	cached bool
}

func splitGateRuns(recs []Record, project string, since time.Time) []*splitGateRun {
	cached := runsWithCacheHit(recs)
	byRun := make(map[string]*splitGateRun)
	for i := range recs {
		r := &recs[i]
		if r.Kind == KindTask && r.Task == GateTask && r.Parent == "" && r.Project == project && r.Run != "" &&
			r.Steps > 0 && r.Time.After(since) {
			byRun[r.Run] = &splitGateRun{gate: r, cached: cached[r.Run]}
		}
	}
	for i := range recs {
		r := &recs[i]
		if r.Kind == KindStep && r.Host == "" {
			if run := byRun[r.Run]; run != nil {
				run.steps = append(run.steps, r)
			}
		}
	}
	var runs []*splitGateRun
	for _, run := range byRun {
		runs = append(runs, run)
	}
	slices.SortFunc(runs, func(a, b *splitGateRun) int {
		if order := cmp.Compare(b.gate.Time.UnixNano(), a.gate.Time.UnixNano()); order != 0 {
			return order
		}
		return cmp.Compare(a.gate.Run, b.gate.Run)
	})
	return runs
}

func splitGateRunMatches(run *splitGateRun, want []string) bool {
	got := make([]string, len(run.steps))
	for i, step := range run.steps {
		got[i] = step.Step
	}
	slices.Sort(got)
	return slices.Equal(got, want)
}

// splitGateStepsAreLight says whether the steps of a run that are in want
// stayed under the limits here; a run with none of them passes.
func splitGateStepsAreLight(steps []*Record, want []string) bool {
	var in []*Record
	for _, step := range steps {
		if _, ok := slices.BinarySearch(want, step.Step); ok {
			in = append(in, step)
		}
	}
	return stepsAreLight(in)
}

// stepsAreLight says whether steps together stayed under the limits here: a
// built-in step without a complete CPU sample, or any step whose memory was
// not read, is not light.
func stepsAreLight(steps []*Record) bool {
	var cpu, memory float64
	for _, step := range steps {
		if step.Builtin && !step.BuiltinCostKnown {
			return false
		}
		if step.TreeMemMB == nil {
			return false
		}
		cpu += step.UserS + step.SysS
		memory = max(memory, *step.TreeMemMB)
	}
	return cpu <= LightGateCPUSec && memory <= LightGateTreeMemMB
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

// localStepsByRun groups the step records run on this machine by their Run.
func localStepsByRun(recs []Record) map[string][]*Record {
	steps := make(map[string][]*Record)
	for i := range recs {
		r := &recs[i]
		if r.Kind == KindStep && r.Host == "" {
			steps[r.Run] = append(steps[r.Run], r)
		}
	}
	return steps
}
