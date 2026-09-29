package lock

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
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
	if err != nil || len(holders) != 1 {
		// Both readers are this process: one note per pid.
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
	// A pid far above any pid_max stands for a process that died.
	stale := notePath(path, 1<<30)
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
