package loadgate

import (
	"os"
	"testing"
	"time"
)

// defaultIOWindowFloor is ioWindowFloor as the package sets it, before
// TestMain turns it off.
var defaultIOWindowFloor = ioWindowFloor

// TestMain turns the IO window floor off: tests of other rules would sleep
// it out between looks. TestPollTwoSecondsKeepsFiveSecondIOWithoutDiskSource
// sets it.
func TestMain(m *testing.M) {
	ioWindowFloor = 0
	os.Exit(m.Run())
}

// TestIOWindowFloorIsFiveSeconds: the floor matches aguard's fixed disk
// window.
func TestIOWindowFloorIsFiveSeconds(t *testing.T) {
	if defaultIOWindowFloor != 5*time.Second {
		t.Errorf("ioWindowFloor = %s; want 5s", defaultIOWindowFloor)
	}
}

// TestAddWaitExtendsAContiguousSpan: polls of one wait make one span, a gap
// or another reason starts a new one, and the sums stay per reason.
func TestAddWaitExtendsAContiguousSpan(t *testing.T) {
	m := &machine{waits: Waits{}}
	t0 := time.Unix(1_000_000, 0)
	m.addWait(WhyQueue, t0, t0.Add(2*time.Second))
	m.addWait(WhyQueue, t0.Add(2*time.Second), t0.Add(4*time.Second))
	m.addWait(WhyQueue, t0.Add(4*time.Second), t0.Add(6*time.Second))
	m.addWait("load", t0.Add(6*time.Second), t0.Add(7*time.Second))
	m.addWait("load", t0.Add(9*time.Second), t0.Add(10*time.Second))
	if len(m.waitSpans) != 3 {
		t.Fatalf("spans = %v; want 3: one queue, two load around a gap", m.waitSpans)
	}
	if got := m.waitSpans[0].End.Sub(m.waitSpans[0].Start); got != 6*time.Second {
		t.Errorf("queue span = %s; want 6s", got)
	}
	if m.waits[WhyQueue] != 6*time.Second || m.waits["load"] != 2*time.Second {
		t.Errorf("waits = %v; want queue 6s, load 2s", m.waits)
	}
}
