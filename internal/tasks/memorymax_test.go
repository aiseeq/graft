package tasks

import (
	"context"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/iosched"
)

// TestMemoryKills: a kill at the scope's own ceiling names the ceiling and how
// to raise it; a kill without the scope's oom names the limit above; no kill
// is no error.
func TestMemoryKills(t *testing.T) {
	p := iosched.Placement{Ceiling: 12 << 30, Source: "40% of RAM, the default", Scope: "graft-heavy-7.scope", Slice: "agents-graft.slice", SliceMax: 15 << 30}
	before := map[string]uint64{"oom": 2, "oom_kill": 4}
	for _, c := range []struct {
		name  string
		after map[string]uint64
		task  string
		want  []string
	}{
		{"own ceiling", map[string]uint64{"oom": 3, "oom_kill": 5}, "test", []string{"graft-heavy-7.scope hit its memory ceiling 12.0 GiB (40% of RAM, the default)", "killed 1 process", "memory_max of task test"}},
		{"own ceiling, exec", map[string]uint64{"oom": 3, "oom_kill": 5}, ExecTask, []string{"pass --memory-max above 12.0 GiB to graft exec"}},
		{"limit above", map[string]uint64{"oom": 2, "oom_kill": 6}, "test", []string{"killed 2 process(es) of heavy scope graft-heavy-7.scope", "below its own ceiling 12.0 GiB", "agents-graft.slice", "15.0 GiB"}},
		{"no kill", map[string]uint64{"oom": 3, "oom_kill": 4}, "test", nil},
		{"counter went back", map[string]uint64{"oom": 0, "oom_kill": 1}, "test", nil},
	} {
		err := memoryKills(p, before, c.after, c.task)
		if c.want == nil {
			if err != nil {
				t.Errorf("%s: %v, want none", c.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: no error", c.name)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: %q lacks %q", c.name, err, w)
			}
		}
	}
}

// TestMemoryKillsOuter: a ceiling an outer graft exec --memory-max holds is
// raised there, or by the task's memory_max above it.
func TestMemoryKillsOuter(t *testing.T) {
	p := iosched.Placement{Ceiling: 1 << 30, Source: outerMemorySource, Scope: "graft-heavy-7.scope"}
	err := memoryKills(p, map[string]uint64{}, map[string]uint64{"oom": 1, "oom_kill": 1}, "test")
	want := "pass --memory-max above 1.0 GiB to the graft exec that started this graft, or set memory_max of task test in .graft.yaml above 1.0 GiB"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want %q", err, want)
	}
}

// TestSplitGateCeiling: each step of a split gate admitted already asks for
// its own task's memory_max with the outer exec's one, and a placement line
// printed once is not printed again.
func TestSplitGateCeiling(t *testing.T) {
	t.Setenv(ExecMemoryMaxEnv, "2G")
	t.Setenv(HeldScopeEnv, "graft-heavy-7.scope")
	var got []iosched.Request
	orig := configureHeavy
	t.Cleanup(func() { configureHeavy = orig })
	configureHeavy = func(_ context.Context, _ int, _ string, req iosched.Request) (iosched.Placement, error) {
		got = append(got, req)
		return iosched.Placement{Message: "memory ceiling " + req.Source + " not applied"}, nil
	}
	var stderr strings.Builder
	r := &Runner{Stderr: &stderr, splitLeave: func() error { return nil }}
	for _, step := range []struct{ task, max string }{{"lint", ""}, {"test", "6G"}, {"test", "6G"}} {
		if err := r.enterSplit(context.Background(), step.task, step.max); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 3 || got[1].Value != "6G" || got[1].Source != "memory_max of task test" || got[0].Outer != "2G" || got[0].OuterSource != outerMemorySource || got[0].Held != "graft-heavy-7.scope" {
		t.Fatalf("requests %+v", got)
	}
	if n := strings.Count(stderr.String(), "memory_max of task test not applied"); n != 1 {
		t.Errorf("line printed %d times, want once:\n%s", n, stderr.String())
	}
}
