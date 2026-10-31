package loadgate

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/userconfig"
)

// fakeMachine points the measures at a directory of a calm machine with 22
// threads, swapping nothing, with a disk state source of a calm disk, and
// returns setters of the tasks ready to run, the IO pressure of the last 5
// seconds and the swap count.
func fakeMachine(t *testing.T) (setRunnable func(int), setIO func(string), setSwap func(int64)) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "pressure"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		// A rename: the measure never reads a half-written file.
		tmp := filepath.Join(dir, "tmp")
		if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(ProcEnv, dir)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	setRunnable = func(n int) {
		var stat strings.Builder
		stat.WriteString("cpu  1 1 1 1\n")
		for i := range 22 {
			fmt.Fprintf(&stat, "cpu%d 1 1 1 1\n", i)
		}
		// procs_running counts the reader too.
		fmt.Fprintf(&stat, "procs_running %d\n", n+1)
		write("stat", stat.String())
	}
	write(filepath.Join("pressure", "memory"), "some avg10=0.00 avg60=0.00 avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1\n")
	write(filepath.Join("pressure", "io"), "some avg10=0.00 avg60=0.00 avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1\n")
	disk := fakeDisk(t)
	setIO = func(full string) {
		pct, err := strconv.ParseFloat(full, 64)
		if err != nil {
			t.Fatal(err)
		}
		disk.set(pct*50, "")
	}
	setSwap = func(pages int64) { write("vmstat", fmt.Sprintf("nr_free_pages 1\npswpin %d\npswpout 0\n", pages)) }
	setRunnable(1)
	setIO("0.00")
	setSwap(7)
	return setRunnable, setIO, setSwap
}

func TestDirtyWritebackKBReadsProcEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte("MemTotal: 10 kB\nDirty: 123 kB\nWriteback: 456 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ProcEnv, dir)
	got, err := DirtyWritebackKB()
	if err != nil || got != 579 {
		t.Fatalf("DirtyWritebackKB() = %d, %v; want 579, nil", got, err)
	}
}

// wait is Wait for a task of projectA, with only its waits.
func wait(ctx context.Context, name string, p *Priority, l userconfig.MachineLoad, lockPath string, log io.Writer) (Waits, func() error, error) {
	adm, release, err := Wait(ctx, name, "projectA:"+name, p, l, lockPath, log)
	return adm.Waits, release, err
}

func awaitFiles(t *testing.T, pattern string, count int) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) >= count {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("timed out waiting for %d files matching %s; found %d", count, pattern, len(matches))
		}
	}
}

var testLoad = userconfig.MachineLoad{WaitAbove: 22, MaxIOFull: 10, MaxSwapKBs: 1024,
	SwapWindow: 20 * time.Millisecond, Settle: time.Hour, Poll: 10 * time.Millisecond}

