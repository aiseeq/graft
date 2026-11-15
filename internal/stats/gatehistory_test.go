package stats

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestLoadGateHistoryMatchesFullJournal(t *testing.T) {
	const project = `projectA/<&"`
	now := time.Now()
	mem := 12.0
	for _, mode := range []string{"light", "foreign-cache", "foreign-cache-escaped-reason", "foreign-remote", "foreign-step", "escaped-link", "failed"} {
		t.Run(mode, func(t *testing.T) {
			var records []Record
			for i := range LightGateRuns {
				run := string(rune('a' + i))
				records = append(records,
					Record{Kind: KindStep, Task: GateTask, Project: project, Run: run, Step: "check", UserS: .1, TreeMemMB: &mem},
					Record{Kind: KindTask, Task: GateTask, Project: project, Run: run, Steps: 1, UserS: .1, TreeMemMB: &mem, Time: now.Add(time.Duration(i) * time.Second)},
					Record{Kind: KindTask, Task: "other", Project: "projectB", Run: "other" + run},
				)
			}
			switch mode {
			case "foreign-cache", "foreign-cache-escaped-reason":
				reason := ""
				if mode == "foreign-cache-escaped-reason" {
					reason = "\x1b failure"
				}
				records = append(records, Record{Kind: KindTask, Task: "nested", Project: "projectB", Run: "child", Parent: "e", Reason: reason}, Record{Kind: KindCacheHit, Task: "nested", Project: "projectB", Run: "grandchild", Parent: "child", Reason: reason})
			case "foreign-remote":
				records = append(records, Record{Kind: KindTask, Task: "nested", Project: "projectB", Run: "child", Parent: "e"}, Record{Kind: KindStep, Task: "nested", Project: "projectB", Run: "child", Host: "hostA"})
			case "foreign-step":
				records = append(records, Record{Kind: KindStep, Task: "nested", Project: "projectB", Run: "e", Step: "check", UserS: 5, TreeMemMB: &mem})
			case "failed":
				records[len(records)-2].Exit = 1
				records[len(records)-2].UserS = 5
			}
			path := filepath.Join(t.TempDir(), "stats.jsonl")
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range records {
				if err := json.NewEncoder(f).Encode(r); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "escaped-link" {
				for _, line := range []string{`{"kind":"task","project":"projectB","task":"nested","\u0072un":"child","PARENT":"e"}`, `{"kind":"cache-hit","project":"projectB","task":"nested","run":"child"}`} {
					if _, err := f.WriteString(line + "\n"); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := f.WriteString(`{"kind":"step","project":"projectB","task":"broken"` + "\n"); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			since := now.Add(-time.Hour)
			full, malformed, err := Load(path, since)
			if err != nil || malformed != 1 {
				t.Fatalf("full: malformed=%d error=%v", malformed, err)
			}
			selected, err := LoadGateHistory(path, project, since)
			if err != nil {
				t.Fatal(err)
			}
			for _, steps := range [][]string{{"check"}, {"other"}, nil} {
				if a, b := LightGate(full, project, since), LightGate(selected, project, since); a != b {
					t.Fatalf("LightGate full=%v selected=%v", a, b)
				}
				if a, b := SplitLightGate(full, project, since, steps), SplitLightGate(selected, project, since, steps); a != b {
					t.Fatalf("SplitLightGate %v full=%v selected=%v", steps, a, b)
				}
			}
			if len(selected) >= len(full) {
				t.Fatalf("no filtering: %d of %d", len(selected), len(full))
			}
		})
	}
}

func TestGateHistorySnapshotAcrossAppendAndRotation(t *testing.T) {
	for _, rotate := range []bool{false, true} {
		t.Run(fmt.Sprintf("rotation_%t", rotate), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stats.jsonl")
			now := time.Now()
			since := now.Add(-time.Hour)
			mem := 12.0
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			enc := json.NewEncoder(f)
			for i := range LightGateRuns - 1 {
				run := fmt.Sprintf("run%d", i)
				for _, rec := range []Record{
					{Kind: KindStep, Task: GateTask, Project: "projectA", Run: run, Step: "check", UserS: .1, TreeMemMB: &mem},
					{Kind: KindTask, Task: GateTask, Project: "projectA", Run: run, Steps: 1, UserS: .1, TreeMemMB: &mem, Time: now.Add(time.Duration(i) * time.Second)},
				} {
					if err := enc.Encode(rec); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+".1", []byte(""), 0600); err != nil {
				t.Fatal(err)
			}
			before, _, err := Load(path, since)
			if err != nil {
				t.Fatal(err)
			}
			selected, err := loadGateHistory(path, "projectA", since, func() {
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				for _, rec := range []Record{
					{Kind: KindCacheHit, Task: "nested", Project: "projectB", Run: "child", Parent: "new"},
					{Kind: KindStep, Task: GateTask, Project: "projectA", Run: "new", Step: "check", UserS: .1, TreeMemMB: &mem},
					{Kind: KindTask, Task: GateTask, Project: "projectA", Run: "new", Steps: 1, UserS: .1, TreeMemMB: &mem, Time: now.Add(5 * time.Second)},
				} {
					if err := json.NewEncoder(f).Encode(rec); err != nil {
						t.Fatal(err)
					}
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
				if rotate {
					if err := os.Rename(path+".1", path+".2"); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(path, path+".1"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			after, _, err := Load(path, since)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(selected, before) {
				t.Fatalf("snapshot changed: got %+v, before %+v", selected, before)
			}
			for _, decision := range []func([]Record) bool{
				func(r []Record) bool { return LightGate(r, "projectA", since) },
				func(r []Record) bool { return SplitLightGate(r, "projectA", since, []string{"check"}) },
			} {
				if decision(selected) != decision(before) || decision(selected) || decision(after) {
					t.Fatal("unexpected light decision for snapshot or complete append")
				}
				var inconsistent []Record
				for _, rec := range after {
					if rec.Project == "projectA" {
						inconsistent = append(inconsistent, rec)
					}
				}
				if !decision(inconsistent) {
					t.Fatal("fixture does not expose inconsistent mixed snapshot")
				}
			}
		})
	}
}

func TestGateHistoryUnusualSelection(t *testing.T) {
	for _, tc := range []struct {
		name, line          string
		edgeRun, edgeParent string
		match               bool
	}{
		{"parent case", `{"run":"child","Parent":"root"}`, "child", "root", true},
		{"duplicate run", `{"run":"wrong","parent":"root","run":"child"}`, "child", "root", true},
		{"escaped run key", `{"\u0072un":"child","parent":"root"}`, "child", "root", true},
		{"plain unrelated", `{"run":"other","parent":"root"}`, "other", "root", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := []byte(tc.line)
			unusual := unusualJSON(line)
			run, parent, fast := gateEdge(line, unusual)
			if !fast {
				var edge struct {
					Run    string `json:"run"`
					Parent string `json:"parent"`
				}
				if err := json.Unmarshal(line, &edge); err != nil {
					t.Fatal(err)
				}
				run, parent = edge.Run, edge.Parent
			}
			if run != tc.edgeRun || parent != tc.edgeParent {
				t.Fatalf("edge = %q, %q", run, parent)
			}
			if got := gateRunMatches(line, map[string]bool{"child": true}, unusual); got != tc.match {
				t.Fatalf("match = %t, want %t", got, tc.match)
			}
		})
	}
}

func TestGateHistorySelectsEscapedProjectWithoutLiteral(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	now := time.Now()
	var lines bytes.Buffer
	for i := range LightGateRuns {
		fmt.Fprintf(&lines, `{"kind":"step","task":"gate","project":"project\u0041","run":"r%d","step":"check","user_s":0.1,"tree_mem_mb":12}`+"\n", i)
		fmt.Fprintf(&lines, `{"kind":"task","task":"gate","project":"project\u0041","run":"r%d","steps":1,"user_s":0.1,"tree_mem_mb":12,"time":%q}`+"\n", i, now.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano))
	}
	if err := os.WriteFile(path, lines.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	since := now.Add(-time.Hour)
	full, malformed, err := Load(path, since)
	if err != nil || malformed != 0 || !LightGate(full, "projectA", since) {
		t.Fatalf("fixture: %v malformed=%d records=%+v", err, malformed, full)
	}
	selected, err := LoadGateHistory(path, "projectA", since)
	if err != nil || !LightGate(selected, "projectA", since) || !SplitLightGate(selected, "projectA", since, []string{"check"}) {
		t.Fatalf("escaped project lost: %v records=%+v", err, selected)
	}
}

func TestGateHistoryEscapesOnlyInRelevantFields(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{`{"project":"projectB","reason":"\u001b[31m failure"}`, false},
		{`{"project":"projectB","reason":"escaped \\\"quote\\\" \u001b"}`, false},
		{`{"project":"projectB","reason":"\/tmp"}`, false},
		{`{"project":"project\u0041"}`, true},
		{`{"project":"projectB","\u0072un":"child"}`, true},
		{`{"project":"projectB","RUN":"child\u0031"}`, true},
		{`{"project":"projectB","parent":"child\/one"}`, true},
		{`{"project":"projectB","reason":{"run":"a\u0031"}}`, false},
		{`{"project":"projectB","reason":"broken \u0041`, true},
	} {
		if got := unusualJSON([]byte(tc.line)); got != tc.want {
			t.Errorf("unusualJSON(%s) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

func TestGateHistoryAcrossRotatedFiles(t *testing.T) {
	now := time.Now()
	since := now.Add(-time.Hour)
	const project = "projectA"
	mem := 12.0
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprintf("nested_older_%t", nested), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stats.jsonl")
			current, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			for i := range LightGateRuns {
				run := string(rune('a' + i))
				for _, rec := range []Record{
					{Kind: KindStep, Task: GateTask, Project: project, Run: run, Step: "check", UserS: .1, TreeMemMB: &mem},
					{Kind: KindTask, Task: GateTask, Project: project, Run: run, Steps: 1, UserS: .1, TreeMemMB: &mem, Time: now.Add(time.Duration(i) * time.Second)},
				} {
					if err := json.NewEncoder(current).Encode(rec); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := current.Close(); err != nil {
				t.Fatal(err)
			}
			old, err := os.Create(path + ".1")
			if err != nil {
				t.Fatal(err)
			}
			if nested {
				for _, rec := range []Record{
					{Kind: KindTask, Task: "nested", Project: "projectB", Run: "child", Parent: "e"},
					{Kind: KindCacheHit, Task: "nested", Project: "projectB", Run: "grandchild", Parent: "child"},
				} {
					if err := json.NewEncoder(old).Encode(rec); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				rec := Record{Kind: KindTask, Task: GateTask, Project: project, Run: "old", Steps: 1, UserS: 20, TreeMemMB: &mem, Time: now.Add(-time.Minute)}
				if err := json.NewEncoder(old).Encode(rec); err != nil {
					t.Fatal(err)
				}
			}
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}
			full, _, err := Load(path, since)
			if err != nil {
				t.Fatal(err)
			}
			selected, err := LoadGateHistory(path, project, since)
			if err != nil {
				t.Fatal(err)
			}
			for _, decision := range []struct {
				name                 string
				full, selected, want bool
			}{
				{"LightGate", LightGate(full, project, since), LightGate(selected, project, since), !nested},
				{"SplitLightGate", SplitLightGate(full, project, since, []string{"check"}), SplitLightGate(selected, project, since, []string{"check"}), !nested},
			} {
				if decision.full != decision.want || decision.selected != decision.full {
					t.Errorf("%s full=%t selected=%t want=%t", decision.name, decision.full, decision.selected, decision.want)
				}
			}
		})
	}
}

// A journal line may carry the project with < and & unescaped: valid JSON
// that Load decodes. LoadGateHistory selects it as Load does.
func TestLoadGateHistorySelectsUnescapedProject(t *testing.T) {
	const project = "projectA/<&"
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	now := time.Now()
	var lines bytes.Buffer
	for i := range LightGateRuns {
		fmt.Fprintf(&lines, `{"kind":"step","task":"gate","project":"%s","run":"r%d","step":"check","user_s":0.1,"tree_mem_mb":12}`+"\n", project, i)
		fmt.Fprintf(&lines, `{"kind":"task","task":"gate","project":"%s","run":"r%d","steps":1,"user_s":0.1,"tree_mem_mb":12,"time":%q}`+"\n", project, i, now.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano))
	}
	if err := os.WriteFile(path, lines.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	since := now.Add(-time.Hour)
	full, malformed, err := Load(path, since)
	if err != nil || malformed != 0 {
		t.Fatalf("fixture: %v malformed=%d", err, malformed)
	}
	selected, err := LoadGateHistory(path, project, since)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := LightGate(full, project, since), LightGate(selected, project, since); a != b || !a {
		t.Fatalf("LightGate full=%v selected=%v (%d of %d records)", a, b, len(selected), len(full))
	}
}
