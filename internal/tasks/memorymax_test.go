package tasks

import (
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
		{"own ceiling, exec", map[string]uint64{"oom": 3, "oom_kill": 5}, ExecTask, []string{"pass --memory-max to graft exec"}},
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
