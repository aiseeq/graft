package lock

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAcquireWaitsForHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graft.lock")
	first, err := Acquire(context.Background(), path, time.Second, "first", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}

	var log bytes.Buffer
	_, err = Acquire(context.Background(), path, 300*time.Millisecond, "second", &log)
	if err == nil {
		t.Fatal("second acquire must time out while the first holds the lock")
	}
	if !strings.Contains(err.Error(), "first") || !strings.Contains(log.String(), "held by first") {
		t.Errorf("holder not named: err=%v log=%q", err, log.String())
	}

	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = first.Release()
	}()
	second, err := Acquire(context.Background(), path, 5*time.Second, "second", &log)
	if err != nil {
		t.Fatalf("second acquire after release: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestReserveConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	const n = 16
	releases := make([]func() error, n)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rel, err := Reserve(dir, "load.lock.*.reserve", fmt.Appendf(nil, `{"cores":%d}`, i))
			if err == nil {
				releases[i] = rel
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := LiveData(dir, "load.lock.*.reserve")
	if err != nil || len(data) != n {
		t.Fatalf("LiveData = %d, %v; want %d", len(data), err, n)
	}
	for _, rel := range releases {
		if err := rel(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReserveCorruptIsAnError(t *testing.T) {
	dir := t.TempDir()
	release, err := Reserve(dir, "load.lock.*.reserve", []byte("{broken"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	files, err := filepath.Glob(filepath.Join(dir, "load.lock.*.reserve"))
	if err != nil || len(files) != 1 {
		t.Fatalf("reserve files = %v, %v; want one", files, err)
	}
	_, err = LiveData(dir, "load.lock.*.reserve")
	if err == nil || !strings.Contains(err.Error(), files[0]) {
		t.Fatalf("LiveData error = %v; want exact path %s", err, files[0])
	}
}

func TestReserveDeadOwnerIsNotCounted(t *testing.T) {
	if dir := os.Getenv("GRAFT_TEST_RESERVE_OWNER"); dir != "" {
		_, err := Reserve(dir, "load.lock.*.reserve", []byte(`{"cores":3}`))
		if err != nil {
			t.Fatal(err)
		}
		return
	}

	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestReserveDeadOwnerIsNotCounted$")
	cmd.Env = append(os.Environ(), "GRAFT_TEST_RESERVE_OWNER="+dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("reserve owner subprocess: %v\n%s", err, output)
	}
	files, err := filepath.Glob(filepath.Join(dir, "load.lock.*.reserve"))
	if err != nil || len(files) != 1 {
		t.Fatalf("reserve files = %v, %v; want one file from dead owner", files, err)
	}
	data, err := LiveData(dir, "load.lock.*.reserve")
	if err != nil || len(data) != 0 {
		t.Fatalf("LiveData = %v, %v; want no dead reservation", data, err)
	}
	if _, err := os.Stat(files[0]); !os.IsNotExist(err) {
		t.Fatalf("dead reserve remains: %v", err)
	}
}

func TestAcquireObservedCountsElapsedQuietTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observed.lock")
	holder, err := Acquire(context.Background(), path, time.Second, "holder", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan fifoResult, 1)
	observed := make(chan struct{}, 4)
	go func() {
		lk, err := AcquireObserved(context.Background(), path, Exclusive, 3*time.Second, "waiter", &bytes.Buffer{}, func() (bool, error) {
			select {
			case observed <- struct{}{}:
			default:
			}
			return true, nil
		})
		result <- fifoResult{lock: lk, err: err}
	}()
	for range 3 {
		select {
		case <-observed:
		case <-time.After(2 * time.Second):
			t.Fatal("lock waiter did not retry three times")
		}
	}
	if err := holder.Release(); err != nil {
		t.Fatal(err)
	}
	got := receiveLock(t, result)
	if got.err != nil {
		t.Fatal(got.err)
	}
	defer got.lock.Release()
	if got.lock.WaitedQuiet < 300*time.Millisecond || got.lock.WaitedQuiet > got.lock.Waited {
		t.Errorf("quiet wait = %s, total lock wait = %s", got.lock.WaitedQuiet, got.lock.Waited)
	}
}
func TestAcquireFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "load.lock")
	holder, err := Acquire(context.Background(), path, time.Second, "occupant", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan fifoResult, 2)
	var firstLog, secondLog safeBuffer
	start := func(name string, log *safeBuffer) {
		go func() {
			lk, err := AcquireFIFOObserved(context.Background(), path, 5*time.Second, name, log, nil)
			results <- fifoResult{lk, err}
		}()
	}
	start("early", &firstLog)
	waitForTickets(t, path, 1)
	start("late", &secondLog)
	waitForTickets(t, path, 2)
	if err := holder.Release(); err != nil {
		t.Fatal(err)
	}
	first := receiveLock(t, results)
	if first.err != nil {
		t.Fatal(first.err)
	}
	select {
	case second := <-results:
		if second.lock != nil {
			_ = second.lock.Release()
		}
		t.Fatalf("late ticket acquired before early ticket released: %v", second.err)
	case <-time.After(250 * time.Millisecond):
	}
	if !strings.Contains(secondLog.String(), "behind early") {
		t.Errorf("wait message does not name the earlier ticket: %q", secondLog.String())
	}
	if err := first.lock.Release(); err != nil {
		t.Fatal(err)
	}
	second := receiveLock(t, results)
	if second.err != nil {
		t.Fatal(second.err)
	}
	if err := second.lock.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestAcquireFIFODropsDeadTicket: a ticket no process holds goes, one its
// owner holds stays whatever pid it was written with: a graft in a sandbox
// of its own PID namespace sees no host pid alive.
func TestAcquireFIFODropsDeadTicket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "load.lock")
	stale := fmt.Sprintf("%s.queue.%020d.x.lticket", path, 1)
	if err := os.WriteFile(path+".queue.next", []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("ghost"), 0o644); err != nil {
		t.Fatal(err)
	}
	owner, held, err := createLive(filepath.Dir(path), filepath.Base(path)+fmt.Sprintf(".queue.%020d.*.lticket", 2), []byte("pid 2 of another namespace"))
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan fifoResult, 1)
	var log safeBuffer
	go func() {
		lk, err := AcquireFIFOObserved(context.Background(), path, 5*time.Second, "live", &log, nil)
		results <- fifoResult{lock: lk, err: err}
	}()
	for deadline := time.Now().Add(3 * time.Second); !strings.Contains(log.String(), "behind") && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("dead ticket kept")
	}
	if _, err := os.Stat(held); err != nil {
		t.Errorf("held ticket removed: %v", err)
	}
	if !strings.Contains(log.String(), "behind pid 2 of another namespace") {
		t.Errorf("not queued behind the held ticket: %q", log.String())
	}
	if err := releaseLive(owner, held); err != nil {
		t.Fatal(err)
	}
	got := receiveLock(t, results)
	if got.err != nil {
		t.Fatal(got.err)
	}
	if err := got.lock.Release(); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(path + ".queue.*.lticket"); len(left) != 0 {
		t.Errorf("tickets left: %v", left)
	}
}

func waitForTickets(t *testing.T, path string, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		matches, err := filepath.Glob(path + ".queue.*.lticket")
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("did not see %d queue tickets", want)
}

type fifoResult struct {
	lock *Lock
	err  error
}

type safeBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func receiveLock(t *testing.T, results <-chan fifoResult) fifoResult {
	t.Helper()
	select {
	case got := <-results:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for queued lock")
		return fifoResult{}
	}
}

func TestSharedAndExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locks", "app.lock")
	ctx := context.Background()
	var log bytes.Buffer
	r1, err := AcquireMode(ctx, path, Shared, time.Second, "test one", &log)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := AcquireMode(ctx, path, Shared, 300*time.Millisecond, "test two", &log)
	if err != nil {
		t.Fatalf("readers must share: %v", err)
	}
	holders, err := Holders(path)
	if err != nil || len(holders) != 2 {
		t.Errorf("holders = %v, %v", holders, err)
	}
	if _, err := AcquireMode(ctx, path, Exclusive, 300*time.Millisecond, "restart", &log); err == nil {
		t.Fatal("writer must wait for readers")
	}
	if err := r1.Release(); err != nil {
		t.Fatal(err)
	}
	if err := r2.Release(); err != nil {
		t.Fatal(err)
	}
	w, err := AcquireMode(ctx, path, Exclusive, time.Second, "restart", &log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireMode(ctx, path, Shared, 300*time.Millisecond, "test", &log); err == nil {
		t.Fatal("reader must wait for the writer")
	}
	if err := w.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestStaleNotesAreDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.lock")
	// No process holds it: its owner died.
	stale := path + ".x.lholder"
	if err := os.WriteFile(stale, []byte("ghost"), 0o644); err != nil {
		t.Fatal(err)
	}
	holders, err := Holders(path)
	if err != nil || len(holders) != 0 {
		t.Errorf("holders = %v, %v", holders, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale note kept")
	}
}

// TestCreateLiveRetriesRemoved: a reader that takes a new live file for dead
// before its owner locks it costs the owner another name, not its lock.
func TestCreateLiveRetriesRemoved(t *testing.T) {
	dir := t.TempDir()
	reaped := 0
	beforeLock = func(name string) {
		if reaped < 2 {
			reaped++
			if dead, err := gone(name); !dead || err != nil {
				t.Errorf("gone(%s) = %v, %v", name, dead, err)
			}
		}
	}
	t.Cleanup(func() { beforeLock = func(string) {} })
	fl, name, err := createLive(dir, "app.lock.*.lholder", []byte("note"))
	if err != nil {
		t.Fatal(err)
	}
	if dead, err := gone(name); dead || err != nil {
		t.Errorf("the live file taken for dead: %v, %v", dead, err)
	}
	if err := releaseLive(fl, name); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Errorf("files left: %v", left)
	}
}