// TestCalmMachineStartsOneAtATime: on a calm machine a task starts at once,
// and the next one waits while the first settles, until it ends or the
// settling is over.
func TestCalmMachineStartsOneAtATime(t *testing.T) {
	fakeMachine(t)
	lockPath := filepath.Join(t.TempDir(), "load.lock")
	var log strings.Builder
	waited, release, err := wait(context.Background(), "build", nil, testLoad, lockPath, &log)
	if err != nil || waited.Total() > 100*time.Millisecond || log.Len() != 0 {
		t.Fatalf("waited %s, %v:\n%s", waited, err, log.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, _, err := wait(ctx, "test", nil, testLoad, lockPath, &log); err == nil {
		t.Error("the next task started while the first settled")
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Errorf("a second release: %v", err)
	}
	waited, release, err = wait(context.Background(), "test", nil, testLoad, lockPath, &log)
	if err != nil || waited.Total() > 100*time.Millisecond {
		t.Fatalf("after the first ended: waited %s, %v:\n%s", waited, err, log.String())
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	// The settling ends by itself.
	short := testLoad
	short.Settle = 20 * time.Millisecond
	if _, release, err = wait(context.Background(), "build", nil, short, lockPath, &log); err != nil {
		t.Fatal(err)
	}
	defer release()
	if waited, release, err := wait(context.Background(), "test", nil, short, lockPath, &log); err != nil || waited.Total() > time.Second {
		t.Errorf("after the settling: waited %s, %v", waited, err)
	} else if err := release(); err != nil {
		t.Error(err)
	}
}

func TestWaitNamesWhoHeldTheQueue(t *testing.T) {
	fakeMachine(t)
	path := filepath.Join(t.TempDir(), "load.lock")
	l := testLoad
	l.Settle = time.Hour
	_, release, err := Wait(context.Background(), "head", "projectA:head", nil, l, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	awaitFiles(t, path+".*.lholder", 1)
	got := make(chan Admission, 1)
	go func() {
		adm, rel, err := Wait(context.Background(), "follower", "projectA:follower", nil, l, path, io.Discard)
		if err == nil {
			_ = rel()
		}
		if err != nil {
			adm.Waits = nil
		}
		got <- adm
	}()
	awaitFiles(t, path+".queue.*.lticket", 1)
	if err := release(); err != nil {
		t.Fatal(err)
	}
	select {
	case adm := <-got:
		if !strings.Contains(adm.WaitedBehind, "graft head") {
			t.Fatalf("WaitedBehind = %q", adm.WaitedBehind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follower did not leave the queue")
	}
}

func TestWaitWithSettleNamesWhoHeldTheSettleQueue(t *testing.T) {
	fakeMachine(t)
	dir := t.TempDir()
	path, settlePath := filepath.Join(dir, "load.lock"), filepath.Join(dir, "load.settle.lock")
	_, release, err := WaitWithSettle(context.Background(), "head", "projectA:head", nil, testLoad, path, settlePath, time.Hour, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	awaitFiles(t, settlePath+".*.lholder", 1)
	got := make(chan Admission, 1)
	go func() {
		adm, rel, err := WaitWithSettle(context.Background(), "follower", "projectA:follower", nil, testLoad, path, settlePath, time.Hour, io.Discard)
		if err == nil {
			_ = rel()
		}
		got <- adm
	}()
	awaitFiles(t, settlePath+".queue.*.lticket", 1)
	if err := release(); err != nil {
		t.Fatal(err)
	}
	select {
	case adm := <-got:
		if !strings.Contains(adm.WaitedBehind, "graft head") {
			t.Fatalf("WaitedBehind = %q", adm.WaitedBehind)
		}
		if adm.Waits[WhyQueue] <= 0 {
			t.Fatalf("queue wait = %s", adm.Waits[WhyQueue])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follower did not leave the settle queue")
	}
}

func TestWaitBehindQuietIsQuiet(t *testing.T) {
	fakeMachine(t)
	setQuiet(t, time.Now().Add(5*time.Second))
	path := filepath.Join(t.TempDir(), "load.lock")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	first := make(chan struct{})
	go func() {
		_, _, _ = Wait(ctx, "head", "projectA:head", nil, testLoad, path, io.Discard)
		close(first)
	}()
	awaitFiles(t, path+".*.lholder", 1)
	followerCtx, stop := context.WithTimeout(context.Background(), 3500*time.Millisecond)
	defer stop()
	type waitResult struct {
		adm Admission
		err error
	}
	got := make(chan waitResult, 1)
	go func() {
		adm, _, err := Wait(followerCtx, "follower", "projectA:follower", nil, testLoad, path, io.Discard)
		got <- waitResult{adm: adm, err: err}
	}()
	awaitFiles(t, path+".queue.*.lticket", 1)
	var result waitResult
	select {
	case result = <-got:
	case <-time.After(4 * time.Second):
		t.Fatal("follower did not leave the queue")
	}
	if result.err == nil {
		t.Fatal("quiet machine admitted a task")
	}
	<-first
	if result.adm.Waits[WhyQuiet] <= 0 || result.adm.Waits[WhyQueue] >= result.adm.Waits[WhyQuiet] {
		t.Fatalf("waits = %v, want quiet queue attribution", result.adm.Waits)
	}
}

// quietUntil is the end of the quiet mode the tests set: well ahead.
var quietUntil = time.Now().Add(time.Hour)

// quietUntilText is quietUntil as akit says it: the time of day, the date
// too when it is not today.
func quietUntilText() string {
	if now := time.Now(); quietUntil.YearDay() != now.YearDay() || quietUntil.Year() != now.Year() {
		return quietUntil.Format("2006-01-02 15:04")
	}
	return quietUntil.Format("15:04")
}

// setQuiet sets the quiet mode akit's busy --quiet-until sets, until the time.
func setQuiet(t *testing.T, until time.Time) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "akit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "quiet-until"), []byte(until.Format(time.RFC3339)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestQuietModeOverHoldsNoTask: a quiet mode whose time has passed holds
// nothing.
func TestQuietModeOverHoldsNoTask(t *testing.T) {
	fakeMachine(t)
	setQuiet(t, time.Now().Add(-time.Minute))
	var log strings.Builder
	waits, release, err := wait(context.Background(), "build", nil, testLoad, filepath.Join(t.TempDir(), "load.lock"), &log)
	if err != nil || waits[WhyQuiet] > 0 {
		t.Fatalf("waits %v, %v:\n%s", waits, err, log.String())
	}
	if err := release(); err != nil {
		t.Error(err)
	}
}

// TestBusyDiskAndSwapHoldTasks: IO pressure or swapping above its limit, or
// the quiet mode, holds a task as the load does, and it says why.
func TestBusyDiskAndSwapHoldTasks(t *testing.T) {
	for _, c := range []struct {
		name, want string
		busy       func(t *testing.T, setIO func(string), setSwap func(int64))
	}{
		{"io", "IO pressure 35.0% of the last 5s is at or above 10% (user config load.max_io_full)",
			func(_ *testing.T, setIO func(string), _ func(int64)) { setIO("35.00") }},
		{"swap", "memory pressure 25.0% is above 10% while swapping",
			func(t *testing.T, _ func(string), setSwap func(int64)) {
				dir := os.Getenv(ProcEnv)
				if err := os.WriteFile(filepath.Join(dir, "pressure", "memory"), []byte("some avg10=25.00 avg60=0.00 avg300=0.00 total=1\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				swapping(t, setSwap)
			}},
		{"quiet", "quiet mode until " + quietUntilText() + " (busy --quiet-off ends it)",
			func(t *testing.T, _ func(string), _ func(int64)) { setQuiet(t, quietUntil) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, setIO, setSwap := fakeMachine(t)
			c.busy(t, setIO, setSwap)
			// Long enough for the first look at a loaded machine, the disk
			// source's included, to end before the wait does.
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			l := testLoad
			if c.name == "swap" {
				// A look after the first measures the swapping since the
				// one before, a poll ago: the fake's writer, stalled for
				// longer under -race, must not leave a look with none.
				l.Poll, l.SwapWindow = 100*time.Millisecond, 100*time.Millisecond
			}
			var log strings.Builder
			p := &Priority{For: "graft deploy prod", Budget: newBudget(t, time.Hour)}
			waits, _, err := wait(ctx, "build", p, l, filepath.Join(t.TempDir(), "load.lock"), &log)
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

// swapping adds a million pages to the swapped ones every few milliseconds,
// far above 1024 KB/s, until the test ends.
func swapping(t *testing.T, setSwap func(int64)) {
	t.Helper()
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for pages := int64(1e6); ; pages += 1e6 {
			select {
			case <-done:
				return
			case <-time.After(2 * time.Millisecond):
				setSwap(pages)
			}
		}
	}()
	t.Cleanup(func() { close(done); <-stopped })
}

// TestSwappingAloneStarts: swapping without memory pressure, pages going out
// to make room for the cache, does not hold a task.
func TestSwappingAloneStarts(t *testing.T) {
	_, _, setSwap := fakeMachine(t)
	swapping(t, setSwap)
	var log strings.Builder
	waits, release, err := wait(context.Background(), "build", nil, testLoad, filepath.Join(t.TempDir(), "load.lock"), &log)
	if err != nil || waits[WhySwap] != 0 || log.Len() != 0 {
		t.Fatalf("waits %v, %v:\n%s", waits, err, log.String())
	}
	if err := release(); err != nil {
		t.Fatal(err)
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
	setRunnable, _, _ := fakeMachine(t)
	lockPath := filepath.Join(t.TempDir(), "load.lock")
	var log strings.Builder
	p := &Priority{For: "graft deploy prod", Budget: newBudget(t, 100*time.Millisecond)}
	// A task that finds the machine calm spends nothing, however long the
	// deploy has run.
	setRunnable(1)
	if waited, _, err := wait(context.Background(), "lint", p, testLoad, lockPath, &log); err != nil || waited.Total() != 0 {
		t.Fatalf("on a calm machine: waited %s, %v", waited, err)
	}
	if left, err := p.spend(0); err != nil || left != 100*time.Millisecond {
		t.Fatalf("budget left after a calm start: %s, %v", left, err)
	}
	setRunnable(30)
	waited, _, err := wait(context.Background(), "build", p, testLoad, lockPath, &log)
	if err != nil || waited.Total() < 90*time.Millisecond || waited.Total() > time.Second || waited[WhyLoad] != waited.Total() {
		t.Fatalf("waited %v, %v", waited, err)
	}
	if !strings.Contains(log.String(), "starting after") || !strings.Contains(log.String(), "all the wait graft deploy prod allows") {
		t.Errorf("log:\n%s", log.String())
	}
	// The next task of the deploy finds the budget spent.
	if waited, _, err := wait(context.Background(), "test", p, testLoad, lockPath, &log); err != nil || waited.Total() > 50*time.Millisecond {
		t.Errorf("a task after the budget was spent waited %s, %v", waited, err)
	}
	p = &Priority{For: "graft deploy prod", Budget: newBudget(t, 100*time.Millisecond), Fail: true}
	if _, _, err := wait(context.Background(), "build", p, testLoad, lockPath, &log); err == nil || !strings.Contains(err.Error(), "past the wait graft deploy prod allows") {
		t.Errorf("fail when the budget is spent: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, _, err := wait(ctx, "build", nil, testLoad, lockPath, &log); err == nil {
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
