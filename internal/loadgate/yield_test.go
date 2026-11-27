package loadgate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiseeq/akit/pressure"
	akitproc "github.com/aiseeq/akit/proc"
	"github.com/aiseeq/graft/v2/internal/lock"
	"github.com/aiseeq/graft/v2/internal/stats"
	"github.com/aiseeq/graft/v2/internal/userconfig"
	"github.com/gofrs/flock"
)

// The tests of this file hold an ordinary task in the load queue on a busy
// CPU, runnable 12 at a limit of 11, and check that it gives load.lock up
// while it waits, so a priority task passes it.

const pastLoadReason = "tl 0000aaaa:1"

// busyCPU makes the fake machine's CPU too busy for an ordinary task and
// returns the setter of its runnable count.
func busyCPU(t *testing.T) func(int) {
	t.Helper()
	setRunnable, _, _ := fakeMachine(t)
	setRunnableLimit(t, 11)
	setRunnable(12)
	return setRunnable
}

// quickReserves lets an admitted task's first CPU measure come within
// milliseconds, so the next admission does not wait the default grace out.
func quickReserves(t *testing.T) {
	oldPoll, oldWindow := reservePollInterval, reserveWindow
	reservePollInterval, reserveWindow = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { reservePollInterval, reserveWindow = oldPoll, oldWindow })
}

type waitOutcome struct {
	name    string
	adm     Admission
	release func() error
	err     error
}

// startWait runs Wait, or WaitWithSettle when settlePath is set, in the
// background.
func startWait(ctx context.Context, name string, p *Priority, path, settlePath, pastLoad string, log io.Writer) <-chan waitOutcome {
	return startWaitLoad(ctx, name, p, testLoad, path, settlePath, pastLoad, log)
}

// startWaitLoad is startWait with the load settings l.
func startWaitLoad(ctx context.Context, name string, p *Priority, l userconfig.MachineLoad, path, settlePath, pastLoad string, log io.Writer) <-chan waitOutcome {
	done := make(chan waitOutcome, 1)
	go func() {
		var out waitOutcome
		out.name = name
		if settlePath == "" {
			out.adm, out.release, out.err = Wait(ctx, name, "projectA:"+name, p, l, path, log, pastLoad)
		} else {
			out.adm, out.release, out.err = WaitWithSettle(ctx, name, "projectA:"+name, p, l, path, settlePath, time.Hour, 1, log, pastLoad)
		}
		done <- out
	}()
	return done
}

// waitLog signals once the task logs that it waits for the machine.
func waitLog() *signalWriter {
	return &signalWriter{text: ", waiting", seen: make(chan struct{})}
}

