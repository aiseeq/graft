package stats

import (
	"testing"
	"time"
)

func TestLightGate(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	mem := func(v float64) *float64 { return &v }
	gate := func(n int, cpu float64) Record {
		return Record{Kind: KindTask, Task: GateTask, Project: "/p", Steps: 1, UserS: cpu, TreeMemMB: mem(20), Run: "r" + string(rune('a'+n)), Time: base.Add(time.Duration(n) * time.Minute)}
	}
	five := func() []Record {
		var recs []Record
		for i := range LightGateRuns {
			recs = append(recs, gate(i, 0.3))
		}
		return recs
	}
	cases := []struct {
		name  string
		edit  func([]Record) []Record
		since time.Time
		want  bool
	}{
		{name: "five light runs", want: true},
		{name: "four runs", edit: func(r []Record) []Record { return r[:4] }},
		{name: "one over the CPU", edit: func(r []Record) []Record { r[2].SysS = 2.5; return r }},
		{name: "one over the memory", edit: func(r []Record) []Record { r[1].TreeMemMB = mem(400); return r }},
		{name: "a run not read for memory", edit: func(r []Record) []Record { r[0].TreeMemMB = nil; return r }},
		{name: "a failed run does not count", edit: func(r []Record) []Record { r[3].Exit = 1; return r }},
		{name: "a run with no step does not count", edit: func(r []Record) []Record { r[3].Steps = 0; return r }},
		{name: "a run that took a step from the cache does not count", edit: func(r []Record) []Record {
			return append(r, Record{Kind: KindCacheHit, Run: r[2].Run})
		}},
		{name: "a heavy failed run keeps it in the queue", edit: func(r []Record) []Record {
			r = append(r, gate(6, 9))
			r[len(r)-1].Exit = 1
			return r
		}},
		{name: "a light failed run is no bar", edit: func(r []Record) []Record {
			r = append(r, gate(6, 0.1))
			r[len(r)-1].Exit = 1
			return r
		}, want: true},
		{name: "a run with a step on a host does not count", edit: func(r []Record) []Record {
			return append(r, Record{Kind: KindStep, Run: r[2].Run, Host: "pc"})
		}},
		{name: "a nested graft's cached step does not count", edit: func(r []Record) []Record {
			return append(r, Record{Kind: KindCacheHit, Run: "child", Parent: r[2].Run})
		}},
		{name: "another project does not count", edit: func(r []Record) []Record { r[4].Project = "/q"; return r }},
		{name: "a nested gate does not count", edit: func(r []Record) []Record { r[4].Parent = "x"; return r }},
		{name: "runs before the config changed do not count", since: base.Add(90 * time.Second)},
		{name: "an older heavy run is forgotten", edit: func(r []Record) []Record {
			return append(r, gate(-1, 9))
		}, want: true},
		{name: "a heavy run after five light ones counts", edit: func(r []Record) []Record {
			return append(r, gate(9, 9))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recs := five()
			if c.edit != nil {
				recs = c.edit(recs)
			}
			if got := LightGate(recs, "/p", c.since); got != c.want {
				t.Errorf("LightGate = %v, want %v", got, c.want)
			}
		})
	}
}
