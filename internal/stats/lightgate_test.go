package stats

import (
	"slices"
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

func TestSplitLightGate(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	mem := func(v float64) *float64 { return &v }
	makeRuns := func() []Record {
		var records []Record
		for i := range LightGateRuns {
			run := string(rune('a' + i))
			when := base.Add(time.Duration(i) * time.Minute)
			records = append(records,
				Record{Kind: KindStep, Run: run, Step: "lex check", Time: when, UserS: .1, TreeMemMB: mem(12), Exit: 0},
				Record{Kind: KindStep, Run: run, Step: "glint", Time: when, UserS: 99, TreeMemMB: mem(900), Host: "remote", Exit: 0},
				Record{Kind: KindTask, Run: run, Task: GateTask, Project: "/p", Steps: 2, UserS: 99, TreeMemMB: mem(900), Exit: 0, Time: when.Add(time.Second)},
			)
		}
		return records
	}
	t.Run("local prices exclude host steps", func(t *testing.T) {
		if !SplitLightGate(makeRuns(), "/p", time.Time{}, []string{"lex check"}) {
			t.Fatal("five light local remainders should pass despite expensive host steps")
		}
	})
	t.Run("an expensive remainder step of a failed partial run keeps the remainder queued", func(t *testing.T) {
		records := makeRuns()
		for i := range records {
			if records[i].Kind == KindStep && records[i].Host == "" {
				records = append(records, Record{Kind: KindStep, Run: records[i].Run, Step: "work-b", Time: records[i].Time, UserS: .1, TreeMemMB: mem(12), Exit: 0})
			}
		}
		when := base.Add(time.Hour)
		records = append(records,
			Record{Kind: KindStep, Run: "failed", Step: "lex check", Time: when, UserS: 10, TreeMemMB: mem(12), Exit: 1},
			Record{Kind: KindTask, Run: "failed", Task: GateTask, Project: "/p", Steps: 1, UserS: 10, TreeMemMB: mem(12), Exit: 1, Time: when.Add(time.Second)},
		)
		if SplitLightGate(records, "/p", time.Time{}, []string{"lex check", "work-b"}) {
			t.Fatal("a failed run that stopped after an expensive remainder step was passed over")
		}
	})
	t.Run("a step outside the remainder run here once does not bar it", func(t *testing.T) {
		records := makeRuns()
		when := base.Add(time.Hour)
		records = append(records,
			Record{Kind: KindStep, Run: "fallback", Step: "lex check", Time: when, UserS: .1, TreeMemMB: mem(12), Exit: 0},
			Record{Kind: KindStep, Run: "fallback", Step: "glint", Time: when, UserS: 99, TreeMemMB: mem(900), Exit: 0},
			Record{Kind: KindTask, Run: "fallback", Task: GateTask, Project: "/p", Steps: 2, UserS: 99, TreeMemMB: mem(900), Exit: 0, Time: when.Add(time.Second)},
		)
		if !SplitLightGate(records, "/p", time.Time{}, []string{"lex check"}) {
			t.Fatal("a host step that once ran here barred the light remainder")
		}
	})
	t.Run("different local step set is skipped", func(t *testing.T) {
		if SplitLightGate(makeRuns(), "/p", time.Time{}, []string{"different"}) {
			t.Fatal("runs with a different local step set counted")
		}
	})
	t.Run("duplicate local commands keep their count", func(t *testing.T) {
		records := makeRuns()
		for i := range LightGateRuns {
			run := string(rune('a' + i))
			records = append(records, Record{Kind: KindStep, Run: run, Step: "lex check", Time: base, UserS: .15, TreeMemMB: mem(12), Exit: 0})
		}
		if !SplitLightGate(records, "/p", time.Time{}, []string{"lex check", "lex check"}) {
			t.Fatal("two matching local command occurrences were not accepted")
		}
		records = slices.DeleteFunc(records, func(r Record) bool {
			return r.Run == "c" && r.Kind == KindStep && r.Step == "lex check" && r.UserS == .15
		})
		if SplitLightGate(records, "/p", time.Time{}, []string{"lex check", "lex check"}) {
			t.Fatal("one recorded occurrence matched two configured commands")
		}
	})
	t.Run("an old lex row without memory keeps the remainder queued", func(t *testing.T) {
		records := makeRuns()
		for i := range records {
			if records[i].Kind == KindStep && records[i].Run == "c" && records[i].Host == "" {
				records[i].TreeMemMB = nil
			}
		}
		if SplitLightGate(records, "/p", time.Time{}, []string{"lex check"}) {
			t.Fatal("unread local memory passed")
		}
	})
	t.Run("builtin step uses its graft run cost", func(t *testing.T) {
		var records []Record
		for i := range LightGateRuns {
			run := string(rune('a' + i))
			when := base.Add(time.Duration(i) * time.Minute)
			records = append(records,
				Record{Kind: KindStep, Run: run, Step: "gofmt -l", Builtin: true, BuiltinCostKnown: true, UserS: .2, TreeMemMB: mem(24), Time: when, Exit: 0},
				Record{Kind: KindTask, Run: run, Task: GateTask, Project: "/p", Steps: 1, UserS: .2, TreeMemMB: mem(24), Exit: 0, Time: when.Add(time.Second)},
			)
		}
		if !SplitLightGate(records, "/p", time.Time{}, []string{"gofmt -l"}) {
			t.Fatal("built-in step without its own sample did not use the graft run cost")
		}
	})
	t.Run("nested graft step is not counted twice", func(t *testing.T) {
		records := makeRuns()
		records = append(records, Record{Kind: KindStep, Run: "child", Parent: "c", Step: "lex check", UserS: 90, TreeMemMB: mem(900), Exit: 0})
		if !SplitLightGate(records, "/p", time.Time{}, []string{"lex check"}) {
			t.Fatal("nested graft step was counted twice")
		}
	})
	t.Run("local CPU over the limit keeps the remainder queued", func(t *testing.T) {
		records := makeRuns()
		for i := range records {
			if records[i].Kind == KindStep && records[i].Run == "c" && records[i].Host == "" {
				records[i].UserS = LightGateCPUSec + 1
			}
		}
		if SplitLightGate(records, "/p", time.Time{}, []string{"lex check"}) {
			t.Fatal("expensive local remainder passed")
		}
	})
	t.Run("cache hit run does not count", func(t *testing.T) {
		records := makeRuns()
		records = append(records, Record{Kind: KindCacheHit, Run: "c"})
		if SplitLightGate(records, "/p", time.Time{}, []string{"lex check"}) {
			t.Fatal("cache hit run counted")
		}
	})
	t.Run("host only run does not count", func(t *testing.T) {
		records := makeRuns()
		for i := range records {
			if records[i].Kind == KindStep && records[i].Run == "c" && records[i].Host == "" {
				records[i].Host = "remote"
			}
		}
		if SplitLightGate(records, "/p", time.Time{}, []string{"lex check"}) {
			t.Fatal("host-only run counted")
		}
	})
}
