// Package loadgate holds a heavy task back while the machine is busy: the
// laptop is shared, and builds and test runs started at once by several
// sessions drive the load, the disk and the swap past what anyone can work
// with.
package loadgate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aiseeq/akit/pressure"
	"github.com/gofrs/flock"

	"github.com/aiseeq/graft/v2/internal/lock"
	"github.com/aiseeq/graft/v2/internal/stats"
	"github.com/aiseeq/graft/v2/internal/userconfig"
)

// queueTimeout bounds the wait for the queue; Ctrl-C stops it sooner.
const queueTimeout = 24 * time.Hour

// Why a heavy task waited, the keys of Waits.
const (
	// WhyQueue is the wait behind other heavy tasks for the queue's lock.
	WhyQueue = "queue"
	// WhyLoad, WhyIO, WhyWriteback and WhySwap are the waits for the load,
	// the IO pressure, the dirty and writeback memory and the swapping to
	// fall below their limits.
	WhyLoad      = "load"
	WhyIO        = "io"
	WhyWriteback = "writeback"
	WhySwap      = "swap"
	// WhyReserve is waiting for live expected-core reservations, and for the
	// admitted tasks the load average does not show yet.
	WhyReserve = "reserve"
	// WhyQuiet is the wait for the quiet mode akit's busy --quiet-until
	// sets to end.
	WhyQuiet = "quiet"
)

// Waits is how long a heavy task waited, by why.
type Waits map[string]time.Duration

// Admission is what a heavy task's admission saw: how long it waited, by
// why.
type Admission struct {
	Waits            Waits
	WaitedBehind     string
	ReserveCores     float64
	ReserveSince     time.Time
	ReserveEnd       func() string
	ReserveStoppedAt func() time.Time
}

// Waited reports whether admission waited for a busy machine or another
// queued task, excluding the time spent checking an available queue.
func (a Admission) Waited() bool {
	if a.WaitedBehind != "" {
		return true
	}
	for why, d := range a.Waits {
		if why != WhyQueue && d > 0 {
			return true
		}
	}
	return false
}

type reserveData struct {
	Cores         float64 `json:"cores"`
	ObservedCores float64 `json:"observed_cores,omitempty"`
	ObservedAt    string  `json:"observed_at,omitempty"`
	Admitted      string  `json:"admitted"`
	Task          string  `json:"task"`
}

var reservePollInterval = 250 * time.Millisecond
var reserveWindow = 2 * time.Second
var reserveCPUReader = treeCPU

// reserveAgeNow is overridden by tests to hold the admitted-core forecast at
// a fixed age; production uses the wall clock.
var reserveAgeNow = time.Now
var pressureMeterFactory = func(root string) machineMeter { return pressure.NewMeter(root) }
var loadLockAcquired = func(string) {}
var loadLockBeforeRelease = func() {}
var loadLockRelease = func(lk *lock.Lock) error { return lk.Release() }

func reserveEstimateChanged(old, next float64) bool {
	delta := math.Abs(next - old)
	scale := max(math.Abs(old), math.Abs(next))
	return delta >= 0.5 || scale > 0 && delta/scale >= 0.25
}

// Total is the whole wait.
func (w Waits) Total() time.Duration {
	var total time.Duration
	for _, d := range w {
		total += d
	}
	return total
}

// The environment of a priority: graft deploy sets it for itself and every
// graft its script starts.
const (
	// EnvPriority names what the heavy tasks run for.
	EnvPriority = "GRAFT_LOAD_PRIORITY"
	// EnvBudget is the file of what is left of the deploy's budget for
	// waiting (NewBudget).
	EnvBudget = "GRAFT_LOAD_BUDGET"
	// EnvThen names the budget exhaustion behavior; start is treated as fail.
	EnvThen = "GRAFT_LOAD_THEN"
)

// Then values accepted from deploy configuration.
const (
	ThenStart = "start"
	ThenFail  = "fail"
)

// Priority is what a deploy gives its heavy tasks and those of grafts its
// script starts: they queue ahead of ordinary tickets and share one wait budget for
// the whole deploy, kept in Budget. Exhausting it refuses the task.
type Priority struct {
	For    string
	Budget string
	Fail   bool
}

// Env is the priority as the variables its grafts read.
func (p Priority) Env() map[string]string {
	return map[string]string{EnvPriority: p.For, EnvBudget: p.Budget, EnvThen: ThenFail}
}

// PriorityFromEnv reads the priority of this process; ok is false without
// one. A priority without its budget file is an error.
func PriorityFromEnv() (p Priority, ok bool, err error) {
	name := os.Getenv(EnvPriority)
	if name == "" {
		return Priority{}, false, nil
	}
	budget := os.Getenv(EnvBudget)
	if budget == "" {
		return Priority{}, false, fmt.Errorf("%s=%s needs %s, the file of its budget for waiting", EnvPriority, name, EnvBudget)
	}
	p = Priority{For: name, Budget: budget}
	switch then := os.Getenv(EnvThen); then {
	case ThenStart:
		p.Fail = true
		fmt.Fprintf(os.Stderr, "graft: warning: %s=start no longer starts over the limit (user rule); treated as fail\n", EnvThen)
	case ThenFail:
		p.Fail = true
	default:
		return Priority{}, false, fmt.Errorf("%s: want %s or %s, got %q", EnvThen, ThenStart, ThenFail, then)
	}
	return p, true, nil
}

// NewBudget writes a budget of d for waiting into a new file in dir and
// returns its path; the deploy removes it when it ends.
func NewBudget(dir string, d time.Duration) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("the budget for waiting: %w", err)
	}
	f, err := os.CreateTemp(dir, "load-budget-*")
	if err != nil {
		return "", fmt.Errorf("the budget for waiting: %w", err)
	}
	_, err = f.WriteString(strconv.FormatInt(int64(d), 10))
	if err = errors.Join(err, f.Close()); err != nil {
		return "", fmt.Errorf("the budget for waiting: %w", err)
	}
	return f.Name(), nil
}