func awaitSignal(t *testing.T, seen <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func receiveOutcome(t *testing.T, done <-chan waitOutcome, within time.Duration, what string) waitOutcome {
	t.Helper()
	select {
	case out := <-done:
		return out
	case <-time.After(within):
		t.Fatalf("%s did not return within %s", what, within)
		return waitOutcome{}
	}
}

func releaseOutcome(t *testing.T, out waitOutcome) {
	t.Helper()
	if out.release == nil {
		return
	}
	if err := out.release(); err != nil {
		t.Errorf("releasing %s: %v", out.name, err)
	}
}

// admissionOrder receives both tasks and orders them by when each wrote its
// reserve, which an admission does under load.lock: the order of admission,
// not of the returns.
func admissionOrder(t *testing.T, a, b <-chan waitOutcome) (first, second waitOutcome) {
	t.Helper()
	first = receiveOutcome(t, a, 10*time.Second, "the first task")
	second = receiveOutcome(t, b, 10*time.Second, "the second task")
	if first.err != nil || second.err != nil {
		releaseOutcome(t, first)
		releaseOutcome(t, second)
		t.Fatalf("admissions: %s %v, %s %v", first.name, first.err, second.name, second.err)
	}
	if second.adm.ReserveSince.Before(first.adm.ReserveSince) {
		first, second = second, first
	}
	return first, second
}

// notAdmitted fails when done returns within d.
func notAdmitted(t *testing.T, done <-chan waitOutcome, d time.Duration, what string) {
	t.Helper()
	select {
	case out := <-done:
		releaseOutcome(t, out)
		t.Fatalf("%s returned early: %+v", what, out)
	case <-time.After(d):
	}
}

// cancelWaiting cancels a task that should still wait and checks that it
// stops without an admission.
func cancelWaiting(t *testing.T, cancel context.CancelFunc, done <-chan waitOutcome, what string) waitOutcome {
	t.Helper()
	cancel()
	out := receiveOutcome(t, done, 5*time.Second, what)
	if !errors.Is(out.err, context.Canceled) {
		releaseOutcome(t, out)
		t.Fatalf("%s after cancel = %+v; want context.Canceled", what, out)
	}
	return out
}

// assertQueueEmpty checks that nothing holds the lock at path or waits for it.
func assertQueueEmpty(t *testing.T, path string) {
	t.Helper()
	holders, err := lock.Holders(path)
	if err != nil || len(holders) != 0 {
		t.Errorf("holders of %s = %v, %v; want none", filepath.Base(path), holders, err)
	}
	queued, err := lock.Queue(context.Background(), path)
	if err != nil || len(queued) != 0 {
		t.Errorf("queue of %s = %v, %v; want none", filepath.Base(path), queued, err)
	}
	if left, _ := filepath.Glob(path + ".queue.*.lticket"); len(left) != 0 {
		t.Errorf("tickets left for %s: %v", filepath.Base(path), left)
	}
}

// TestOrdinaryWaitingForLoadYieldsToPastLoad: case 1 of the ticket. The
// ordinary task that took load.lock and waits for the load does not hold
// --past-load back; it keeps waiting itself.
func TestOrdinaryWaitingForLoadYieldsToPastLoad(t *testing.T) {
	busyCPU(t)
	path := filepath.Join(t.TempDir(), "load.lock")
	ordinaryCtx, cancelOrdinary := context.WithCancel(context.Background())
	defer cancelOrdinary()
	waiting := waitLog()
	ordinary := startWait(ordinaryCtx, "ordinary", nil, path, "", "", waiting)
	awaitSignal(t, waiting.seen, "the ordinary task to wait for the load")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	adm, release, err := Wait(ctx, "authorized", "projectA:authorized", nil, testLoad, path, io.Discard, pastLoadReason)
	if err != nil {
		t.Fatalf("--past-load behind an ordinary task waiting for the load: %v", err)
	}
	if adm.Waits[WhyLoad] != 0 {
		t.Errorf("--past-load waited for the load: %v", adm.Waits)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	notAdmitted(t, ordinary, 100*time.Millisecond, "the ordinary task on a busy CPU")
	cancelWaiting(t, cancelOrdinary, ordinary, "the ordinary task")
	assertQueueEmpty(t, path)
}

// TestOrdinaryBetweenDeployStepsYieldsToTheNextStep: case 2 of the ticket.
// An ordinary task takes load.lock between two heavy steps of a deploy run
// with --past-load and waits for the load; the next step still passes it.
func TestOrdinaryBetweenDeployStepsYieldsToTheNextStep(t *testing.T) {
	busyCPU(t)
	path := filepath.Join(t.TempDir(), "load.lock")
	p := &Priority{For: "projectA deploy", Budget: newBudget(t, time.Minute), Fail: true}
	step := func(name string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, release, err := Wait(ctx, name, "projectA:"+name, p, testLoad, path, io.Discard, pastLoadReason)
		if err != nil {
			t.Fatalf("deploy step %s: %v", name, err)
		}
		if err := release(); err != nil {
			t.Fatal(err)
		}
	}
	step("security")
	ordinaryCtx, cancelOrdinary := context.WithCancel(context.Background())
	defer cancelOrdinary()
	waiting := waitLog()
	ordinary := startWait(ordinaryCtx, "ordinary", nil, path, "", "", waiting)
	awaitSignal(t, waiting.seen, "the ordinary task to wait for the load")
	step("test")
	notAdmitted(t, ordinary, 100*time.Millisecond, "the ordinary task on a busy CPU")
	cancelWaiting(t, cancelOrdinary, ordinary, "the ordinary task")
	assertQueueEmpty(t, path)
}

// hookMeter calls onRead with the task's kind and whether the machine's
// rule found it busy on each reading, before the reading returns.
type hookMeter struct {
	inner  machineMeter
	onRead func(kind string, busy bool)
}

func (m *hookMeter) Check(ctx context.Context, l pressure.Limits, kind string, fresh bool) (pressure.Verdict, error) {
	v, err := m.inner.Check(ctx, l, kind, fresh)
	if err == nil {
		m.onRead(kind, v.Busy())
	}
	return v, err
}

// awaitGlob waits for a file matching pattern without failing the test from
// another goroutine.
func awaitGlob(pattern string) error {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if matches, err := filepath.Glob(pattern); err != nil || len(matches) > 0 {
			return err
		}
	}
	return fmt.Errorf("no file matching %s", pattern)
}

// TestPriorityJoiningAtTheAdmissionGoesFirst: a priority ticket that joins
// while the ordinary holder takes its calm reading, before it writes its
// reserve, goes first, and does not wait for a reserve of the task it passed.
func TestPriorityJoiningAtTheAdmissionGoesFirst(t *testing.T) {
	setRunnable := busyCPU(t)
	quickReserves(t)
	path := filepath.Join(t.TempDir(), "load.lock")
	p := &Priority{For: "projectA deploy", Budget: newBudget(t, time.Minute), Fail: true}
	priorityCtx, cancelPriority := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelPriority()
	var once sync.Once
	joined := make(chan struct{})
	var priority <-chan waitOutcome
	var joinErr error
	var calmReads atomic.Int32
	oldFactory := pressureMeterFactory
	pressureMeterFactory = func(root string) machineMeter {
		return &hookMeter{inner: oldFactory(root), onRead: func(kind string, busy bool) {
			if kind != "projectA:ordinary" || busy {
				return
			}
			calmReads.Add(1)
			once.Do(func() {
				priority = startWait(priorityCtx, "priority", p, path, "", "", io.Discard)
				joinErr = awaitGlob(path + ".queue.-*.lticket")
				close(joined)
			})
		}}
	}
	defer func() { pressureMeterFactory = oldFactory }()

	ordinaryCtx, cancelOrdinary := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelOrdinary()
	waiting := waitLog()
	ordinary := startWait(ordinaryCtx, "ordinary", nil, path, "", "", waiting)
	awaitSignal(t, waiting.seen, "the ordinary task to wait for the load")
	setRunnable(1)
	awaitSignal(t, joined, "the priority ticket to join at the ordinary task's calm reading")
	if joinErr != nil {
		t.Fatal(joinErr)
	}
	first, second := admissionOrder(t, priority, ordinary)
	defer releaseOutcome(t, first)
	defer releaseOutcome(t, second)
	if first.name != "priority" || first.err != nil {
		t.Fatalf("first admission = %s, %v; want the priority task", first.name, first.err)
	}
	if first.adm.Waits[WhyReserve] != 0 {
		t.Errorf("the priority task waited for a reserve of the task it passed: %v", first.adm.Waits)
	}
	if second.err != nil || second.adm.Waits[WhyQueue] == 0 {
		t.Errorf("the ordinary task = %v, waits %v; want an admission after a queue wait", second.err, second.adm.Waits)
	}
	// The ordinary task reads the machine again after the priority one, with
	// its reserve, and is admitted by that reading, leaving the queue.
	if n := calmReads.Load(); n < 2 {
		t.Errorf("the ordinary task took %d calm readings; want one more after the priority task went first", n)
	}
	assertQueueEmpty(t, path)
}

// TestOrdinaryResumesWhenThePriorityAheadLeaves: a priority task that took
// the head and the lock from a waiting ordinary task and is then cancelled
// leaves nothing behind: the ordinary task takes the lock back itself and
// starts once the machine is calm.
func TestOrdinaryResumesWhenThePriorityAheadLeaves(t *testing.T) {
	setRunnable := busyCPU(t)
	path := filepath.Join(t.TempDir(), "load.lock")
	ordinaryCtx := t.Context()
	waiting := waitLog()
	ordinary := startWait(ordinaryCtx, "ordinary", nil, path, "", "", waiting)
	awaitSignal(t, waiting.seen, "the ordinary task to wait for the load")

	// A deploy without --past-load waits for the load too, at the head.
	p := &Priority{For: "projectA deploy", Budget: newBudget(t, time.Minute), Fail: true}
	priorityCtx, cancelPriority := context.WithCancel(context.Background())
	defer cancelPriority()
	priorityWaiting := waitLog()
	priority := startWait(priorityCtx, "priority", p, path, "", "", priorityWaiting)
	awaitSignal(t, priorityWaiting.seen, "the priority task to take the lock and wait for the load")
	cancelWaiting(t, cancelPriority, priority, "the priority task")

	setRunnable(1)
	out := receiveOutcome(t, ordinary, 5*time.Second, "the ordinary task")
	defer releaseOutcome(t, out)
	if out.err != nil {
		t.Fatalf("the ordinary task after the priority left: %v", out.err)
	}
	if out.adm.Waits[WhyLoad] == 0 {
		t.Errorf("waits = %v; want the load wait", out.adm.Waits)
	}
}

// TestYieldCrashHelper is the process of TestOrdinaryCrashDuringYieldFreesTheQueue
// that dies while it gives the lock up for an hour's pause.
func TestYieldCrashHelper(t *testing.T) {
	path := os.Getenv("GRAFT_TEST_YIELD_LOCK")
	if path == "" {
		t.Skip("the helper process of TestOrdinaryCrashDuringYieldFreesTheQueue")
	}
	l := testLoad
	l.Poll = time.Hour
	_, _, err := Wait(context.Background(), "crashing", "projectA:crashing", nil, l, path, io.Discard, "")
	t.Fatalf("the helper's wait returned: %v", err)
}

// TestOrdinaryCrashDuringYieldFreesTheQueue: an ordinary task that dies while
// it keeps its ticket without the lock does not stop the queue.
func TestOrdinaryCrashDuringYieldFreesTheQueue(t *testing.T) {
	setRunnable := busyCPU(t)
	path := filepath.Join(t.TempDir(), "load.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestYieldCrashHelper$")
	cmd.Env = append(os.Environ(), "GRAFT_TEST_YIELD_LOCK="+path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	kill := func() error {
		_, err := akitproc.StopChild(cmd.Process.Pid, 0, func() error { return cmd.Process.Kill() })
		return err
	}
	killed := false
	defer func() {
		if !killed {
			_ = kill()
			_ = cmd.Wait()
		}
	}()
	yielding := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		queued, err := lock.Queue(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		holders, err := lock.Holders(path)
		if err != nil {
			t.Fatal(err)
		}
		admitted, err := filepath.Glob(admittedFilesPattern(path))
		if err != nil {
			t.Fatal(err)
		}
		if len(queued) == 1 && strings.Contains(queued[0], "crashing") && len(holders) == 0 && len(admitted) == 0 {
			yielding = true
			break
		}
	}
	if !yielding {
		t.Fatal("the helper did not keep its ticket with the lock given up")
	}
	if err := kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	killed = true

	setRunnable(1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, release, err := Wait(ctx, "next", "projectA:next", nil, testLoad, path, io.Discard, "")
	if err != nil {
		t.Fatalf("the next task after the crash: %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	assertQueueEmpty(t, path)
}

// TestRepeatedYieldsCountEachWaitOnce: an ordinary task that several
// priority tasks pass counts each interval of its wait once, by one why, and
// journals one admission; a priority task that gives the lock up at every
// poll spends no more of its budget than the time it waited.
func TestRepeatedYieldsCountEachWaitOnce(t *testing.T) {
	t.Run("ordinary", func(t *testing.T) {
		setRunnable := busyCPU(t)
		path := filepath.Join(t.TempDir(), "load.lock")
		// Each priority task's first reading of the machine takes a while
		// with the lock held, so the ordinary one comes back from its pause
		// to a queue it waits in.
		var slow atomic.Bool
		var reads atomic.Int32
		oldFactory := pressureMeterFactory
		pressureMeterFactory = func(root string) machineMeter {
			return &hookMeter{inner: oldFactory(root), onRead: func(kind string, _ bool) {
				if kind != "projectA:authorized" {
					return
				}
				reads.Add(1)
				if slow.CompareAndSwap(true, false) {
					time.Sleep(500 * time.Millisecond)
				}
			}}
		}
		defer func() { pressureMeterFactory = oldFactory }()
		started := time.Now()
		ordinaryCtx, cancelOrdinary := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancelOrdinary()
		waiting := waitLog()
		// Pauses of 400ms, shorter than a priority task's reading: the
		// pause lost before a wait behind a priority task shows in the sum
		// well past the tolerance.
		l := testLoad
		l.Poll = 400 * time.Millisecond
		ordinary := startWaitLoad(ordinaryCtx, "ordinary", nil, l, path, "", "", waiting)
		awaitSignal(t, waiting.seen, "the ordinary task to wait for the load")
		for i := range 3 {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			var log lockedLog
			began := time.Now()
			reads.Store(0)
			slow.Store(true)
			_, release, err := Wait(ctx, fmt.Sprintf("authorized-%d", i), "projectA:authorized", nil, testLoad, path, &log, pastLoadReason)
			cancel()
			if err != nil {
				t.Fatalf("--past-load %d after %s and %d readings: %v; log:\n%s", i, time.Since(began).Round(time.Millisecond), reads.Load(), err, log.String())
			}
			if err := release(); err != nil {
				t.Fatal(err)
			}
		}
		setRunnable(1)
		out := receiveOutcome(t, ordinary, 5*time.Second, "the ordinary task")
		elapsed := time.Since(started)
		defer releaseOutcome(t, out)
		if out.err != nil {
			t.Fatal(out.err)
		}
		if out.adm.Waits[WhyQueue] == 0 || out.adm.Waits[WhyLoad] == 0 {
			t.Errorf("waits = %v; want both the queue behind the priority tasks and the load", out.adm.Waits)
		}
		if total := out.adm.Waits.Total(); total > elapsed {
			t.Errorf("waits total %s over the %s the task took: an interval counted twice (%v)", total, elapsed, out.adm.Waits)
		}
		spans := slicesSortedSpans(out.adm)
		for i := 1; i < len(spans); i++ {
			if spans[i].Start.Before(spans[i-1].End) {
				t.Errorf("wait spans overlap: %+v and %+v", spans[i-1], spans[i])
			}
		}
		var admitted, refused int
		for _, rec := range readAdmissionLines(t, filepath.Join(os.Getenv("XDG_CACHE_HOME"), "graft", "admissions.jsonl")) {
			if rec.Kind != "projectA:ordinary" {
				continue
			}
			switch rec.Verdict {
			case "admitted":
				admitted++
				var sum float64
				for _, s := range rec.Waits {
					sum += s
				}
				// Every interval of the wait goes to one why: the sum is the
				// admission's time but for its last reading and its record.
				if sum > rec.WaitedS+0.001 || sum < rec.WaitedS-0.15 {
					t.Errorf("journaled waits %v sum %.3fs; want the waited_s %.3fs within 150ms", rec.Waits, sum, rec.WaitedS)
				}
			case "refused":
				refused++
			}
		}
		if admitted != 1 || refused != 0 {
			t.Errorf("journal of the ordinary task: %d admitted, %d refused; want one admission", admitted, refused)
		}
	})
	t.Run("priority-budget", func(t *testing.T) {
		setRunnable := busyCPU(t)
		path := filepath.Join(t.TempDir(), "load.lock")
		p := &Priority{For: "projectA deploy", Budget: newBudget(t, time.Minute), Fail: true}
		started := time.Now()
		waiting := waitLog()
		priority := startWait(context.Background(), "priority", p, path, "", "", waiting)
		awaitSignal(t, waiting.seen, "the priority task to wait for the load")
		time.Sleep(300 * time.Millisecond)
		setRunnable(1)
		out := receiveOutcome(t, priority, 5*time.Second, "the priority task")
		elapsed := time.Since(started)
		defer releaseOutcome(t, out)
		if out.err != nil {
			t.Fatal(out.err)
		}
		left, err := p.spend(time.Time{}, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		spent := time.Minute - left
		if spent > elapsed || spent < 250*time.Millisecond {
			t.Errorf("budget spent %s for a wait of at least 300ms within %s", spent, elapsed)
		}
	})
}

func slicesSortedSpans(adm Admission) []stats.WaitSpan {
	spans := slices.Clone(adm.WaitSpans)
	sort.Slice(spans, func(i, j int) bool { return spans[i].Start.Before(spans[j].Start) })
	return spans
}

// TestSettleWaitYieldsToPrioritySettle: an ordinary task with a settling
// period that waits for the load gives the settle lock up too, so a
// --past-load task with its own settling period passes it; the settling
// period after that admission holds the ordinary settle task back, and not a
// task without a settling period.
func TestSettleWaitYieldsToPrioritySettle(t *testing.T) {
	setRunnable := busyCPU(t)
	quickReserves(t)
	dir := t.TempDir()
	path, settlePath := filepath.Join(dir, "load.lock"), filepath.Join(dir, "load.settle.lock")
	ordinaryCtx, cancelOrdinary := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelOrdinary()
	waiting := waitLog()
	ordinary := startWait(ordinaryCtx, "ordinary", nil, path, settlePath, "", waiting)
	awaitSignal(t, waiting.seen, "the ordinary settle task to wait for the load")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, releaseAuthorized, err := WaitWithSettle(ctx, "authorized", "projectA:authorized", nil, testLoad, path, settlePath, time.Hour, 1, io.Discard, pastLoadReason)
	if err != nil {
		t.Fatalf("--past-load with settle behind an ordinary settle task waiting for the load: %v", err)
	}
	authorizedReleased := false
	defer func() {
		if !authorizedReleased {
			_ = releaseAuthorized()
		}
	}()
	holders, err := lock.Holders(settlePath)
	if err != nil || len(holders) != 1 || !strings.Contains(holders[0], "authorized") {
		t.Fatalf("settle lock holders after the admission = %v, %v; want the settling --past-load task", holders, err)
	}

	setRunnable(1)
	plainCtx, plainCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer plainCancel()
	_, releasePlain, err := Wait(plainCtx, "plain", "projectA:plain", nil, testLoad, path, io.Discard, "")
	if err != nil {
		t.Fatalf("a task without a settling period during another's settling: %v", err)
	}
	if err := releasePlain(); err != nil {
		t.Fatal(err)
	}
	notAdmitted(t, ordinary, 300*time.Millisecond, "the ordinary settle task during the settling period")

	authorizedReleased = true
	if err := releaseAuthorized(); err != nil {
		t.Fatal(err)
	}
	out := receiveOutcome(t, ordinary, 5*time.Second, "the ordinary settle task")
	if out.err != nil {
		t.Fatalf("the ordinary settle task after the settling period: %v", out.err)
	}
	releaseOutcome(t, out)
	assertQueueEmpty(t, path)
	assertQueueEmpty(t, settlePath)
}

// TestSettleTaskQueueingForLoadHoldsNoSettleLock: an ordinary settle task
// queued for load.lock behind a plain head that waits for the load holds its
// settle ticket, not the settle lock, so a --past-load settle task passes
// both of them.
func TestSettleTaskQueueingForLoadHoldsNoSettleLock(t *testing.T) {
	busyCPU(t)
	quickReserves(t)
	dir := t.TempDir()
	path, settlePath := filepath.Join(dir, "load.lock"), filepath.Join(dir, "load.settle.lock")
	headCtx, cancelHead := context.WithCancel(context.Background())
	defer cancelHead()
	waiting := waitLog()
	head := startWait(headCtx, "head", nil, path, "", "", waiting)
	awaitSignal(t, waiting.seen, "the plain head to wait for the load")

	settleCtx, cancelSettle := context.WithCancel(context.Background())
	defer cancelSettle()
	settler := startWait(settleCtx, "settler", nil, path, settlePath, "", io.Discard)
	awaitFiles(t, settlePath+".queue.*.lticket", 1)
	awaitFiles(t, path+".queue.*.lticket", 2)
	if holders, err := lock.Holders(settlePath); err != nil || len(holders) != 0 {
		t.Errorf("settle lock holders while the settle task queues for load = %v, %v; want none", holders, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, release, err := WaitWithSettle(ctx, "authorized", "projectA:authorized", nil, testLoad, path, settlePath, time.Hour, 1, io.Discard, pastLoadReason)
	if err != nil {
		t.Errorf("--past-load settle task behind an ordinary settle task queueing for load: %v", err)
	} else if err := release(); err != nil {
		t.Error(err)
	}
	cancelWaiting(t, cancelSettle, settler, "settler")
	cancelWaiting(t, cancelHead, head, "head")
	assertQueueEmpty(t, path)
	assertQueueEmpty(t, settlePath)
}

// TestPrioritySettleJoiningAtTheAdmissionGoesFirst: a --past-load settle
// ticket that joins at an ordinary settle task's calm reading goes first,
// though the ordinary task heads the load queue.
func TestPrioritySettleJoiningAtTheAdmissionGoesFirst(t *testing.T) {
	setRunnable := busyCPU(t)
	quickReserves(t)
	dir := t.TempDir()
	path, settlePath := filepath.Join(dir, "load.lock"), filepath.Join(dir, "load.settle.lock")
	pctx, pcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pcancel()
	var once sync.Once
	joined := make(chan struct{})
	var priority <-chan waitOutcome
	oldFactory := pressureMeterFactory
	pressureMeterFactory = func(root string) machineMeter {
		return &hookMeter{inner: oldFactory(root), onRead: func(kind string, busy bool) {
			if kind != "projectA:ordinary" || busy {
				return
			}
			once.Do(func() {
				priority = startWait(pctx, "priority", nil, path, settlePath, pastLoadReason, io.Discard)
				_ = awaitGlob(settlePath + ".queue.-*.lticket")
				close(joined)
			})
		}}
	}
	defer func() { pressureMeterFactory = oldFactory }()
	octx, ocancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ocancel()
	waiting := waitLog()
	ordinary := startWait(octx, "ordinary", nil, path, settlePath, "", waiting)
	awaitSignal(t, waiting.seen, "the ordinary settle task to wait for the load")
	setRunnable(1)
	awaitSignal(t, joined, "the priority settle ticket to join")
	out := receiveOutcome(t, priority, 6*time.Second, "the priority settle task")
	if out.err != nil {
		t.Errorf("the priority settle task joining at the ordinary one's admission: %v", out.err)
	}
	notAdmitted(t, ordinary, 200*time.Millisecond, "the ordinary settle task during the priority one's settling period")
	releaseOutcome(t, out)
	o := receiveOutcome(t, ordinary, 10*time.Second, "the ordinary settle task")
	if o.err != nil {
		t.Errorf("the ordinary settle task after the priority one: %v", o.err)
	}
	releaseOutcome(t, o)
}

// TestPriorityBudgetCountsThePauseBeforeTheQueue: a priority task that
// comes back from a pause to a held lock waits for it only for the budget
// the pause left.
func TestPriorityBudgetCountsThePauseBeforeTheQueue(t *testing.T) {
	busyCPU(t)
	path := filepath.Join(t.TempDir(), "load.lock")
	// A pause as long as the budget: refused when the pause ends, at 1s,
	// where a wait that left the pause unspent would end at 2s.
	p := &Priority{For: "projectA deploy", Budget: newBudget(t, time.Second), Fail: true}
	l := testLoad
	l.Poll = time.Second
	waiting := waitLog()
	started := time.Now()
	priority := startWaitLoad(context.Background(), "priority", p, l, path, "", "", waiting)
	awaitSignal(t, waiting.seen, "the priority task to wait for the load")
	var held *lock.Lock
	for deadline := time.Now().Add(time.Second); held == nil && time.Now().Before(deadline); {
		var err error
		if held, err = lock.TryAcquire(path, "projectA old holder"); err != nil {
			t.Fatal(err)
		}
		if held == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if held == nil {
		t.Fatal("the priority task did not give the lock up for its pause")
	}
	defer func() { _ = held.Release() }()
	out := receiveOutcome(t, priority, 5*time.Second, "the priority task")
	elapsed := time.Since(started)
	releaseOutcome(t, out)
	if out.err == nil {
		t.Fatal("the priority task was admitted behind a held lock past its budget")
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("refused after %s on a budget of 1s: the pause was not spent before the wait for the lock (%v)", elapsed, out.err)
	}
}

// TestCancelledResumeKeepsItsQuietWait: an ordinary task cancelled while it
// waits to take the lock back behind a priority ticket in quiet mode counts
// that wait as quiet, not as queue.
func TestCancelledResumeKeepsItsQuietWait(t *testing.T) {
	busyCPU(t)
	path := filepath.Join(t.TempDir(), "load.lock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := waitLog()
	behind := &signalWriter{text: "queue behind", seen: make(chan struct{})}
	ordinary := startWait(ctx, "ordinary", nil, path, "", "", io.MultiWriter(waiting, behind))
	awaitSignal(t, waiting.seen, "the ordinary task to wait for the load")
	ticket, err := lock.JoinFIFO(context.Background(), path, "graft deploy (starting a heavy task)", true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ticket.Leave() }()
	awaitSignal(t, behind.seen, "the ordinary task to wait behind the priority ticket")
	quietFrom := time.Now()
	setQuiet(t, quietFrom.Add(time.Hour))
	stop := time.AfterFunc(500*time.Millisecond, cancel)
	defer stop.Stop()
	out := receiveOutcome(t, ordinary, 5*time.Second, "the cancelled ordinary task")
	quietSpan := time.Since(quietFrom)
	releaseOutcome(t, out)
	if out.err == nil {
		t.Fatal("the cancelled ordinary task was admitted")
	}
	if got := out.adm.Waits[WhyQuiet]; got < quietSpan/2 {
		t.Errorf("quiet wait = %s of a %s quiet wait in the queue (waits %v)", got, quietSpan.Round(time.Millisecond), out.adm.Waits)
	}
}

// TestBusyNowCountsAHeadThatGaveTheLockUp: a queued ticket whose holder gave
// load.lock up for a pause keeps the queue busy on a calm machine.
func TestBusyNowCountsAHeadThatGaveTheLockUp(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	path := filepath.Join(t.TempDir(), "load.lock")
	ticket, err := lock.JoinFIFO(context.Background(), path, "graft head (starting a heavy task)", false)
	if err != nil {
		t.Fatal(err)
	}
	busy, err := BusyNowWithReserves(context.Background(), "next", "projectA:next", testLoad, path, true, io.Discard)
	if err != nil || !busy {
		t.Errorf("busy with a queued head that holds no lock = %v, %v; want busy", busy, err)
	}
	if err := ticket.Leave(); err != nil {
		t.Fatal(err)
	}
	busy, err = BusyNowWithReserves(context.Background(), "next", "projectA:next", testLoad, path, true, io.Discard)
	if err != nil || busy {
		t.Errorf("busy after the head left = %v, %v; want free", busy, err)
	}
}

// TestCancelledAdmissionFreesTheSettleLock: a settle task cancelled at its
// calm reading, with both locks held, gives both up and leaves both queues.
func TestCancelledAdmissionFreesTheSettleLock(t *testing.T) {
	setRunnable := busyCPU(t)
	dir := t.TempDir()
	path, settlePath := filepath.Join(dir, "load.lock"), filepath.Join(dir, "load.settle.lock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	oldFactory := pressureMeterFactory
	pressureMeterFactory = func(root string) machineMeter {
		return &hookMeter{inner: oldFactory(root), onRead: func(kind string, busy bool) {
			if kind == "projectA:ordinary" && !busy {
				cancel()
			}
		}}
	}
	defer func() { pressureMeterFactory = oldFactory }()
	waiting := waitLog()
	ordinary := startWait(ctx, "ordinary", nil, path, settlePath, "", waiting)
	awaitSignal(t, waiting.seen, "the ordinary settle task to wait for the load")
	setRunnable(1)
	out := receiveOutcome(t, ordinary, 5*time.Second, "the cancelled settle task")
	releaseOutcome(t, out)
	if out.err == nil {
		t.Fatal("the settle task cancelled at its calm reading was admitted")
	}
	for _, p := range []string{path, settlePath} {
		if holders, err := lock.Holders(p); err != nil || len(holders) != 0 {
			t.Errorf("holders of %s after the cancelled admission = %v, %v; want none", filepath.Base(p), holders, err)
		}
	}
	assertQueueEmpty(t, path)
	assertQueueEmpty(t, settlePath)
}

// TestExhaustedPriorityRefusedBehindAYieldedPriority: a priority task whose
// budget is spent is refused at once behind a priority head that waits for
// the load, though that head gave the lock up for its pause, in the load
// queue and in the settle queue alike.
func TestExhaustedPriorityRefusedBehindAYieldedPriority(t *testing.T) {
	for _, settle := range []bool{false, true} {
		t.Run(map[bool]string{false: "load", true: "settle"}[settle], func(t *testing.T) {
			busyCPU(t)
			dir := t.TempDir()
			path, settlePath := filepath.Join(dir, "load.lock"), ""
			if settle {
				settlePath = filepath.Join(dir, "load.settle.lock")
			}
			first := &Priority{For: "projectA deploy one", Budget: newBudget(t, time.Minute), Fail: true}
			firstCtx, cancelFirst := context.WithCancel(context.Background())
			defer cancelFirst()
			waiting := waitLog()
			one := startWait(firstCtx, "one", first, path, settlePath, "", waiting)
			awaitSignal(t, waiting.seen, "the first priority task to wait for the load")
			awaitFiles(t, path+".queue.-*.lticket", 1)
			// A refused task does not join a queue: its ticket would make
			// a head that reads the machine let it go first.
			var joined atomic.Int32
			oldJoining := queueJoining
			queueJoining = func(string) { joined.Add(1) }
			t.Cleanup(func() { queueJoining = oldJoining })
			spent := &Priority{For: "projectA deploy two", Budget: newBudget(t, 0), Fail: true}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			started := time.Now()
			var err error
			var release func() error
			if settle {
				_, release, err = WaitWithSettle(ctx, "two", "projectA:two", spent, testLoad, path, settlePath, time.Hour, 1, io.Discard, "")
			} else {
				_, release, err = Wait(ctx, "two", "projectA:two", spent, testLoad, path, io.Discard, "")
			}
			took := time.Since(started)
			if err == nil {
				_ = release()
				t.Fatal("a priority task with a spent budget was admitted behind a priority head")
			}
			if took > time.Second || !strings.Contains(err.Error(), "budget exhausted") || !strings.Contains(err.Error(), "graft one") {
				t.Errorf("refused after %s: %v; want a refusal at once naming the head", took.Round(time.Millisecond), err)
			}
			if n := joined.Load(); n != 0 {
				t.Errorf("the refused task joined a queue %d times; want none", n)
			}
			cancelWaiting(t, cancelFirst, one, "the first priority task")
		})
	}
}

// TestCancelledFirstQueueWaitCounts: a task cancelled in its first wait for
// a held lock, the load lock or the settle lock, counts that wait as the
// queue and names the holder.
func TestCancelledFirstQueueWaitCounts(t *testing.T) {
	for _, settle := range []bool{false, true} {
		t.Run(map[bool]string{false: "load", true: "settle"}[settle], func(t *testing.T) {
			busyCPU(t)
			dir := t.TempDir()
			path, settlePath := filepath.Join(dir, "load.lock"), ""
			held := path
			if settle {
				settlePath = filepath.Join(dir, "load.settle.lock")
				held = settlePath
			}
			holder, err := lock.Acquire(context.Background(), held, time.Second, "projectA holder", io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Release() }()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := time.Now()
			stop := time.AfterFunc(400*time.Millisecond, cancel)
			defer stop.Stop()
			out := receiveOutcome(t, startWait(ctx, "ordinary", nil, path, settlePath, "", io.Discard), 5*time.Second, "the cancelled task")
			span := time.Since(started)
			releaseOutcome(t, out)
			if out.err == nil {
				t.Fatal("the task was admitted past a held lock")
			}
			if got := out.adm.Waits[WhyQueue]; got < span/2 || got > span {
				t.Errorf("queue wait = %s of a %s wait for the held lock (waits %v)", got, span.Round(time.Millisecond), out.adm.Waits)
			}
			if !strings.Contains(out.adm.WaitedBehind, "projectA holder") {
				t.Errorf("waited behind %q; want the holder", out.adm.WaitedBehind)
			}
		})
	}
}

// TestSettleTaskRejoinsAtItsLoadPlace: a settle task that leaves the load
// queue to wait for its settle turn rejoins it at its place, ahead of a task
// that joined meanwhile, and is admitted first.
func TestSettleTaskRejoinsAtItsLoadPlace(t *testing.T) {
	setRunnable := busyCPU(t)
	quickReserves(t)
	dir := t.TempDir()
	path, settlePath := filepath.Join(dir, "load.lock"), filepath.Join(dir, "load.settle.lock")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	waiting := waitLog()
	settler := startWait(ctx, "settler", nil, path, settlePath, "", waiting)
	awaitSignal(t, waiting.seen, "the settle task to wait for the load")
	// A settling period starts while the settle task waits: it comes back
	// from its pause to a held settle lock and leaves the load queue.
	authorizedCtx, cancelAuthorized := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelAuthorized()
	_, releaseAuthorized, err := WaitWithSettle(authorizedCtx, "authorized", "projectA:authorized", nil, testLoad, path, settlePath, time.Hour, 1, io.Discard, pastLoadReason)
	if err != nil {
		t.Fatal(err)
	}
	authorizedReleased := false
	defer func() {
		if !authorizedReleased {
			_ = releaseAuthorized()
		}
	}()
	awaitNoFiles(t, path+".queue.*.lticket")
	later := startWait(ctx, "later", nil, path, "", "", io.Discard)
	awaitFiles(t, path+".queue.*.lticket", 1)
	authorizedReleased = true
	if err := releaseAuthorized(); err != nil {
		t.Fatal(err)
	}
	awaitFiles(t, path+".queue.*.lticket", 2)
	queued, err := lock.Queue(context.Background(), path)
	if err != nil || len(queued) != 2 || !strings.HasPrefix(queued[0], "graft settler") {
		t.Errorf("load queue after the settle task's return = %v, %v; want it at its place, ahead of the later task", queued, err)
	}
	setRunnable(1)
	first, second := admissionOrder(t, settler, later)
	defer releaseOutcome(t, first)
	defer releaseOutcome(t, second)
	if first.name != "settler" {
		t.Errorf("first admission = %s; want the settle task, queued first", first.name)
	}
}

// awaitNoFiles waits until no file matches pattern.
func awaitNoFiles(t *testing.T, pattern string) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("files matching %s stay: %v", pattern, matches)
		}
	}
}

// TestSettleOrderKeptAcrossTheSettleWait: an ordinary settle task B waits
// for the load; a --past-load settle task P passes it and settles; an
// ordinary settle task C joins after B. B is still admitted before C.
func TestSettleOrderKeptAcrossTheSettleWait(t *testing.T) {
	setRunnable := busyCPU(t)
	quickReserves(t)
	dir := t.TempDir()
	path, settlePath := filepath.Join(dir, "load.lock"), filepath.Join(dir, "load.settle.lock")
	l := testLoad
	l.Poll = 30 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	run := func(name, past string, d time.Duration, log io.Writer) <-chan waitOutcome {
		done := make(chan waitOutcome, 1)
		go func() {
			var out waitOutcome
			out.name = name
			out.adm, out.release, out.err = WaitWithSettle(ctx, name, "projectA:"+name, nil, l, path, settlePath, d, 1, log, past)
			done <- out
		}()
		return done
	}
	bw := waitLog()
	b := run("B", "", 50*time.Millisecond, bw)
	awaitSignal(t, bw.seen, "B to wait")
	p := run("P", pastLoadReason, 400*time.Millisecond, io.Discard)
	po := receiveOutcome(t, p, 5*time.Second, "P")
	if po.err != nil {
		t.Fatal(po.err)
	}
	// B comes back, finds P settling and leaves the load queue for its
	// settle turn.
	awaitNoFiles(t, path+".queue.*.lticket")
	c := run("C", "", 50*time.Millisecond, io.Discard)
	awaitFiles(t, settlePath+".queue.*.lticket", 2)
	setRunnable(1)
	// P's settling ends by its timer (400ms); release later.
	first, second := admissionOrder(t, b, c)
	if first.name != "B" {
		t.Errorf("C, joined after B, admitted first")
	}
	releaseOutcome(t, po)
	releaseOutcome(t, first)
	releaseOutcome(t, second)
}

// TestSpentPriorityTakesFreeLocks: a priority task whose budget is spent
// takes the load lock, and the settle lock beside it, when neither would
// wait: on a calm machine with empty queues it is admitted and holds the
// settle lock for its settling period.
func TestSpentPriorityTakesFreeLocks(t *testing.T) {
	for _, settle := range []bool{false, true} {
		t.Run(map[bool]string{false: "load", true: "settle"}[settle], func(t *testing.T) {
			setRunnable, _, _ := fakeMachine(t)
			setRunnable(1)
			dir := t.TempDir()
			path, settlePath := filepath.Join(dir, "load.lock"), ""
			if settle {
				settlePath = filepath.Join(dir, "load.settle.lock")
			}
			p := &Priority{For: "projectA deploy", Budget: newBudget(t, 0), Fail: true}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			out := receiveOutcome(t, startWait(ctx, "spent", p, path, settlePath, "", io.Discard), 5*time.Second, "the spent priority task")
			if out.err != nil {
				t.Fatalf("a spent priority task on a calm machine with free locks: %v", out.err)
			}
			if settle {
				holders, err := lock.Holders(settlePath)
				if err != nil || len(holders) != 1 || !strings.Contains(holders[0], "graft spent") {
					t.Errorf("settle lock holders after the admission = %v, %v; want the admitted task", holders, err)
				}
			}
			releaseOutcome(t, out)
			assertQueueEmpty(t, path)
		})
	}
}

// TestSpentPriorityRefusedForATakenSettleLock: a spent priority settle task
// that holds the load lock and finds the settle lock taken beside it is
// refused at once and gives the load lock up: the settle lock would wait.
func TestSpentPriorityRefusedForATakenSettleLock(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	dir := t.TempDir()
	path, settlePath := filepath.Join(dir, "load.lock"), filepath.Join(dir, "load.settle.lock")
	taken := flock.New(settlePath)
	var takeErr error
	oldJoining := queueJoining
	queueJoining = func(at string) {
		if at == path {
			_, takeErr = taken.TryLock()
		}
	}
	t.Cleanup(func() { queueJoining = oldJoining; _ = taken.Unlock() })
	p := &Priority{For: "projectA deploy", Budget: newBudget(t, 0), Fail: true}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	started := time.Now()
	out := receiveOutcome(t, startWait(ctx, "spent", p, path, settlePath, "", io.Discard), 5*time.Second, "the spent priority task")
	took := time.Since(started)
	releaseOutcome(t, out)
	if takeErr != nil || !taken.Locked() {
		t.Fatalf("the settle lock was not taken: %v", takeErr)
	}
	if out.err == nil {
		t.Fatal("a spent priority task was admitted without the settle lock")
	}
	if took > time.Second || !strings.Contains(out.err.Error(), "budget exhausted") {
		t.Errorf("refused after %s: %v; want a budget refusal at once", took.Round(time.Millisecond), out.err)
	}
	assertQueueEmpty(t, path)
	if left, _ := filepath.Glob(settlePath + ".queue.*.lticket"); len(left) != 0 {
		t.Errorf("settle tickets left: %v", left)
	}
}

// TestSpentPriorityRefusedForAPriorityAheadAtItsAdmission: a spent priority
// task that a priority ticket passes at its calm reading, one that rejoins
// at the place it left for its settle turn, is refused at once when it comes
// to take the load lock back: that lock would wait.
func TestSpentPriorityRefusedForAPriorityAheadAtItsAdmission(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	path := filepath.Join(t.TempDir(), "load.lock")
	left, err := lock.JoinFIFO(context.Background(), path, "projectA other", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := left.Leave(); err != nil {
		t.Fatal(err)
	}
	var other *lock.Ticket
	var joinErr error
	var once sync.Once
	oldFactory := pressureMeterFactory
	pressureMeterFactory = func(root string) machineMeter {
		return &hookMeter{inner: oldFactory(root), onRead: func(kind string, busy bool) {
			if kind == "projectA:spent" && !busy {
				once.Do(func() {
					other, joinErr = left.Rejoin(context.Background())
				})
			}
		}}
	}
	t.Cleanup(func() { pressureMeterFactory = oldFactory })
	p := &Priority{For: "projectA deploy", Budget: newBudget(t, 0), Fail: true}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	started := time.Now()
	out := receiveOutcome(t, startWait(ctx, "spent", p, path, "", "", io.Discard), 5*time.Second, "the spent priority task")
	took := time.Since(started)
	releaseOutcome(t, out)
	if joinErr != nil || other == nil {
		t.Fatalf("the priority ticket did not rejoin: %v", joinErr)
	}
	defer func() { _ = other.Leave() }()
	if out.err == nil {
		t.Fatal("a spent priority task was admitted with a priority ticket ahead")
	}
	if took > time.Second || !strings.Contains(out.err.Error(), "budget exhausted") || !strings.Contains(out.err.Error(), "queued behind projectA other") {
		t.Errorf("refused after %s: %v; want a budget refusal at once naming the ticket ahead", took.Round(time.Millisecond), out.err)
	}
	holders, err := lock.Holders(path)
	if err != nil || len(holders) != 0 {
		t.Errorf("load lock holders after the refusal = %v, %v; want none", holders, err)
	}
	if queued, err := lock.Queue(context.Background(), path); err != nil || len(queued) != 1 {
		t.Errorf("queue after the refusal = %v, %v; want only the priority ticket", queued, err)
	}
}

// TestSpentPriorityPassesAPausingOrdinaryHead: a priority task whose budget
// is spent goes ahead of an ordinary head that gave the lock up for its
// pause: only a holder or a priority ticket ahead would make it wait.
func TestSpentPriorityPassesAPausingOrdinaryHead(t *testing.T) {
	setRunnable := busyCPU(t)
	path := filepath.Join(t.TempDir(), "load.lock")
	l := testLoad
	l.Poll = 3 * time.Second
	ordinaryCtx, cancelOrdinary := context.WithCancel(context.Background())
	defer cancelOrdinary()
	waiting := waitLog()
	ordinary := startWaitLoad(ordinaryCtx, "ordinary", nil, l, path, "", "", waiting)
	awaitSignal(t, waiting.seen, "the ordinary task to wait for the load")
	awaitFiles(t, path+".queue.*.lticket", 1)
	// It logs the wait before it gives the lock up for the pause.
	awaitNoHolder(t, path)
	setRunnable(1)
	p := &Priority{For: "projectA deploy", Budget: newBudget(t, 0), Fail: true}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, release, err := Wait(ctx, "spent", "projectA:spent", p, testLoad, path, io.Discard, "")
	if err != nil {
		t.Fatalf("a spent priority task behind a pausing ordinary head on a calm machine: %v", err)
	}
	if err := release(); err != nil {
		t.Error(err)
	}
	cancelWaiting(t, cancelOrdinary, ordinary, "the ordinary task")
}

// TestSpentPriorityRefusedByATicketJoiningAsItJoins: a priority ticket that
// joins the load or settle queue after a spent priority task found nothing
// ahead, and before its own ticket joined, refuses it at once: with its
// budget spent the task does not wait for the lock at all.
func TestSpentPriorityRefusedByATicketJoiningAsItJoins(t *testing.T) {
	for _, settle := range []bool{false, true} {
		t.Run(map[bool]string{false: "load", true: "settle"}[settle], func(t *testing.T) {
			setRunnable, _, _ := fakeMachine(t)
			setRunnable(1)
			dir := t.TempDir()
			path, settlePath := filepath.Join(dir, "load.lock"), ""
			raced := path
			if settle {
				settlePath = filepath.Join(dir, "load.settle.lock")
				raced = settlePath
			}
			var other *lock.Ticket
			var joinErr error
			var once atomic.Bool
			oldJoining := queueJoining
			queueJoining = func(at string) {
				if at == raced && once.CompareAndSwap(false, true) {
					other, joinErr = lock.JoinFIFO(context.Background(), raced, "projectA other", true)
				}
			}
			t.Cleanup(func() { queueJoining = oldJoining })
			p := &Priority{For: "projectA deploy", Budget: newBudget(t, 0), Fail: true}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			started := time.Now()
			out := receiveOutcome(t, startWait(ctx, "spent", p, path, settlePath, "", io.Discard), 5*time.Second, "the spent priority task")
			took := time.Since(started)
			releaseOutcome(t, out)
			if joinErr != nil || other == nil {
				t.Fatalf("the racing ticket did not join: %v", joinErr)
			}
			defer func() { _ = other.Leave() }()
			if out.err == nil {
				t.Fatal("a spent priority task was admitted behind a priority ticket that joined ahead of it")
			}
			if took > time.Second || !strings.Contains(out.err.Error(), "budget exhausted") || !strings.Contains(out.err.Error(), "projectA other") {
				t.Errorf("refused after %s: %v; want a refusal at once naming the ticket ahead", took.Round(time.Millisecond), out.err)
			}
			queued, err := lock.Queue(context.Background(), raced)
			if err != nil || len(queued) != 1 || queued[0] != "projectA other" && !strings.HasPrefix(queued[0], "projectA other,") {
				t.Errorf("queue after the refusal = %v, %v; want only the racing ticket", queued, err)
			}
		})
	}
}

// TestBusyNowForPriorityPassesOrdinaryTickets: for a priority task the load
// queue is busy with a holder or a priority ticket, not with an ordinary
// ticket it would go ahead of.
func TestBusyNowForPriorityPassesOrdinaryTickets(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	path := filepath.Join(t.TempDir(), "load.lock")
	check := func(what string, want bool) {
		t.Helper()
		busy, err := BusyNowForPriority(context.Background(), "next", "projectA:next", testLoad, path, io.Discard)
		if err != nil || busy != want {
			t.Errorf("priority busy with %s = %v, %v; want %v", what, busy, err, want)
		}
	}
	holder, err := lock.Acquire(context.Background(), path, time.Second, "projectA holder", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	check("a holder", true)
	if err := holder.Release(); err != nil {
		t.Fatal(err)
	}
	check("a free queue", false)
	ordinary, err := lock.JoinFIFO(context.Background(), path, "graft ordinary (starting a heavy task)", false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ordinary.Leave() }()
	check("an ordinary ticket queued", false)
	if busy, err := BusyNowWithReserves(context.Background(), "next", "projectA:next", testLoad, path, true, io.Discard); err != nil || !busy {
		t.Errorf("ordinary busy with an ordinary ticket queued = %v, %v; want busy", busy, err)
	}
	priority, err := lock.JoinFIFO(context.Background(), path, "graft deploy (starting a heavy task)", true)
	if err != nil {
		t.Fatal(err)
	}
	check("a priority ticket queued", true)
	if err := priority.Leave(); err != nil {
		t.Fatal(err)
	}
	check("the ordinary ticket only", false)
}

// TestSpentPriorityRefusedForAHeldQueueGuard: a priority task whose budget is
// spent waits for a queue's guard no longer than guardWait and is refused
// then, naming the guard, with no lock and no ticket left: the guard of the
// load queue or of the settle queue held as it checks the queue, the settle
// queue's as it takes the settle lock beside the load lock, the load queue's
// at its admission.
func TestSpentPriorityRefusedForAHeldQueueGuard(t *testing.T) {
	for _, held := range []string{"load", "settle-queue", "settle", "admission"} {
		t.Run(held, func(t *testing.T) {
			setRunnable, _, _ := fakeMachine(t)
			setRunnable(1)
			dir := t.TempDir()
			path, settlePath := filepath.Join(dir, "load.lock"), ""
			if strings.HasPrefix(held, "settle") {
				settlePath = filepath.Join(dir, "load.settle.lock")
			}
			guarded := map[string]string{"load": path, "settle-queue": settlePath, "settle": settlePath, "admission": path}[held]
			guard := flock.New(guarded + ".queue.lock")
			var takeErr error
			var once sync.Once
			hold := func() { once.Do(func() { _, takeErr = guard.TryLock() }) }
			t.Cleanup(func() { _ = guard.Unlock() })
			switch held {
			case "load", "settle-queue":
				hold()
			case "settle":
				oldJoining := queueJoining
				queueJoining = func(at string) {
					if at == path {
						hold()
					}
				}
				t.Cleanup(func() { queueJoining = oldJoining })
			case "admission":
				oldFactory := pressureMeterFactory
				pressureMeterFactory = func(root string) machineMeter {
					return &hookMeter{inner: oldFactory(root), onRead: func(kind string, busy bool) {
						if kind == "projectA:spent" && !busy {
							hold()
						}
					}}
				}
				t.Cleanup(func() { pressureMeterFactory = oldFactory })
			}
			p := &Priority{For: "projectA deploy", Budget: newBudget(t, 0), Fail: true}
			ctx, cancel := context.WithTimeout(context.Background(), 4*guardWait)
			defer cancel()
			started := time.Now()
			out := receiveOutcome(t, startWait(ctx, "spent", p, path, settlePath, "", io.Discard), 5*guardWait, "the spent priority task")
			took := time.Since(started)
			releaseOutcome(t, out)
			if takeErr != nil || !guard.Locked() {
				t.Fatalf("the queue guard was not held: %v", takeErr)
			}
			if out.err == nil {
				t.Fatal("a spent priority task was admitted past a held queue guard")
			}
			if took > guardWait+guardWait/2 || !strings.Contains(out.err.Error(), "budget exhausted") || !strings.Contains(out.err.Error(), "queue guard") {
				t.Errorf("refused after %s: %v; want a budget refusal naming the queue guard within %s", took.Round(time.Millisecond), out.err, guardWait)
			}
			if named := "queue guard of " + filepath.Base(guarded); held != "admission" && !strings.Contains(out.err.Error(), named) {
				t.Errorf("refusal %v; want it to name the %s", out.err, named)
			}
			if err := guard.Unlock(); err != nil {
				t.Fatal(err)
			}
			assertQueueEmpty(t, path)
			if settlePath != "" {
				assertQueueEmpty(t, settlePath)
			}
		})
	}
}

// awaitNoHolder waits until nothing holds the lock at path.
func awaitNoHolder(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		holders, err := lock.Holders(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(holders) == 0 {
			return
		}
	}
	t.Fatalf("%s still held", filepath.Base(path))
}
