package stats

import (
	"strings"
	"testing"
)

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