// spend takes the part of [from, to] not already counted from the shared wall-time budget.
// Empty endpoints read the remaining budget without changing its watermark.
func (p *Priority) spend(from, to time.Time) (left time.Duration, err error) {
	fl := flock.New(p.Budget + ".lock")
	if err := fl.Lock(); err != nil {
		return 0, fmt.Errorf("locking the budget for waiting %s: %w", p.Budget, err)
	}
	defer func() { err = errors.Join(err, fl.Unlock()) }()
	data, err := os.ReadFile(p.Budget)
	if err != nil {
		return 0, fmt.Errorf("the budget for waiting of %s: %w", p.For, err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("the budget for waiting %s: %q is not a number of nanoseconds", p.Budget, data)
	}
	left = time.Duration(n)
	if from.IsZero() && to.IsZero() {
		return left, nil
	}
	from, to = time.Unix(0, from.UnixNano()), time.Unix(0, to.UnixNano())
	untilPath := p.Budget + ".until"
	untilData, readErr := os.ReadFile(untilPath)
	var until time.Time
	haveUntil := readErr == nil
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return 0, fmt.Errorf("reading the budget wall-time mark %s: %w", untilPath, readErr)
	}
	if haveUntil {
		ns, err := strconv.ParseInt(strings.TrimSpace(string(untilData)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("budget wall-time mark %s: %q is not Unix nanoseconds", untilPath, untilData)
		}
		until = time.Unix(0, ns)
	}
	if haveUntil && until.Sub(to) >= time.Minute {
		until, haveUntil = to, true
		left -= max(to.Sub(from), 0)
	} else {
		accountedFrom := from
		if haveUntil && until.After(accountedFrom) {
			accountedFrom = until
		}
		left -= max(to.Sub(accountedFrom), 0)
		if !haveUntil || to.After(until) {
			until, haveUntil = to, true
		}
	}
	if err := os.WriteFile(untilPath, []byte(strconv.FormatInt(until.UnixNano(), 10)), 0o644); err != nil {
		return 0, fmt.Errorf("writing the budget wall-time mark %s: %w", untilPath, err)
	}
	if err := os.WriteFile(p.Budget, []byte(strconv.FormatInt(int64(max(left, 0)), 10)), 0o644); err != nil {
		return 0, fmt.Errorf("the budget for waiting: %w", err)
	}
	return left, nil
}

// Wait admits a heavy task and returns how long it waited and why, and the
// release the task calls when it ends. Tasks queue under lockPath while
// measuring the machine and writing their live expected-core reservation.
// A priority task uses the same FIFO queue and spends its shared wait budget.
//
// kind is the task's kind as the stats profile names it.
func Wait(ctx context.Context, name, kind string, p *Priority, l userconfig.MachineLoad, lockPath string, log io.Writer) (Admission, func() error, error) {
	return WaitWithSettle(ctx, name, kind, p, l, lockPath, "", 0, 0, log)
}

// WaitWithSettle admits a heavy task and, when settleLockPath is set,
// serializes tasks that requested their own settling period on that lock.
// The ordinary queue writes a live reservation before releasing its lock:
// cores when positive, else the profile's estimate.
func WaitWithSettle(ctx context.Context, name, kind string, p *Priority, l userconfig.MachineLoad, lockPath, settleLockPath string, settleDuration time.Duration, cores float64, log io.Writer) (admission Admission, release func() error, retErr error) {
	m := newMachine(l, log, name, kind)
	priority := p != nil
	defer func() {
		if retErr != nil {
			m.recordRefused(retErr.Error(), admission, priority)
		} else {
			m.recordAdmitted(admission, priority)
		}
	}()
	holder := "graft " + name + " (starting a heavy task)"
	settleCtx := ctx
	var cancel context.CancelFunc
	if p != nil {
		if left, budgetErr := p.spend(time.Time{}, time.Time{}); budgetErr != nil {
			return m.admission(), released, budgetErr
		} else if left <= 0 && settleLockPath != "" && settleDuration > 0 {
			holders, holderErr := lock.Holders(settleLockPath)
			if holderErr != nil {
				return m.admission(), released, holderErr
			}
			if len(holders) > 0 {
				err := m.priorityFailure(ctx, p, time.Time{}, time.Time{})
				return m.admission(), released, err
			}
		} else if left > 0 {
			settleCtx, cancel = context.WithTimeout(ctx, left)
			defer cancel()
		}
	}
	settleLock, behind, start, err := acquireSettle(settleCtx, m, p, settleLockPath, settleDuration, holder, log)
	if err != nil {
		if p != nil && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			err = m.priorityFailure(ctx, p, start, time.Now())
		}
		return m.admission(), released, err
	}
	if p != nil && behind != "" {
		left, spendErr := p.spend(start, time.Now())
		if spendErr != nil {
			return m.admission(), released, errors.Join(spendErr, settleLock.Release())
		}
		if left <= 0 {
			err := m.priorityFailure(ctx, p, time.Time{}, time.Time{})
			return m.admission(), released, errors.Join(err, settleLock.Release())
		}
	}
	if cancel != nil {
		cancel()
	}
	return waitQueued(ctx, m, p, kind, l, lockPath, cores, settleLock, settleDuration, start, behind, log)
}

func acquireSettle(ctx context.Context, m *machine, p *Priority, settleLockPath string, settleDuration time.Duration, holder string, log io.Writer) (*lock.Lock, string, time.Time, error) {
	start := time.Now()
	if settleLockPath == "" || settleDuration <= 0 {
		return nil, "", start, nil
	}
	var settleLock *lock.Lock
	var err error
	if p != nil {
		settleLock, err = lock.AcquireFIFOPriorityObserved(ctx, settleLockPath, queueTimeout, holder+" with settle", log, nil)
	} else {
		settleLock, err = lock.AcquireFIFOObserved(ctx, settleLockPath, queueTimeout, holder+" with settle", log, nil)
	}
	if err != nil {
		return nil, "", start, err
	}
	if settleLock.WaitedBehind != "" {
		m.waits[WhyQueue] += time.Since(start)
	}
	return settleLock, settleLock.WaitedBehind, start, nil
}

