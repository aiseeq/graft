package loadgate

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiseeq/akit/pressure"
)

// diskSource is a disk state source on its socket in XDG_RUNTIME_DIR: it
// answers every reading with the IO full time of the last 5 seconds set,
// or the error set, and the kind it was asked for as the profile.
type diskSource struct {
	mu    sync.Mutex
	ioMS  float64
	fail  string
	kinds []string
}

func fakeDisk(t *testing.T) *diskSource {
	t.Helper()
	// A socket's path is short: the temporary directory of a test with a
	// long name would not fit.
	dir, err := os.MkdirTemp("", "disk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("XDG_RUNTIME_DIR", dir)
	ln, err := net.Listen("unix", filepath.Join(dir, "io-state.sock"))
	if err != nil {
		t.Fatal(err)
	}
	d := &diskSource{}
	var served sync.WaitGroup
	served.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			d.answer(t, conn)
		}
	})
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Error(err)
		}
		served.Wait()
	})
	return d
}

func (d *diskSource) set(ioMS float64, fail string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ioMS, d.fail = ioMS, fail
}

func (d *diskSource) asked() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.kinds...)
}

func (d *diskSource) answer(t *testing.T, conn net.Conn) {
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Errorf("disk source: %v", err)
		return
	}
	var req struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		t.Errorf("disk source: %q: %v", line, err)
		return
	}
	d.mu.Lock()
	d.kinds = append(d.kinds, req.Kind)
	answer := map[string]any{"v": 1, "at": time.Now().Format(time.RFC3339Nano),
		"5s": map[string]any{"io_full_ms": d.ioMS}, "open": map[string]any{"open": false, "ms": 0},
		"profile": map[string]any{"kind": req.Kind}}
	if d.fail != "" {
		answer = map[string]any{"v": 1, "error": d.fail}
	}
	d.mu.Unlock()
	if err := json.NewEncoder(conn).Encode(answer); err != nil {
		t.Errorf("disk source: %v", err)
	}
}

// TestDiskSourceKnowsTheKind: the admission asks the disk state source for
// the task's kind and carries the profile it gave back.
func TestDiskSourceKnowsTheKind(t *testing.T) {
	fakeMachine(t)
	d := fakeDisk(t)
	var log strings.Builder
	adm, release, err := Wait(context.Background(), "build", "projectA:build", nil, testLoad, filepath.Join(t.TempDir(), "load.lock"), &log)
	if err != nil || log.Len() != 0 {
		t.Fatalf("%v:\n%s", err, log.String())
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if kinds := d.asked(); len(kinds) != 1 || kinds[0] != "projectA:build" {
		t.Errorf("the source was asked for %q", kinds)
	}
	if string(adm.DiskProfile) != `{"kind":"projectA:build"}` {
		t.Errorf("disk profile %s", adm.DiskProfile)
	}
}

// TestNoDiskSourceUsesPSI: without the disk state source, or with one that
// fails, the admission says so once and measures the disk by the IO
// pressure's total; the profile is then unknown.
func TestNoDiskSourceUsesPSI(t *testing.T) {
	for _, c := range []struct {
		name, want string
		source     func(t *testing.T)
	}{
		{"none", "graft: build: " + pressure.ErrNoIOSource.Error() + ", PSI total used\n",
			func(t *testing.T) { t.Setenv("XDG_RUNTIME_DIR", t.TempDir()) }},
		{"failing", "graft: build: " + pressure.ErrIOSourceFailed.Error() + ": no data yet, PSI total used\n",
			func(t *testing.T) { fakeDisk(t).set(0, "no data yet") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			setRunnable, _, _ := fakeMachine(t)
			c.source(t)
			setRunnable(30)
			go func() {
				time.Sleep(100 * time.Millisecond)
				setRunnable(1)
			}()
			var log strings.Builder
			adm, release, err := Wait(context.Background(), "build", "projectA:build", nil, testLoad, filepath.Join(t.TempDir(), "load.lock"), &log)
			if err != nil {
				t.Fatal(err)
			}
			if err := release(); err != nil {
				t.Fatal(err)
			}
			// Told once over the readings of a wait.
			if got := strings.Count(log.String(), "PSI total used"); got != 1 || !strings.HasPrefix(log.String(), c.want) {
				t.Errorf("log, want %q once:\n%s", c.want, log.String())
			}
			if adm.DiskProfile != nil {
				t.Errorf("a profile without the source: %s", adm.DiskProfile)
			}
		})
	}
}

// TestBusyNowAsksTheKind: BusyNow reads the machine as Wait does, for the
// kind given.
func TestBusyNowAsksTheKind(t *testing.T) {
	fakeMachine(t)
	d := fakeDisk(t)
	d.set(2000, "") // 40% of 5 s
	var log strings.Builder
	busy, err := BusyNow(context.Background(), "test", "projectA:test", testLoad, "", &log)
	if err != nil || !busy {
		t.Fatalf("busy %v, %v:\n%s", busy, err, log.String())
	}
	if kinds := d.asked(); len(kinds) != 1 || kinds[0] != "projectA:test" {
		t.Errorf("the source was asked for %q", kinds)
	}
}
