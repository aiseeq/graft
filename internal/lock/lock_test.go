package lock

import (
	"bytes"
	"context"
	"fmt"
	"os"
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
			lk, err := AcquireFIFO(context.Background(), path, 5*time.Second, name, log)
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
		lk, err := AcquireFIFO(context.Background(), path, 5*time.Second, "live", &log)
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