func priorityQueueContext(ctx context.Context, m *machine, p *Priority, lockPath string, behind string) (context.Context, context.CancelFunc, error) {
	if p == nil {
		return ctx, func() {}, nil
	}
	left, err := p.spend(time.Time{}, time.Time{})
	if err != nil {
		return ctx, func() {}, err
	}
	if left <= 0 {
		holders, err := lock.Holders(lockPath)
		if err != nil {
			return ctx, func() {}, err
		}
		if len(holders) > 0 || behind != "" {
			return ctx, func() {}, m.priorityFailure(ctx, p, time.Time{}, time.Time{})
		}
		return ctx, func() {}, nil
	}
	queueCtx, cancel := context.WithTimeout(ctx, left)
	return queueCtx, cancel, nil
}

func queuedFailure(m *machine, behind string, err error, settleLock, queueLock *lock.Lock) (Admission, func() error, error) {
	if settleLock != nil {
		err = errors.Join(err, settleLock.Release())
	}
	if queueLock != nil {
		err = errors.Join(err, queueLock.Release())
	}
	return m.behind(behind), released, err
}

func reserveQueued(ctx context.Context, m *machine, p *Priority, kind string, l userconfig.MachineLoad, lockPath string, cores float64, behind string, log io.Writer) (Admission, func() error, error) {
	cores, err := expectedReserveCores(kind, cores)
	if err != nil {
		return Admission{}, nil, err
	}
	m.candidate = cores
	m.candidateKnown = true
	if err := m.admitWithReserves(ctx, p, lockPath); err != nil {
		return Admission{}, nil, err
	}
	release, since, end, stoppedAt, err := startAdmission(lockPath, m.name, cores, l.Settle, log)
	if err != nil {
		return Admission{}, nil, err
	}
	adm := m.behind(behind)
	adm.ReserveCores, adm.ReserveSince, adm.ReserveEnd, adm.ReserveStoppedAt = cores, since, end, stoppedAt
	return adm, release, nil
}

func acquireLoadQueue(ctx context.Context, m *machine, p *Priority, lockPath string, start time.Time, behind string, log io.Writer) (*lock.Lock, string, bool, error) {
	quiet, err := quietMode()
	if err != nil {
		return nil, behind, false, err
	}
	queueCtx, cancel, err := priorityQueueContext(ctx, m, p, lockPath, behind)
	if err != nil {
		return nil, behind, false, err
	}
	defer cancel()
	holder := "graft " + m.name + " (starting a heavy task)"
	var lk *lock.Lock
	if p != nil {
		lk, err = lock.AcquireFIFOPriorityObserved(queueCtx, lockPath, queueTimeout, holder, log, quiet)
	} else {
		lk, err = lock.AcquireFIFOObserved(queueCtx, lockPath, queueTimeout, holder, log, quiet)
	}
	if lk != nil && (lk.WaitedBehind != "" || lk.WaitedQuiet > 0) {
		waited := time.Since(start)
		if lk.WaitedQuiet > 0 {
			m.waits[WhyQuiet] += min(lk.WaitedQuiet, waited)
		}
		m.waits[WhyQueue] += max(0, waited-m.waits[WhyQuiet]-m.waits[WhyQueue])
	}
	if lk != nil && lk.WaitedBehind != "" {
		behind = lk.WaitedBehind
	}
	if lk != nil {
		loadLockAcquired(lockPath)
	}
	return lk, behind, lk != nil && (lk.WaitedBehind != "" || lk.WaitedQuiet > 0), err
}

func waitQueued(ctx context.Context, m *machine, p *Priority, kind string, l userconfig.MachineLoad, lockPath string, cores float64, settleLock *lock.Lock, settleDuration time.Duration, start time.Time, behind string, log io.Writer) (Admission, func() error, error) {
	lk, behind, queueWaited, err := acquireLoadQueue(ctx, m, p, lockPath, start, behind, log)
	if err != nil {
		if p != nil && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			err = m.priorityFailure(ctx, p, start, time.Now())
		}
		return queuedFailure(m, behind, err, settleLock, nil)
	}
	if p != nil && queueWaited {
		left, spendErr := p.spend(start, time.Now())
		if spendErr != nil {
			return queuedFailure(m, behind, spendErr, settleLock, lk)
		}
		if left <= 0 && (behind != "" || lk.WaitedBehind != "" || lk.WaitedQuiet > 0) {
			err := m.priorityFailure(ctx, p, time.Time{}, time.Time{})
			return queuedFailure(m, behind, err, settleLock, lk)
		}
	}
	adm, releaseFn, err := reserveQueued(ctx, m, p, kind, l, lockPath, cores, behind, log)
	if err != nil {
		return queuedFailure(m, behind, err, settleLock, lk)
	}
	if err := loadLockRelease(lk); err != nil {
		return queuedFailure(m, behind, errors.Join(err, releaseFn()), settleLock, nil)
	}
	if settleLock != nil {
		settleRelease := settle(settleLock, settleDuration)
		return adm, func() error { return errors.Join(releaseFn(), settleRelease()) }, nil
	}
	return adm, releaseFn, nil
}

// admitWithReserves waits for the machine to be calm and to have room beside
// the reserves at lockPath and the admitted tasks the load average does not
// show yet; a priority task waits no longer than its budget allows, spending
// it as it waits. The time between two readings goes to what the first of
// them found busy, the first reading's own to what it found.
func (m *machine) admitWithReserves(ctx context.Context, p *Priority, lockPath string) error {
	start, mark := m.now(), m.now()
	spent := start
	var said time.Time
	waited := false
	var why string
	for {
		b, err := m.measureOrReserve(ctx, !waited, lockPath)
		now := m.now()
		if why == "" {
			why = b.why
		}
		if why != "" {
			m.waits[why] += now.Sub(mark)
		}
		mark, why = now, b.why
		if err != nil {
			return err
		}
		if p != nil && waited {
			var finished bool
			spent, finished, err = m.spendPriorityWait(p, spent)
			if err != nil {
				return err
			}
			if finished {
				return m.priorityExhausted()
			}
		}
		if b.why == "" {
			if waited {
				fmt.Fprintf(m.log, "graft: %s: runnable %.0f, starting after %s\n", m.name, m.runnable, m.now().Sub(start).Round(time.Second))
			}
			return nil
		}
		// The measure or the reserves beside it: either keeps the task waiting.
		m.recordWait(b, p != nil)
		waited = true
		if p != nil {
			var finished bool
			spent, finished, err = m.spendPriorityWait(p, spent)
			if err != nil {
				return err
			}
			if finished {
				return m.priorityExhausted()
			}
		}
		m.waits[b.why] += 0
		why = b.why
		if m.now().Sub(said) >= time.Minute {
			fmt.Fprintf(m.log, "graft: %s: %s, waiting\n", m.name, b.text)
			said = m.now()
		}
		pause, err := m.reserveWaitPause(ctx, b, p)
		if err != nil {
			return err
		}
		if err := m.pause(ctx, pause); err != nil {
			if why != "" {
				m.waits[why] += m.now().Sub(mark)
			}
			return err
		}
	}
}

