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

const unlimitedPriorityBudget = time.Duration(1 << 62)

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
	WaitSpans        []stats.WaitSpan
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
	Rate          float64 `json:"rate"`
	EMA           float64 `json:"ema"`
	At            string  `json:"at,omitempty"`
	CPUError      string  `json:"cpu_error,omitempty"`
	ObservedCores float64 `json:"observed_cores,omitempty"`
	ObservedAt    string  `json:"observed_at,omitempty"`
	Admitted      string  `json:"admitted"`
	Task          string  `json:"task"`
}

var reservePollInterval = 250 * time.Millisecond
var reserveWindow = 2 * time.Second
var reserveCPUReader = treeCPU
var reservePollNow = time.Now

// reserveAgeNow is overridden by tests to hold the admitted-core forecast at
// a fixed age; production uses the wall clock.
var reserveAgeNow = time.Now
var admittedRecordStat = os.Stat
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
	// Unlimited is for a single explicit --past-load admission. It gets
	// queue ordering without spending a deploy's shared wait budget.
	Unlimited bool
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
	if d < 0 {
		return "", fmt.Errorf("the budget for waiting must not be negative")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("the budget for waiting: %w", err)
	}
	f, err := os.CreateTemp(dir, "load-budget-*")
	if err != nil {
		return "", fmt.Errorf("the budget for waiting: %w", err)
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("the budget for waiting: %w", err)
	}
	amount := []byte(strconv.FormatInt(int64(d), 10))
	if err := writeAtomic(path, amount); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("the budget for waiting: %w", err)
	}
	if err := writeAtomic(path+".total", amount); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("the budget total for waiting: %w", err)
	}
	return path, nil
}

func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("creating atomic file for %s: %w", path, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		return fmt.Errorf("setting permissions on atomic file for %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing atomic file for %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("syncing atomic file for %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing atomic file for %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("renaming atomic file to %s: %w", path, err)
	}
	return nil
}

func parseBudgetState(remainingData, totalData []byte, path string, hasTotal bool) (budgetState, error) {
	remaining, err := strconv.ParseInt(strings.TrimSpace(string(remainingData)), 10, 64)
	if err != nil {
		var oldJSON struct {
			Total     *int64 `json:"total"`
			Remaining *int64 `json:"remaining"`
		}
		if jsonErr := json.Unmarshal(remainingData, &oldJSON); jsonErr != nil || oldJSON.Total == nil || oldJSON.Remaining == nil {
			return budgetState{}, fmt.Errorf("budget file %s: remaining is not a number of nanoseconds", path)
		}
		remaining = *oldJSON.Remaining
		if !hasTotal {
			totalData = []byte(strconv.FormatInt(*oldJSON.Total, 10))
			hasTotal = true
		}
	}
	if remaining < 0 {
		return budgetState{}, fmt.Errorf("budget file %s: remaining budget is negative", path)
	}
	total := remaining
	if hasTotal {
		parsedTotal, err := strconv.ParseInt(strings.TrimSpace(string(totalData)), 10, 64)
		if err != nil || parsedTotal < 0 {
			return budgetState{}, fmt.Errorf("budget total file %s.total: invalid number of nanoseconds", path)
		}
		total = parsedTotal
	}
	if remaining > total {
		return budgetState{}, fmt.Errorf("budget file %s: remaining budget exceeds total", path)
	}
	return budgetState{Total: total, Remaining: remaining}, nil
}

type budgetState struct {
	Total     int64
	Remaining int64
}

func readBudgetState(path string) (budgetState, error) {
	remainingData, err := os.ReadFile(path)
	if err != nil {
		return budgetState{}, fmt.Errorf("reading the budget for waiting %s: %w", path, err)
	}
	totalData, err := os.ReadFile(path + ".total")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return budgetState{}, fmt.Errorf("reading the budget total %s.total: %w", path, err)
	}
	hasTotal := !errors.Is(err, os.ErrNotExist)
	return parseBudgetState(remainingData, totalData, path, hasTotal)
}

func (p *Priority) budgetTotal() (total time.Duration, err error) {
	fl := flock.New(p.Budget + ".lock")
	if err := fl.Lock(); err != nil {
		return 0, fmt.Errorf("locking the budget for waiting %s: %w", p.Budget, err)
	}
	defer func() { err = errors.Join(err, fl.Unlock()) }()
	state, err := readBudgetState(p.Budget)
	return time.Duration(state.Total), err
}

type budgetInterval struct {
	From      int64 `json:"from"`
	To        int64 `json:"to"`
	OpenStart bool  `json:"open_start,omitempty"`
}

