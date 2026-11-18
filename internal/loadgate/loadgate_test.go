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
	"sync/atomic"
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
	rel, err = lock.Reserve(filepath.Dir(path), filepath.Base(path)+".*.reserve", []byte(`{"cores":9,"admitted":"x","task":"first"}`))
	if err != nil {
		t.Fatal(err)
	}
	// 8 runnable + 9 reserved + 4 of the candidate stays below 22.
	_, release, err = Wait(context.Background(), "head", "projectA:head", nil, testLoad, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	_ = release()
	if err := rel(); err != nil {
		t.Fatal(err)
	}
	setRunnable(8)
	// 8 runnable + 13 reserved stays below 22; the candidate's 4 reaches it.
	rel, err = lock.Reserve(filepath.Dir(path), filepath.Base(path)+".*.reserve", []byte(`{"cores":13,"admitted":"x","task":"first"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	ctx, cancel = context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancel()
	adm, _, err = Wait(ctx, "head", "projectA:head", nil, testLoad, path, io.Discard)
	if err == nil || adm.Waits[WhyReserve] == 0 {
		t.Fatalf("admission with candidate cores = %+v, %v; want reserve wait", adm, err)
	}
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
func TestReserveReadErrorIsLoggedOnceAndPollingContinues(t *testing.T) {
	oldPoll, oldWindow, oldReader := reservePollInterval, reserveWindow, reserveCPUReader
	reservePollInterval, reserveWindow = 5*time.Millisecond, 20*time.Millisecond
	var calls atomic.Int32
	reserveCPUReader = func(int) (float64, error) {
		calls.Add(1)
		return 0, fmt.Errorf("synthetic proc read")
	}
	defer func() { reservePollInterval, reserveWindow, reserveCPUReader = oldPoll, oldWindow, oldReader }()
	path := filepath.Join(t.TempDir(), "load.lock")
	var log strings.Builder
	release, _, end, _, err := startReserve(path, "task", 1, 60*time.Millisecond, nil, &log)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for end() == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := end(); got != "cap" {
		t.Fatalf("reserve end %q; want cap after continued read errors", got)
	}
	if calls.Load() < 2 || strings.Count(log.String(), "reading reserved CPU: synthetic proc read") != 1 {
		t.Fatalf("reader calls=%d, log=%q; want retries with one warning", calls.Load(), log.String())
	}
	_ = release()
}

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
	reserveCPUReader = func(int) (float64, error) { return reader(), nil }
	path := filepath.Join(t.TempDir(), "load.lock")
	cap := 60 * time.Millisecond
	if want == "cpu" {
		cap = 300 * time.Millisecond
	}
	if want == "done" {
		cap = time.Second
	}
	release, _, end, _, err := startReserve(path, "task", 1, cap, nil, io.Discard)
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
	setProfileHistory(t, "projectA:huge", 20)
	path := filepath.Join(t.TempDir(), "load.lock")
	adm, release, err := Wait(context.Background(), "huge", "projectA:huge", nil, testLoad, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if adm.ReserveCores != 20 {
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
	setRunnableLimit(t, 25)
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

func TestPriorityCannotPassHolderOfLoadLock(t *testing.T) {
	fakeMachine(t)
	path := filepath.Join(t.TempDir(), "load.lock")
	holder, err := lock.Acquire(context.Background(), path, time.Second, "ordinary waiter", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	p := &Priority{For: "deploy", Budget: newBudget(t, time.Minute)}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, release, err := Wait(ctx, "priority", "projectA:priority", p, testLoad, path, io.Discard); err == nil {
		_ = release()
		t.Fatal("priority passed the ordinary holder of load.lock")
	}
	if files, err := filepath.Glob(admittedFilesPattern(path)); err != nil || len(files) != 0 {
		t.Fatalf("priority wrote admission while queued: %v, %v", files, err)
	}
}

func TestPriorityReservationConstrainsConcurrentPriorityAndOrdinary(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setRunnableLimit(t, 11)
	setLoadavg(t, 7)
	oldAgeNow := reserveAgeNow
	measuredAt := time.Now()
	reserveAgeNow = func() time.Time { return measuredAt }
	defer func() { reserveAgeNow = oldAgeNow }()
	forecastPath := filepath.Join(t.TempDir(), "forecast.lock")
	releaseForecast := admitOne(t, forecastPath, 2)
	forecast := newMachine(testLoad, io.Discard, "forecast", "projectA:forecast")
	forecast.runnable, forecast.runnableLimit = 1, 11
	forecast.avg1, forecast.avg1Known = 7, true
	forecast.candidate, forecast.candidateKnown = 2, true
	if b, err := forecast.reservesBusy(forecastPath); err != nil || b.why != WhyReserve {
		t.Fatalf("LA1 7 + unseen 2 + candidate 2: %+v, %v; want reserve wait at limit 11", b, err)
	}
	if err := releaseForecast(); err != nil {
		t.Fatal(err)
	}
	reserveAgeNow = oldAgeNow

	path := filepath.Join(t.TempDir(), "load.lock")
	p := &Priority{For: "deploy", Budget: newBudget(t, time.Minute)}
	type result struct {
		release func() error
		err     error
	}
	results := make(chan result, 3)
	start := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	for _, tc := range []struct {
		name     string
		priority *Priority
	}{{"priority-one", p}, {"priority-two", p}, {"ordinary", nil}} {
		go func() {
			<-start
			_, release, err := WaitWithSettle(ctx, tc.name, "projectA:"+tc.name, tc.priority, testLoad, path, "", 0, 2, io.Discard)
			results <- result{release: release, err: err}
		}()
	}
	close(start)
	admitted := 0
	for range 3 {
		r := <-results
		if r.err == nil {
			admitted++
			if err := r.release(); err != nil {
				t.Error(err)
			}
		}
	}
	if admitted != 1 {
		t.Fatalf("admitted %d of three 2-core candidates at LA1 7, unseen 1, limit 11; want only the first reservation", admitted)
	}
}

func setProfileHistory(t *testing.T, profile string, values ...float64) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "graft")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string][]float64{profile: values})
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

func TestPriorityWaitUsesLA1InsteadOfKernelSampleMean(t *testing.T) {
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
	if elapsed := now.Sub(start); elapsed < 20*time.Second || m.avg1 >= 23 {
		t.Fatalf("admitted after %s at avg1 %.2f; want to wait until LA1 falls below 23", elapsed, m.avg1)
	}
}

func TestPriorityForeignLoadWaits(t *testing.T) {
	fakeMachine(t)
	setRunnableLimit(t, 23)
	setLoadavg(t, 29)
	m := newMachine(testLoad, io.Discard, "sample-check", "projectA:sample-check")
	m.runnable, m.runnableLimit = 1, 23
	m.avg1, m.avg1Known = 29, true
	b, err := m.busy(context.Background(), true)
	if err != nil || b.why != WhyLoad || !strings.Contains(b.text, "load average 1m 29.00") {
		t.Fatalf("foreign load verdict = %+v, %v; want LA1 reason", b, err)
	}
	p := &Priority{For: "projectA deploy", Budget: filepath.Join(t.TempDir(), "budget"), Fail: true}
	if err := os.WriteFile(p.Budget, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = m.admitWithReserves(context.Background(), p, filepath.Join(t.TempDir(), "load.lock"))
	if err == nil || !strings.Contains(err.Error(), "load average 1m 29.00") {
		t.Fatalf("priority failure = %v; want LA1 reason", err)
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
	b, err := m.busy(context.Background(), true)
	if err != nil || b.why != "" {
		t.Fatalf("avg1 22 without samples: %+v, %v; want admitted", b, err)
	}
	setLoadavg(t, 25)
	b, err = m.busy(context.Background(), false)
	if err != nil || b.why != WhyLoad {
		t.Fatalf("avg1 25 before two samples: %+v, %v; want wait on avg1", b, err)
	}
	setLoadavg(t, 25)
	withSamples := newMachine(testLoad, io.Discard, "stable-check", "projectA:stable-check")
	withSamples.runnable, withSamples.runnableLimit = 1, 23
	if b, err := withSamples.busy(context.Background(), true); err != nil || b.why != WhyLoad {
		t.Fatalf("priority decision with samples 12, 12 and avg1 25: %+v, %v; want LA1 wait", b, err)
	}
}

func TestPriorityRunnableStillBlocks(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(23)
	setRunnableLimit(t, 23)
	setLoadavg(t, 10)
	m := newMachine(testLoad, io.Discard, "sample-check", "projectA:sample-check")
	b, err := m.busy(context.Background(), true)
	if err != nil || b.why != WhyLoad || !strings.Contains(b.text, "runnable") {
		t.Fatalf("runnable threshold verdict = %+v, %v; want immediate runnable wait", b, err)
	}
	setRunnable(1)
	setLoadavg(t, 25)
	withSamples := newMachine(testLoad, io.Discard, "sample-check", "projectA:sample-check")
	withSamples.runnableLimit = 23
	withSamples.avg1, withSamples.avg1Known = 25, true
	if b, err := withSamples.busy(context.Background(), true); err != nil || b.why != WhyLoad {
		t.Fatalf("samples 10, 10 with avg1 25 after runnable clears: %+v, %v; want LA1 wait", b, err)
	}
}

func TestPriorityReservesUseSameLA1AsOrdinary(t *testing.T) {
	fakeMachine(t)
	setRunnableLimit(t, 23)
	m := newMachine(testLoad, io.Discard, "sample-check", "projectA:sample-check")
	m.runnable, m.runnableLimit = 1, 23
	m.avg1, m.avg1Known = 25, true
	lockPath := filepath.Join(t.TempDir(), "load.lock")
	release := admitOne(t, lockPath, 5)
	defer release()
	b, err := m.reservesBusy(lockPath)
	if err != nil || b.why != WhyReserve {
		t.Fatalf("priority avg1 25 plus reserve 5: %+v, %v; want reserve wait", b, err)
	}
	b, err = m.reservesBusy(lockPath)
	if err != nil || b.why != WhyReserve {
		t.Fatalf("ordinary avg1 25 plus reserve 5: %+v, %v; want reserve wait", b, err)
	}
}

func TestPriorityReservesUseSameForecastAsOrdinary(t *testing.T) {
	fakeMachine(t)
	setRunnableLimit(t, 23)
	oldAgeNow := reserveAgeNow
	defer func() { reserveAgeNow = oldAgeNow }()
	lockPath := filepath.Join(t.TempDir(), "load.lock")
	release := admitOne(t, lockPath, 8)
	defer release()
	later := time.Now().Add(10 * time.Second)
	reserveAgeNow = func() time.Time { return later }
	verdict := func() string {
		m := newMachine(testLoad, io.Discard, "x", "projectA:x")
		m.runnable, m.runnableLimit = 1, 23
		m.now = func() time.Time { return later }
		m.avg1, m.avg1Known = 19, true
		b, err := m.reservesBusy(lockPath)
		if err != nil {
			t.Fatal(err)
		}
		return b.why
	}
	if got := verdict(); got != WhyReserve {
		t.Fatalf("reserve verdict %q; want unseen reserve", got)
	}
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
	setRunnable(1)
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

func TestOrdinaryAdmissionWaitsForCPUInHalfMode(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(12) // Above half of 22 threads, below the normal threshold.
	setLoadavg(t, 0)
	dir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "akit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "half-until"), []byte(time.Now().Add(time.Hour).Format(time.RFC3339)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		setRunnable(1)
	}()

	adm, release, err := Wait(context.Background(), "half admission", "projectA:half-admission", nil,
		testLoad, filepath.Join(t.TempDir(), "load.lock"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if adm.Waits[WhyLoad] < 30*time.Millisecond {
		t.Fatalf("load wait = %s; ordinary admission should wait above the half-mode CPU threshold", adm.Waits[WhyLoad])
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

func TestLoadQueueWhyReportsRoom(t *testing.T) {
	fakeMachine(t)
	setQuiet(t, time.Now().Add(-time.Minute))
	why, err := LoadQueueWhy(context.Background(), "locks", "projectA:locks", testLoad,
		filepath.Join(t.TempDir(), "load.lock"), io.Discard)
	if err != nil || why != "" {
		t.Fatalf("load queue reason = %q, %v; want room", why, err)
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

// TestPriorityBudget: exceeding the shared deploy budget refuses a priority
// task, while a task that finds the machine calm spends nothing; a task
// without a priority keeps waiting.
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
	if left, err := p.spend(time.Time{}, time.Time{}); err != nil || left > 100*time.Millisecond || left < 99*time.Millisecond {
		t.Fatalf("budget left after a calm start: %s, %v", left, err)
	}
	setRunnable(30)
	waited, _, err := wait(context.Background(), "build", p, testLoad, lockPath, &log)
	if err == nil || !strings.Contains(err.Error(), "deploy.load_wait: budget exhausted") || waited.Total() < 90*time.Millisecond || waited.Total() > time.Second || waited[WhyLoad] != waited.Total() {
		t.Fatalf("waited %v, %v", waited, err)
	}
	// The next task of the deploy finds the budget spent.
	if _, _, err := wait(context.Background(), "test", p, testLoad, lockPath, &log); err == nil {
		t.Errorf("a task after the budget was spent waited %s, %v", waited, err)
	}
	p = &Priority{For: "graft deploy prod", Budget: newBudget(t, 100*time.Millisecond), Fail: true}
	if _, _, err := wait(context.Background(), "build", p, testLoad, lockPath, &log); err == nil || !strings.Contains(err.Error(), "deploy.load_wait: budget exhausted") {
		t.Errorf("fail when the budget is spent: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, _, err := wait(ctx, "build", nil, testLoad, lockPath, &log); err == nil {
		t.Error("a task without a priority started at load 30")
	}
}

func TestPriorityZeroBudgetDoesNotStartOverLA1(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setRunnableLimit(t, 11)
	setLoadavg(t, 12)
	p := &Priority{For: "graft deploy prod", Budget: newBudget(t, 0)}
	var log strings.Builder
	_, _, err := Wait(context.Background(), "deploy-step", "projectA:deploy-step", p, testLoad, filepath.Join(t.TempDir(), "load.lock"), &log)
	if err == nil || !strings.Contains(err.Error(), "budget exhausted; load average 1m 12.00") || !strings.Contains(err.Error(), "limit 11") || !strings.Contains(err.Error(), "deploy-step not started") {
		t.Fatalf("zero-budget admission error = %v; log: %s", err, log.String())
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
	r, w, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatal(pipeErr)
	}
	oldStderr := os.Stderr
	os.Stderr = w
	got, ok, err = PriorityFromEnv()
	_ = w.Close()
	os.Stderr = oldStderr
	warning, readErr := io.ReadAll(r)
	_ = r.Close()
	if err != nil || !ok || !got.Fail || readErr != nil || !strings.Contains(string(warning), "GRAFT_LOAD_THEN=start no longer starts over the limit") {
		t.Errorf("env start handling = %+v, %v, warning %q, read error %v", got, err, warning, readErr)
	}
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
	if _, err := m.busy(lateTimer{context.Background()}, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy past the deadline: %v, want %v", err, context.DeadlineExceeded)
	}
	if _, err := m.busy(context.Background(), true); err != nil {
		t.Fatalf("busy without a deadline: %v", err)
	}
}

// TestHalfModeQueueDoesNotAdmitABurst: in half mode the admitted tasks the
// load average does not show yet count against half the threads, not all of
// them: with the load average at 8 of a half limit of 11, a queue lets in
// one task, whose expected cores take it to the limit, not one after another up to 22.
func TestHalfModeQueueDoesNotAdmitABurst(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setLoadavg(t, 8)
	setProfileHistory(t, "projectA:burst", 2)
	dir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "akit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "half-until"), []byte(time.Now().Add(time.Hour).Format(time.RFC3339)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "load.lock")
	admitted := 0
	for range 20 {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
		_, release, err := Wait(ctx, "burst", "projectA:burst", nil, testLoad, path, io.Discard)
		cancel()
		if err != nil {
			break
		}
		admitted++
		t.Cleanup(func() { _ = release() })
	}
	if admitted != 1 {
		t.Fatalf("admitted %d tasks back to back at load average 8 in half mode (limit 11); want 1", admitted)
	}
}

func TestMeasuredCPUReplacesLowReservationInUnseenLoad(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setLoadavg(t, 0)
	setRunnableLimit(t, 7)
	setProfileHistory(t, "projectA:measured-eight", .5, .5, .5)
	oldPoll, oldWindow, oldReader := reservePollInterval, reserveWindow, reserveCPUReader
	reservePollInterval, reserveWindow = 5*time.Millisecond, 20*time.Millisecond
	var readAt time.Time
	var cpuSeconds float64
	reserveCPUReader = func(int) (float64, error) {
		at := time.Now()
		if !readAt.IsZero() {
			cpuSeconds += 8 * at.Sub(readAt).Seconds()
		}
		readAt = at
		return cpuSeconds, nil
	}
	defer func() { reservePollInterval, reserveWindow, reserveCPUReader = oldPoll, oldWindow, oldReader }()
	path := filepath.Join(t.TempDir(), "load.lock")
	var log strings.Builder
	adm, release, err := Wait(context.Background(), "measured-eight", "projectA:measured-eight", nil, testLoad, path, &log)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	deadline := time.Now().Add(time.Second)
	var cores float64
	for time.Now().Before(deadline) {
		cores, err = unseenAdmittedCores(path, func(time.Time) float64 { return 1 })
		if err != nil {
			t.Fatal(err)
		}
		if cores >= 7.9 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cores, err = unseenAdmittedCores(path, func(time.Time) float64 { return 1 })
	if err != nil || cores < 7.9 {
		files, _ := lock.LiveData(filepath.Dir(path), admittedPattern(path))
		t.Fatalf("unseen admitted cores = %v, %v; reserve end %q, log %q, records %+v; want measured about 8 (expected %.1f)", cores, err, adm.ReserveEnd(), log.String(), files, adm.ReserveCores)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	nextAdm, _, err := Wait(ctx, "next", "projectA:next", nil, testLoad, path, io.Discard)
	if err == nil || nextAdm.Waits[WhyReserve] == 0 {
		t.Fatalf("next admission = %+v, %v; want it held by measured 8-core task", nextAdm, err)
	}
}

func TestWaitHoldsQueueUntilFirstCPUObservation(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setLoadavg(t, 8)
	setRunnableLimit(t, 11) // half mode on a 22-thread machine
	setProfileHistory(t, "projectA:first-measure", .5, .5, .5)
	oldPoll, oldWindow, oldReader := reservePollInterval, reserveWindow, reserveCPUReader
	reservePollInterval, reserveWindow = 5*time.Millisecond, 80*time.Millisecond
	var readAt time.Time
	var cpuSeconds float64
	reserveCPUReader = func(int) (float64, error) {
		at := time.Now()
		if !readAt.IsZero() {
			cpuSeconds += 8 * at.Sub(readAt).Seconds()
		}
		readAt = at
		return cpuSeconds, nil
	}
	defer func() { reservePollInterval, reserveWindow, reserveCPUReader = oldPoll, oldWindow, oldReader }()
	path := filepath.Join(t.TempDir(), "load.lock")
	_, release, err := Wait(context.Background(), "first-measure", "projectA:first-measure", nil, testLoad, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	// The first admission is live but has not completed its CPU window. Check
	// both the reason and the public Wait path before that window can finish.
	m := newMachine(testLoad, io.Discard, "next", "projectA:next")
	m.runnable, m.avg1, m.avg1Known, m.runnableLimit = 1, 8, true, 11
	b, err := m.reservesBusy(path)
	if err != nil || b.why != WhyReserve || !strings.Contains(b.text, "waiting for the first CPU measure of first-measure") {
		t.Fatalf("before first CPU observation: %+v, %v; want first-measure wait", b, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	firstNext, _, err := Wait(ctx, "next-before-measure", "projectA:next", nil, testLoad, path, io.Discard)
	if err == nil || firstNext.Waits[WhyReserve] == 0 {
		t.Fatalf("next admission before first CPU measure = %+v, %v; want it held", firstNext, err)
	}

	measured := waitObservedCores(t, path, func(d reserveData) bool { return d.ObservedCores >= 7.5 }, time.Second)
	ctxAfterMeasure, cancelAfterMeasure := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancelAfterMeasure()
	secondNext, _, err := Wait(ctxAfterMeasure, "next-after-measure", "projectA:next", nil, testLoad, path, io.Discard)
	if err == nil || secondNext.Waits[WhyReserve] == 0 {
		t.Fatalf("next admission after %.2f measured cores = %+v, %v; want it held", measured.ObservedCores, secondNext, err)
	}
}

func TestWaitRechecksAtFirstMeasureDeadline(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setLoadavg(t, 0)
	setProfileHistory(t, "projectA:first-measure-deadline", .5, .5, .5)
	oldPoll, oldWindow, oldReader := reservePollInterval, reserveWindow, reserveCPUReader
	reservePollInterval, reserveWindow = 250*time.Millisecond, 2*time.Second
	reserveCPUReader = func(int) (float64, error) { return 0, fmt.Errorf("synthetic unavailable CPU reader") }
	defer func() { reservePollInterval, reserveWindow, reserveCPUReader = oldPoll, oldWindow, oldReader }()
	path := filepath.Join(t.TempDir(), "load.lock")
	l := testLoad
	l.Poll, l.Settle = 5*time.Second, time.Hour
	_, release, err := Wait(context.Background(), "first-measure", "projectA:first-measure-deadline", nil, l, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := time.Now()
	adm, releaseNext, err := Wait(ctx, "next", "projectA:next", nil, l, path, io.Discard)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("next admission after waiting %s: %+v, %v", elapsed, adm, err)
	}
	defer releaseNext()
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("next admission waited %s; want recheck by the 2.25s first-measure deadline", elapsed)
	}
	if adm.Waits[WhyReserve] == 0 {
		t.Fatalf("next admission waits = %+v; want time spent awaiting the first measurement", adm.Waits)
	}
}

func TestReserveKeepsPollingAfterCap(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setProfileHistory(t, "projectA:after-cap", .5, .5, .5)
	oldPoll, oldWindow, oldReader := reservePollInterval, reserveWindow, reserveCPUReader
	reservePollInterval, reserveWindow = 5*time.Millisecond, 20*time.Millisecond
	const capDuration = 120 * time.Millisecond
	const highPhaseAfter = capDuration + 20*time.Millisecond
	var started time.Time
	var cpuSeconds float64
	var readAt time.Time
	reserveCPUReader = func(int) (float64, error) {
		at := time.Now()
		if started.IsZero() {
			started = at
		}
		if !readAt.IsZero() {
			rate := 0.5
			if at.Sub(started) >= highPhaseAfter {
				rate = 8
			}
			cpuSeconds += rate * at.Sub(readAt).Seconds()
		}
		readAt = at
		return cpuSeconds, nil
	}
	defer func() { reservePollInterval, reserveWindow, reserveCPUReader = oldPoll, oldWindow, oldReader }()
	path := filepath.Join(t.TempDir(), "load.lock")
	l := testLoad
	l.Settle = capDuration
	_, release, err := Wait(context.Background(), "after-cap", "projectA:after-cap", nil, l, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	measured := waitObservedCores(t, path, func(d reserveData) bool { return d.ObservedCores >= 7.5 }, time.Second)
	if measured.ObservedCores < 7.5 {
		t.Fatalf("CPU after cap = %.2f; want about 8 cores", measured.ObservedCores)
	}
}

func TestZeroCPUQueueWaitEndsAfterFirstWindowGrace(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setRunnableLimit(t, 7)
	setProfileHistory(t, "projectA:idle", .5)
	oldPoll, oldWindow, oldReader := reservePollInterval, reserveWindow, reserveCPUReader
	reservePollInterval, reserveWindow = 5*time.Millisecond, 20*time.Millisecond
	polled := make(chan struct{}, 1)
	reserveCPUReader = func(int) (float64, error) {
		select {
		case polled <- struct{}{}:
		default:
		}
		return 0, nil
	}
	defer func() { reservePollInterval, reserveWindow, reserveCPUReader = oldPoll, oldWindow, oldReader }()
	path := filepath.Join(t.TempDir(), "load.lock")
	_, release, err := Wait(context.Background(), "idle", "projectA:idle", nil, testLoad, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	select {
	case <-polled:
	case <-time.After(time.Second):
		t.Fatal("reserve CPU was not sampled")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	adm, releaseNext, err := Wait(ctx, "next-idle", "projectA:next-idle", nil, testLoad, path, io.Discard)
	if err != nil {
		t.Fatalf("zero-CPU task held the next admission: %+v, %v", adm, err)
	}
	if adm.Waits[WhyReserve] <= 0 || adm.Waits[WhyReserve] > reserveWindow+reservePollInterval+40*time.Millisecond {
		t.Fatalf("reserve wait = %s; want only first-window grace (%s)", adm.Waits[WhyReserve], reserveWindow+reservePollInterval)
	}
	if err := releaseNext(); err != nil {
		t.Fatal(err)
	}
}

func TestWaitWithSettleHoldsNextAdmissionForDuration(t *testing.T) {
	fakeMachine(t)
	oldPoll, oldWindow, oldReader := reservePollInterval, reserveWindow, reserveCPUReader
	reservePollInterval, reserveWindow = 5*time.Millisecond, 20*time.Millisecond
	var readAt time.Time
	var cpuSeconds float64
	reserveCPUReader = func(int) (float64, error) {
		at := time.Now()
		if !readAt.IsZero() {
			cpuSeconds += .5 * at.Sub(readAt).Seconds()
		}
		readAt = at
		return cpuSeconds, nil
	}
	defer func() { reservePollInterval, reserveWindow, reserveCPUReader = oldPoll, oldWindow, oldReader }()
	path, settlePath := filepath.Join(t.TempDir(), "load.lock"), filepath.Join(t.TempDir(), "load.settle.lock")
	duration := 80 * time.Millisecond
	_, release, err := WaitWithSettle(context.Background(), "head", "projectA:head", nil, testLoad, path, settlePath, duration, .5, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, releaseNext, err := WaitWithSettle(ctx, "next", "projectA:next", nil, testLoad, path, settlePath, duration, .5, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseNext()
	if waited := time.Since(start); waited < duration-10*time.Millisecond {
		t.Fatalf("next admission waited %s; want at least %s", waited, duration-10*time.Millisecond)
	}
}

func TestReserveTracksLateCPUIncrease(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setProfileHistory(t, "projectA:late-phase", .5)
	oldPoll, oldWindow, oldReader := reservePollInterval, reserveWindow, reserveCPUReader
	reservePollInterval, reserveWindow = 5*time.Millisecond, 20*time.Millisecond
	var started time.Time
	const firstPhase = 2 * time.Second
	reserveCPUReader = func(int) (float64, error) {
		at := time.Now()
		if started.IsZero() {
			started = at
		}
		elapsed := at.Sub(started).Seconds()
		if elapsed < firstPhase.Seconds() {
			return .5 * elapsed, nil
		}
		return .5*firstPhase.Seconds() + 8*(elapsed-firstPhase.Seconds()), nil
	}
	defer func() { reservePollInterval, reserveWindow, reserveCPUReader = oldPoll, oldWindow, oldReader }()
	path := filepath.Join(t.TempDir(), "load.lock")
	_, release, err := Wait(context.Background(), "late-phase", "projectA:late-phase", nil, testLoad, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	measured := waitObservedCores(t, path, func(d reserveData) bool { return d.ObservedCores >= 7.5 }, 4*time.Second)
	observedAt, err := time.Parse(time.RFC3339Nano, measured.ObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	virtualNow := observedAt.Add(30 * time.Second)
	oldAgeNow := reserveAgeNow
	reserveAgeNow = func() time.Time { return virtualNow }
	defer func() { reserveAgeNow = oldAgeNow }()
	m := newMachine(testLoad, io.Discard, "next", "projectA:next")
	m.now = func() time.Time { return virtualNow }
	m.runnable, m.avg1, m.avg1Known, m.runnableLimit = 1, 3.2, true, 7
	b, err := m.reservesBusy(path)
	if err != nil || b.why != WhyReserve {
		t.Fatalf("after 30s with late measured 8-core phase: %+v, %v; want next admission held", b, err)
	}
}

func TestReserveKeepsObservationAfterReadError(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setProfileHistory(t, "projectA:read-error", .5)
	oldPoll, oldWindow, oldReader := reservePollInterval, reserveWindow, reserveCPUReader
	reservePollInterval, reserveWindow = 5*time.Millisecond, 20*time.Millisecond
	var started time.Time
	var failReads atomic.Bool
	var errorsRead atomic.Int32
	reserveCPUReader = func(int) (float64, error) {
		if failReads.Load() {
			errorsRead.Add(1)
			return 0, fmt.Errorf("synthetic post-drop read")
		}
		at := time.Now()
		if started.IsZero() {
			started = at
		}
		return .5 * at.Sub(started).Seconds(), nil
	}
	defer func() { reservePollInterval, reserveWindow, reserveCPUReader = oldPoll, oldWindow, oldReader }()
	path := filepath.Join(t.TempDir(), "load.lock")
	var log strings.Builder
	_, release, err := Wait(context.Background(), "read-error", "projectA:read-error", nil, testLoad, path, &log)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	first := waitObservedCores(t, path, func(d reserveData) bool { return d.ObservedAt != "" }, time.Second)
	if first.ObservedCores < .45 || first.ObservedCores > .55 {
		t.Fatalf("initial measured CPU = %.3f; want about .5", first.ObservedCores)
	}
	failReads.Store(true) // the task now uses 8 cores, but proc reads fail
	deadline := time.Now().Add(time.Second)
	for errorsRead.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	last, ok := latestObservedCores(t, path)
	if !ok || last.ObservedCores != first.ObservedCores || last.ObservedAt != first.ObservedAt {
		t.Fatalf("observation after read errors = %+v, present=%v; want last successful sample %+v", last, ok, first)
	}
	if errorsRead.Load() < 3 || strings.Count(log.String(), "reading reserved CPU: synthetic post-drop read") != 1 {
		t.Fatalf("read errors=%d, log=%q; want retries and one warning", errorsRead.Load(), log.String())
	}
}

func TestReserveDropsMeasuredPeakAfterLoadFalls(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setProfileHistory(t, "projectA:short-peak", .5)
	oldPoll, oldWindow, oldReader := reservePollInterval, reserveWindow, reserveCPUReader
	reservePollInterval, reserveWindow = 5*time.Millisecond, 20*time.Millisecond
	var started time.Time
	const peakDuration = 2 * time.Second
	reserveCPUReader = func(int) (float64, error) {
		at := time.Now()
		if started.IsZero() {
			started = at
		}
		elapsed := at.Sub(started).Seconds()
		if elapsed < peakDuration.Seconds() {
			return 20 * elapsed, nil
		}
		return 20*peakDuration.Seconds() + .2*(elapsed-peakDuration.Seconds()), nil
	}
	defer func() { reservePollInterval, reserveWindow, reserveCPUReader = oldPoll, oldWindow, oldReader }()
	path := filepath.Join(t.TempDir(), "load.lock")
	_, release, err := Wait(context.Background(), "short-peak", "projectA:short-peak", nil, testLoad, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	measured := waitObservedCores(t, path, func(d reserveData) bool { return d.ObservedCores <= .3 }, 4*time.Second)
	if measured.ObservedCores > .3 {
		t.Fatalf("latest measured CPU = %.3f; want low post-peak rate", measured.ObservedCores)
	}
	observedAt, err := time.Parse(time.RFC3339Nano, measured.ObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	virtualNow := observedAt.Add(31 * time.Second)
	oldAgeNow := reserveAgeNow
	reserveAgeNow = func() time.Time { return virtualNow }
	defer func() { reserveAgeNow = oldAgeNow }()
	m := newMachine(testLoad, io.Discard, "next", "projectA:next")
	m.now = func() time.Time { return virtualNow }
	m.runnable, m.avg1, m.avg1Known, m.runnableLimit = 1, .4, true, 11
	b, err := m.reservesBusy(path)
	if err != nil || b.why != "" {
		t.Fatalf("after 31s with current .2-core rate: %+v, %v; want candidate admitted", b, err)
	}
}

func waitObservedCores(t *testing.T, path string, match func(reserveData) bool, timeout time.Duration) reserveData {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if d, ok := latestObservedCores(t, path); ok && match(d) {
			return d
		}
		time.Sleep(time.Millisecond)
	}
	d, ok := latestObservedCores(t, path)
	t.Fatalf("timed out waiting for observed CPU, latest=%+v present=%v", d, ok)
	return reserveData{}
}

func latestObservedCores(t *testing.T, path string) (reserveData, bool) {
	t.Helper()
	files, err := lock.LiveData(filepath.Dir(path), admittedPattern(path))
	if err != nil {
		t.Fatal(err)
	}
	var latest reserveData
	var latestAt time.Time
	for _, file := range files {
		var d reserveData
		if err := json.Unmarshal(file.Data, &d); err != nil {
			t.Fatal(err)
		}
		if d.ObservedAt == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, d.ObservedAt)
		if err != nil {
			t.Fatalf("parsing observed CPU timestamp %q from %s: %v", d.ObservedAt, file.Path, err)
		}
		if latestAt.IsZero() || at.After(latestAt) {
			latest, latestAt = d, at
		}
	}
	return latest, !latestAt.IsZero()
}
