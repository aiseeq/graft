package loadgate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// setLoadavg sets the 1-minute load average of the machine of fakeMachine.
func setLoadavg(t *testing.T, avg1 float64) {
	t.Helper()
	dir := os.Getenv(ProcEnv)
	tmp := filepath.Join(dir, "loadavg.tmp")
	if err := os.WriteFile(tmp, fmt.Appendf(nil, "%.2f 0.00 0.00 1/100 1\n", avg1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "loadavg")); err != nil {
		t.Fatal(err)
	}
}

// TestQueueDipAdmitsOneAtATime: a queue that stood while the load average was
// over the threshold, when it dips below for a while, lets in what the dip
// has room for, not the whole queue: the load average shows a task it let in
// a minute later, so the next counts what the ones before it will add.
func TestQueueDipAdmitsOneAtATime(t *testing.T) {
	for _, c := range []struct {
		dip  float64
		want int
	}{
		{21, 1},   // Exit hatch: no reserves of its own yet, and 21 < 22 lets one in.
		{18.5, 1}, // 18.5 + 2 unseen + 2 candidate reaches 22.5
	} {
		t.Run(fmt.Sprint(c.dip), func(t *testing.T) {
			if got := dipAdmits(t, c.dip, 2, c.want); got != c.want {
				t.Errorf("a dip of the load average to %v let %d of the queued tasks of 2 cores in, want %d", c.dip, got, c.want)
			}
		})
	}
}

