package loadgate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/loadavg"
	"github.com/aiseeq/graft/v2/internal/userconfig"
)

// fakeMachine points the readers at files of a calm machine, swapping
// nothing, and returns setters of the load and the IO pressure.
func fakeMachine(t *testing.T) (setLoad, setIO func(string)) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(loadavg.FileEnv, filepath.Join(dir, "loadavg"))
	t.Setenv(loadavg.PressureEnv, filepath.Join(dir, "io"))
	t.Setenv(loadavg.VMStatEnv, filepath.Join(dir, "vmstat"))
	setLoad = func(l string) { write("loadavg", l+" 1 1 1/1 1\n") }
	setIO = func(full string) {
		write("io", "some avg10=0.00 avg60=0.00 avg300=0.00 total=1\nfull avg10=0.00 avg60="+full+" avg300=0.00 total=1\n")
	}
	setLoad("1.00")
	setIO("0.00")
	write("vmstat", "nr_free_pages 1\npswpin 7\npswpout 0\n")
	return setLoad, setIO
}

var testLoad = userconfig.MachineLoad{FreeBelow: 22, WaitAbove: 22, MaxIOFull: 10, MaxSwapKBs: 1024,
	SwapWindow: 20 * time.Millisecond, Settle: time.Hour, Poll: 10 * time.Millisecond}

// TestCalmMachineStartsAtOnce: below every limit a task neither queues nor
// settles.
func TestCalmMachineStartsAtOnce(t *testing.T) {
	fakeMachine(t)
	var log strings.Builder
	waited, err := Wait(context.Background(), "build", nil, testLoad, filepath.Join(t.TempDir(), "load.lock"), &log)
	if err != nil || waited.Total() != 0 || log.Len() != 0 {
		t.Errorf("waited %s, %v:\n%s", waited, err, log.String())
	}
}

// TestBusyDiskAndSwapHoldTasks: IO pressure or swapping above its limit
// holds a task as the load does, and it says why.
func TestBusyDiskAndSwapHoldTasks(t *testing.T) {
	for _, c := range []struct {
		name, want string
		busy       func(setIO func(string))
	}{
		{"io", "IO pressure 35.0% is above 10%",
			func(setIO func(string)) { setIO("35.00") }},
		{"swap", "KB/s is above 1024",
			func(func(string)) {
				// A million pages more on every read: far above 1024 KB/s.
				var pages int64
				swapPages = func() (int64, error) { pages += 1e6; return pages, nil }
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, setIO := fakeMachine(t)
			defer func(read func() (int64, error)) { swapPages = read }(swapPages)
			c.busy(setIO)
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			var log strings.Builder
			p := &Priority{For: "graft deploy prod", Budget: newBudget(t, time.Hour)}
			waits, err := Wait(ctx, "build", p, testLoad, filepath.Join(t.TempDir(), "load.lock"), &log)
			if err == nil {
				t.Errorf("started on a busy machine:\n%s", log.String())
			}
			if waits[c.name] <= 0 || len(waits) != 1 {
				t.Errorf("waits %v, want all of it for %s", waits, c.name)
			}
			if !strings.Contains(log.String(), c.want) {
				t.Errorf("log lacks %q:\n%s", c.want, log.String())
			}
		})
	}
}

func newBudget(t *testing.T, d time.Duration) string {
	t.Helper()
	path, err := NewBudget(t.TempDir(), d)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPriorityBudget: a priority task waits out of the deploy's budget, then
// starts, or with Fail fails; the deploy's tasks share the budget, and only
// waiting spends it, not the time the deploy runs; a task without a
// priority keeps waiting.
func TestPriorityBudget(t *testing.T) {
	setLoad, _ := fakeMachine(t)
	lockPath := filepath.Join(t.TempDir(), "load.lock")
	var log strings.Builder
	p := &Priority{For: "graft deploy prod", Budget: newBudget(t, 100*time.Millisecond)}
	// A task that finds the machine calm spends nothing, however long the
	// deploy has run.
	setLoad("1.00")
	if waited, err := Wait(context.Background(), "lint", p, testLoad, lockPath, &log); err != nil || waited.Total() != 0 {
		t.Fatalf("on a calm machine: waited %s, %v", waited, err)
	}
	if left, err := p.spend(0); err != nil || left != 100*time.Millisecond {
		t.Fatalf("budget left after a calm start: %s, %v", left, err)
	}
	setLoad("30.00")
	waited, err := Wait(context.Background(), "build", p, testLoad, lockPath, &log)
	if err != nil || waited.Total() < 90*time.Millisecond || waited.Total() > time.Second || waited[WhyLoad] != waited.Total() {
		t.Fatalf("waited %v, %v", waited, err)
	}
	if !strings.Contains(log.String(), "starting after") || !strings.Contains(log.String(), "all the wait graft deploy prod allows") {
		t.Errorf("log:\n%s", log.String())
	}
	// The next task of the deploy finds the budget spent.
	if waited, err := Wait(context.Background(), "test", p, testLoad, lockPath, &log); err != nil || waited.Total() > 50*time.Millisecond {
		t.Errorf("a task after the budget was spent waited %s, %v", waited, err)
	}
	p = &Priority{For: "graft deploy prod", Budget: newBudget(t, 100*time.Millisecond), Fail: true}
	if _, err := Wait(context.Background(), "build", p, testLoad, lockPath, &log); err == nil || !strings.Contains(err.Error(), "past the wait graft deploy prod allows") {
		t.Errorf("fail when the budget is spent: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := Wait(ctx, "build", nil, testLoad, lockPath, &log); err == nil {
		t.Error("a task without a priority started at load 30")
	}
}

// TestPriorityFromEnv: a priority needs its budget and a known then.
func TestPriorityFromEnv(t *testing.T) {
	want := Priority{For: "graft deploy prod", Budget: "/tmp/budget", Fail: true}
	for k, v := range want.Env() {
		t.Setenv(k, v)
	}
	got, ok, err := PriorityFromEnv()
	if err != nil || !ok || got != want {
		t.Errorf("got %+v, %v", got, err)
	}
	t.Setenv(EnvThen, "maybe")
	if _, _, err := PriorityFromEnv(); err == nil {
		t.Error("then maybe accepted")
	}
	t.Setenv(EnvThen, ThenStart)
	t.Setenv(EnvBudget, "")
	if _, _, err := PriorityFromEnv(); err == nil {
		t.Error("a priority without a budget accepted")
	}
	t.Setenv(EnvPriority, "")
	if p, ok, err := PriorityFromEnv(); ok || err != nil {
		t.Errorf("no priority: %+v, %v", p, err)
	}
}
