package tasks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/iosched"
	"github.com/aiseeq/graft/v2/internal/loadgate"
	"github.com/aiseeq/graft/v2/internal/stats"
)

func TestLightSplitRemainderUsesLocalHistory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".graft.yaml"), []byte("schema: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	now := time.Now().Add(time.Second)
	mem := 16.0
	var records []stats.Record
	for i := range stats.LightGateRuns {
		run := string(rune('a' + i))
		records = append(records,
			stats.Record{Kind: stats.KindStep, Run: run, Task: "gate", Step: "lex check", Time: now, UserS: .1, TreeMemMB: &mem},
			stats.Record{Kind: stats.KindStep, Run: run, Task: "glint", Step: "glint", Time: now, Host: "remote", UserS: 90, TreeMemMB: new(900.0)},
			stats.Record{Kind: stats.KindTask, Run: run, Task: stats.GateTask, Project: root, Steps: 2, Exit: 0, Time: now},
		)
	}
	path, err := stats.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if err := json.NewEncoder(f).Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := stats.Load(path, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !stats.SplitLightGate(loaded, root, time.Now().Add(-time.Hour), []string{"lex check"}) {
		t.Fatalf("fixture history does not pass: %+v", loaded)
	}
	var stderr strings.Builder
	r := &Runner{Root: root, Stderr: &stderr}
	if !r.lightSplitRemainder(context.Background(), []string{"lex check"}) {
		t.Fatalf("local history was not accepted: %s", stderr.String())
	}
	records[6].UserS = stats.LightGateCPUSec + 1
	f, err = os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if err := json.NewEncoder(f).Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if r.lightSplitRemainder(context.Background(), []string{"lex check"}) {
		t.Fatal("a heavy local history was accepted")
	}
}

func TestEnterLightRemainderSkipsAdmissionAndReleases(t *testing.T) {
	orig := configureHeavy
	t.Cleanup(func() { configureHeavy = orig })
	configured := 0
	configureHeavy = func(_ context.Context, _ int, _ string, _ iosched.Request) (iosched.Placement, error) {
		configured++
		return iosched.Placement{}, nil
	}
	admitted := 0
	r := &Runner{Stderr: &strings.Builder{}, admitTask: func(context.Context, string) (loadgate.Admission, func() error, error) {
		admitted++
		return loadgate.Admission{}, func() error { return nil }, nil
	}}
	leave, err := r.enterLightRemainder(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.heavy != 1 || configured != 1 || admitted != 0 {
		t.Fatalf("heavy=%d configured=%d admissions=%d", r.heavy, configured, admitted)
	}
	if err := leave(); err != nil {
		t.Fatal(err)
	}
	if r.heavy != 0 {
		t.Fatalf("heavy after leave = %d", r.heavy)
	}
}