// dipAdmits queues five heavy tasks of the given cores at load average 30 and
// runnable 13, dips the load average and returns how many of them it let in.
func dipAdmits(t *testing.T, dip, cores float64, want int) int {
	t.Helper()
	setRunnable, _, _ := fakeMachine(t)
	oldP, oldR := reservePollInterval, reserveCPUReader
	oldW := reserveWindow
	oldAgeNow := reserveAgeNow
	reservePollInterval, reserveWindow = 5*time.Millisecond, 20*time.Millisecond
	reserveCPUReader = func(int) (float64, error) { return 0, nil }
	frozenAge := time.Now()
	reserveAgeNow = func() time.Time { return frozenAge }
	defer func() {
		reservePollInterval, reserveWindow, reserveCPUReader, reserveAgeNow = oldP, oldW, oldR, oldAgeNow
	}()
	setRunnable(13)
	setLoadavg(t, 30)
	l := testLoad
	l.Settle = time.Hour
	path := filepath.Join(t.TempDir(), "load.lock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logw := &lockedLog{}
	var mu sync.Mutex
	admitted := 0
	var releases []func() error
	var wg sync.WaitGroup
	for i := range 5 {
		wg.Go(func() {
			_, release, err := WaitWithSettle(ctx, fmt.Sprintf("task%d", i), "projectA:agent", nil, l, path, "", 0, cores, logw, "")
			if err != nil {
				return
			}
			mu.Lock()
			admitted++
			releases = append(releases, release)
			mu.Unlock()
		})
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return admitted }
	// The head of the queue waits for the machine.
	if !waitUntil(10*time.Second, func() bool { return strings.Contains(logw.String(), "load average 1m 30.00") }) {
		t.Fatalf("no task waited for the load average 30\n%s", logw.String())
	}
	if n := count(); n != 0 {
		t.Fatalf("%d admitted at load average 30", n)
	}
	setLoadavg(t, dip)
	// Freeze reserve-age time so scheduling delays cannot age the forecast.
	// Keep the original queue and dip open for a fixed second after want;
	// any delayed extra admission is counted before the queue is canceled.
	reached := waitUntil(10*time.Second, func() bool { return count() >= want })
	if !reached {
		t.Errorf("only %d of %d expected tasks entered during the dip\n%s", count(), want, logw.String())
	}
	if reached {
		time.Sleep(time.Second)
	}
	setLoadavg(t, 30)
	cancel()
	wg.Wait()
	got := count()
	for _, r := range releases {
		if err := r(); err != nil {
			t.Error(err)
		}
	}
	if t.Failed() || got == 0 {
		t.Log(logw.String())
	}
	return got
}

// waitUntil polls cond until it holds or the timeout passes and reports
// whether it held.
func waitUntil(timeout time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(timeout); ; time.Sleep(5 * time.Millisecond) {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// admitOne admits a heavy task of the given cores at once and returns its
// release.
func admitOne(t *testing.T, path string, cores float64) func() error {
	t.Helper()
	_, release, err := WaitWithSettle(context.Background(), "first", "projectA:first", nil, testLoad, path, "", 0, cores, io.Discard, "")
	if err != nil {
		t.Fatal(err)
	}
	// These tests model an already measured admission with new-format rate data;
	// the production monitor has not necessarily completed a CPU window yet.
	files, err := filepath.Glob(admittedFilesPattern(path))
	if err != nil || len(files) != 1 {
		t.Fatalf("admitted records = %v, %v; want one", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var d reserveData
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	d.Rate, d.EMA, d.At = cores, 0, time.Now().Format(time.RFC3339Nano)
	d.ObservedCores = cores
	d.ObservedAt = d.Admitted
	data, err = json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files[0], data, 0o600); err != nil {
		t.Fatal(err)
	}
	return release
}

func admittedFilesPattern(path string) string { return path + ".*.admitted" }

// waitBriefly tries to admit a task for 150ms.
func waitBriefly(path string, p *Priority) (Admission, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	adm, release, err := Wait(ctx, "next", "projectA:next", p, testLoad, path, io.Discard, "")
	if err == nil {
		_ = release()
	}
	return adm, err
}

// TestEndedAdmittedTaskLeavesTheForecast: the forecast counts running tasks
// only; one that ended adds nothing to the load average from then on, and the
// next is not held for the minute its share would take to fade.
func TestEndedAdmittedTaskLeavesTheForecast(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setLoadavg(t, 0)
	path := filepath.Join(t.TempDir(), "load.lock")
	release := admitOne(t, path, 8)
	setLoadavg(t, 20)
	if adm, err := waitBriefly(path, nil); err == nil || adm.Waits[WhyReserve] == 0 {
		t.Fatalf("next beside a running task of 8 cores at load average 20: %+v, %v", adm, err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	setLoadavg(t, 17)
	if adm, err := waitBriefly(path, nil); err != nil {
		t.Fatalf("next after the task of 8 cores ended: %+v, %v", adm, err)
	}
}

// TestPriorityCountsTheForecast: a deploy task checks admitted work not yet
// visible in the load average, as an ordinary queued task does.
func TestPriorityCountsTheForecast(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setLoadavg(t, 0)
	path := filepath.Join(t.TempDir(), "load.lock")
	release := admitOne(t, path, 4)
	setLoadavg(t, 21)
	defer release()
	p := &Priority{For: "deploy", Budget: filepath.Join(t.TempDir(), "budget"), Fail: true}
	if err := os.WriteFile(p.Budget, []byte("100000000"), 0o644); err != nil {
		t.Fatal(err)
	}
	if adm, err := waitBriefly(path, p); err == nil || adm.Waits[WhyReserve] == 0 {
		t.Fatalf("deploy task at load average 21 beside a task of 4 cores just admitted: %+v, %v", adm, err)
	}
}

func TestPriorityRefusalNamesReserveForecast(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setLoadavg(t, 21)
	path := filepath.Join(t.TempDir(), "load.lock")
	release := admitOne(t, path, 4)
	defer release()
	p := &Priority{For: "deploy", Budget: newBudget(t, 30*time.Millisecond), Fail: true}
	l := testLoad
	l.Poll = time.Millisecond
	_, _, err := WaitWithSettle(context.Background(), "forecast", "projectA:forecast", p, l, path, "", 0, 0, io.Discard, "")
	forecast := "load average 1m 21.0 plus 4.0 of the admitted tasks"
	if err == nil || !strings.Contains(err.Error(), forecast) || !strings.Contains(err.Error(), "now: load average 1m") {
		t.Fatalf("priority refusal = %v; want forecast %q and a fresh now sample", err, forecast)
	}
	lines := readAdmissionLines(t, filepath.Join(os.Getenv("XDG_CACHE_HOME"), "graft", "admissions.jsonl"))
	last := lines[len(lines)-1]
	if last.Verdict != "refused" || !strings.Contains(last.Why, forecast) || !strings.Contains(last.Why, "now:") {
		t.Fatalf("refused admission = %+v; want its reason to include the forecast and now sample", last)
	}
}

// TestNoLoadAverageNoForecast: without a load average to read the forecast
// has nothing to add to; the runnable count and the reserves still hold.
func TestNoLoadAverageNoForecast(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	if err := os.Remove(filepath.Join(os.Getenv(ProcEnv), "loadavg")); err != nil {
		t.Fatal(err)
	}
	oldR := reserveCPUReader
	reserveCPUReader = func(int) (float64, error) { return 0, nil }
	defer func() { reserveCPUReader = oldR }()
	path := filepath.Join(t.TempDir(), "load.lock")
	release := admitOne(t, path, 8)
	defer release()
	if adm, err := waitBriefly(path, nil); err != nil {
		t.Fatalf("next at runnable 1 plus a reserve of 8, no load average: %+v, %v", adm, err)
	}
}

type lockedLog struct {
	mu sync.Mutex
	b  []byte
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b = append(l.b, []byte(time.Now().Format("05.000")+" ")...)
	l.b = append(l.b, p...)
	return len(p), nil
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.b)
}

// TestBusyNowCountsTheForecast: the check that places a heavy task elsewhere
// when this machine would make it wait counts the admitted tasks the load
// average does not show yet, as the wait does.
func TestBusyNowCountsTheForecast(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setLoadavg(t, 0)
	path := filepath.Join(t.TempDir(), "load.lock")
	release := admitOne(t, path, 4)
	defer release()
	setLoadavg(t, 21)
	for _, queue := range []bool{true, false} {
		busy, err := BusyNowWithReserves(context.Background(), "next", "projectA:next", testLoad, path, queue, io.Discard)
		if err != nil || !busy {
			t.Errorf("queue %v: busy %v, %v at load average 21 beside a task of 4 cores just admitted", queue, busy, err)
		}
	}
}

// TestPrioritySettleCountsTheForecast: a deploy's task with a settling period
// checks the forecast too, and its admission counts for the tasks after it.
func TestPrioritySettleCountsTheForecast(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setLoadavg(t, 0)
	dir := t.TempDir()
	path, settlePath := filepath.Join(dir, "load.lock"), filepath.Join(dir, "load.settle.lock")
	p := &Priority{For: "deploy", Budget: filepath.Join(dir, "budget"), Fail: true}
	if err := os.WriteFile(p.Budget, []byte("100000000"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, release, err := WaitWithSettle(context.Background(), "deploy", "projectA:deploy", p, testLoad, path, settlePath, time.Hour, 8, io.Discard, "")
	if err != nil {
		t.Fatal(err)
	}
	setLoadavg(t, 20)
	if adm, err := waitBriefly(path, nil); err == nil || adm.Waits[WhyReserve] == 0 {
		t.Errorf("next beside a deploy task of 8 cores with settle at load average 20: %+v, %v", adm, err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	setLoadavg(t, 0)
	release = admitOne(t, path, 4)
	defer release()
	setLoadavg(t, 21)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	adm, rel, err := WaitWithSettle(ctx, "deploy", "projectA:deploy", p, testLoad, path, settlePath, time.Hour, 1, io.Discard, "")
	if err == nil {
		_ = rel()
	}
	if err == nil || adm.Waits[WhyReserve] == 0 {
		t.Errorf("deploy task with settle at load average 21 beside a task of 4 cores just admitted: %+v, %v", adm, err)
	}
}

// TestNoStatAdmits: without /proc/stat (Windows, macOS) the machine's threads
// are not known, nor, unless pressure.yaml sets runnable, a runnable limit:
// the reserves and the forecast have nothing to reach, and the queue still
// admits.
func TestNoStatAdmits(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	if err := os.Remove(filepath.Join(os.Getenv(ProcEnv), "stat")); err != nil {
		t.Fatal(err)
	}
	limits := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "akit", "pressure.yaml")
	if err := os.WriteFile(limits, []byte("max_io_full: 10\nmax_swap_kbs: 1024\nswap_window: 20ms\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "load.lock")
	for _, name := range []string{"first", "next"} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		adm, release, err := WaitWithSettle(ctx, name, "projectA:"+name, nil, testLoad, path, "", 0, 4, io.Discard, "")
		cancel()
		if err != nil {
			t.Fatalf("%s task of 4 cores, no /proc/stat: %+v, %v", name, adm, err)
		}
		defer release()
	}
}
