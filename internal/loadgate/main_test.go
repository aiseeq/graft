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
