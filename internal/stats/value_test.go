package stats

import "testing"

func TestValueWallStats(t *testing.T) {
	recs := []Record{
		{Kind: KindTask, Task: "build", Top: "commit", Profile: "projectA:build", WallS: 10, WaitS: 2, Exit: 0},
		{Kind: KindTask, Task: "build", Top: "build", Profile: "projectA:build", WallS: 20, Exit: 0},
		{Kind: KindTask, Task: "build", Top: "build", Profile: "projectA:build", WallS: 30, Exit: 0},
		{Kind: KindTask, Task: "build", Top: "build", Profile: "projectA:build", WallS: 100, Exit: 1},
	}
	for _, test := range []struct {
		stat ValueStat
		want float64
	}{
		{ValueWallP50, 20},
		{ValueWallP95, 30},
	} {
		got, runs, err := Value(recs, ValueFilter{Stat: test.stat, Task: "build"})
		if err != nil || runs != 3 || got != test.want {
			t.Errorf("%s = %v (%d runs, %v), want %v (3 runs)", test.stat, got, runs, err, test.want)
		}
	}
	got, runs, err := Value(recs[:3], ValueFilter{Stat: ValueWallP50, Task: "build"})
	if err != nil || runs != 3 || got != 20 {
		t.Errorf("odd median = %v (%d runs, %v), want 20 (3 runs)", got, runs, err)
	}
	got, runs, err = Value(recs[:2], ValueFilter{Stat: ValueWallP50, Task: "build"})
	if err != nil || runs != 2 || got != 14 {
		t.Errorf("even median = %v (%d runs, %v), want 14 (2 runs)", got, runs, err)
	}
}

func TestValueWaitP95UsesOuterTasksAndProjectWorktrees(t *testing.T) {
	recs := []Record{
		{Kind: KindTask, Task: "outer", Project: "/repo", Profile: "projectA:outer", WaitS: 1},
		{Kind: KindTask, Task: "dep", Project: "/worktree", Profile: "projectA:dep", In: "outer", WaitS: 100},
		{Kind: KindTask, Task: "other", Project: "/other", Profile: "projectB:other", WaitS: 50},
		{Kind: KindTask, Task: "outer", Project: "/repo", Profile: "projectA:outer", WaitS: 3},
	}
	f := ValueFilter{Stat: ValueWaitP95, ProjectRoots: map[string]bool{"/repo": true, "/worktree": true}, Profile: "projectA"}
	got, runs, err := Value(recs, f)
	if err != nil || runs != 2 || got != 3 {
		t.Errorf("outer waits = %v (%d runs, %v), want 3 (2 runs)", got, runs, err)
	}
	f.Task = "dep"
	got, runs, err = Value(recs, f)
	if err != nil || runs != 1 || got != 100 {
		t.Errorf("dep waits = %v (%d runs, %v), want 100 (1 run)", got, runs, err)
	}
	f.Task = ""
	f.ProjectRoots = nil
	got, runs, err = Value([]Record{
		{Kind: KindTask, Task: "one", WaitS: 1},
		{Kind: KindTask, Task: "two", WaitS: 2},
		{Kind: KindTask, Task: "three", WaitS: 3},
	}, f)
	if err != nil || runs != 3 || got != 3 {
		t.Errorf("all outer waits p95 = %v (%d runs, %v), want 3 (3 runs)", got, runs, err)
	}
}

func TestProjectRecordUsesPathWhenProfileIsUnavailable(t *testing.T) {
	roots := map[string]bool{"/repo": true, "/worktree": true}
	if !projectRecord(&Record{Project: "/worktree"}, roots, "") {
		t.Error("profileless worktree record was excluded without a profile")
	}
	if !projectRecord(&Record{Project: "/worktree"}, roots, "projectA") {
		t.Error("profileless worktree record was excluded when the repository has a profile name")
	}
	if projectRecord(&Record{Project: "/worktree", Profile: "projectB:gate"}, roots, "projectA") {
		t.Error("a record of another repository's profile was included")
	}
	if !projectRecord(&Record{Project: "/elsewhere", Profile: "projectA:gate"}, roots, "projectA") {
		t.Error("a record with the repository's profile was excluded")
	}
}

func TestPercentileNearestRank(t *testing.T) {
	if got := Percentile([]float64{9, 1, 5, 3}, .5); got != 3 {
		t.Errorf("p50 = %v, want 3", got)
	}
	if got := Percentile([]float64{9, 1, 5, 3}, .95); got != 9 {
		t.Errorf("p95 = %v, want 9", got)
	}
}
