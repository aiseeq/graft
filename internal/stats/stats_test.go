package stats

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExpectedCoresP90(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	dir := filepath.Join(cache, "graft")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	values := make([]float64, 20)
	for i := range values {
		values[i] = float64(i+1) / 2
	}
	data, err := json.Marshal(coreHistory{"projectA:build": values})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "load.cores.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ExpectedCores("projectA:build")
	want := min(9.0, float64(runtime.NumCPU()))
	if err != nil || got != want {
		t.Fatalf("ExpectedCores = %v, %v; want %v", got, err, want)
	}
	if err := os.Remove(filepath.Join(dir, "load.cores.json")); err != nil {
		t.Fatal(err)
	}
	got, err = ExpectedCores("new")
	if err != nil || got != 4 {
		t.Fatalf("ExpectedCores without history = %v, %v; want 4", got, err)
	}
}

func TestUsageKeepsPartialLoadPeakAndRejectsUnknownStep(t *testing.T) {
	peak := Usage{}
	if err := peak.Watched(Sample{LoadStart: 3, LoadMax: 30, LoadKnown: true, LoadErr: errors.New("temporary read failure")}); err == nil {
		t.Fatal("load read failure was not reported")
	}
	if start, maxLoad := peak.Loads(); start == nil || maxLoad == nil || *start != 3 || *maxLoad != 30 {
		t.Fatalf("partial load sample lost: %v %v", start, maxLoad)
	}
	whole := Usage{LoadStart: 3, LoadMax: 3, LoadKnown: true}
	whole.Add(Usage{})
	if start, maxLoad := whole.Loads(); start != nil || maxLoad != nil {
		t.Fatalf("incomplete task retained a misleading load: %v %v", start, maxLoad)
	}
}

func TestLoadSampleDoesNotEraseTreePeak(t *testing.T) {
	usage := Usage{TreeMem: 64 << 20, TreeKnown: true}
	if err := usage.Watched(Sample{LoadStart: 3, LoadMax: 30, LoadKnown: true, TreeErr: ErrNoTree}); err != nil {
		t.Fatal(err)
	}
	if !usage.TreeKnown || usage.TreeMem != 64<<20 {
		t.Errorf("load-only sample changed tree memory: %+v", usage)
	}
}

func TestStepText(t *testing.T) {
	if got := StepText("set -eu\n  go test \t./...\n"); got != "set -eu go test ./..." {
		t.Errorf("one line: %q", got)
	}
	long := strings.Repeat("ж", 300)
	got := StepText(long)
	if r := []rune(got); len(r) != maxStepText || !strings.HasSuffix(got, "...") {
		t.Errorf("cut to %d runes: %q", len(r), got)
	}
	if got := StepText("go vet ./..."); got != "go vet ./..." {
		t.Errorf("short text changed: %q", got)
	}
}

func TestLimitReason(t *testing.T) {
	got := LimitReason("\x1b[31m" + strings.Repeat("ж", 301) + "\x1b[0m\nsecond\n")
	lines := strings.Split(got, "\n")
	if len(lines) != 2 || len([]rune(lines[0])) != 300 || strings.Contains(got, "\x1b") || lines[1] != "second" {
		t.Fatalf("unexpected bounded reason: %q", got)
	}
	many := LimitReason(strings.Repeat(strings.Repeat("x", 300)+"\n", 12))
	if strings.Count(many, "\n") > 9 {
		t.Errorf("kept more than 10 lines: %q", many)
	}
	if len([]rune(many)) > 2000 {
		t.Errorf("reason exceeds total limit: %d", len([]rune(many)))
	}
}

