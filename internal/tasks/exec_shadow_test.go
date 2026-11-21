package tasks

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/stats"
)

func TestExecShadowFieldsFollowFallbackToLocalRun(t *testing.T) {
	r := &Runner{}
	f := &frame{remote: true}
	r.recordExecShadow(f, true, "projectA-key", true, "ok")
	if f.execCmdKey != "" || f.execWouldCheap != nil {
		t.Fatal("host run recorded local shadow fields")
	}
	// fallbackBrokenRemote changes remote to false after the local admission.
	f.remote = false
	r.recordExecShadow(f, true, "projectA-key", true, "ok")
	if f.execCmdKey != "projectA-key" || f.execWouldCheap == nil || !*f.execWouldCheap || f.execCheapPlace != "ok" {
		t.Fatalf("local fallback shadow fields = %+v", f)
	}
}

func TestExecHistoryRequiresACompletedLocalStep(t *testing.T) {
	f := &frame{execShadowLocal: true, execCmdKey: "projectA-key"}
	if _, ok := completedExecShadowRecord(f, time.Now(), stats.Usage{}, 0); ok {
		t.Fatal("history was made without a started exec step")
	}
	f.steps = 1
	if record, ok := completedExecShadowRecord(f, time.Now(), stats.Usage{}, 0); !ok || record.CmdKey != f.execCmdKey {
		t.Fatalf("completed local step history = %+v, recorded %v", record, ok)
	}
}

func TestExecShadowMalformedHistoryWarnsAndStaysFalse(t *testing.T) {
	fields := []string{"time", "cmd_key", "cpu_s", "exit", "cached", "host"}
	type invalid struct {
		name   string
		mutate func(map[string]any)
	}
	var cases []invalid
	for _, field := range fields {
		cases = append(cases,
			invalid{name: "missing " + field, mutate: func(record map[string]any) { delete(record, field) }},
			invalid{name: "null " + field, mutate: func(record map[string]any) { record[field] = nil }},
		)
	}
	cases = append(cases,
		invalid{name: "zero time", mutate: func(record map[string]any) { record["time"] = time.Time{} }},
		invalid{name: "empty cmd key", mutate: func(record map[string]any) { record["cmd_key"] = "" }},
	)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			statsPath, err := stats.Path()
			if err != nil {
				t.Fatal(err)
			}
			project, argv := "projectA", []string{"go", "test", "./..."}
			key := stats.ExecCommandKey(project, argv)
			for range stats.CheapHistoryRuns {
				err := stats.AppendExecHistory(statsPath, stats.ExecHistoryRecord{Time: time.Now(), CmdKey: key, CPUS: 1, TreeMemMB: new(20.0), Exit: 0})
				if err != nil {
					t.Fatal(err)
				}
			}
			badRecord := map[string]any{
				"time": time.Now().UTC(), "cmd_key": key, "cpu_s": 1.0,
				"tree_mem_mb": nil, "exit": 0, "cached": false, "host": false,
			}
			tc.mutate(badRecord)
			badLine, err := json.Marshal(badRecord)
			if err != nil {
				t.Fatal(err)
			}
			historyPath := stats.ExecHistoryPath(statsPath)
			f, err := os.OpenFile(historyPath, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write(append(badLine, '\n')); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}

			var stderr bytes.Buffer
			r := &Runner{Stderr: &stderr, Stats: stats.NewRecorder(&stderr)}
			_, cheap, _ := r.execShadow(project, argv)
			if cheap {
				t.Fatal("malformed history allowed would_cheap")
			}
			if strings.Count(stderr.String(), "graft: exec history unreadable:") != 1 {
				t.Fatalf("history warnings = %q, want exactly one", stderr.String())
			}
			r.Stats.Warn(errors.New("synthetic stats write failure"))
			if strings.Count(stderr.String(), "resource stats not recorded") != 1 {
				t.Fatalf("stats warning was suppressed by history warning: %q", stderr.String())
			}
		})
	}
}
