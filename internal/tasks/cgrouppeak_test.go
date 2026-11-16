package tasks

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/iosched"
	"github.com/aiseeq/graft/v2/internal/loadgate"
	"github.com/aiseeq/graft/v2/internal/stats"
)

func TestHeavyScopeCgroupPeakStats(t *testing.T) {
	for _, tc := range []struct {
		name     string
		own      bool
		peak     string
		wantPeak *float64
	}{
		{name: "created scope", own: true, peak: "681574400\n", wantPeak: new(650.0)},
		{name: "inherited scope", peak: "681574400\n"},
		{name: "memory.peak unavailable", own: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := t.TempDir()
			t.Setenv("XDG_CACHE_HOME", cache)
			procRoot := t.TempDir()
			cgroup := "/user.slice/agents-graft.slice/graft-heavy-7.scope"
			write := func(path, content string) {
				t.Helper()
				file := filepath.Join(procRoot, filepath.FromSlash(path))
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			write("self/cgroup", "0::"+cgroup+"\n")
			if tc.peak != "" {
				write("cgroup"+cgroup+"/memory.peak", tc.peak)
			}
			t.Setenv(loadgate.ProcEnv, procRoot)

			oldConfigure, oldHalfMode := configureHeavy, halfMode
			configureHeavy = func(_ context.Context, _ int, _ string, _ iosched.Request) (iosched.Placement, error) {
				return iosched.Placement{Ceiling: 1 << 30, Scope: "graft-heavy-7.scope", Own: tc.own}, nil
			}
			halfMode = func() (int64, int, time.Time, error) { return 0, 0, time.Time{}, nil }
			t.Cleanup(func() { configureHeavy, halfMode = oldConfigure, oldHalfMode })

			var stderr bytes.Buffer
			f := &frame{task: "synthetic", start: time.Now(), heavy: true}
			r := &Runner{
				Root: t.TempDir(), Stats: stats.NewRecorder(io.Discard), Stderr: &stderr,
				noRepoName: true, frames: []*frame{f},
				admitTask: func(context.Context, string) (loadgate.Admission, func() error, error) {
					return loadgate.Admission{}, func() error { return nil }, nil
				},
			}
			leave, err := r.enterHeavy(context.Background(), f.task, "")
			if err != nil {
				t.Fatal(err)
			}
			r.recordStep("synthetic step", stats.Usage{}, 0, "")
			if err := leave(); err != nil {
				t.Fatal(err)
			}
			r.end(f, nil)

			path, err := stats.Path()
			if err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			scanner := bufio.NewScanner(file)
			var taskFound, stepFound bool
			for scanner.Scan() {
				var record map[string]json.RawMessage
				if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
					t.Fatal(err)
				}
				var kind string
				if err := json.Unmarshal(record["kind"], &kind); err != nil {
					t.Fatal(err)
				}
				_, hasPeak := record["cgroup_peak_mb"]
				switch kind {
				case stats.KindStep:
					stepFound = true
					if hasPeak {
						t.Errorf("step record unexpectedly has cgroup_peak_mb: %s", scanner.Text())
					}
				case stats.KindTask:
					taskFound = true
					if tc.wantPeak == nil {
						if hasPeak {
							t.Errorf("task record unexpectedly has cgroup_peak_mb: %s", scanner.Text())
						}
					} else {
						var got float64
						if !hasPeak || json.Unmarshal(record["cgroup_peak_mb"], &got) != nil || got != *tc.wantPeak {
							t.Errorf("task cgroup_peak_mb = %s; want %g", record["cgroup_peak_mb"], *tc.wantPeak)
						}
					}
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			if !taskFound || !stepFound {
				t.Fatalf("task record found=%v, step record found=%v", taskFound, stepFound)
			}
			if stderr.Len() != 0 {
				t.Errorf("reading optional memory.peak wrote an error: %s", stderr.String())
			}
		})
	}
}
