package loadgate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/lock"
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
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	limitsPath := filepath.Join(configHome, "akit", "pressure.yaml")
	if err := os.MkdirAll(filepath.Dir(limitsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(limitsPath, []byte("runnable: 22\nmax_io_full: 10\nmax_swap_kbs: 1024\nswap_window: 20ms\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
	write("loadavg", "0.00 0.00 0.00 1/100 1\n")
	write(filepath.Join("pressure", "memory"), "some avg10=0.00 avg60=0.00 avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1\n")
	write(filepath.Join("pressure", "io"), "some avg10=0.00 avg60=0.00 avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1\n")
	write("meminfo", "Dirty:              1024 kB\nWriteback:             0 kB\n")
	if err := os.MkdirAll(filepath.Join(dir, "sys", "vm"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join("sys", "vm", "dirty_background_bytes"), "67108864\n")
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

func TestReserveLightTasksStartBackToBack(t *testing.T) {
	fakeMachine(t)
	oldPoll, oldWindow := reservePollInterval, reserveWindow
	reservePollInterval = 5 * time.Millisecond
	reserveWindow = 20 * time.Millisecond
	defer func() { reservePollInterval, reserveWindow = oldPoll, oldWindow }()
	l := testLoad
	l.Settle = 300 * time.Millisecond
	path := filepath.Join(t.TempDir(), "load.lock")
	_, release, err := Wait(context.Background(), "first", "projectA:first", nil, l, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	started := time.Now()
	_, release2, err := Wait(context.Background(), "second", "projectA:second", nil, l, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer release2()
	if time.Since(started) >= l.Settle {
		t.Fatalf("second task waited %s", time.Since(started))
	}
}

func TestReservePollPanicIsLoggedAndDropped(t *testing.T) {
	fakeMachine(t)
	oldPoll, oldReader := reservePollInterval, reserveCPUReader
	reservePollInterval = time.Millisecond
	reserveCPUReader = func(int) (float64, error) { panic("synthetic reserve panic") }
	defer func() { reservePollInterval, reserveCPUReader = oldPoll, oldReader }()

	var log strings.Builder
	l := testLoad
	l.Settle = time.Hour
	path := filepath.Join(t.TempDir(), "load.lock")
	adm, release, err := Wait(context.Background(), "panic-check", "projectA:panic-check", nil, l, path, &log)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !adm.ReserveStoppedAt().IsZero() {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if adm.ReserveStoppedAt().IsZero() {
		t.Fatal("reserve was not stopped after panic")
	}
	if got := log.String(); !strings.Contains(got, "graft: panic-check: panic in reserve poll: synthetic reserve panic") {
		t.Fatalf("panic log = %q", got)
	}
	if sum, err := liveReserveCores(path); err != nil || sum != 0 {
		t.Fatalf("live reserve cores = %v, %v; want 0", sum, err)
	}
}

func TestReserveSumHoldsTheHead(t *testing.T) {
	_, _, _ = fakeMachine(t)
	setRunnable := func(n int) {
		d := os.Getenv(ProcEnv)
		var b strings.Builder
		b.WriteString("cpu 1 1 1 1\n")
		for i := range 22 {
			fmt.Fprintf(&b, "cpu%d 1 1 1 1\n", i)
		}
		fmt.Fprintf(&b, "procs_running %d\n", n+1)
		_ = os.WriteFile(filepath.Join(d, "stat"), []byte(b.String()), 0o644)
	}
	path := filepath.Join(t.TempDir(), "load.lock")
	setRunnable(8)
	rel, err := lock.Reserve(filepath.Dir(path), filepath.Base(path)+".*.reserve", []byte(`{"cores":14,"admitted":"x","task":"first"}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancel()
	adm, _, err := Wait(ctx, "head", "projectA:head", nil, testLoad, path, io.Discard)
	if err == nil || adm.Waits[WhyReserve] == 0 {
		t.Fatalf("admission = %+v, %v; want reserve wait", adm, err)
	}
	if err := rel(); err != nil {
		t.Fatal(err)
	}
	_, release, err := Wait(context.Background(), "head", "projectA:head", nil, testLoad, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	_ = release()
	setRunnable(8)
	rel, err = lock.Reserve(filepath.Dir(path), filepath.Base(path)+".*.reserve", []byte(`{"cores":13,"admitted":"x","task":"first"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	_, release, err = Wait(context.Background(), "head", "projectA:head", nil, testLoad, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	_ = release()
}

func TestReserveDefaultRunnableIsThreads(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	path := filepath.Join(t.TempDir(), "load.lock")
	noReserve := newMachine(testLoad, io.Discard, "head", "projectA:head")
	noReserve.limits.Runnable = 0
	if err := noReserve.admitWithReserves(context.Background(), nil, path); err != nil {
		t.Fatalf("runnable 1 with no reserve: %v", err)
	}

	reserve, err := lock.Reserve(filepath.Dir(path), filepath.Base(path)+".*.reserve", []byte(`{"cores":22,"admitted":"x","task":"first"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer reserve()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	withReserve := newMachine(testLoad, io.Discard, "head", "projectA:head")
	withReserve.limits.Runnable = 0
	err = withReserve.admitWithReserves(ctx, nil, path)
	if err == nil || withReserve.waits[WhyReserve] == 0 {
		t.Fatalf("admission wait = %v, %v; want wait at default thread limit", withReserve.waits, err)
	}
}

func TestReserveReleasedOnCPU(t *testing.T) {
	var n float64
	testReserveEnd(t, "cpu", func() float64 {
		n += .1
		return n
	})
}
func TestReserveReleasedAtTheCap(t *testing.T) { testReserveEnd(t, "cap", func() float64 { return 0 }) }
func TestReserveReleasedWhenTheTaskEnds(t *testing.T) {
	testReserveEnd(t, "done", func() float64 { return 0 })
}
func TestReserveUnreadableCPU(t *testing.T) { testReserveEnd(t, "unread", func() float64 { return 0 }) }

func testReserveEnd(t *testing.T, want string, reader func() float64) {
	t.Helper()
	oldP, oldW, oldR := reservePollInterval, reserveWindow, reserveCPUReader
	reservePollInterval = 5 * time.Millisecond
	if want == "cpu" {
		reserveWindow = 100 * time.Millisecond
	} else {
		reserveWindow = 20 * time.Millisecond
	}
	defer func() { reservePollInterval, reserveWindow, reserveCPUReader = oldP, oldW, oldR }()
	switch want {
	case "unread":
		reserveCPUReader = func(int) (float64, error) { return 0, fmt.Errorf("synthetic proc read") }
	default:
		reserveCPUReader = func(int) (float64, error) { return reader(), nil }
	}
	path := filepath.Join(t.TempDir(), "load.lock")
	cap := 60 * time.Millisecond
	if want == "cpu" {
		cap = 300 * time.Millisecond
	}
	if want == "done" {
		cap = time.Second
	}
	release, _, end, _, err := startReserve(path, "task", 1, cap, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	for end() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if want == "done" {
		if err := release(); err != nil {
			t.Fatal(err)
		}
	}
	for end() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := end(); got != want {
		t.Fatalf("reserve end %q; want %q", got, want)
	}
	_ = release()
}

func TestReserveMissedHistoryStillMeetsRunnable(t *testing.T) {
	fakeMachine(t)
	setProfileHistory(t, "projectA:next", 1)
	d := os.Getenv(ProcEnv)
	b := fmt.Sprintf("cpu 1 1 1 1\nprocs_running %d\n", 25)
	_ = os.WriteFile(filepath.Join(d, "stat"), []byte(b), 0o644)
	// The wait is cut when it says it waits, not after a fixed time: a busy
	// machine under -race may not even measure within one.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	observed := make(chan struct{}, 1)
	go func() {
		<-observed
		cancel()
	}()
	adm, _, err := Wait(ctx, "next", "projectA:next", nil, testLoad, filepath.Join(t.TempDir(), "load.lock"), waitLogWriter{observed: observed})
	if err == nil || adm.Waits[WhyLoad] == 0 {
		t.Fatalf("high runnable admitted: %+v %v", adm, err)
	}
}

func TestReserveHugeHeadRunsAlone(t *testing.T) {
	fakeMachine(t)
	setProfileHistory(t, "projectA:huge", 22)
	path := filepath.Join(t.TempDir(), "load.lock")
	adm, release, err := Wait(context.Background(), "huge", "projectA:huge", nil, testLoad, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if adm.ReserveCores != 22 {
		t.Fatalf("reserve = %v", adm.ReserveCores)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, _, err = Wait(ctx, "next", "projectA:next", nil, testLoad, path, io.Discard)
	if err == nil {
		t.Fatal("next passed huge live reservation")
	}
}

func TestReservePriorityWritesItsReserve(t *testing.T) {
	fakeMachine(t)
	setProfileHistory(t, "projectA:priority", 22)
	path := filepath.Join(t.TempDir(), "load.lock")
	p := &Priority{For: "deploy", Budget: filepath.Join(t.TempDir(), "budget")}
	_ = os.WriteFile(p.Budget, []byte("1000000000"), 0o644)
	adm, release, err := Wait(context.Background(), "priority", "projectA:priority", p, testLoad, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if adm.ReserveCores != 22 {
		t.Fatalf("priority reserve = %v", adm.ReserveCores)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	next, _, err := Wait(ctx, "next", "projectA:next", nil, testLoad, path, io.Discard)
	if err == nil || next.Waits[WhyReserve] == 0 {
		t.Fatalf("next admission = %+v, %v", next, err)
	}
}

func setProfileHistory(t *testing.T, profile string, value float64) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "graft")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string][]float64{profile: {value}})
	if err := os.WriteFile(filepath.Join(dir, "load.cores.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}
func TestTreeCPUCountsReapedChildren(t *testing.T) {
	root := t.TempDir()
	pid := os.Getpid()
	dir := filepath.Join(root, fmt.Sprint(pid))
	if err := os.MkdirAll(filepath.Join(dir, "task", "1"), 0o755); err != nil {
		t.Fatal(err)
	}
	fields := make([]string, 15)
	fields[0] = "S"
	for i := 1; i < len(fields); i++ {
		fields[i] = "0"
	}
	fields[11] = "0"
	fields[12] = "0"
	fields[13] = "25"
	fields[14] = "25"
	line := fmt.Sprintf("%d (synthetic) %s\n", pid, strings.Join(fields, " "))
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task", "1", "children"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ProcEnv, root)
	got, err := treeCPU(pid)
	if err != nil || got != .5 {
		t.Fatalf("treeCPU = %v, %v; want .5", got, err)
	}
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

func TestPressureUserLimitsControlSwapAdmission(t *testing.T) {
	_, _, setSwap := fakeMachine(t)
	proc := os.Getenv(ProcEnv)
	if err := os.WriteFile(filepath.Join(proc, "pressure", "memory"), []byte("some avg10=25.00 avg60=0.00 avg300=0.00 total=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	configHome := os.Getenv("XDG_CONFIG_HOME")
	path := filepath.Join(configHome, "akit", "pressure.yaml")
	if err := os.WriteFile(path, []byte("runnable: 22\nmax_io_full: 10\nmax_mem_some: 10\nmax_swap_kbs: 1e20\nswap_window: 10ms\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	swapping(t, setSwap)
	busy, err := BusyNow(context.Background(), "build", "projectA:build", testLoad, "", io.Discard)
	if err != nil || busy {
		t.Fatalf("BusyNow = %v, %v with pressure.yaml max_swap_kbs 1e20; want admitted", busy, err)
	}
}

func TestBrokenPressureLimitsFailBusyCheck(t *testing.T) {
	fakeMachine(t)
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "akit", "pressure.yaml")
	if err := os.WriteFile(path, []byte("max_swap_kbs: not-a-number\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := BusyNow(context.Background(), "build", "projectA:build", testLoad, "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("BusyNow error = %v; want pressure limits error naming %s", err, path)
	}
}

// wait is Wait for a task of projectA, with only its waits.
func wait(ctx context.Context, name string, p *Priority, l userconfig.MachineLoad, lockPath string, log io.Writer) (Waits, func() error, error) {
	adm, release, err := Wait(ctx, name, "projectA:"+name, p, l, lockPath, log)
	return adm.Waits, release, err
}

// signalWriter closes seen once a write contains text.
type signalWriter struct {
	text string
	seen chan struct{}
	once sync.Once
}

func (w *signalWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.text) {
		w.once.Do(func() { close(w.seen) })
	}
	return len(p), nil
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

func kernelLoad(old int64, n int) int64 {
	newFixed := old*1884 + int64(n)*2048*(2048-1884)
	if int64(n)*2048 >= old {
		newFixed += 2047
	}
	return newFixed >> 11
}

func visibleLoad(fixed int64) float64 {
	return math.Round(float64(fixed)/2048*100) / 100
}

func TestKernelSamplesFromLoadAvg(t *testing.T) {
	s := kernelSamples{}
	at := time.Unix(0, 0)
	fixed, avg := int64(0), 0.0
	s.observe(at, avg, true)
	want := []int{12, 17, 17, 12, 11, 17, 31, 46}
	for i, n := range want {
		at = at.Add(500 * time.Millisecond)
		before := avg
		fixed = kernelLoad(fixed, n)
		avg = visibleLoad(fixed)
		s.observe(at, before, true)
		at = at.Add(500 * time.Millisecond)
		s.observe(at, avg, true)
		got := s.values[len(s.values)-1]
		if math.Abs(got-float64(n)) > .15 {
			t.Fatalf("sample %d = %.3f; want %d ±0.15 (visible avg %.2f)", i, got, n, avg)
		}
		for range 4 {
			at = at.Add(time.Second)
			s.observe(at, avg, true)
		}
	}
	last := len(s.values)
	if last != 2 {
		t.Fatalf("retained %d samples, want the latest two", last)
	}
	mean := (s.values[0] + s.values[1]) / 2
	if got := (&machine{avg1: avg, samples: s}).loadValue(true); got != mean || got == avg {
		t.Fatalf("priority load value %.2f, sample mean %.2f, avg1 %.2f", got, mean, avg)
	}
	before := len(s.values)
	at = at.Add(2 * time.Second)
	avg = visibleLoad(kernelLoad(fixed, 20))
	s.observe(at, avg, true)
	if len(s.values) != 0 || !s.have {
		t.Fatalf("missed kernel tick did not clear sample history: %+v (was %d samples)", s, before)
	}
	s.observe(at.Add(time.Second), 0, false)
	if len(s.values) != 0 || s.have {
		t.Fatalf("failed loadavg read did not clear sampling baseline: %+v", s)
	}
}

func TestPriorityWaitPollsKernelSamplesBetweenLoadChecks(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setRunnableLimit(t, 23)
	fixed := int64(26 * 2048) // A prior stage has left an avg1 tail above the limit.
	for range 6 {
		fixed = kernelLoad(fixed, 32) // Background 12 plus this stage's 20, for 30 seconds.
	}
	avg := visibleLoad(fixed)
	if avg <= 23 {
		t.Fatalf("stage-end avg1 %.2f; want the prior tail still above 23", avg)
	}
	setLoadavg(t, avg)
	m := newMachine(testLoad, io.Discard, "sample-cycle", "projectA:sample-cycle")
	m.l.Poll = 5 * time.Second
	start := time.Unix(1000, 0)
	now := start
	m.now = func() time.Time { return now }
	nextKernelTick := start.Add(5 * time.Second)
	m.sleep = func(ctx context.Context, d time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		now = now.Add(d)
		for !nextKernelTick.After(now) {
			fixed = kernelLoad(fixed, 12)
			avg = visibleLoad(fixed)
			setLoadavg(t, avg)
			nextKernelTick = nextKernelTick.Add(5 * time.Second)
		}
		return nil
	}
	budget, err := NewBudget(t.TempDir(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p := &Priority{For: "projectA deploy", Budget: budget}
	if err := m.admitWithReserves(context.Background(), p, filepath.Join(t.TempDir(), "load.lock")); err != nil {
		t.Fatalf("priority admission: %v", err)
	}
	a, b, ok := m.samples.lastTwo()
	if !ok || math.Abs(a-12) > .15 || math.Abs(b-12) > .15 {
		t.Fatalf("admitted without two recovered background samples: %.2f, %.2f, %v", a, b, ok)
	}
	if elapsed := now.Sub(start); elapsed > 10*time.Second+500*time.Millisecond+m.l.Poll || m.avg1 <= 23 {
		t.Fatalf("admitted after %s at avg1 %.2f; want within two samples plus one Poll while avg1 stays above 23", elapsed, m.avg1)
	}
}

func TestPriorityOwnTailDoesNotSpendBudget(t *testing.T) {
	fakeMachine(t)
	setRunnableLimit(t, 23)
	fixed := int64(20 * 2048)
	avg := visibleLoad(fixed)
	at := time.Unix(1000, 0)
	samples := kernelSamples{}
	samples.observe(at, avg, true)
	step := func(n int) {
		for range 4 {
			at = at.Add(time.Second)
			samples.observe(at, avg, true)
		}
		at = at.Add(time.Second)
		fixed = kernelLoad(fixed, n)
		avg = visibleLoad(fixed)
		samples.observe(at, avg, true)
	}
	for range 6 { // 30 seconds at background 20 plus this stage's 20.
		step(40)
	}
	stageEnded := at
	for range 2 { // Two fresh background samples arrive within 10 seconds.
		step(20)
	}
	if at.Sub(stageEnded) > 10500*time.Millisecond || avg <= 23 {
		t.Fatalf("after-stage observation took %s with load average %.2f; want ≤10.5s and tail >23", at.Sub(stageEnded), avg)
	}
	a, b, ok := samples.lastTwo()
	if !ok || a > 23 || b > 23 {
		t.Fatalf("post-stage kernel samples %.1f, %.1f, %v; want both below 23", a, b, ok)
	}
	setLoadavg(t, avg)
	m := newMachine(testLoad, io.Discard, "sample-check", "projectA:sample-check")
	m.runnable = 1
	m.runnableLimit = 23
	m.avg1, m.avg1Known = avg, true
	m.samples = samples
	m.now = func() time.Time { return at }
	priority, err := m.busy(context.Background(), true, true)
	if err != nil || priority.why != "" {
		t.Fatalf("priority task with post-stage samples %.1f, %.1f: %+v, %v; want admitted", a, b, priority, err)
	}
	ordinary, err := m.busy(context.Background(), false, false)
	if err != nil || ordinary.why != WhyLoad {
		t.Fatalf("ordinary task at avg1 %.2f: %+v, %v; want load wait", avg, ordinary, err)
	}
}

func TestPriorityForeignLoadWaits(t *testing.T) {
	fakeMachine(t)
	setRunnableLimit(t, 23)
	setLoadavg(t, 29)
	m := newMachine(testLoad, io.Discard, "sample-check", "projectA:sample-check")
	m.runnable, m.runnableLimit = 1, 23
	m.avg1, m.avg1Known = 29, true
	m.samples.values = []float64{30, 30}
	m.samples.at, m.samples.avg, m.samples.have = time.Now(), 29, true
	m.samples.stableAt = m.samples.at
	b, err := m.busy(context.Background(), true, true)
	if err != nil || b.why != WhyLoad || !strings.Contains(b.text, "kernel load samples 30.0, 30.0 reach 23") {
		t.Fatalf("foreign load verdict = %+v, %v; want sample reason", b, err)
	}
	p := &Priority{For: "projectA deploy", Budget: filepath.Join(t.TempDir(), "budget"), Fail: true}
	if err := os.WriteFile(p.Budget, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = m.admitWithReserves(context.Background(), p, filepath.Join(t.TempDir(), "load.lock"))
	if err == nil || !strings.Contains(err.Error(), "kernel load samples 30.0, 30.0 reach 23") {
		t.Fatalf("priority failure = %v; want sample reason", err)
	}
	if left, readErr := os.ReadFile(p.Budget); readErr != nil || string(left) != "0" {
		t.Fatalf("remaining wait budget = %q, %v; want spent budget 0", left, readErr)
	}
}

func TestPriorityWithoutSamplesUsesAvg1(t *testing.T) {
	fakeMachine(t)
	setRunnableLimit(t, 23)
	setLoadavg(t, 22)
	m := newMachine(testLoad, io.Discard, "sample-check", "projectA:sample-check")
	m.runnable, m.runnableLimit = 1, 23
	m.avg1, m.avg1Known = 22, true
	b, err := m.busy(context.Background(), true, true)
	if err != nil || b.why != "" {
		t.Fatalf("avg1 22 without samples: %+v, %v; want admitted", b, err)
	}
	setLoadavg(t, 25)
	b, err = m.busy(context.Background(), false, true)
	if err != nil || b.why != WhyLoad {
		t.Fatalf("avg1 25 before two samples: %+v, %v; want wait on avg1", b, err)
	}
	at := time.Unix(100, 0)
	s := kernelSamples{}
	for i := 0; i <= 12; i++ {
		s.observe(at.Add(time.Duration(i)*time.Second), 25, true)
	}
	if a, b, ok := s.lastTwo(); !ok || a != 25 || b != 25 {
		t.Fatalf("stable avg1 samples = %.1f, %.1f, %v; want 25, 25 after frequent polls", a, b, ok)
	}
	setLoadavg(t, 25)
	withSamples := newMachine(testLoad, io.Discard, "stable-check", "projectA:stable-check")
	withSamples.runnable, withSamples.runnableLimit = 1, 23
	withSamples.avg1, withSamples.avg1Known = 25, true
	withSamples.samples.values = []float64{12, 12}
	withSamples.samples.at, withSamples.samples.avg = time.Now(), 25
	withSamples.samples.have = true
	withSamples.samples.stableAt = withSamples.samples.at
	if b, err := withSamples.busy(context.Background(), true, true); err != nil || b.why != "" {
		t.Fatalf("priority decision with samples 12, 12 and avg1 25: %+v, %v; want admitted", b, err)
	}
}

func TestPriorityRunnableStillBlocks(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(23)
	setRunnableLimit(t, 23)
	setLoadavg(t, 10)
	m := newMachine(testLoad, io.Discard, "sample-check", "projectA:sample-check")
	m.samples.values = []float64{10, 10}
	m.samples.at, m.samples.avg, m.samples.have = time.Now(), 10, true
	b, err := m.busy(context.Background(), true, true)
	if err != nil || b.why != WhyLoad || !strings.Contains(b.text, "runnable") {
		t.Fatalf("runnable threshold verdict = %+v, %v; want immediate runnable wait", b, err)
	}
	setRunnable(1)
	setLoadavg(t, 25)
	withSamples := newMachine(testLoad, io.Discard, "sample-check", "projectA:sample-check")
	withSamples.runnableLimit = 23
	withSamples.avg1, withSamples.avg1Known = 25, true
	withSamples.samples.values = []float64{10, 10}
	withSamples.samples.at, withSamples.samples.avg = time.Now(), 25
	withSamples.samples.have = true
	withSamples.samples.stableAt = withSamples.samples.at
	if b, err := withSamples.busy(context.Background(), true, true); err != nil || b.why != "" {
		t.Fatalf("samples 10, 10 with avg1 25 after runnable clears: %+v, %v; want admitted", b, err)
	}
}

func TestPriorityReservesUseSamples(t *testing.T) {
	fakeMachine(t)
	setRunnableLimit(t, 23)
	m := newMachine(testLoad, io.Discard, "sample-check", "projectA:sample-check")
	m.runnable, m.runnableLimit = 1, 23
	m.avg1, m.avg1Known = 25, true
	m.samples.values = []float64{12, 12}
	lockPath := filepath.Join(t.TempDir(), "load.lock")
	release := admitOne(t, lockPath, 5)
	defer release()
	b, err := m.reservesBusy(lockPath, true)
	if err != nil || b.why != "" {
		t.Fatalf("samples 12, 12 plus reserve 5: %+v, %v; want admitted despite avg1 25", b, err)
	}
	b, err = m.reservesBusy(lockPath, false)
	if err != nil || b.why != WhyReserve {
		t.Fatalf("ordinary avg1 25 plus reserve 5: %+v, %v; want reserve wait", b, err)
	}
}

// TestPrioritySamplesCountAStageOnce: a stage of the deploy admitted before
// both kernel samples is in them, and the priority task adds nothing for it
// however recent its admission; one admitted after a sample is missing from
// it and counts by the samples that miss it. A task without a priority keeps
// the load average's share.
func TestPrioritySamplesCountAStageOnce(t *testing.T) {
	fakeMachine(t)
	setRunnableLimit(t, 23)
	lockPath := filepath.Join(t.TempDir(), "load.lock")
	release := admitOne(t, lockPath, 8)
	defer release()
	admitted := time.Now()
	check := func(first, second time.Duration, sampled, background float64, priority bool, want string) {
		t.Helper()
		later := admitted.Add(10 * time.Second)
		m := newMachine(testLoad, io.Discard, "x", "projectA:x")
		m.runnable, m.runnableLimit = 1, 23
		m.now = func() time.Time { return later }
		// The background with the stage's 8 shown 10 s in.
		m.avg1, m.avg1Known = background+8*(1-math.Exp(-10.0/60)), true
		m.samples.append(sampled, admitted.Add(first))
		m.samples.append(sampled, admitted.Add(second))
		b, err := m.reservesBusy(lockPath, priority)
		if err != nil || b.why != want {
			t.Errorf("samples at %s and %s after admission, priority %v: %+v, %v; want %q", first, second, priority, b, err, want)
		}
	}
	check(5*time.Second, 10*time.Second, 20, 12, true, "")
	check(-time.Second, 5*time.Second, 20, 12, true, WhyReserve)  // 20 + 8/2 reaches 23
	check(-time.Second, 5*time.Second, 16, 12, true, "")          // 16 + 8/2 stays under
	check(-2*time.Second, -time.Second, 16, 12, true, WhyReserve) // 16 + 8 reaches 23
	// avg1 15.2 plus the 6.8 it does not show yet stays under 23, though
	// both samples predate the stage.
	check(-2*time.Second, -time.Second, 20, 14, false, "")
}

func setRunnableLimit(t *testing.T, n int) {
	t.Helper()
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "akit", "pressure.yaml")
	if err := os.WriteFile(path, fmt.Appendf(nil, "runnable: %d\nmax_io_full: 10\nmax_swap_kbs: 1024\nswap_window: 20ms\n", n), 0o644); err != nil {
		t.Fatal(err)
	}
}

type waitLogWriter struct{ observed chan struct{} }

func (w waitLogWriter) Write(p []byte) (int, error) {
	select {
	case w.observed <- struct{}{}:
	default:
	}
	return len(p), nil
}

var testLoad = userconfig.MachineLoad{Settle: time.Hour, Poll: 10 * time.Millisecond}

// TestCalmMachineStartsBackToBack checks that a light task does not hold the
// FIFO lock for the reservation cap.
func TestCalmMachineStartsBackToBack(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(21)
	setProfileHistory(t, "projectA:build", 1)
	setProfileHistory(t, "projectA:test", 1)
	lockPath := filepath.Join(t.TempDir(), "load.lock")
	var log strings.Builder
	waited, release, err := wait(context.Background(), "build", nil, testLoad, lockPath, &log)
	if err != nil || waited.Total() > 100*time.Millisecond || log.Len() != 0 {
		t.Fatalf("waited %s, %v:\n%s", waited, err, log.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	blocked, _, err := wait(ctx, "test", nil, testLoad, lockPath, &log)
	if err == nil || blocked[WhyReserve] == 0 {
		t.Fatalf("second task wait = %v, %v; want it held by first reserve", blocked, err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	_, release2, err := wait(context.Background(), "test", nil, testLoad, lockPath, &log)
	if err != nil {
		t.Fatalf("second task did not start after the first reserve was removed: %v", err)
	}
	if err := release2(); err != nil {
		t.Fatal(err)
	}
}

func TestWaitWithSettleReleasesSettleLockOnHistoryError(t *testing.T) {
	fakeMachine(t)
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(cache, "graft", "load.cores.json")
	if err := os.MkdirAll(filepath.Dir(history), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(history, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	lockPath, settlePath := filepath.Join(dir, "load.lock"), filepath.Join(dir, "load.settle.lock")
	_, _, err = WaitWithSettle(context.Background(), "head", "projectA:bad", nil, testLoad, lockPath, settlePath, time.Hour, 0, io.Discard)
	if err == nil || !strings.Contains(err.Error(), history) {
		t.Fatalf("WaitWithSettle error = %v; want corrupt history path %s", err, history)
	}
	next, err := lock.Acquire(context.Background(), settlePath, time.Second, "next settle", io.Discard)
	if err != nil {
		t.Fatalf("settle lock remained held after admission error: %v", err)
	}
	if err := next.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestWaitNamesWhoHeldTheQueue(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	path := filepath.Join(t.TempDir(), "load.lock")
	l := testLoad
	l.Settle = time.Hour
	setRunnable(22)
	type result struct {
		adm Admission
		rel func() error
		err error
	}
	got := make(chan result, 2)
	go func() {
		adm, rel, err := Wait(context.Background(), "head", "projectA:head", nil, l, path, io.Discard)
		got <- result{adm, rel, err}
	}()
	awaitFiles(t, path+".*.lholder", 1)
	observed := make(chan struct{}, 1)
	go func() {
		adm, rel, err := Wait(context.Background(), "follower", "projectA:follower", nil, l, path, waitLogWriter{observed: observed})
		got <- result{adm, rel, err}
	}()
	awaitFiles(t, path+".queue.*.lticket", 1)
	// A ticket can exist before the follower observes the queue head. Wait for
	// that observation before admitting the head, or this test races the first poll.
	select {
	case <-observed:
	case <-time.After(2 * time.Second):
		t.Fatal("follower did not observe the queue head")
	}
	setRunnable(1)
	head, follower := <-got, <-got
	if head.err != nil || follower.err != nil {
		t.Fatalf("head/follower errors: %v / %v", head.err, follower.err)
	}
	if !strings.Contains(follower.adm.WaitedBehind, "graft head") {
		t.Fatalf("WaitedBehind = %q", follower.adm.WaitedBehind)
	}
	if err := head.rel(); err != nil {
		t.Fatal(err)
	}
	if err := follower.rel(); err != nil {
		t.Fatal(err)
	}
}

func TestWaitWithSettleNamesWhoHeldTheSettleQueue(t *testing.T) {
	fakeMachine(t)
	dir := t.TempDir()
	path, settlePath := filepath.Join(dir, "load.lock"), filepath.Join(dir, "load.settle.lock")
	_, release, err := WaitWithSettle(context.Background(), "head", "projectA:head", nil, testLoad, path, settlePath, time.Hour, 0, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	awaitFiles(t, settlePath+".*.lholder", 1)
	type result struct {
		adm Admission
		err error
	}
	got := make(chan result, 1)
	// The follower names who it waits behind once its try of the settle
	// lock fails; head is released only after the follower says it waits.
	waiting := &signalWriter{text: "waiting for the write lock", seen: make(chan struct{})}
	go func() {
		adm, rel, err := WaitWithSettle(context.Background(), "follower", "projectA:follower", nil, testLoad, path, settlePath, time.Hour, 0, waiting)
		if err == nil {
			err = rel()
		}
		got <- result{adm, err}
	}()
	select {
	case <-waiting.seen:
	case <-time.After(3 * time.Second):
		t.Fatal("the follower did not wait for the settle lock")
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-got:
		if res.err != nil {
			t.Fatalf("follower: %v", res.err)
		}
		adm := res.adm
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

// TestBusyDiskAndSwapHoldTasks: IO pressure, write-out or swapping above its limit, or
// the quiet mode, holds a task as the load does, and it says why.
func TestBusyDiskAndSwapHoldTasks(t *testing.T) {
	for _, c := range []struct {
		name, want string
		busy       func(t *testing.T, setIO func(string), setSwap func(int64))
	}{
		{"io", "IO pressure 35.0% of the last 5s is at or above 10% (pressure.yaml max_io_full)",
			func(_ *testing.T, setIO func(string), _ func(int64)) { setIO("35.00") }},
		{"writeback", "dirty and writeback memory 200 MiB is at or above 64 MiB (pressure.yaml max_dirty_mb, else vm.dirty_background_bytes or _ratio)",
			func(t *testing.T, _ func(string), _ func(int64)) {
				if err := os.WriteFile(filepath.Join(os.Getenv(ProcEnv), "meminfo"), []byte("Dirty:            153600 kB\nWriteback:         51200 kB\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}},
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
				l.Poll = 100 * time.Millisecond
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

func TestBudgetSpendsWallTimeOfParallelWaits(t *testing.T) {
	p := &Priority{For: "projectA deploy", Budget: newBudget(t, 3*time.Minute)}
	base := time.Unix(1_800_000_000, 0)
	intervals := []struct {
		from, to time.Time
	}{
		{base, base.Add(2 * time.Second)},
		{base.Add(2 * time.Second), base.Add(4 * time.Second)}, // B then A at this tick.
		{base.Add(2 * time.Second), base.Add(4 * time.Second)},
		{base.Add(4 * time.Second), base.Add(6 * time.Second)}, // A then B.
		{base.Add(4 * time.Second), base.Add(6 * time.Second)},
		{base.Add(6 * time.Second), base.Add(8 * time.Second)}, // B then A.
		{base.Add(6 * time.Second), base.Add(8 * time.Second)},
		{base.Add(8 * time.Second), base.Add(10 * time.Second)},
	}
	for i, interval := range intervals {
		if _, err := p.spend(interval.from, interval.to); err != nil {
			t.Fatalf("spend interval %d [%s, %s]: %v", i, interval.from, interval.to, err)
		}
	}
	left, err := p.spend(time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	spent := 3*time.Minute - left
	if spent < 8*time.Second || spent > 12*time.Second {
		t.Fatalf("parallel intervals spent %s; want 10s ± one 2s poll", spent)
	}
}

func TestBudgetSequentialWaitsAddUp(t *testing.T) {
	p := &Priority{For: "projectA deploy", Budget: newBudget(t, time.Minute)}
	base := time.Unix(1_800_000_000, 0)
	for _, interval := range [][2]time.Time{
		{base, base.Add(5 * time.Second)},
		{base.Add(20 * time.Second), base.Add(25 * time.Second)},
	} {
		if _, err := p.spend(interval[0], interval[1]); err != nil {
			t.Fatal(err)
		}
	}
	left, err := p.spend(time.Time{}, time.Time{})
	if err != nil || left != time.Minute-10*time.Second {
		t.Fatalf("remaining budget = %s, %v; want 50s", left, err)
	}
}

func TestBudgetClockBackwards(t *testing.T) {
	p := &Priority{For: "projectA deploy", Budget: newBudget(t, time.Minute)}
	base := time.Unix(1_800_000_000, 0)
	from, to := base.Add(10*time.Second), base.Add(20*time.Second)
	if err := os.WriteFile(p.Budget+".until", []byte(strconv.FormatInt(to.Add(2*time.Minute).UnixNano(), 10)), 0o644); err != nil {
		t.Fatal(err)
	}
	left, err := p.spend(from, to)
	if err != nil || left != time.Minute-10*time.Second {
		t.Fatalf("remaining budget after clock reversal = %s, %v; want 50s", left, err)
	}
	data, err := os.ReadFile(p.Budget + ".until")
	if err != nil || strings.TrimSpace(string(data)) != strconv.FormatInt(to.UnixNano(), 10) {
		t.Fatalf("watermark after clock reversal = %q, %v; want to=%d", data, err, to.UnixNano())
	}
}

func TestBudgetParallelExhaustion(t *testing.T) {
	p := &Priority{For: "projectA deploy", Budget: newBudget(t, 3*time.Minute)}
	base := time.Unix(1_800_000_000, 0)
	for _, interval := range [][2]time.Time{
		{base, base.Add(2 * time.Minute)},
		{base, base.Add(2 * time.Minute)},
	} {
		left, err := p.spend(interval[0], interval[1])
		if err != nil || left <= 0 {
			t.Fatalf("parallel spend [%s, %s] left %s, %v; neither task should exhaust a 3m budget", interval[0], interval[1], left, err)
		}
	}
	left, err := p.spend(time.Time{}, time.Time{})
	if err != nil || left != time.Minute {
		t.Fatalf("remaining budget = %s, %v; want 1m", left, err)
	}
}

// TestBudgetConcurrentSpendsKeepEveryWait: tasks of one deploy spending the
// budget at the same moment do not lose each other's waits. Every task
// waited from the same start to its own end, so whatever order they come in
// the budget loses exactly the longest wait; a spend that read the budget
// before another wrote it would leave more.
func TestBudgetConcurrentSpendsKeepEveryWait(t *testing.T) {
	const tasks = 32
	base := time.Unix(1_800_000_000, 0)
	for round := range 10 {
		p := &Priority{For: "projectA deploy", Budget: newBudget(t, time.Hour)}
		start := make(chan struct{})
		errs := make(chan error, tasks)
		var wg sync.WaitGroup
		for i := 1; i <= tasks; i++ {
			wg.Go(func() {
				<-start
				_, err := p.spend(base, base.Add(time.Duration(i)*time.Second))
				errs <- err
			})
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		left, err := p.spend(time.Time{}, time.Time{})
		if err != nil || time.Hour-left != tasks*time.Second {
			t.Fatalf("round %d: spent %s, %v; want %ds, the longest wait", round, time.Hour-left, err, tasks)
		}
	}
}

func TestBudgetInvalidWatermarkIsError(t *testing.T) {
	p := &Priority{For: "projectA deploy", Budget: newBudget(t, time.Minute)}
	if err := os.WriteFile(p.Budget+".until", []byte("invalid"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1_800_000_000, 0)
	if _, err := p.spend(base, base.Add(time.Second)); err == nil || !strings.Contains(err.Error(), ".until") {
		t.Fatalf("spend with malformed watermark error = %v; want an explicit .until error", err)
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
	if left, err := p.spend(time.Time{}, time.Time{}); err != nil || left != 100*time.Millisecond {
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

// TestLoadAverageHoldsACalmMoment: a 1-minute load average at or above
// runnable makes the machine busy even when few tasks are ready to run this
// moment.
func TestLoadAverageHoldsACalmMoment(t *testing.T) {
	fakeMachine(t)
	if err := os.WriteFile(filepath.Join(os.Getenv(ProcEnv), "loadavg"), []byte("30.00 10.00 5.00 1/100 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	busy, err := BusyNow(context.Background(), "build", "projectA:build", testLoad, "", io.Discard)
	if err != nil || !busy {
		t.Fatalf("BusyNow = %v, %v with load average 30 over runnable 22; want busy", busy, err)
	}
}

// lateTimer is a context whose deadline has passed while its own timer has
// not set its error yet, as when a dial cut by that deadline returns first.
type lateTimer struct{ context.Context }

func (lateTimer) Deadline() (time.Time, bool) { return time.Now().Add(-time.Millisecond), true }
func (lateTimer) Err() error                  { return nil }

// TestLookPastTheDeadlineIsNoMeasure: a look that ended after the wait's
// deadline, its disk source cut by it, does not find the machine calm: the
// wait ends instead of starting the task.
func TestLookPastTheDeadlineIsNoMeasure(t *testing.T) {
	fakeMachine(t)
	m := newMachine(testLoad, io.Discard, "build", "projectA:build")
	if _, err := m.busy(lateTimer{context.Background()}, true, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy past the deadline: %v, want %v", err, context.DeadlineExceeded)
	}
	if _, err := m.busy(context.Background(), true, false); err != nil {
		t.Fatalf("busy without a deadline: %v", err)
	}
}