func (m *machine) measureOrReserve(ctx context.Context, fresh bool, lockPath string) (busyness, error) {
	b, err := m.busy(ctx, fresh)
	if err != nil || b.why != "" {
		return b, err
	}
	return m.reservesBusy(lockPath)
}

func (m *machine) spendPriorityWait(p *Priority, spent time.Time) (time.Time, bool, error) {
	now := m.now()
	left, err := p.spend(spent, now)
	if err != nil {
		return now, false, err
	}
	if left > 0 {
		return now, false, nil
	}
	return now, true, nil
}

func (m *machine) priorityExhausted() error {
	return fmt.Errorf("deploy.load_wait: budget exhausted; load average 1m %.2f, runnable %.0f, limit %.0f; %s not started", m.avg1, m.runnable, m.runnableLimit, m.name)
}

func (m *machine) priorityFailure(ctx context.Context, p *Priority, from, to time.Time) error {
	if !from.IsZero() {
		if _, err := p.spend(from, to); err != nil {
			return err
		}
	}
	if _, err := m.busy(ctx, true); err != nil {
		return errors.Join(m.priorityExhausted(), err)
	}
	return m.priorityExhausted()
}

func (m *machine) reserveWaitPause(ctx context.Context, b busyness, p *Priority) (time.Duration, error) {
	pause := m.l.Poll
	if !b.retryAt.IsZero() {
		pause = min(pause, max(time.Until(b.retryAt), 0))
	}
	if deadline, ok := ctx.Deadline(); ok {
		pause = min(pause, max(time.Until(deadline), 0))
	}
	if p != nil {
		left, err := p.spend(time.Time{}, time.Time{})
		if err != nil {
			return 0, err
		}
		pause = min(pause, left)
	}
	return pause, nil
}

func (m *machine) pause(ctx context.Context, d time.Duration) error {
	return m.sleep(ctx, d)
}

// reservesBusy is why the machine m measured last has no room for the
// candidate beside live reserves and the admitted tasks its load average does
// not show yet, empty when it has. The candidate's expected cores are included
// before its reservation is written under the queue lock. Without a runnable limit (no
// /proc/stat to count the threads from, as on Windows, and no runnable in
// pressure.yaml, a measure busy skips and tells) there is nothing to reach.
func (m *machine) reservesBusy(lockPath string) (busyness, error) {
	m.exitHatch = false
	if m.runnableLimit <= 0 {
		return busyness{}, nil
	}
	sum, unseen, task, retryAt, err := reserveCounts(lockPath, m)
	if err != nil {
		return busyness{}, err
	}
	candidateCapped := capCandidate(m.candidate, m.runnableLimit)
	m.decision.Unseen, m.decision.LiveReserve = new(unseen), new(sum)
	m.decision.CandidateCapped = new(candidateCapped)
	if task != "" {
		return busyness{why: WhyReserve, text: fmt.Sprintf("waiting for the first CPU measure of %s", task), retryAt: retryAt}, nil
	}
	if unseen == 0 && sum == 0 {
		return m.exitHatchBusy(), nil
	}
	return m.reserveForecastBusy(sum, unseen, candidateCapped), nil
}

func reserveCounts(lockPath string, m *machine) (float64, float64, string, time.Time, error) {
	sum, err := liveReserveCores(lockPath)
	if err != nil {
		return 0, 0, "", time.Time{}, err
	}
	unseen, err := unseenAdmittedCores(lockPath, m.unseenShare())
	if err != nil {
		return 0, 0, "", time.Time{}, err
	}
	task, retryAt, pending, err := firstUnmeasuredAdmission(lockPath, time.Now(), reserveWindow+reservePollInterval)
	if err != nil {
		return 0, 0, "", time.Time{}, err
	}
	if pending {
		return sum, unseen, task, retryAt, nil
	}
	return sum, unseen, "", time.Time{}, nil
}

func (m *machine) exitHatchBusy() busyness {
	m.exitHatch = true
	m.decision.CandidateCapped = new(float64(0))
	if float64(m.runnable) >= m.runnableLimit {
		return busyness{why: WhyReserve, text: fmt.Sprintf("runnable %.0f is at or above %.0f", m.runnable, m.runnableLimit)}
	}
	if m.avg1Known && m.avg1 >= m.runnableLimit {
		return busyness{why: WhyReserve, text: fmt.Sprintf("load average 1m %.2f is at or above %.0f", m.avg1, m.runnableLimit)}
	}
	return busyness{}
}

func (m *machine) reserveForecastBusy(sum, unseen, candidate float64) busyness {
	if float64(m.runnable)+sum+candidate >= m.runnableLimit {
		return busyness{why: WhyReserve, text: fmt.Sprintf("runnable %.0f plus %.1f of the live reserves plus %.1f of this task reaches %.0f", m.runnable, sum, candidate, m.runnableLimit)}
	}
	if !m.avg1Known || m.avg1+unseen+candidate < m.runnableLimit {
		return busyness{}
	}
	return busyness{why: WhyReserve, text: fmt.Sprintf("load average 1m %.1f plus %.1f of the admitted tasks plus %.1f of this task reaches %.0f", m.avg1, unseen, candidate, m.runnableLimit)}
}

// capCandidate drops an estimate that could never fit under the limit: such a
// task starts by the plain rule (load and live reserves below the limit), one
// at a time, since the next candidate sees its reserve. Smaller estimates count
// in full.
func capCandidate(candidate, limit float64) float64 {
	if limit > 0 && candidate >= limit {
		return 0
	}
	return candidate
}

