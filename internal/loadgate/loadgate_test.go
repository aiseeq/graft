package loadgate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/loadavg"
	"github.com/aiseeq/graft/v2/internal/userconfig"
)

// TestPriorityWaitIsBounded: a priority task above wait_above starts once it
// has waited priorityWait, and says so; without a priority it keeps waiting.
func TestPriorityWaitIsBounded(t *testing.T) {
	dir := t.TempDir()
	load := filepath.Join(dir, "loadavg")
	if err := os.WriteFile(load, []byte("30.00 1 1 1/1 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(loadavg.FileEnv, load)
	defer func(d time.Duration) { priorityWait = d }(priorityWait)
	priorityWait = 100 * time.Millisecond
	l := userconfig.MachineLoad{FreeBelow: 16, WaitAbove: 22, Settle: time.Hour, Poll: 10 * time.Millisecond}
	var log strings.Builder
	waited, err := Wait(context.Background(), "build", "graft deploy prod", l, filepath.Join(dir, "load.lock"), &log)
	if err != nil || waited < priorityWait || waited > 10*priorityWait {
		t.Fatalf("waited %s, %v", waited, err)
	}
	if !strings.Contains(log.String(), "the most a priority task waits") {
		t.Errorf("log:\n%s", log.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*priorityWait)
	defer cancel()
	if _, err := Wait(ctx, "build", "", l, filepath.Join(dir, "load.lock"), &log); err == nil {
		t.Error("a task without a priority started at load 30")
	}
}