func TestMedian(t *testing.T) {
	for _, c := range []struct {
		in   []float64
		want float64
	}{{[]float64{3, 1, 2}, 2}, {[]float64{4, 1, 3, 2}, 2.5}, {[]float64{7}, 7}} {
		if got := median(c.in); got != c.want {
			t.Errorf("median(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// rateOf feeds readings to a writeRate a second apart, each a set of
// pid:parent:megabytes processes, -1 megabytes for one graft may not read,
// and returns the megabytes it counted.
func rateOf(t *testing.T, readings ...string) (counted int64, r *writeRate) {
	t.Helper()
	r = &writeRate{}
	at := time.Unix(1000, 0)
	for i, reading := range readings {
		now := map[int]treeProc{}
		for proc := range strings.FieldsSeq(reading) {
			var pid, parent int
			var mb int64
			if _, err := fmt.Sscanf(proc, "%d:%d:%d", &pid, &parent, &mb); err != nil {
				t.Fatalf("reading %q: %v", reading, err)
			}
			now[pid] = treeProc{parent: parent, writes: mb << 20, readable: mb >= 0}
			if mb < 0 {
				now[pid] = treeProc{parent: parent}
			}
		}
		r.add(at.Add(time.Duration(i)*time.Second), now)
	}
	return r.total >> 20, r
}

func TestWriteRateCountsEveryWriteOnce(t *testing.T) {
	for _, c := range []struct {
		name     string
		readings []string
		want     int64
	}{
		{"growth from the first reading", []string{"1:0:5", "1:0:7 2:1:3", "1:0:9 2:1:4"}, 4 + 3 + 1},
		// The parent takes in a reaped child's 4 MB, 1 of them new.
		{"a reaped child", []string{"1:0:0 2:1:0", "1:0:0 2:1:3", "1:0:4"}, 4},
		// Reaped while the tree was read: gone before its parent grew.
		{"a child reaped while reading", []string{"1:0:0 2:1:0", "1:0:0 2:1:3", "1:0:0", "1:0:3"}, 3},
		{"a grandchild reaped twice", []string{"1:0:0 2:1:0 3:2:0", "1:0:0 2:1:1 3:2:2", "1:0:0 2:1:3", "1:0:3"}, 3},
		// sudo's 100 MB are not known: not a burst when its parent reaps it.
		{"an unreadable child", []string{"1:0:0 2:1:-1", "1:0:1 2:1:-1", "1:0:101", "1:0:101", "1:0:102"}, 2},
		{"read, then unreadable", []string{"1:0:0 2:1:0", "1:0:0 2:1:5", "1:0:0 2:1:-1", "1:0:9", "1:0:9"}, 5},
		// What it wrote unseen stays out; what it writes after counts.
		{"unreadable, then read again", []string{"1:0:0 2:1:0", "1:0:0 2:1:-1", "1:0:0 2:1:50", "1:0:0 2:1:52"}, 2},
		{"a child and grandchild gone at once", []string{"1:0:0 2:1:0 3:2:0", "1:0:0 2:1:0 3:2:5", "1:0:5", "1:0:5"}, 5},
		{"an unreadable grandchild gone with its parent", []string{"1:0:0 2:1:0 3:2:-1", "1:0:0 2:1:0 3:2:-1", "1:0:100", "1:0:100", "1:0:101"}, 1},
		// Children going one by one do not keep the daemon's credit alive.
		{"a daemon leaving a busy parent", []string{"1:0:0 2:1:0 3:1:0", "1:0:0 2:1:8 3:1:0 4:1:0", "1:0:2 3:1:0 4:1:0", "1:0:4 4:1:0 5:1:0", "1:0:6 5:1:0", "1:0:8", "1:0:10"}, 8 + 6},
		// The second process of the first reading starts there too.
		{"the first reading", []string{"1:0:5 2:1:7", "1:0:5 2:1:7"}, 0},
		// Taken for reaped, it costs its parent's 2 MB over two readings.
		{"a process leaving the tree", []string{"1:0:0 2:1:0", "1:0:0 2:1:8", "1:0:1", "1:0:2", "1:0:5"}, 8 + 3},
	} {
		if got, _ := rateOf(t, c.readings...); got != c.want {
			t.Errorf("%s: counted %d MB, want %d", c.name, got, c.want)
		}
	}
	if _, r := rateOf(t, "1:0:-1", "1:0:-1 2:1:-1", "1:0:-1"); r.known {
		t.Errorf("a tree graft may not read has a peak: %d", r.peak)
	}
}

func TestWriteRatePeak(t *testing.T) {
	var r writeRate
	at := time.Unix(1000, 0)
	tick := func(ms int, mb int64) {
		r.add(at.Add(time.Duration(ms)*time.Millisecond), map[int]treeProc{1: {writes: mb << 20, readable: true}})
	}
	tick(0, 5)
	tick(200, 5)
	tick(800, 7)
	if r.known {
		t.Fatalf("a peak under a second: %d", r.peak)
	}
	tick(1000, 7)
	if !r.known || r.peak != 2<<20 {
		t.Fatalf("2 MB over the first second: %d %v", r.peak, r.known)
	}
	tick(1200, 19)
	// 2 + 12 MB since the reading at 200 ms, a second before.
	if r.peak != 14<<20 {
		t.Errorf("14 MB in the second up to 1.2 s: %d", r.peak)
	}
	tick(3200, 19)
	if r.peak != 14<<20 {
		t.Errorf("an idle stretch lowered the peak: %d", r.peak)
	}
}

func TestUsageSumsDiskIO(t *testing.T) {
	var task Usage
	task.Add(Usage{Read: 1 << 20, Write: 3 << 20, IOKnown: true, PeakWrite: 4 << 20, PeakKnown: true})
	task.Add(Usage{Read: 2 << 20, Write: 5 << 20, IOKnown: true})
	task.Add(Usage{Write: 1 << 20, IOKnown: true, PeakWrite: 2 << 20, PeakKnown: true})
	var rec Record
	task.Fill(&rec)
	if rec.ReadMB == nil || *rec.ReadMB != 3 || rec.WriteMB == nil || *rec.WriteMB != 9 || rec.PeakWriteMBs == nil || *rec.PeakWriteMBs != 4 {
		t.Errorf("read %v write %v peak %v", rec.ReadMB, rec.WriteMB, rec.PeakWriteMBs)
	}
	var unknown Record
	(&Usage{}).Fill(&unknown)
	if unknown.ReadMB != nil || unknown.WriteMB != nil || unknown.PeakWriteMBs != nil {
		t.Errorf("unknown IO was written: %+v", unknown)
	}
}