// firstUnmeasuredAdmission keeps the queue closed briefly after an admission,
// until its first CPU window should have been sampled. After that grace period
// the expected reserve continues to protect tasks whose CPU cannot be read.
func firstUnmeasuredAdmission(lockPath string, now time.Time, grace time.Duration) (string, time.Time, bool, error) {
	files, err := lock.LiveData(filepath.Dir(lockPath), admittedPattern(lockPath))
	if err != nil {
		return "", time.Time{}, false, err
	}
	var firstTask string
	var retryAt time.Time
	for _, file := range files {
		var d reserveData
		if err := json.Unmarshal(file.Data, &d); err != nil {
			return "", time.Time{}, false, fmt.Errorf("decoding admitted task %s: %w", file.Path, err)
		}
		if d.ObservedAt != "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, d.Admitted)
		if err != nil {
			return "", time.Time{}, false, fmt.Errorf("admitted task %s: %w", file.Path, err)
		}
		until := at.Add(grace)
		if now.Before(until) && (retryAt.IsZero() || until.Before(retryAt)) {
			task := d.Task
			if task == "" {
				task = "unknown task"
			}
			firstTask, retryAt = task, until
		}
	}
	return firstTask, retryAt, !retryAt.IsZero(), nil
}

// loadAvgTau is the time constant of the kernel's 1-minute load average: a
// load of c that starts now shows in it as c·(1 − e^(−t/τ)).
const loadAvgTau = time.Minute

// admittedPattern names the live record of an admitted task that lives as
// long as the task, past its reserve.
func admittedPattern(lockPath string) string { return filepath.Base(lockPath) + ".*.admitted" }

// unseenShare is the share of an admitted task's cores the 1-minute load
// average does not show yet, by the time of its admission.
func (m *machine) unseenShare() func(admitted time.Time) float64 {
	now := reserveAgeNow()
	return func(admitted time.Time) float64 {
		return math.Exp(-max(now.Sub(admitted), 0).Seconds() / loadAvgTau.Seconds())
	}
}

// unseenAdmittedCores is how much of the expected or observed cores of the
// running admitted tasks the load does not show yet. Expected cores decay
// from admission; observed cores decay from when that estimate was measured.
// The load average of a queue's dip is that of the machine before the tasks it
// let in: the next counts them so.
func unseenAdmittedCores(lockPath string, share func(admitted time.Time) float64) (float64, error) {
	files, err := lock.LiveData(filepath.Dir(lockPath), admittedPattern(lockPath))
	if err != nil {
		return 0, err
	}
	type admittedEstimate struct {
		at             time.Time
		expected       float64
		observed       float64
		observedAt     time.Time
		hasObservation bool
	}
	byAdmission := make(map[string]admittedEstimate, len(files))
	for _, file := range files {
		var d reserveData
		if err := json.Unmarshal(file.Data, &d); err != nil {
			return 0, fmt.Errorf("decoding admitted task %s: %w", file.Path, err)
		}
		at, err := time.Parse(time.RFC3339Nano, d.Admitted)
		if err != nil {
			return 0, fmt.Errorf("admitted task %s: %w", file.Path, err)
		}
		var observedAt time.Time
		if d.ObservedAt != "" {
			observedAt, err = time.Parse(time.RFC3339Nano, d.ObservedAt)
			if err != nil {
				return 0, fmt.Errorf("observed CPU of admitted task %s: %w", file.Path, err)
			}
		}
		key := d.Admitted
		if key == "" {
			key = file.Path
		}
		old, ok := byAdmission[key]
		if !ok {
			old.at = at
		}
		old.expected = max(old.expected, d.Cores)
		if !observedAt.IsZero() && (!old.hasObservation || observedAt.After(old.observedAt)) {
			old.observed, old.observedAt, old.hasObservation = d.ObservedCores, observedAt, true
		}
		byAdmission[key] = old
	}
	var sum float64
	for _, key := range slices.Sorted(maps.Keys(byAdmission)) {
		estimate := byAdmission[key]
		expected := estimate.expected * share(estimate.at)
		observed := estimate.observed * share(estimate.observedAt)
		sum += max(expected, observed)
	}
	return sum, nil
}

func liveReserveCores(lockPath string) (float64, error) {
	dir, pattern := filepath.Dir(lockPath), filepath.Base(lockPath)+".*.reserve"
	files, err := lock.LiveData(dir, pattern)
	if err != nil {
		return 0, err
	}
	var sum float64
	for _, file := range files {
		var d reserveData
		if err := json.Unmarshal(file.Data, &d); err != nil {
			return 0, fmt.Errorf("decoding live reserve %s: %w", file.Path, err)
		}
		sum += d.Cores
	}
	return sum, nil
}

func expectedReserveCores(kind string, cores float64) (float64, error) {
	if cores > 0 {
		return cores, nil
	}
	return stats.ExpectedCores(kind)
}

// startAdmission writes the live record of an admitted task, which the
// forecast counts until the task's release, and starts its reserve.
func startAdmission(lockPath, name string, cores float64, cap time.Duration, log io.Writer) (func() error, time.Time, func() string, func() time.Time, error) {
	admitted := time.Now().Format(time.RFC3339Nano)
	raw, err := json.Marshal(reserveData{Cores: cores, Admitted: admitted, Task: name})
	if err != nil {
		return released, time.Now(), nil, nil, fmt.Errorf("encoding admitted task: %w", err)
	}
	admittedRel, err := lock.Reserve(filepath.Dir(lockPath), admittedPattern(lockPath), raw)
	if err != nil {
		return released, time.Now(), nil, nil, err
	}
	loadLockBeforeRelease()
	var observedMu sync.Mutex
	var observedRel func() error
	observedReleased := false
	recordObserved := func(observed float64, observedAt time.Time) error {
		observedMu.Lock()
		defer observedMu.Unlock()
		if observedReleased {
			return nil
		}
		data, err := json.Marshal(reserveData{
			Cores: cores, ObservedCores: observed, ObservedAt: observedAt.Format(time.RFC3339Nano),
			Admitted: admitted, Task: name,
		})
		if err != nil {
			return fmt.Errorf("encoding measured admitted cores: %w", err)
		}
		rel, err := lock.Reserve(filepath.Dir(lockPath), admittedPattern(lockPath), data)
		if err != nil {
			return fmt.Errorf("recording measured admitted cores: %w", err)
		}
		if observedRel != nil {
			if err := observedRel(); err != nil {
				return errors.Join(err, rel())
			}
		}
		observedRel = rel
		return nil
	}
	release, since, end, stoppedAt, err := startReserve(lockPath, name, cores, cap, recordObserved, log)
	if err != nil {
		return released, since, nil, nil, errors.Join(err, admittedRel())
	}
	var once sync.Once
	var admittedErr error
	return func() error {
		err := release()
		once.Do(func() {
			observedMu.Lock()
			observedReleased = true
			if observedRel != nil {
				admittedErr = observedRel()
			}
			admittedErr = errors.Join(admittedErr, admittedRel())
			observedMu.Unlock()
		})
		return errors.Join(err, admittedErr)
	}, since, end, stoppedAt, nil
}

