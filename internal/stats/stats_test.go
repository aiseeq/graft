package stats

import (
	"errors"
	"strings"
	"testing"
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
