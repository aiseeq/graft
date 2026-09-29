package lock

import (
	"bytes"
	"context"
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