type reserveSample struct {
	at  time.Time
	cpu float64
}

type reservePollState struct {
	samples          []reserveSample
	reserveDropped   bool
	haveObserved     bool
	observed         float64
	readErrorLogged  bool
	writeErrorLogged bool
}

func (state *reservePollState) readError(name string, err error, log io.Writer) {
	if !state.readErrorLogged {
		fmt.Fprintf(log, "graft: %s: reading reserved CPU: %v\n", name, err)
		state.readErrorLogged = true
	}
	state.samples = nil
}

func (state *reservePollState) sampleSpeed(cpu float64, at time.Time, window time.Duration) (float64, bool) {
	state.samples = append(state.samples, reserveSample{at: at, cpu: cpu})
	for len(state.samples) > 1 && at.Sub(state.samples[1].at) >= window {
		state.samples = state.samples[1:]
	}
	if len(state.samples) < 2 || at.Sub(state.samples[0].at) < window {
		return 0, false
	}
	speed := max((cpu-state.samples[0].cpu)/at.Sub(state.samples[0].at).Seconds(), 0)
	return speed, true
}

func (state *reservePollState) updateObservation(speed float64, at time.Time, onCPU func(float64, time.Time) error) (bool, error) {
	if state.haveObserved && !reserveEstimateChanged(state.observed, speed) {
		return true, nil
	}
	if onCPU != nil {
		if err := onCPU(speed, at); err != nil {
			return false, err
		}
	}
	state.observed, state.haveObserved = speed, true
	return true, nil
}

func (state *reservePollState) logWriteError(name string, err error, log io.Writer) {
	if !state.writeErrorLogged {
		fmt.Fprintf(log, "graft: %s: recording measured reserved CPU: %v\n", name, err)
		state.writeErrorLogged = true
	}
}

func (state *reservePollState) handleTick(name string, cores float64, window time.Duration, readCPU func(int) (float64, error), onCPU func(float64, time.Time) error, setEnd func(string), drop func(), log io.Writer) {
	cpu, err := readCPU(os.Getpid())
	if err != nil {
		state.readError(name, err, log)
		return
	}
	at := time.Now()
	speed, ready := state.sampleSpeed(cpu, at, window)
	if !ready || !state.reserveDropped && speed < cores/2 {
		return
	}
	updated, err := state.updateObservation(speed, at, onCPU)
	if err != nil {
		state.logWriteError(name, err, log)
		return
	}
	if !updated {
		return
	}
	if state.reserveDropped {
		return
	}
	state.reserveDropped = true
	setEnd("cpu")
	drop()
}

func runReservePoll(name string, cores float64, cap, interval, window time.Duration, readCPU func(int) (float64, error), onCPU func(float64, time.Time) error, log io.Writer, done <-chan struct{}, setEnd func(string), drop func()) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(log, "graft: %s: panic in reserve poll: %v\n", name, r)
			setEnd("panic")
			drop()
		}
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	deadline := time.NewTimer(cap)
	defer deadline.Stop()
	deadlineC := deadline.C
	var state reservePollState
	for {
		select {
		case <-done:
			return
		case <-deadlineC:
			deadlineC = nil
			setEnd("cap")
			drop()
		case <-ticker.C:
			state.handleTick(name, cores, window, readCPU, onCPU, setEnd, drop, log)
		}
	}
}

func startReserve(lockPath, name string, cores float64, cap time.Duration, onCPU func(float64, time.Time) error, log io.Writer) (func() error, time.Time, func() string, func() time.Time, error) {
	now := time.Now()
	since := now
	raw, err := json.Marshal(reserveData{Cores: cores, Admitted: now.Format(time.RFC3339Nano), Task: name})
	if err != nil {
		return released, since, nil, nil, fmt.Errorf("encoding reserve: %w", err)
	}
	rel, err := lock.Reserve(filepath.Dir(lockPath), filepath.Base(lockPath)+".*.reserve", raw)
	if err != nil {
		return released, since, nil, nil, err
	}
	var mu sync.Mutex
	end := ""
	var stoppedAt time.Time
	done := make(chan struct{})
	var once sync.Once
	setEnd := func(value string) {
		mu.Lock()
		if end == "" {
			end = value
		}
		mu.Unlock()
	}
	setStoppedAt := func() {
		mu.Lock()
		if stoppedAt.IsZero() {
			stoppedAt = time.Now()
		}
		mu.Unlock()
	}
	var dropOnce sync.Once
	var dropErr error
	drop := func() {
		dropOnce.Do(func() {
			dropErr = rel()
			if dropErr != nil {
				fmt.Fprintf(log, "graft: %s: releasing reserve: %v\n", name, dropErr)
			}
			setStoppedAt()
		})
	}
	// The goroutine outlives the call: it reads its settings from here, not
	// from the package variables a test may swap meanwhile.
	interval, window, readCPU := reservePollInterval, reserveWindow, reserveCPUReader
	go runReservePoll(name, cores, cap, interval, window, readCPU, onCPU, log, done, setEnd, drop)
	return func() error {
		once.Do(func() { setEnd("done"); close(done) })
		drop()
		return dropErr
	}, since, func() string { mu.Lock(); defer mu.Unlock(); return end }, func() time.Time { mu.Lock(); defer mu.Unlock(); return stoppedAt }, nil
}