type budgetIntervals []budgetInterval

func (spans budgetIntervals) merge(next budgetInterval) budgetIntervals {
	spans = append(spans, next)
	slices.SortFunc(spans, func(a, b budgetInterval) int {
		if a.OpenStart != b.OpenStart {
			if a.OpenStart {
				return -1
			}
			return 1
		}
		if a.From < b.From {
			return -1
		}
		if a.From > b.From {
			return 1
		}
		return 0
	})
	merged := spans[:0]
	for _, span := range spans {
		if span.To <= span.From && !span.OpenStart {
			continue
		}
		if len(merged) == 0 {
			merged = append(merged, span)
			continue
		}
		last := &merged[len(merged)-1]
		if span.From <= last.To {
			if span.To > last.To {
				last.To = span.To
			}
		} else {
			merged = append(merged, span)
		}
	}
	return merged
}

const budgetIntervalLimit = 64

func distance(high, low int64) uint64 {
	return uint64(high) - uint64(low)
}

func (spans budgetIntervals) compact() (budgetIntervals, time.Duration) {
	var charged uint64
	for len(spans) > budgetIntervalLimit {
		mergeAt := 0
		minGap := distance(spans[1].From, spans[0].To)
		for i := 1; i+1 < len(spans); i++ {
			gap := distance(spans[i+1].From, spans[i].To)
			if gap < minGap {
				mergeAt, minGap = i, gap
			}
		}
		gap := distance(spans[mergeAt+1].From, spans[mergeAt].To)
		charged += gap
		spans[mergeAt].To = spans[mergeAt+1].To
		spans = append(spans[:mergeAt+1], spans[mergeAt+2:]...)
	}
	return spans, saturatedDuration(charged)
}

func saturatedDuration(n uint64) time.Duration {
	if n > uint64(math.MaxInt64) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(n)
}

func subtractBudget(left, spent time.Duration) time.Duration {
	if spent > 0 && left < time.Duration(math.MinInt64)+spent {
		return time.Duration(math.MinInt64)
	}
	return left - spent
}

func (spans budgetIntervals) uncovered(from, to int64) time.Duration {
	var uncovered uint64
	cursor := from
	for _, span := range spans {
		lo := span.From
		if span.OpenStart {
			lo = from
		}
		if lo < from {
			lo = from
		}
		hi := min(span.To, to)
		if hi <= cursor || hi <= lo {
			continue
		}
		if lo > cursor {
			uncovered += distance(lo, cursor)
		}
		if hi > cursor {
			cursor = hi
		}
	}
	if cursor < to {
		uncovered += distance(to, cursor)
	}
	return saturatedDuration(uncovered)
}

func wallClockMovedBack(spans budgetIntervals, toNS int64, observedAt time.Time) bool {
	if len(spans) == 0 {
		return false
	}
	latest := spans[len(spans)-1].To
	if !atLeastDistance(latest, toNS, uint64(time.Minute)) {
		return false
	}
	// The old watermark rule protected against a wall-clock jump. Require the
	// measured end to be close to the current clock as well, so delayed
	// out-of-order writes keep their intervals instead of looking like a jump.
	now := observedAt.UnixNano()
	if now >= toNS {
		return distance(now, toNS) < uint64(time.Minute)
	}
	return distance(toNS, now) < uint64(time.Minute)
}

func atLeastDistance(high, low int64, threshold uint64) bool {
	return high > low && distance(high, low) >= threshold
}

func parseBudgetIntervals(data []byte, path string) (budgetIntervals, error) {
	if ns, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
		return budgetIntervals{{To: ns, OpenStart: true}}, nil
	}
	var spans budgetIntervals
	if err := json.Unmarshal(data, &spans); err != nil {
		return nil, fmt.Errorf("budget wall-time mark %s: invalid interval set: %w", path, err)
	}
	if len(spans) == 0 {
		return nil, fmt.Errorf("budget wall-time mark %s: interval set is empty", path)
	}
	openStarts := 0
	for i, span := range spans {
		if span.OpenStart {
			openStarts++
			if i != 0 {
				return nil, fmt.Errorf("budget wall-time mark %s: open-start interval must be first", path)
			}
		} else if span.To <= span.From {
			return nil, fmt.Errorf("budget wall-time mark %s: interval %d is empty or reversed", path, i)
		}
		if i > 0 && spans[i-1].To >= span.From {
			return nil, fmt.Errorf("budget wall-time mark %s: intervals %d and %d overlap or are not merged", path, i-1, i)
		}
	}
	if openStarts > 1 {
		return nil, fmt.Errorf("budget wall-time mark %s: multiple open-start intervals", path)
	}
	return spans, nil
}

