package stats

import (
	"errors"
	"strings"
	"testing"
	"time"
)

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

func TestWriteRatePeak(t *testing.T) {
	var r writeRate
	at := time.Unix(1000, 0)
	tick := func(ms int, sum int64) { r.add(at.Add(time.Duration(ms)*time.Millisecond), sum) }
	tick(0, 5<<20)
	tick(200, 5<<20)
	tick(800, 7<<20)
	if r.known {
		t.Fatalf("a peak under a second: %d", r.peak)
	}
	// A process leaving the tree takes its writes out of the sum: that is
	// no negative write, and the next growth counts from the lower sum.
	tick(1000, 1<<20)
	if !r.known || r.peak != 2<<20 {
		t.Fatalf("2 MB over the first second: %d %v", r.peak, r.known)
	}
	tick(1200, 13<<20)
	// 2 + 12 MB since the point of 200 ms, a second before.
	if r.peak != 14<<20 {
		t.Errorf("14 MB in the second up to 1.2 s: %d", r.peak)
	}
	tick(3200, 13<<20)
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