// quietMode reads only akit's quiet-until state. Queue polling must not run
// the machine meter: it reads procfs, disk state and pressure windows.
func quietMode() (func() (bool, error), error) {
	path, err := pressure.QuietPath()
	if err != nil {
		return nil, fmt.Errorf("reading quiet mode path: %w", err)
	}
	return func() (bool, error) {
		until, err := pressure.ReadQuiet(path)
		if err != nil {
			return false, fmt.Errorf("reading quiet mode: %w", err)
		}
		return time.Now().Before(until), nil
	}, nil
}

// released is the release of a task that holds nothing.
func released() error { return nil }

// settle keeps the queue's lock for d and returns the release that gives it
// up sooner, when the task ends: its load ends with it. The lock's error
// comes back from the release, whichever of them gave it up.
func settle(lk *lock.Lock, d time.Duration) func() error {
	var once sync.Once
	var err error
	unlock := func() { err = lk.Release() }
	timer := time.AfterFunc(d, func() { once.Do(unlock) })
	return func() error {
		timer.Stop()
		once.Do(unlock)
		return err
	}
}

// ProcEnv, when set, is the directory read instead of /proc to measure the
// machine: for tests.
const ProcEnv = "GRAFT_PROC"

// DirtyWritebackKB reads the memory the machine has queued to write, in kB.
func DirtyWritebackKB() (int64, error) {
	proc := "/proc"
	if dir := os.Getenv(ProcEnv); dir != "" {
		proc = dir
	}
	path := filepath.Join(proc, "meminfo")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read memory writeback state: %w", err)
	}
	var dirty, writeback int64
	var haveDirty, haveWriteback bool
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		if key != "Dirty" && key != "Writeback" {
			continue
		}
		if len(fields) < 2 {
			return 0, fmt.Errorf("read memory writeback state: malformed %s line", key)
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("read memory writeback state: malformed %s value %q", key, fields[1])
		}
		switch key {
		case "Dirty":
			dirty = value
			haveDirty = true
		case "Writeback":
			writeback = value
			haveWriteback = true
		}
	}
	if !haveDirty || !haveWriteback {
		return 0, fmt.Errorf("read memory writeback state: %s is missing Dirty or Writeback", path)
	}
	return dirty + writeback, nil
}

// machineMeter is the machine reading loadgate needs; tests replace it with a
// fixed sequence of decisions.
type machineMeter interface {
	Check(context.Context, pressure.Limits, string, bool) (pressure.Verdict, error)
}

// machine reads what makes the machine busy for one task.
type machine struct {
	l                 userconfig.MachineLoad
	limits            pressure.Limits
	limitsErr         error
	log               io.Writer
	name, kind        string
	meter             machineMeter
	lastView          pressure.Verdict
	haveView          bool
	lastLoggedMeasure string
	// admissionStart is when the admission began, the queue included.
	admissionStart time.Time
	now            func() time.Time
	sleep          func(context.Context, time.Duration) error
	// runnable is the last count of the tasks ready to run.
	runnable      float64
	runnableLimit float64
	// avg1 is the last 1-minute load average, when avg1Known.
	avg1      float64
	avg1Known bool
	// unchecked holds the measures the system does not have, told once.
	unchecked map[error]bool
	// ignoreCPUPressure keeps disk-only work from waiting on CPU, including
	// when akit's half mode caps the configured runnable limit.
	ignoreCPUPressure bool
	// waits is how long admit waited, by why.
	waits          Waits
	candidate      float64
	candidateKnown bool
	decision       admissionDecision
	exitHatch      bool
}

func newMachine(l userconfig.MachineLoad, log io.Writer, name, kind string) *machine {
	proc := "/proc"
	if dir := os.Getenv(ProcEnv); dir != "" {
		proc = dir
	}
	limits, limitsErr := pressure.UserLimits()
	return &machine{l: l, limits: limits, limitsErr: limitsErr, log: log, name: name, kind: kind, now: time.Now, sleep: sleep, meter: pressureMeterFactory(proc), waits: Waits{}, admissionStart: time.Now()}
}

func (m *machine) admission() Admission { return Admission{Waits: m.waits} }

// behind is the admission of a task that waited in a queue behind holder.
func (m *machine) behind(holder string) Admission {
	adm := m.admission()
	adm.WaitedBehind = holder
	return adm
}

// BusyNow tells whether Wait would wait now, without entering the queue: a
// task holds the queue's lock at lockPath (starting, or settling after its
// start), or the machine measures Wait uses, read once, say busy. A missing
// measure is skipped and reported as it is in Wait. An empty lockPath leaves
// out the queue and its reserves: the machine alone is checked.
func BusyNow(ctx context.Context, name, kind string, l userconfig.MachineLoad, lockPath string, log io.Writer) (bool, error) {
	return BusyNowWithReserves(ctx, name, kind, l, lockPath, lockPath != "", log)
}

// BusyNowWithReserves checks the machine, its live reservations and the
// admitted tasks its load average does not show yet without queueing. queue
// controls whether a current queue holder also counts.
func BusyNowWithReserves(ctx context.Context, name, kind string, l userconfig.MachineLoad, lockPath string, queue bool, log io.Writer) (bool, error) {
	if queue {
		holders, err := lock.Holders(lockPath)
		if err != nil || len(holders) > 0 {
			return err == nil, err
		}
	}
	m := newMachine(l, log, name, kind)
	b, err := m.busy(ctx, true)
	if err != nil || b.why != "" || lockPath == "" {
		return b.why != "", err
	}
	b, err = m.reservesBusy(lockPath)
	return b.why != "", err
}

// LoadQueueWhy reports the machine or live-reservation reason the load queue
// would wait for now, without checking whether the queue lock itself is held.
// It makes one fresh machine measurement, as BusyNowWithReserves does.
func LoadQueueWhy(ctx context.Context, name, kind string, l userconfig.MachineLoad, lockPath string, log io.Writer) (string, error) {
	m := newMachine(l, log, name, kind)
	b, err := m.busy(ctx, true)
	if err != nil || b.why != "" {
		return b.text, err
	}
	b, err = m.reservesBusy(lockPath)
	return b.text, err
}