func readBudgetIntervals(budget string) (budgetIntervals, string, error) {
	intervalsPath := budget + ".intervals"
	path := intervalsPath
	data, err := os.ReadFile(intervalsPath)
	if errors.Is(err, os.ErrNotExist) {
		// Older files have only the scalar watermark; an intermediate format
		// stored ranges there as JSON. Both remain readable during upgrade.
		path = budget + ".until"
		data, err = os.ReadFile(path)
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, path, nil
	}
	if err != nil {
		return nil, path, fmt.Errorf("reading budget intervals %s: %w", path, err)
	}
	spans, err := parseBudgetIntervals(data, path)
	return spans, path, err
}

func writeBudgetSpend(budget string, state budgetState, left time.Duration, spans budgetIntervals) error {
	if len(spans) == 0 {
		return nil
	}
	intervalsPath := budget + ".intervals"
	encoded, err := json.Marshal(spans)
	if err != nil {
		return fmt.Errorf("encoding budget intervals %s: %w", intervalsPath, err)
	}
	state.Remaining = int64(max(left, 0))
	if err := writeAtomic(budget, []byte(strconv.FormatInt(state.Remaining, 10))); err != nil {
		return fmt.Errorf("writing the remaining budget %s: %w", budget, err)
	}
	totalPath := budget + ".total"
	if _, err := os.Stat(totalPath); errors.Is(err, os.ErrNotExist) {
		if err := writeAtomic(totalPath, []byte(strconv.FormatInt(state.Total, 10))); err != nil {
			return fmt.Errorf("writing the budget total %s: %w", totalPath, err)
		}
	} else if err != nil {
		return fmt.Errorf("checking the budget total %s: %w", totalPath, err)
	}
	if err := writeAtomic(intervalsPath, encoded); err != nil {
		return fmt.Errorf("writing budget intervals %s: %w", intervalsPath, err)
	}
	watermark := spans[len(spans)-1].To
	untilPath := budget + ".until"
	if err := writeAtomic(untilPath, []byte(strconv.FormatInt(watermark, 10))); err != nil {
		return fmt.Errorf("writing budget wall-time mark %s: %w", untilPath, err)
	}
	return nil
}

// spend takes the part of [from, to] not already counted from the shared wall-time budget.
// Empty endpoints read the remaining budget without changing its watermark.
func (p *Priority) spend(from, to time.Time) (left time.Duration, err error) {
	if p.Unlimited {
		return unlimitedPriorityBudget, nil
	}
	fl := flock.New(p.Budget + ".lock")
	if err := fl.Lock(); err != nil {
		return 0, fmt.Errorf("locking the budget for waiting %s: %w", p.Budget, err)
	}
	defer func() { err = errors.Join(err, fl.Unlock()) }()
	state, err := readBudgetState(p.Budget)
	if err != nil {
		return 0, err
	}
	left = time.Duration(state.Remaining)
	if from.IsZero() && to.IsZero() {
		return left, nil
	}
	from, to = time.Unix(0, from.UnixNano()), time.Unix(0, to.UnixNano())
	spans, _, err := readBudgetIntervals(p.Budget)
	if err != nil {
		return 0, err
	}
	fromNS, toNS := from.UnixNano(), to.UnixNano()
	if toNS <= fromNS && len(spans) == 0 {
		return left, nil
	}
	if toNS > fromNS && wallClockMovedBack(spans, toNS, time.Now()) {
		spans = nil // The wall clock moved backwards; begin a fresh accounting epoch.
	}
	if toNS > fromNS {
		left = subtractBudget(left, spans.uncovered(fromNS, toNS))
		var compressed time.Duration
		spans, compressed = spans.merge(budgetInterval{From: fromNS, To: toNS}).compact()
		left = subtractBudget(left, compressed)
	}
	if err := writeBudgetSpend(p.Budget, state, left, spans); err != nil {
		return 0, err
	}
	return left, nil
}

// Wait admits a heavy task and returns how long it waited and why, and the
// release the task calls when it ends. Tasks queue under lockPath while
// measuring the machine and writing their live expected-core reservation.
// A priority task uses the same FIFO queue and spends its shared wait budget.
//
// kind is the task's kind as the stats profile names it.
func Wait(ctx context.Context, name, kind string, p *Priority, l userconfig.MachineLoad, lockPath string, log io.Writer, pastLoad string) (Admission, func() error, error) {
	return WaitWithSettle(ctx, name, kind, p, l, lockPath, "", 0, 0, log, pastLoad)
}

