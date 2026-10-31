package app

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/gocache"
	"github.com/aiseeq/graft/v2/internal/loadgate"
	"github.com/aiseeq/graft/v2/internal/userconfig"
)

func trimProc(t *testing.T, dirty, writeback int) string {
	t.Helper()
	root := t.TempDir()
	for name, data := range map[string]string{
		"meminfo":         fmt.Sprintf("Dirty: %d kB\nWriteback: %d kB\n", dirty, writeback),
		"stat":            "cpu 1 1 1 1\ncpu0 1 1 1 1\nprocs_running 1\n",
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
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(root, "meminfo"), []byte("Dirty: 1 kB\nWriteback: 1 kB\n"), 0o644)
	}()
	proceed, waited, err := waitForTrimBatch(context.Background(), time.Second, 5*time.Millisecond,
		trimMemoryCalm, func(context.Context) (bool, error) { calls++; return false, nil })
	if err != nil || !proceed || waited < 20*time.Millisecond || calls != 1 {
		t.Fatalf("proceed %v, waited %s, disk checks %d, error %v", proceed, waited, calls, err)
	}
}

func TestTrimWaitsForBusyDisk(t *testing.T) {
	trimProc(t, 1, 1)
	serveTrimDisk(t, 1000)
	load := userconfig.MachineLoad{WaitAbove: 10, MaxIOFull: 10, MaxSwapKBs: 1024,
		SwapWindow: time.Millisecond, Poll: 5 * time.Millisecond}
	checker := loadgate.NewBusyChecker("gocache trim", trimKind, load, nil)
	proceed, waited, err := waitForTrimBatch(context.Background(), 35*time.Millisecond, load.Poll,
		trimMemoryCalm, checker.Busy)
	if err != nil || proceed || waited < 30*time.Millisecond {
		t.Fatalf("proceed %v, waited %s, error %v", proceed, waited, err)
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
			trimMemoryCalm, func(context.Context) (bool, error) { return true, nil })
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
			_, _ = fmt.Fprintf(conn, `{"v":1,"at":%q,"5s":{"io_full_ms":%d},"open":{"open":false,"ms":0},"profile":{"kind":"gocache-trim"}}`+"\n", time.Now().Format(time.RFC3339Nano), ioFullMS)
			_ = conn.Close()
		}
	}()
}
