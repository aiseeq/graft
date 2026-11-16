package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/iosched"
	"github.com/aiseeq/graft/v2/internal/loadgate"
	"github.com/aiseeq/graft/v2/internal/stats"
	"github.com/aiseeq/graft/v2/internal/userconfig"
)

type gateLineObserver struct{ observe func(string) }

func (w gateLineObserver) Write(p []byte) (int, error) {
	w.observe(string(p))
	return len(p), nil
}

func TestGateWithAdvisoryHistoryLifecycle(t *testing.T) {
	for _, mode := range []string{"success", "success-no-parent", "admission-error", "split-error"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			cmd := exec.Command("git", "init", "-q", root)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git init: %s: %v", out, err)
			}
			for name, data := range map[string]string{".graft.yaml": "schema: 1\n", "go.mod": "module projectA\n\ngo 1.26\n"} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cfgDir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", cfgDir)
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			t.Setenv(HeavyEnv, "")
			t.Setenv(AdmittedEnv, "")
			if err := os.MkdirAll(filepath.Join(cfgDir, "graft"), 0700); err != nil {
				t.Fatal(err)
			}
			if mode != "admission-error" {
				data := "remote_heavy: {host: hostA, up: [/bin/true], run: [/bin/true], when: busy}\n"
				if err := os.WriteFile(filepath.Join(cfgDir, "graft", "config.yaml"), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			oldLoad, oldBusy := loadGateHistory, checkMachineBusy
			t.Cleanup(func() { loadGateHistory, checkMachineBusy = oldLoad, oldBusy })
			reads := 0
			loadGateHistory = func(_, _ string, _ time.Time) ([]stats.Record, error) { reads++; return nil, nil }
			checks := 0
			checkMachineBusy = func(context.Context, string, string, userconfig.MachineLoad, string, io.Writer) (bool, error) {
				checks++
				if mode == "split-error" && checks == 2 {
					return false, errors.New("split check failed")
				}
				return true, nil
			}
			cfg := &config.Config{GoFmt: config.GoFmt{Enabled: new(false)}}
			r := NewRunner(root, cfg, io.Discard, io.Discard)
			r.Load = func() (userconfig.MachineLoad, error) { return userconfig.MachineLoad{Threads: 1}, nil }
			r.admitTask = func(context.Context, string) (loadgate.Admission, func() error, error) {
				if mode == "admission-error" {
					return loadgate.Admission{}, nil, errors.New("admission failed")
				}
				return loadgate.Admission{}, func() error { return nil }, nil
			}
			previous := &gateHistory{loaded: true, records: []stats.Record{{Run: "previous"}}}
			if mode == "success-no-parent" {
				previous = nil
			}
			r.gateHistory = previous
			stepObserved := false
			r.Stdout = gateLineObserver{observe: func(line string) {
				if strings.Contains(line, "graft: gate 1/") {
					stepObserved = true
					if r.gateHistory != previous {
						t.Error("previous history not restored before the step")
					}
					if previous != nil {
						recs, _, err := r.lightGateRecords()
						if err != nil || len(recs) != 1 || recs[0].Run != "previous" {
							t.Errorf("step sees %v: %v", recs, err)
						}
					}
				}
			}}
			err := r.GateWithAdvisory(context.Background(), nil, nil)
			if mode == "admission-error" && (err == nil || !strings.Contains(err.Error(), "admission failed")) {
				t.Fatalf("admission error: %v", err)
			}
			if mode == "split-error" && (err == nil || !strings.Contains(err.Error(), "split check failed")) {
				t.Fatalf("split error: %v", err)
			}
			if strings.HasPrefix(mode, "success") && err != nil {
				t.Fatalf("gate: %v", err)
			}
			if reads != 1 {
				t.Errorf("history reads = %d, want one", reads)
			}
			if stepObserved != strings.HasPrefix(mode, "success") {
				t.Errorf("step observed = %t", stepObserved)
			}
			if r.gateHistory != previous {
				t.Error("previous history not restored after gate")
			}
		})
	}
}

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
	if !r.lightSplitRemainder([]string{"lex check"}) {
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
	if r.lightSplitRemainder([]string{"lex check"}) {
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

func TestRemoteFallbackUpgradesLightRemainderToAdmission(t *testing.T) {
	orig := configureHeavy
	t.Cleanup(func() { configureHeavy = orig })
	configureHeavy = func(_ context.Context, _ int, _ string, _ iosched.Request) (iosched.Placement, error) {
		return iosched.Placement{}, nil
	}
	admitted := 0
	r := &Runner{Stderr: &strings.Builder{}, admitTask: func(context.Context, string) (loadgate.Admission, func() error, error) {
		admitted++
		return loadgate.Admission{}, func() error { return nil }, nil
	}}
	lightLeave, err := r.enterLightRemainder(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.enterSplit(context.Background(), stats.GateTask, "", true); err != nil {
		t.Fatal(err)
	}
	if admitted != 1 || r.heavy != 1 || r.lightRemainder {
		t.Fatalf("admissions=%d heavy=%d lightRemainder=%v", admitted, r.heavy, r.lightRemainder)
	}
	if err := lightLeave(); err != nil {
		t.Fatal(err)
	}
	if r.splitLeave != nil {
		if err := r.splitLeave(); err != nil {
			t.Fatal(err)
		}
	}
	if r.heavy != 0 {
		t.Fatalf("heavy after release = %d", r.heavy)
	}
}

func TestLightRemainderLocalStepsDoNotRequestAdmission(t *testing.T) {
	orig := configureHeavy
	t.Cleanup(func() { configureHeavy = orig })
	configureHeavy = func(_ context.Context, _ int, _ string, _ iosched.Request) (iosched.Placement, error) {
		return iosched.Placement{}, nil
	}
	for _, step := range []string{"lex check", GoFmtSource} {
		t.Run(step, func(t *testing.T) {
			mem := 16.0
			var history []stats.Record
			since := time.Now().Add(-time.Hour)
			for i := range stats.LightGateRuns {
				run := string(rune('a' + i))
				local := stats.Record{Kind: stats.KindStep, Run: run, Step: step, UserS: .1, TreeMemMB: &mem}
				if step == GoFmtSource {
					local.Builtin, local.BuiltinCostKnown = true, true
				}
				history = append(history, local,
					stats.Record{Kind: stats.KindStep, Run: run, Step: "glint", Host: "remote", UserS: 90, TreeMemMB: new(900.0)},
					stats.Record{Kind: stats.KindTask, Run: run, Task: stats.GateTask, Project: "/project", Steps: 2, Time: time.Now(), Exit: 0},
				)
			}
			if !stats.SplitLightGate(history, "/project", since, []string{step}) {
				t.Fatal("light split-gate history was rejected")
			}
			admitted := 0
			r := &Runner{Stderr: &strings.Builder{}, admitTask: func(context.Context, string) (loadgate.Admission, func() error, error) {
				admitted++
				return loadgate.Admission{}, func() error { return nil }, nil
			}}
			lightLeave, err := r.enterLightRemainder(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := r.enterSplit(context.Background(), stats.GateTask, "", false); err != nil {
				t.Fatal(err)
			}
			if admitted != 0 || r.heavy != 1 || !r.lightRemainder {
				t.Fatalf("local step %q: admissions=%d heavy=%d lightRemainder=%v", step, admitted, r.heavy, r.lightRemainder)
			}
			if err := lightLeave(); err != nil {
				t.Fatal(err)
			}
			if r.heavy != 0 {
				t.Fatalf("heavy after leave = %d", r.heavy)
			}
		})
	}
}

func TestSplitLocalStepNamesFillThreads(t *testing.T) {
	cfg, err := config.Parse([]byte("schema: 1\nversion: {mode: none}\ntasks:\n  work:\n    run: [\"echo -p {threads}\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{
		Root: t.TempDir(), Config: cfg, Stderr: io.Discard, Stdout: io.Discard, noRepoName: true,
		Load: func() (userconfig.MachineLoad, error) { return userconfig.MachineLoad{Threads: 4}, nil },
	}
	names, err := r.splitLocalStepNames([]gateStep{{step: config.Step{Task: "work"}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"echo -p 4"}; !slices.Equal(names, want) {
		t.Fatalf("local step names %q, want %q as the journal records them", names, want)
	}
}

func TestGateHistoryIsSharedOnlyDuringGate(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if err := os.WriteFile(filepath.Join(root, ".graft.yaml"), []byte("schema: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path, err := stats.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(rec stats.Record) {
		t.Helper()
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(stats.Record{Kind: stats.KindTask, Task: stats.GateTask, Project: root, Run: "first"})
	r := &Runner{Root: root, gateHistory: &gateHistory{}}
	first, since, err := r.lightGateRecords()
	if err != nil {
		t.Fatal(err)
	}
	original, _, err := stats.Load(path, since)
	if err != nil || !reflect.DeepEqual(first, original) {
		t.Fatalf("cached history differs from original: %v", err)
	}
	if stats.LightGate(first, root, since) != stats.LightGate(original, root, since) || stats.SplitLightGate(first, root, since, []string{"check"}) != stats.SplitLightGate(original, root, since, []string{"check"}) {
		t.Fatal("history changed gate placement")
	}
	write(stats.Record{Kind: stats.KindTask, Task: stats.GateTask, Project: root, Run: "second"})
	second, since2, err := r.lightGateRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(second) != 1 || first[0].Run != "first" || second[0].Run != "first" || since != since2 {
		t.Fatalf("cached history changed: %v %v", first, second)
	}
	r.gateHistory = nil
	fresh, _, err := r.lightGateRecords()
	if err != nil || len(fresh) != 1 || fresh[0].Run != "second" {
		t.Fatalf("fresh history = %v, %v", fresh, err)
	}
}