// WaitWithSettle admits a heavy task and, when settleLockPath is set,
// serializes tasks that requested their own settling period on that lock.
// The ordinary queue writes a live reservation before releasing its lock:
// cores when positive, else the profile's estimate.
func WaitWithSettle(ctx context.Context, name, kind string, p *Priority, l userconfig.MachineLoad, lockPath, settleLockPath string, settleDuration time.Duration, cores float64, log io.Writer, pastLoad string) (admission Admission, release func() error, retErr error) {
	m := newMachine(l, log, name, kind)
	if pastLoad != "" {
		m.pastLoad = pastLoad
		if p == nil {
			p = &Priority{For: "graft " + name, Unlimited: true}
		}
	}
	priority := p != nil
	defer func() {
		if retErr != nil {
			m.recordRefused(retErr.Error(), admission, priority)
		} else {
			m.recordAdmitted(admission, priority)
		}
	}()
	holder := "graft " + name + " (starting a heavy task)"
	settleCtx, cancel, err := prioritySettleContext(ctx, m, p, settleLockPath, settleDuration)
	if err != nil {
		return m.admission(), released, err
	}
	if cancel != nil {
		defer cancel()
	}
	settleLock, behind, start, err := acquireSettle(settleCtx, m, p, settleLockPath, settleDuration, holder, log)
	if err != nil {
		if p != nil && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			m.noteQueueWait(settleLockPath, start, time.Now())
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

func prioritySettleContext(ctx context.Context, m *machine, p *Priority, settleLockPath string, settleDuration time.Duration) (context.Context, context.CancelFunc, error) {
	if p == nil {
		return ctx, nil, nil
	}
	left, err := p.spend(time.Time{}, time.Time{})
	if err != nil {
		return ctx, nil, err
	}
	m.priorityBudget = left
	if left <= 0 && settleLockPath != "" && settleDuration > 0 {
		holders, err := lock.Holders(settleLockPath)
		if err != nil {
			return ctx, nil, err
		}
		if len(holders) > 0 {
			m.waitedBehind = holders[0]
			m.lastWaitText = "queued behind " + holders[0]
			return ctx, nil, m.priorityFailure(ctx, p, time.Time{}, time.Time{})
		}
	}
	if left > 0 {
		settleCtx, cancel := context.WithTimeout(ctx, left)
		return settleCtx, cancel, nil
	}
	return ctx, nil, nil
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
		m.addWait(WhyQueue, start, time.Now())
		m.waitedBehind = settleLock.WaitedBehind
		m.lastWaitText = "queued behind " + settleLock.WaitedBehind
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
			if behind != "" {
				m.waitedBehind = behind
				m.lastWaitText = "queued behind " + behind
			} else if len(holders) > 0 {
				m.waitedBehind = holders[0]
				m.lastWaitText = "queued behind " + holders[0]
			}
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
	queueStart := time.Now()
	if p != nil {
		lk, err = lock.AcquireFIFOPriorityObserved(queueCtx, lockPath, queueTimeout, holder, log, quiet)
	} else {
		lk, err = lock.AcquireFIFOObserved(queueCtx, lockPath, queueTimeout, holder, log, quiet)
	}
	behind, queueWaited := m.loadQueueResult(ctx, p, lockPath, queueStart, start, behind, quiet, lk, err)
	if lk != nil {
		loadLockAcquired(lockPath)
	}
	return lk, behind, queueWaited, err
}

func (m *machine) loadQueueResult(ctx context.Context, p *Priority, lockPath string, queueStart, start time.Time, behind string, quiet func() (bool, error), lk *lock.Lock, err error) (string, bool) {
	if lk == nil {
		if p != nil && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			m.noteQueueWait(lockPath, queueStart, time.Now())
			quietNow, quietErr := quiet()
			if quietErr == nil && quietNow {
				m.lastWaitText = "quiet mode kept this task waiting"
			}
		}
		return behind, false
	}
	queueWaited := lk.WaitedBehind != "" || lk.WaitedQuiet > 0
	if queueWaited {
		now := time.Now()
		waited := now.Sub(start)
		quietWait := min(lk.WaitedQuiet, waited)
		queueWait := max(0, waited-m.waits[WhyQuiet]-quietWait-m.waits[WhyQueue])
		from := now.Add(-quietWait - queueWait)
		m.addWait(WhyQueue, from, from.Add(queueWait))
		m.addWait(WhyQuiet, from.Add(queueWait), now)
	}
	if lk.WaitedBehind != "" {
		behind = lk.WaitedBehind
	}
	if lk.WaitedQuiet > 0 {
		m.lastWaitText = "quiet mode kept this task waiting"
	} else if lk.WaitedBehind != "" {
		m.waitedBehind = lk.WaitedBehind
		m.lastWaitText = "queued behind " + lk.WaitedBehind
	}
	return behind, queueWaited
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
	if m.pastLoad != "" {
		fmt.Fprintf(log, "graft: %s: passed CPU load limit by --past-load (%s)\n", m.name, m.pastLoad)
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
			m.addWait(why, mark, now)
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
				return m.priorityExhausted(p)
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
		m.lastWaitText = b.text
		waited = true
		if p != nil {
			var finished bool
			spent, finished, err = m.spendPriorityWait(p, spent)
			if err != nil {
				return err
			}
			if finished {
				return m.priorityExhausted(p)
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
				m.addWait(why, mark, m.now())
			}
			return err
		}
	}
}

func (m *machine) measureOrReserve(ctx context.Context, fresh bool, lockPath string) (busyness, error) {
	b, err := m.busy(ctx, fresh)
	if err != nil {
		return b, err
	}
	if b.why != "" {
		return b, nil
	}
	if m.pastLoad != "" {
		return busyness{}, nil
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

func (m *machine) priorityExhausted(p *Priority) error {
	total, err := p.budgetTotal()
	amount := "unknown"
	if err == nil {
		amount = total.String()
	}
	lastWait := m.lastWaitText
	if lastWait == "" {
		lastWait = "this task did not wait"
	}
	spentNote := ""
	waited := m.waits.Total()
	if m.priorityBudget <= 0 {
		spentNote = "; budget already exhausted by other tasks of this deploy"
		waited = 0
	}
	now := m.priorityNow
	if now == "" {
		now = fmt.Sprintf("now: load average 1m %.2f, runnable %.0f, limit %.0f", m.avg1, m.runnable, m.runnableLimit)
	}
	refusal := fmt.Errorf("deploy.load_wait: budget exhausted (budget %s); last wait: %s%s; waited %s; budget on entry %s; %s; %s not started", amount, lastWait, spentNote, waited.Round(time.Millisecond), m.priorityBudget.Round(time.Millisecond), now, m.name)
	return errors.Join(refusal, err)
}

func (m *machine) priorityFailure(ctx context.Context, p *Priority, from, to time.Time) error {
	if !from.IsZero() {
		if _, err := p.spend(from, to); err != nil {
			return err
		}
	}
	refreshErr := m.refreshPriorityNow(ctx)
	return errors.Join(m.priorityExhausted(p), refreshErr)
}

func (m *machine) noteQueueWait(path string, from, to time.Time) {
	m.addWait(WhyQueue, from, to)
	holders, err := lock.Holders(path)
	if err == nil && len(holders) > 0 {
		m.waitedBehind = holders[0]
		m.lastWaitText = "queued behind " + holders[0]
	} else {
		m.lastWaitText = "queued in the FIFO lock"
	}
}

func (m *machine) refreshPriorityNow(ctx context.Context) error {
	lastView, haveView, decision, exitHatch := m.lastView, m.haveView, m.decision, m.exitHatch
	runnable, runnableLimit, avg1, avg1Known := m.runnable, m.runnableLimit, m.avg1, m.avg1Known
	_, err := m.busy(ctx, true)
	m.priorityNow = fmt.Sprintf("now: load average 1m %.2f, runnable %.0f, limit %.0f", m.avg1, m.runnable, m.runnableLimit)
	m.lastView, m.haveView, m.decision, m.exitHatch = lastView, haveView, decision, exitHatch
	m.runnable, m.runnableLimit, m.avg1, m.avg1Known = runnable, runnableLimit, avg1, avg1Known
	return err
}

func (m *machine) reserveWaitPause(ctx context.Context, b busyness, p *Priority) (time.Duration, error) {
	pause := m.pollInterval()
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
	files, err := lock.LiveDataRaw(filepath.Dir(lockPath), admittedPattern(lockPath))
	if err != nil {
		return "", time.Time{}, false, err
	}
	var firstTask string
	var retryAt time.Time
	for _, file := range files {
		d, at, unknownAdmission, err := decodeAdmissionRecord(file)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return "", time.Time{}, false, err
		}
		_, _, hasEMA, malformedMeasurement := parseAdmissionMeasurements(d)
		if !unknownAdmission && d.CPUError == "" && hasEMA && !malformedMeasurement {
			continue
		}
		if unknownAdmission {
			at = notAfter(at, now)
		}
		until := at.Add(grace)
		if now.Before(until) && (retryAt.IsZero() || until.Before(retryAt)) {
			task := d.Task
			if unknownAdmission || task == "" {
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

func liveRecordModTime(path string) (time.Time, error) {
	info, err := admittedRecordStat(path)
	if err != nil {
		return time.Time{}, fmt.Errorf("stat admitted record %s: %w", path, err)
	}
	return info.ModTime(), nil
}

func notAfter(at, now time.Time) time.Time {
	if at.After(now) {
		return time.Time{}
	}
	return at
}

// unseenShare is the share of an admitted task's cores the 1-minute load
// average does not show yet, by the time of its admission.
func (m *machine) unseenShare() func(admitted time.Time) float64 {
	now := reserveAgeNow()
	return func(admitted time.Time) float64 {
		return math.Exp(-max(now.Sub(admitted), 0).Seconds() / loadAvgTau.Seconds())
	}
}

type admittedEstimate struct {
	at             time.Time
	expected       float64
	observed       float64
	observedAt     time.Time
	hasObservation bool
	rate           float64
	ema            float64
	rateAt         time.Time
	hasEMA         bool
	cpuError       bool
}

// unseenAdmittedCores is how much load from running admitted tasks the load
// average does not show yet. Records without EMA and unreadable records use
// the conservative candidate/observed forecast; measured records use rate minus EMA60.
func unseenAdmittedCores(lockPath string, share func(admitted time.Time) float64) (float64, error) {
	files, err := lock.LiveDataRaw(filepath.Dir(lockPath), admittedPattern(lockPath))
	if err != nil {
		return 0, err
	}
	byAdmission := make(map[string]admittedEstimate, len(files))
	for _, file := range files {
		if err := mergeAdmissionEstimate(byAdmission, file); err != nil {
			return 0, err
		}
	}
	var sum float64
	for _, key := range slices.Sorted(maps.Keys(byAdmission)) {
		estimate := byAdmission[key]
		expected := estimate.expected * share(estimate.at)
		observed := estimate.observed * share(estimate.observedAt)
		if estimate.hasEMA && !estimate.cpuError {
			sum += max(0, estimate.rate-estimate.ema) * share(estimate.rateAt)
		} else {
			sum += max(expected, observed)
		}
	}
	return sum, nil
}

func mergeAdmissionEstimate(byAdmission map[string]admittedEstimate, file lock.LiveFile) error {
	d, at, unknownAdmission, err := decodeAdmissionRecord(file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	observedAt, rateAt, hasEMA, malformedMeasurement := parseAdmissionMeasurements(d)
	key := d.Admitted
	if key == "" || unknownAdmission {
		key = file.Path
	}
	old, ok := byAdmission[key]
	if !ok {
		old.at = at
	}
	old.expected = max(old.expected, d.Cores)
	if unknownAdmission {
		old.expected, old.cpuError = max(old.expected, 4), true
	}
	if d.CPUError != "" || malformedMeasurement {
		old.cpuError = true
	}
	if hasEMA && (!old.hasEMA || rateAt.After(old.rateAt)) {
		old.rate, old.ema, old.rateAt, old.hasEMA = d.Rate, d.EMA, rateAt, true
	}
	if !observedAt.IsZero() && (!old.hasObservation || observedAt.After(old.observedAt)) {
		old.observed, old.observedAt, old.hasObservation = d.ObservedCores, observedAt, true
	}
	byAdmission[key] = old
	return nil
}

func decodeAdmissionRecord(file lock.LiveFile) (reserveData, time.Time, bool, error) {
	var d reserveData
	if err := json.Unmarshal(file.Data, &d); err != nil { //nolint:error-masking // Malformed live records conservatively become unknown reservations.
		mtime, statErr := liveRecordModTime(file.Path)
		if statErr != nil {
			return d, time.Time{}, false, statErr
		}
		mtime = notAfter(mtime, reserveAgeNow())
		d.Cores = 4
		d.CPUError = "invalid admitted JSON"
		d.Admitted = file.Path
		return d, mtime, true, nil
	}
	at, err := time.Parse(time.RFC3339Nano, d.Admitted)
	unknownAdmission := err != nil
	if unknownAdmission {
		at, err = liveRecordModTime(file.Path)
		if err != nil {
			return d, time.Time{}, true, err
		}
		at = notAfter(at, reserveAgeNow())
	}
	return d, at, unknownAdmission, nil
}

func parseAdmissionMeasurements(d reserveData) (observedAt, rateAt time.Time, hasEMA, malformed bool) {
	malformedMeasurement := false
	if d.ObservedAt != "" {
		parsed, err := time.Parse(time.RFC3339Nano, d.ObservedAt)
		if err != nil { //nolint:fallback-return,silent-error-handling // An invalid observation time disables measurement and keeps the candidate fallback.
			malformedMeasurement = true
		} else {
			observedAt = parsed
		}
	}
	if d.At != "" {
		parsed, parseErr := time.Parse(time.RFC3339Nano, d.At)
		if parseErr != nil { //nolint:silent-error-handling // An invalid rate timestamp disables measurement and keeps the candidate fallback.
			malformedMeasurement = true
		} else if !malformedMeasurement {
			rateAt, hasEMA = parsed, true
		}
	}
	if malformedMeasurement {
		return observedAt, time.Time{}, false, true
	}
	return observedAt, rateAt, hasEMA, false
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
	var lastRate, lastEMA float64
	var lastAt time.Time
	recordObserved := func(rate, ema float64, observedAt time.Time) error {
		observedMu.Lock()
		defer observedMu.Unlock()
		if observedReleased {
			return nil
		}
		data, err := json.Marshal(reserveData{
			Cores: cores, Rate: rate, EMA: ema, At: observedAt.Format(time.RFC3339Nano),
			ObservedCores: rate, ObservedAt: observedAt.Format(time.RFC3339Nano),
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
		lastRate, lastEMA, lastAt = rate, ema, observedAt
		return nil
	}
	recordCPUError := func(readErr error) error {
		observedMu.Lock()
		defer observedMu.Unlock()
		if observedReleased {
			return nil
		}
		d := reserveData{Cores: cores, CPUError: readErr.Error(), Admitted: admitted, Task: name}
		if !lastAt.IsZero() {
			d.Rate, d.EMA = lastRate, lastEMA
			d.ObservedCores, d.ObservedAt = lastRate, lastAt.Format(time.RFC3339Nano)
		}
		data, err := json.Marshal(d)
		if err != nil {
			return fmt.Errorf("encoding CPU error for admitted task: %w", err)
		}
		rel, err := lock.Reserve(filepath.Dir(lockPath), admittedPattern(lockPath), data)
		if err != nil {
			return fmt.Errorf("recording CPU error for admitted task: %w", err)
		}
		if observedRel != nil {
			if err := observedRel(); err != nil {
				return errors.Join(err, rel())
			}
		}
		observedRel = rel
		return nil
	}
	release, since, end, stoppedAt, err := startReserve(lockPath, name, cores, cap, recordObserved, recordCPUError, log)
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
	cpuErrorActive   bool
	cpuErrorPending  bool
	writeErrorLogged bool
	lastEMAAt        time.Time
	ema              float64
	lastRate         float64
	haveRate         bool
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

func (state *reservePollState) updateEMA(speed float64, at time.Time) {
	if state.lastEMAAt.IsZero() {
		state.lastEMAAt = at
	}
	dt := max(at.Sub(state.lastEMAAt), 0).Seconds()
	if !state.haveRate {
		state.ema = speed + (state.ema-speed)*math.Exp(-dt/loadAvgTau.Seconds())
		state.haveRate = true
	} else {
		state.ema = state.lastRate + (state.ema-state.lastRate)*math.Exp(-dt/loadAvgTau.Seconds())
	}
	state.lastEMAAt = at
	state.lastRate = speed
}

func (state *reservePollState) updateObservation(speed float64, at time.Time, onCPU func(float64, float64, time.Time) error, force bool) (bool, error) {
	state.updateEMA(speed, at)
	if !force && state.haveObserved && !reserveEstimateChanged(state.observed, speed) {
		return true, nil
	}
	if onCPU != nil {
		if err := onCPU(speed, state.ema, at); err != nil {
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

func (state *reservePollState) handleTick(name string, cores float64, window time.Duration, readCPU func(int) (float64, error), onCPU func(float64, float64, time.Time) error, onError func(error) error, setEnd func(string), drop func(), log io.Writer) {
	cpu, err := readCPU(os.Getpid())
	if err != nil {
		newEpisode := !state.cpuErrorActive
		state.cpuErrorActive = true
		state.cpuErrorPending = true
		state.readError(name, err, log)
		if newEpisode && onError != nil {
			if recordErr := onError(err); recordErr != nil {
				state.logWriteError(name, recordErr, log)
			}
		}
		return
	}
	state.cpuErrorActive = false
	at := reservePollNow()
	speed, ready := state.sampleSpeed(cpu, at, window)
	if !ready {
		return
	}
	if !state.reserveDropped && speed < cores/2 {
		if state.cpuErrorPending {
			_, err := state.updateObservation(speed, at, onCPU, true)
			if err != nil {
				state.logWriteError(name, err, log)
				return
			}
			state.cpuErrorPending = false
			return
		}
		// Track the low-rate phase for EMA, but retain the expected reservation
		// until the existing half-candidate release threshold is reached.
		state.updateEMA(speed, at)
		return
	}
	updated, err := state.updateObservation(speed, at, onCPU, state.cpuErrorPending)
	if err != nil {
		state.logWriteError(name, err, log)
		return
	}
	if !updated {
		return
	}
	state.cpuErrorPending = false
	if state.reserveDropped {
		return
	}
	state.reserveDropped = true
	setEnd("cpu")
	drop()
}

func runReservePoll(name string, cores float64, cap, interval, window time.Duration, admitted time.Time, readCPU func(int) (float64, error), onCPU func(float64, float64, time.Time) error, onError func(error) error, log io.Writer, done <-chan struct{}, setEnd func(string), drop func()) {
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
	state := reservePollState{lastEMAAt: admitted}
	for {
		select {
		case <-done:
			return
		case <-deadlineC:
			deadlineC = nil
			setEnd("cap")
			drop()
		case <-ticker.C:
			state.handleTick(name, cores, window, readCPU, onCPU, onError, setEnd, drop, log)
		}
	}
}

func startReserve(lockPath, name string, cores float64, cap time.Duration, onCPU func(float64, float64, time.Time) error, onError func(error) error, log io.Writer) (func() error, time.Time, func() string, func() time.Time, error) {
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
	go runReservePoll(name, cores, cap, interval, window, now, readCPU, onCPU, onError, log, done, setEnd, drop)
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
	pastLoad          string
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
	waitSpans      []stats.WaitSpan
	candidate      float64
	candidateKnown bool
	decision       admissionDecision
	exitHatch      bool
	lastWaitText   string
	priorityBudget time.Duration
	priorityNow    string
	waitedBehind   string
}

func newMachine(l userconfig.MachineLoad, log io.Writer, name, kind string) *machine {
	proc := "/proc"
	if dir := os.Getenv(ProcEnv); dir != "" {
		proc = dir
	}
	limits, limitsErr := pressure.UserLimits()
	return &machine{l: l, limits: limits, limitsErr: limitsErr, log: log, name: name, kind: kind, now: time.Now, sleep: sleep, meter: pressureMeterFactory(proc), waits: Waits{}, admissionStart: time.Now()}
}

func (m *machine) admission() Admission {
	return Admission{Waits: m.waits, WaitSpans: m.waitSpans, WaitedBehind: m.waitedBehind}
}

func (m *machine) addWait(why string, from, to time.Time) {
	if !to.After(from) {
		return
	}
	m.waits[why] += to.Sub(from)
	// A wait polled again goes on where the last one ended: extend that span
	// rather than add one per poll, so a long wait stays one span.
	if n := len(m.waitSpans); n > 0 {
		last := &m.waitSpans[n-1]
		if last.Why == why && !from.After(last.End) && !from.Before(last.Start) {
			last.End = later(last.End, to)
			return
		}
	}
	m.waitSpans = append(m.waitSpans, stats.NewWaitSpan(from, to, why))
}

// later is the later of a and b.
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// behind is the admission of a task that waited in a queue behind holder.
func (m *machine) behind(holder string) Admission {
	adm := m.admission()
	if holder != "" {
		adm.WaitedBehind = holder
	}
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

// ioWindowFloor is the shortest IO window a look without aguard's disk state
// source measures over.
var ioWindowFloor = 5 * time.Second

// pollInterval is load.poll, but at least ioWindowFloor while the last look
// had no disk state source and measured IO pressure: akit measures it since
// the previous look then, so a shorter poll would shorten that window. A
// retry or a deadline may still wake the caller earlier.
func (m *machine) pollInterval() time.Duration {
	if m.haveView && m.lastView.Load.Disk == nil && !slices.Contains(m.lastView.Skipped, pressure.ErrNoIOPressure) {
		return max(m.l.Poll, ioWindowFloor)
	}
	return m.l.Poll
}

// Poll is the pause before the next Why, by the same rule as the admission
// queue's poll.
func (c *BusyChecker) Poll() time.Duration { return c.machine.pollInterval() }

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
	v = m.withoutCPUPressure(v)
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

func (m *machine) withoutCPUPressure(v pressure.Verdict) pressure.Verdict {
	if !m.ignoreCPUPressure && m.pastLoad == "" {
		return v
	}
	skipped := v.Skipped
	v = pressure.Rule(v.Load, m.limits, append(slices.Clone(skipped), pressure.ErrNoRunnable, pressure.ErrNoLoadAvg))
	v.Skipped = skipped
	return v
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
