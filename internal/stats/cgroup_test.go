package stats

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWatchCgroup: the CPU a cgroup used while watched, and the highest
// memory it held.
func TestWatchCgroup(t *testing.T) {
	dir := t.TempDir()
	// Replaced whole, as the kernel shows them: the watch reads them while
	// the test writes.
	write := func(name, data string) {
		t.Helper()
		tmp := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(tmp, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	write("cpu.stat", "usage_usec 1000000\nuser_usec 600000\n")
	write("memory.current", "1048576\n")
	// The test ticks: a tick is taken only between two readings, so the
	// second of two ticks after a write proves a reading of it.
	stop, tick := make(chan struct{}), make(chan time.Time)
	done := make(chan DBSample)
	go func() { done <- followCgroup(dir, stop, tick) }()
	tick <- time.Time{}
	write("memory.current", "3145728\n")
	tick <- time.Time{}
	tick <- time.Time{}
	write("memory.current", "2097152\n")
	write("cpu.stat", "usage_usec 3500000\nuser_usec 900000\n")
	close(stop)
	if s := <-done; s.Err != nil || s.CPU != 2500*time.Millisecond || s.Mem != 3<<20 {
		t.Errorf("sample %+v", s)
	}
	if s := WatchCgroup(dir).Stop(); s.Err != nil || s.Mem != 2<<20 {
		t.Errorf("the watch: %+v", s)
	}
	if s := WatchCgroup(filepath.Join(dir, "gone")).Stop(); s.Err == nil {
		t.Error("a missing cgroup gave no error")
	}
}
