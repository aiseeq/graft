package app

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/gocache"
	"github.com/aiseeq/graft/v2/internal/loadgate"
	"github.com/aiseeq/graft/v2/internal/userconfig"
)

func TestGocacheFreeBelowSkipsCacheWalkWhenSpaceIsEnough(t *testing.T) {
	dir, old := makeTrimCache(t)
	t.Setenv("GOCACHE", dir)
	var calls int
	previous := freeSpace
	freeSpace = func(path string) (int64, error) {
		calls++
		if path != dir {
			t.Fatalf("measured %s, want %s", path, dir)
		}
		return 80 << 30, nil
	}
	t.Cleanup(func() { freeSpace = previous })
	var out bytes.Buffer
	err := (&App{Stdout: &out}).GocacheTrim(context.Background(), 6*time.Hour, true, 0, 80<<30)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !strings.Contains(out.String(), "80.0 GiB free on "+dir+", at or above --free-below 80.0 GiB: nothing to do") {
		t.Fatalf("calls %d, output %q", calls, out.String())
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("skip removed stale entry: %v", err)
	}
}

func TestGocacheFreeBelowMeasuresBeforeAndAfterTrim(t *testing.T) {
	dir, old := makeTrimCache(t)
	t.Setenv("GOCACHE", dir)
	trimProc(t, 1, 1)
	serveTrimDisk(t, 0)
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	limits := filepath.Join(configHome, "akit", "pressure.yaml")
	if err := os.MkdirAll(filepath.Dir(limits), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(limits, []byte("runnable: 10\nmax_io_full: 10\nmax_swap_kbs: 1024\nswap_window: 1ms\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	previous := freeSpace
	values := []int64{40 << 30, 42 << 30}
	freeSpace = func(string) (int64, error) {
		value := values[0]
		values = values[1:]
		return value, nil
	}
	t.Cleanup(func() { freeSpace = previous })
	var out bytes.Buffer
	err := (&App{Stdout: &out, Stderr: &bytes.Buffer{}}).GocacheTrim(context.Background(), 25*time.Hour, true, time.Second, 80<<30)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "removed 1 entries") || !strings.Contains(out.String(), "free space 40.0 GiB before, 42.0 GiB after") {
		t.Errorf("output %q", out.String())
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("low free space did not remove stale entry: %v", err)
	}
}

func makeTrimCache(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	old := filepath.Join(dir, "3f", strings.Repeat("3", 64)+"-d")
	if err := os.MkdirAll(filepath.Dir(old), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(old, time.Now().Add(-48*time.Hour), time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("This directory holds cached build artifacts from the Go build system.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, old
}

func trimProc(t *testing.T, dirty, writeback int) string {
	t.Helper()
	root := t.TempDir()
	for name, data := range map[string]string{
		"meminfo":         fmt.Sprintf("Dirty: %d kB\nWriteback: %d kB\n", dirty, writeback),
		"stat":            "cpu 1 1 1 1\ncpu0 1 1 1 1\nprocs_running 1\n",
		"loadavg":         "0.00 0.00 0.00 1/100 1\n",
		"vmstat":          "nr_free_pages 1\npswpin 0\npswpout 0\n",
		"pressure/memory": "some avg10=0.00 avg60=0.00 avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1\n",
		"pressure/io":     "some avg10=0.00 avg60=0.00 avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1\n",
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(loadgate.ProcEnv, root)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	return root
}

func TestTrimWaitsForDirty(t *testing.T) {
	root := trimProc(t, maxDirtyWritebackKB, 1)
	var calls int
	// The new meminfo replaces the old one whole: written in place, a poll
	// could read it truncated, as the kernel's never is.
	next := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(next, []byte("Dirty: 1 kB\nWriteback: 1 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = os.Rename(next, filepath.Join(root, "meminfo"))
	}()
	proceed, waits, err := waitForTrimBatch(context.Background(), time.Second, 5*time.Millisecond,
		trimMemoryCalm, func(context.Context) (string, error) { calls++; return "", nil })
	if err != nil || !proceed || waits[loadgate.WhyWriteback] < 20*time.Millisecond || calls != 1 {
		t.Fatalf("proceed %v, waits %v, disk checks %d, error %v", proceed, waits, calls, err)
	}
}

func TestTrimWaitsForBusyDisk(t *testing.T) {
	trimProc(t, 1, 1)
	serveTrimDisk(t, 1000)
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	limits := filepath.Join(configHome, "akit", "pressure.yaml")
	if err := os.MkdirAll(filepath.Dir(limits), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(limits, []byte("runnable: 10\nmax_io_full: 10\nmax_swap_kbs: 1024\nswap_window: 1ms\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	load := userconfig.MachineLoad{Poll: 5 * time.Millisecond}
	checker := loadgate.NewDiskBusyChecker("gocache trim", trimKind, load, io.Discard)
	proceed, waits, err := waitForTrimBatch(context.Background(), 35*time.Millisecond, load.Poll,
		trimMemoryCalm, checker.Why)
	if err != nil || proceed || waits[loadgate.WhyIO] < 35*time.Millisecond {
		t.Fatalf("proceed %v, waits %v, error %v", proceed, waits, err)
	}
}

func TestTrimWaitCountsTheTimeOfItsChecks(t *testing.T) {
	proceed, waits, err := waitForTrimBatch(context.Background(), 50*time.Millisecond, time.Millisecond,
		func() (bool, error) { return true, nil },
		func(context.Context) (string, error) {
			time.Sleep(20 * time.Millisecond)
			return loadgate.WhyIO, nil
		})
	if err != nil || proceed || waits[loadgate.WhyIO] < 50*time.Millisecond {
		t.Fatalf("proceed %v, waits %v, error %v", proceed, waits, err)
	}
}

func TestTrimIgnoresCPUAndLoadAverage(t *testing.T) {
	root := trimProc(t, 1, 1)
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte("cpu 1 1 1 1\ncpu0 1 1 1 1\nprocs_running 100\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "loadavg"), []byte("100.00 100.00 100.00 100/100 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	serveTrimDisk(t, 0)
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	limits := filepath.Join(configHome, "akit", "pressure.yaml")
	if err := os.MkdirAll(filepath.Dir(limits), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(limits, []byte("runnable: 1\nmax_io_full: 10\nmax_swap_kbs: 1024\nswap_window: 1ms\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	load := userconfig.MachineLoad{Poll: 5 * time.Millisecond}
	checker := loadgate.NewDiskBusyChecker("gocache trim", trimKind, load, io.Discard)
	proceed, waits, err := waitForTrimBatch(context.Background(), 100*time.Millisecond, load.Poll,
		trimMemoryCalm, checker.Why)
	if err != nil || !proceed || waitTotal(waits) > 20*time.Millisecond {
		t.Fatalf("proceed %v, waits %v, error %v", proceed, waits, err)
	}
}

func TestTrimWaitTracksQuietAndWritebackSeparately(t *testing.T) {
	proceed, waits, err := waitForTrimBatch(context.Background(), 20*time.Millisecond, 3*time.Millisecond,
		func() (bool, error) { return false, nil },
		func(context.Context) (string, error) { t.Fatal("busy check ran during writeback"); return "", nil })
	if err != nil || proceed || waits[loadgate.WhyWriteback] == 0 || waits[loadgate.WhyQuiet] != 0 {
		t.Fatalf("writeback: proceed %v, waits %v, error %v", proceed, waits, err)
	}
	checks := 0
	proceed, waits, err = waitForTrimBatch(context.Background(), 20*time.Millisecond, 3*time.Millisecond,
		func() (bool, error) { return true, nil },
		func(context.Context) (string, error) {
			checks++
			if checks > 2 {
				return "", nil
			}
			return loadgate.WhyQuiet, nil
		})
	if err != nil || !proceed || waits[loadgate.WhyQuiet] == 0 || waits[loadgate.WhyWriteback] != 0 {
		t.Fatalf("quiet: proceed %v, waits %v, error %v", proceed, waits, err)
	}
}

func TestFormatTrimWaits(t *testing.T) {
	if got := formatTrimWaits(nil); got != "" {
		t.Fatalf("zero wait formatting = %q", got)
	}
	got := formatTrimWaits(loadgate.Waits{loadgate.WhyQuiet: time.Second, loadgate.WhyWriteback: 2 * time.Second, "custom": 3 * time.Second})
	if got != " (write-out 2s, quiet 1s, custom 3s)" {
		t.Fatalf("wait breakdown = %q", got)
	}
}

func TestTrimStopsAtMaxWait(t *testing.T) {
	trimProc(t, 1, 1)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("This directory holds cached build artifacts from the Go build system.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	shard := filepath.Join(dir, "00")
	if err := os.Mkdir(shard, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		path := filepath.Join(shard, fmt.Sprintf("%064x-a", i))
		if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, time.Now().Add(-48*time.Hour), time.Now().Add(-48*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now()
	result, err := gocache.TrimWithOptions(dir, 25*time.Hour, time.Now(), gocache.Options{Apply: true, BeforeBatch: func() (bool, error) {
		ok, _, err := waitForTrimBatch(context.Background(), 20*time.Millisecond, 5*time.Millisecond,
			trimMemoryCalm, func(context.Context) (string, error) { return loadgate.WhyIO, nil })
		return ok, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 100*time.Millisecond || result.Removed != 0 || result.Remaining != 3 {
		t.Fatalf("elapsed %s, result %+v", time.Since(started), result)
	}
}

func serveTrimDisk(t *testing.T, ioFullMS int) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	listener, err := net.Listen("unix", filepath.Join(dir, "io-state.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = bufio.NewReader(conn).ReadString('\n')
			_, _ = fmt.Fprintf(conn, `{"v":1,"at":%q,"5s":{"io_full_ms":%d},"open":{"open":false,"ms":0}}`+"\n", time.Now().Format(time.RFC3339Nano), ioFullMS)
			_ = conn.Close()
		}
	}()
}

// TestTrimMemoryCalmWithoutTheMeasure: where the machine reports no memory
// queued to write (no /proc/meminfo outside Linux), a batch waits for the
// busy check alone instead of failing the trim.
func TestTrimMemoryCalmWithoutTheMeasure(t *testing.T) {
	t.Setenv(loadgate.ProcEnv, t.TempDir())
	measured := dirtyMeasured
	t.Cleanup(func() { dirtyMeasured = measured })
	dirtyMeasured = func() bool { return false }
	if calm, err := trimMemoryCalm(); err != nil || !calm {
		t.Fatalf("without the measure: calm %v, error %v", calm, err)
	}
	dirtyMeasured = measured
	if _, err := trimMemoryCalm(); err == nil {
		t.Fatal("with the measure and no meminfo: no error")
	}
}
