package deploystate

import (
	"slices"
	"testing"
	"time"
)

// TestOthersBefore: outside a deploy an install waits for every deployment;
// inside one, not for its own chain and only for deployments that started
// before the earliest of the chain, so of two deploys installing at the same
// time the earlier one goes on and the later one waits; a marker without a
// start (an older graft's) is waited for.
func TestOthersBefore(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	a := Deployment{Project: "/src/projectA", Target: "test", Started: base}
	b := Deployment{Project: "/src/projectB", Target: "test", Started: base.Add(time.Second)}
	nested := Deployment{Project: "/src/projectC", Target: "test", Started: base.Add(2 * time.Second)}
	old := Deployment{Project: "/src/projectD", Target: "test"}
	// Two started at the same moment: the one of the smaller identity first.
	first, second := a, Deployment{Project: "/src/projectE", Target: "test", Started: base}
	if Identity(second.Project, second.Target) < Identity(first.Project, first.Target) {
		first, second = second, first
	}
	names := func(rows []Deployment) []string {
		var out []string
		for _, row := range rows {
			out = append(out, row.Project)
		}
		return out
	}
	for _, tc := range []struct {
		name string
		env  string
		rows []Deployment
		want []string
	}{
		{"outside a deploy", "", []Deployment{a, b}, []string{a.Project, b.Project}},
		{"the earlier deploy goes on", Identity(a.Project, a.Target), []Deployment{a, b}, nil},
		{"the later deploy waits", Identity(b.Project, b.Target), []Deployment{a, b}, []string{a.Project}},
		{"a nested deploy counts from the outer one", ChildIdentity(Identity(a.Project, a.Target), nested.Project, nested.Target), []Deployment{nested, b, a}, nil},
		{"of two started together the second waits", Identity(second.Project, second.Target), []Deployment{first, second}, []string{first.Project}},
		{"of two started together the first goes on", Identity(first.Project, first.Target), []Deployment{first, second}, nil},
		{"an older marker is waited for", Identity(a.Project, a.Target), []Deployment{a, old}, []string{old.Project}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(Env, tc.env)
			if got := names(othersBefore(tc.rows)); !slices.Equal(got, tc.want) {
				t.Errorf("othersBefore = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRegisterRecordsTheStart: a live marker carries its project, target and
// the time it started, which orders it among the deployments.
func TestRegisterRecordsTheStart(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	before := time.Now()
	release, err := Register("/src/projectA", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	rows, err := Active()
	if err != nil || len(rows) != 1 {
		t.Fatalf("Active = %+v, %v; want the one marker", rows, err)
	}
	if got := rows[0]; got.Project != "/src/projectA" || got.Target != "test" || got.Started.Before(before) || got.Started.After(time.Now()) {
		t.Errorf("marker = %+v, want projectA test started since %s", got, before)
	}
}
