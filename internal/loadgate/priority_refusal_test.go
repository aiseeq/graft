package loadgate

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/pressure"
	"github.com/aiseeq/graft/v2/internal/lock"
)

func TestProbeReviewFIFORefusal(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	path := filepath.Join(t.TempDir(), "load.lock")
	holder, err := lock.Acquire(context.Background(), path, time.Second, "ordinary holder", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	p := &Priority{For: "deploy", Budget: newBudget(t, 80*time.Millisecond), Fail: true}
	adm, _, err := Wait(context.Background(), "fifo", "projectA:fifo", p, testLoad, path, io.Discard)
	assertQueueRefusal(t, "ordinary holder", adm, err)
}

func TestProbeReviewSettleRefusal(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	dir := t.TempDir()
	settlePath := filepath.Join(dir, "settle.lock")
	holder, err := lock.Acquire(context.Background(), settlePath, time.Second, "settling holder", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	p := &Priority{For: "deploy", Budget: newBudget(t, 80*time.Millisecond), Fail: true}
	adm, _, err := WaitWithSettle(context.Background(), "settle", "projectA:settle", p, testLoad, filepath.Join(dir, "load.lock"), settlePath, time.Second, 0, io.Discard)
	assertQueueRefusal(t, "settling holder", adm, err)
}

func assertQueueRefusal(t *testing.T, holder string, adm Admission, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a priority refusal")
	}
	for _, want := range []string{
		"deploy.load_wait: budget exhausted (budget 80ms)",
		"queued behind " + holder,
		"waited " + adm.Waits.Total().Round(time.Millisecond).String(),
		"budget on entry 80ms",
		"now: load average 1m",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
	if adm.Waits[WhyQueue] < 50*time.Millisecond {
		t.Errorf("queue wait = %s; want at least 50ms", adm.Waits[WhyQueue])
	}
}

type refusalMeasureSequence struct{ calls int }

func (m *refusalMeasureSequence) Check(_ context.Context, _ pressure.Limits, _ string, fresh bool) (pressure.Verdict, error) {
	m.calls++
	if fresh && m.calls > 1 {
		return pressure.Verdict{Load: pressure.Load{Runnable: 1, Threads: 22}}, nil
	}
	return pressure.Verdict{Measure: pressure.Swap, Value: 20, Limit: 10,
		Load: pressure.Load{Runnable: 1, Threads: 22, SwapKBs: 10, MemSome: 20}}, nil
}

func TestPriorityRefusalKeepsTheMeasureThatBlocked(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	sequence := &refusalMeasureSequence{}
	oldFactory := pressureMeterFactory
	pressureMeterFactory = func(string) machineMeter { return sequence }
	defer func() { pressureMeterFactory = oldFactory }()
	l := testLoad
	l.Poll = time.Millisecond
	p := &Priority{For: "deploy", Budget: newBudget(t, 50*time.Millisecond), Fail: true}
	_, _, err := Wait(context.Background(), "swap-refusal", "projectA:swap-refusal", p, l, filepath.Join(t.TempDir(), "load.lock"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "last wait: memory pressure") {
		t.Fatalf("priority refusal = %v; want the blocking swap reason", err)
	}
	lines := readAdmissionLines(t, filepath.Join(os.Getenv("XDG_CACHE_HOME"), "graft", "admissions.jsonl"))
	last := lines[len(lines)-1]
	if last.Verdict != "refused" || last.Measure != pressure.Swap {
		t.Fatalf("refused admission measure = %q, record=%+v; want %q", last.Measure, last, pressure.Swap)
	}
}

func TestPriorityQuietQueueRefusalNamesQuiet(t *testing.T) {
	fakeMachine(t)
	setQuiet(t, time.Now().Add(time.Second))
	p := &Priority{For: "deploy", Budget: newBudget(t, 80*time.Millisecond), Fail: true}
	path := filepath.Join(t.TempDir(), "load.lock")
	holder, err := lock.Acquire(context.Background(), path, time.Second, "quiet holder", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	_, _, err = Wait(context.Background(), "quiet-refusal", "projectA:quiet-refusal", p, testLoad, path, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "last wait: quiet mode kept this task waiting") {
		t.Fatalf("priority refusal = %v; want quiet mode as the queue reason", err)
	}
}