// BusyChecker keeps the pressure meter across readings, so repeated checks
// measure since the previous reading without waiting for another swap window.
type BusyChecker struct{ machine *machine }

// NewDiskBusyChecker checks disk, writeback, swap and quiet mode before the
// next batch of an already-running process. The batch mainly adds disk load,
// so CPU runnable and load-average limits do not apply to it.
func NewDiskBusyChecker(name, kind string, l userconfig.MachineLoad, log io.Writer) *BusyChecker {
	checker := &BusyChecker{machine: newMachine(l, log, name, kind)}
	checker.machine.ignoreCPUPressure = true
	return checker
}

// Why reports the current load rule that prevents work, or an empty string.
func (c *BusyChecker) Why(ctx context.Context) (string, error) {
	b, err := c.machine.busy(ctx, false)
	return b.why, err
}

// busyness is why the machine cannot take a heavy task, one of the Why
// constants, and the words that say it; a calm machine has neither.
type busyness struct {
	why, text string
	retryAt   time.Time
}

// measures are the rule's measures as the waits name them, with what sets
// each limit.
var measures = map[string]struct{ why, setting string }{
	pressure.CPU:       {WhyLoad, "pressure.yaml runnable"},
	pressure.IO:        {WhyIO, "pressure.yaml max_io_full"},
	pressure.Writeback: {WhyWriteback, "pressure.yaml max_dirty_mb, else vm.dirty_background_bytes or _ratio"},
	pressure.Swap:      {WhySwap, "pressure.yaml max_mem_some, max_swap_kbs"},
	pressure.Quiet:     {WhyQuiet, "busy --quiet-off ends it"},
}

// busy says why the machine cannot take a heavy task now, nothing when it
// can, by akit's rule: the tasks ready to run at or above runnable, read
// once, a disk freeze going on or the IO pressure of the last 5 seconds at
// or above its limit, or the memory pressure above its limit while the
// machine swaps. fresh measures the swapping, and without the disk state
// source the IO pressure, over the configured swap window; otherwise since
// the last call. A measure the system lacks is
// skipped and told once.
func (m *machine) busy(ctx context.Context, fresh bool) (busyness, error) {
	m.decision = admissionDecision{}
	m.exitHatch = false
	if m.candidateKnown {
		m.decision.Candidate = new(m.candidate)
	}
	if m.limitsErr != nil {
		return busyness{}, fmt.Errorf("reading pressure limits: %w", m.limitsErr)
	}
	v, err := m.meter.Check(ctx, m.limits, m.kind, fresh)
	if err != nil {
		return busyness{}, fmt.Errorf("measuring the machine: %w", err)
	}
	if m.ignoreCPUPressure {
		skipped := v.Skipped
		v = pressure.Rule(v.Load, m.limits, append(slices.Clone(skipped), pressure.ErrNoRunnable, pressure.ErrNoLoadAvg))
		v.Skipped = skipped
	}
	// A look that the wait's end cut short is no measure: its disk source
	// failed for the deadline, not for the machine.
	if err := ended(ctx); err != nil {
		return busyness{}, err
	}
	noPSI := slices.Contains(v.Skipped, pressure.ErrNoIOPressure)
	for _, none := range v.Skipped {
		m.skip(none, v.Load.DiskFault, noPSI)
	}
	m.recordLoadDecision(v)
	m.lastView, m.haveView = v, true
	var b busyness
	if v.Busy() {
		// A measure graft does not know yet still makes the machine busy.
		ms, known := measures[v.Measure]
		b = busyness{why: v.Measure, text: v.Why()}
		if known {
			b.why = ms.why
			if b.text == v.Why() {
				b.text = fmt.Sprintf("%s (%s)", v.Why(), ms.setting)
			}
		}
	}
	return b, nil
}

func (m *machine) recordLoadDecision(v pressure.Verdict) {
	m.runnable = float64(v.Load.Runnable)
	m.avg1, m.avg1Known = v.Load.Avg1, !slices.Contains(v.Skipped, pressure.ErrNoLoadAvg)
	// The limit the reserves and the admitted tasks the load does not show
	// yet are counted against: the rule's own, half the threads in half mode.
	m.runnableLimit = m.limits.RunnableLimit(v.Load)
	if v.Measure == pressure.CPU {
		m.runnableLimit = v.Limit
	}
	m.decision = admissionDecision{}
	if !slices.Contains(v.Skipped, pressure.ErrNoLoadAvg) {
		m.decision.LA1 = new(v.Load.Avg1)
	}
	if !slices.Contains(v.Skipped, pressure.ErrNoRunnable) {
		m.decision.Runnable = new(float64(v.Load.Runnable))
	}
	if m.runnableLimit > 0 {
		m.decision.Limit = new(m.runnableLimit)
	}
	if m.candidateKnown {
		m.decision.Candidate = new(m.candidate)
	}
}

// skip says the first time that the system lacks a measure. Without the disk
// state source, which fault tells why, the disk is still measured by the IO
// pressure's total, unless the system lacks that too (noPSI).
func (m *machine) skip(none, fault error, noPSI bool) {
	if m.unchecked[none] {
		return
	}
	if m.unchecked == nil {
		m.unchecked = map[error]bool{}
	}
	m.unchecked[none] = true
	fallback := "PSI total used"
	if noPSI {
		fallback = "and no IO pressure either: the disk is not checked"
	}
	switch {
	case errors.Is(none, pressure.ErrNoIOSource):
		fmt.Fprintf(m.log, "graft: %s: %v, %s\n", m.name, none, fallback)
	case errors.Is(none, pressure.ErrIOSourceFailed):
		fmt.Fprintf(m.log, "graft: %s: %v, %s\n", m.name, fault, fallback)
	default:
		fmt.Fprintf(m.log, "graft: %s: %v, not checked\n", m.name, none)
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ended is the context's error, or DeadlineExceeded once its deadline has
// passed: a dial cut by that deadline returns before the context's own timer
// sets its error.
func ended(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}
