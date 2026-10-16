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
	w := WatchCgroup(dir)
	time.Sleep(2 * watchInterval)
	write("memory.current", "3145728\n")
	time.Sleep(2 * watchInterval)
	write("memory.current", "2097152\n")
	write("cpu.stat", "usage_usec 3500000\nuser_usec 900000\n")
	s := w.Stop()
	if s.Err != nil || s.CPU != 2500*time.Millisecond || s.Mem != 3<<20 {
		t.Errorf("sample %+v", s)
	}
	if s := WatchCgroup(filepath.Join(dir, "gone")).Stop(); s.Err == nil {
		t.Error("a missing cgroup gave no error")
	}
}
