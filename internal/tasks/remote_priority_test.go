package tasks

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/loadgate"
	"github.com/aiseeq/graft/v2/internal/userconfig"
)

// TestWouldWaitTakesThePriorityQueueView: a priority task asks whether it
// would wait with the priority view of the load queue, an ordinary task with
// the whole queue.
func TestWouldWaitTakesThePriorityQueueView(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	oldOrdinary, oldPriority := checkMachineBusy, checkPriorityBusy
	t.Cleanup(func() { checkMachineBusy, checkPriorityBusy = oldOrdinary, oldPriority })
	var asked string
	checkMachineBusy = func(context.Context, string, string, userconfig.MachineLoad, string, io.Writer) (bool, error) {
		asked = "ordinary"
		return false, nil
	}
	checkPriorityBusy = func(context.Context, string, string, userconfig.MachineLoad, string, io.Writer) (bool, error) {
		asked = "priority"
		return false, nil
	}
	budget, err := loadgate.NewBudget(t.TempDir(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, priority := range []bool{false, true} {
		if priority {
			t.Setenv(loadgate.EnvPriority, "projectA deploy")
			t.Setenv(loadgate.EnvBudget, budget)
			t.Setenv(loadgate.EnvThen, loadgate.ThenFail)
		} else {
			t.Setenv(loadgate.EnvPriority, "")
		}
		asked = ""
		r := NewRunner(t.TempDir(), &config.Config{}, io.Discard, io.Discard)
		r.Load = func() (userconfig.MachineLoad, error) { return userconfig.MachineLoad{Threads: 1}, nil }
		if _, err := r.wouldWait(context.Background(), "build"); err != nil {
			t.Fatal(err)
		}
		want := map[bool]string{false: "ordinary", true: "priority"}[priority]
		if asked != want {
			t.Errorf("priority %v asked the %q view; want %q", priority, asked, want)
		}
	}
}
