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
	Cores    float64 `json:"cores"`
	Admitted string  `json:"admitted"`
	Task     string  `json:"task"`
}

var reservePollInterval = 250 * time.Millisecond
var reserveWindow = 2 * time.Second
var reserveCPUReader = treeCPU
var pressureMeterFactory = func(root string) machineMeter { return pressure.NewMeter(root) }

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
	// EnvThen is what a task does at the deadline: start or fail.
	EnvThen = "GRAFT_LOAD_THEN"
)

// Then values: at the deadline a priority task starts, or fails.
const (
	ThenStart = "start"
	ThenFail  = "fail"
)

// Priority is what a deploy gives its heavy tasks and those of the grafts
// its script starts: they skip the queue, a deploy not standing behind the
// test runs of every project, and wait for the machine out of one budget for
// the whole deploy, kept in the file Budget; when it is spent they start, or
// with Fail fail. Only waiting spends it: a task that finds the machine calm
// late in a long deploy takes nothing from it.
type Priority struct {
	For    string
	Budget string
	Fail   bool
}

// Env is the priority as the variables its grafts read.
func (p Priority) Env() map[string]string {
	then := ThenStart
	if p.Fail {
		then = ThenFail
	}
	return map[string]string{EnvPriority: p.For, EnvBudget: p.Budget, EnvThen: then}
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
// A task with a priority skips the queue and waits no longer than its budget.
//
// kind is the task's kind as the stats profile names it, which the disk state
// source keeps its profiles by.
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
	if p != nil && (settleLockPath == "" || settleDuration <= 0) {
		return reservePriority(ctx, m, name, kind, p, l, lockPath, cores, log)
	}
	holder := "graft " + name + " (starting a heavy task)"
	settleLock, behind, start, err := acquireSettle(ctx, m, settleLockPath, settleDuration, holder, log)
	if err != nil {
		return m.admission(), released, err
	}
	return waitQueued(ctx, m, p, kind, l, lockPath, cores, settleLock, settleDuration, start, behind, log)
}

func acquireSettle(ctx context.Context, m *machine, settleLockPath string, settleDuration time.Duration, holder string, log io.Writer) (*lock.Lock, string, time.Time, error) {
	start := time.Now()
	if settleLockPath == "" || settleDuration <= 0 {
		return nil, "", start, nil
	}
	settleLock, err := lock.AcquireFIFOObserved(ctx, settleLockPath, queueTimeout, holder+" with settle", log, nil)
	m.waits[WhyQueue] += time.Since(start)
	if err != nil {
		return nil, "", start, err
	}
	return settleLock, settleLock.WaitedBehind, time.Now(), nil
}

func waitQueued(ctx context.Context, m *machine, p *Priority, kind string, l userconfig.MachineLoad, lockPath string, cores float64, settleLock *lock.Lock, settleDuration time.Duration, start time.Time, behind string, log io.Writer) (Admission, func() error, error) {
	if p != nil {
		return waitPriority(ctx, m, p, kind, l, lockPath, cores, settleLock, settleDuration, behind, log)
	}
	quiet, err := quietMode()
	if err != nil {
		if settleLock != nil {
			err = errors.Join(err, settleLock.Release())
		}
		return m.behind(behind), released, err
	}
	holder := "graft " + m.name + " (starting a heavy task)"
	lk, err := lock.AcquireFIFOObserved(ctx, lockPath, queueTimeout, holder, log, quiet)
	waited := time.Since(start)
	if lk != nil && lk.WaitedQuiet > 0 {
		m.waits[WhyQuiet] += min(lk.WaitedQuiet, waited)
	}
	m.waits[WhyQueue] += max(0, waited-m.waits[WhyQuiet])
	if lk != nil && lk.WaitedBehind != "" {
		behind = lk.WaitedBehind
	}
	if err != nil {
		if settleLock != nil {
			err = errors.Join(err, settleLock.Release())
		}
		return m.admission(), released, err
	}
	if err := m.admitWithReserves(ctx, nil, lockPath); err != nil {
		if settleLock != nil {
			err = errors.Join(err, settleLock.Release())
		}
		return m.behind(behind), released, errors.Join(err, lk.Release())
	}
	cores, err = expectedReserveCores(kind, cores)
	if err != nil {
		if settleLock != nil {
			err = errors.Join(err, settleLock.Release())
		}
		return m.behind(behind), released, errors.Join(err, lk.Release())
	}
	release, since, end, stoppedAt, err := startAdmission(lockPath, m.name, cores, l.Settle, log)
	if err != nil {
		if settleLock != nil {
			err = errors.Join(err, settleLock.Release())
		}
		return m.behind(behind), released, errors.Join(err, lk.Release())
	}
	if err := lk.Release(); err != nil {
		releaseErr := release()
		if settleLock != nil {
			err = errors.Join(err, settleLock.Release())
		}
		return m.behind(behind), released, errors.Join(err, releaseErr)
	}
	adm := m.behind(behind)
	adm.ReserveCores, adm.ReserveSince, adm.ReserveEnd, adm.ReserveStoppedAt = cores, since, end, stoppedAt
	if settleLock != nil {
		settleRelease := settle(settleLock, settleDuration)
		return adm, func() error { return errors.Join(release(), settleRelease()) }, nil
	}
	return adm, release, nil
}

// waitPriority admits a task with a priority that took the settle lock as
// reservePriority does: beside the machine it waits for the live reserves and
// the admitted tasks the load average does not show yet, spending its budget
// on them too, and leaves its own reserve. It holds the lock for its settling
// period.
func waitPriority(ctx context.Context, m *machine, p *Priority, kind string, l userconfig.MachineLoad, lockPath string, cores float64, settleLock *lock.Lock, settleDuration time.Duration, behind string, log io.Writer) (Admission, func() error, error) {
	adm, release, err := reservePriority(ctx, m, m.name, kind, p, l, lockPath, cores, log)
	adm.WaitedBehind = behind
	if err != nil {
		var releaseErr error
		if settleLock != nil {
			releaseErr = settleLock.Release()
		}
		return adm, released, errors.Join(err, releaseErr)
	}
	settleRelease := settle(settleLock, settleDuration)
	return adm, func() error { return errors.Join(release(), settleRelease()) }, nil
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
		b, err := m.busy(ctx, !waited, p != nil)
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
		if b.why == "" {
			if b, err = m.reservesBusy(lockPath, p != nil); err != nil {
				return err
			}
			if b.why == "" {
				if waited {
					fmt.Fprintf(m.log, "graft: %s: runnable %.0f, starting after %s\n", m.name, m.runnable, m.now().Sub(start).Round(time.Second))
				}
				return nil
			}
		}
		// The measure or the reserves beside it: either keeps the task waiting.
		m.recordWait(b, p != nil)
		if p != nil && !waited {
			fmt.Fprintf(m.log, "graft: %s: %s, not queueing behind other heavy tasks: it runs for %s\n", m.name, b.text, p.For)
		}
		waited = true
		if p != nil {
			now := m.now()
			left, err := p.spend(spent, now)
			if err != nil {
				return err
			}
			spent = now
			if left <= 0 {
				if p.Fail {
					return fmt.Errorf("%s: %s after %s, past the wait %s allows (deploy.load_wait)", m.name, b.text, m.now().Sub(start).Round(time.Second), p.For)
				}
				fmt.Fprintf(m.log, "graft: %s: %s, starting after %s, all the wait %s allows (deploy.load_wait)\n", m.name, b.text, m.now().Sub(start).Round(time.Second), p.For)
				return nil
			}
		}
		m.waits[b.why] += 0
		why = b.why
		if m.now().Sub(said) >= time.Minute {
			fmt.Fprintf(m.log, "graft: %s: %s, waiting\n", m.name, b.text)
			said = m.now()
		}
		pause := m.l.Poll
		if p != nil {
			left, err := p.spend(time.Time{}, time.Time{})
			if err != nil {
				return err
			}
			pause = min(pause, left)
		}
		if err := m.pause(ctx, pause, p != nil); err != nil {
			if why != "" {
				m.waits[why] += m.now().Sub(mark)
			}
			return err
		}
	}
}

const prioritySampleInterval = 500 * time.Millisecond

func (m *machine) pause(ctx context.Context, d time.Duration, priority bool) error {
	if !priority {
		return m.sleep(ctx, d)
	}
	for d > 0 {
		step := min(d, prioritySampleInterval)
		if err := m.sleep(ctx, step); err != nil {
			return err
		}
		d -= step
		if _, err := m.observeKernelSample(); err != nil {
			m.warnKernelSampleError(err)
		}
	}
	return nil
}

func (m *machine) loadValue(priority bool) float64 {
	if priority {
		if a, b, ok := m.samples.lastTwo(); ok {
			return (a + b) / 2
		}
	}
	return m.avg1
}

// reservesBusy is why the machine m measured last has no room for one more
// heavy task beside the live reserves and the admitted tasks its load average
// does not show yet, empty when it has. Without a runnable limit (no
// /proc/stat to count the threads from, as on Windows, and no runnable in
// pressure.yaml, a measure busy skips and tells) there is nothing to reach.
func (m *machine) reservesBusy(lockPath string, priority bool) (busyness, error) {
	if m.runnableLimit <= 0 {
		return busyness{}, nil
	}
	sum, err := liveReserveCores(lockPath)
	if err != nil {
		return busyness{}, err
	}
	unseen, err := unseenAdmittedCores(lockPath, m.now())
	if err != nil {
		return busyness{}, err
	}
	switch {
	case float64(m.runnable)+sum >= m.runnableLimit:
		return busyness{why: WhyReserve, text: fmt.Sprintf("runnable %.0f plus reserves %.1f reaches %.0f", m.runnable, sum, m.runnableLimit)}, nil
	case m.avg1Known && m.loadValue(priority)+unseen >= m.runnableLimit:
		load := m.avg1
		text := fmt.Sprintf("load average 1m %.2f plus %.1f of the admitted tasks it does not show yet reaches %.0f", load, unseen, m.runnableLimit)
		if a, b, ok := m.samples.lastTwo(); priority && ok {
			text = fmt.Sprintf("kernel load samples %.1f, %.1f plus %.1f of the admitted tasks it does not show yet reach %.0f", a, b, unseen, m.runnableLimit)
		}
		return busyness{why: WhyReserve, text: text}, nil
	}
	return busyness{}, nil
}

// loadAvgTau is the time constant of the kernel's 1-minute load average: a
// load of c that starts now shows in it as c·(1 − e^(−t/τ)).
const loadAvgTau = time.Minute

// admittedPattern names the live record of an admitted task that lives as
// long as the task, past its reserve.
func admittedPattern(lockPath string) string { return filepath.Base(lockPath) + ".*.admitted" }

// unseenAdmittedCores is how much of the expected cores of the running
// admitted tasks the 1-minute load average does not show yet at now: c·e^(−t/τ)
// each, t since its admission. The load average of a queue's dip is that of
// the machine before the tasks it let in: the next counts them so.
func unseenAdmittedCores(lockPath string, now time.Time) (float64, error) {
	files, err := lock.LiveData(filepath.Dir(lockPath), admittedPattern(lockPath))
	if err != nil {
		return 0, err
	}
	var sum float64
	for _, file := range files {
		var d reserveData
		if err := json.Unmarshal(file.Data, &d); err != nil {
			return 0, fmt.Errorf("decoding admitted task %s: %w", file.Path, err)
		}
		at, err := time.Parse(time.RFC3339Nano, d.Admitted)
		if err != nil {
			return 0, fmt.Errorf("admitted task %s: %w", file.Path, err)
		}
		sum += d.Cores * math.Exp(-max(now.Sub(at), 0).Seconds()/loadAvgTau.Seconds())
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

func reservePriority(ctx context.Context, m *machine, name, kind string, p *Priority, l userconfig.MachineLoad, lockPath string, cores float64, log io.Writer) (Admission, func() error, error) {
	if err := m.admitWithReserves(ctx, p, lockPath); err != nil {
		return m.admission(), released, err
	}
	cores, err := expectedReserveCores(kind, cores)
	if err != nil {
		return m.admission(), released, err
	}
	rel, since, end, stoppedAt, err := startAdmission(lockPath, name, cores, l.Settle, log)
	if err != nil {
		return m.admission(), released, err
	}
	a := m.admission()
	a.ReserveCores, a.ReserveSince, a.ReserveEnd, a.ReserveStoppedAt = cores, since, end, stoppedAt
	return a, rel, nil
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
	raw, err := json.Marshal(reserveData{Cores: cores, Admitted: time.Now().Format(time.RFC3339Nano), Task: name})
	if err != nil {
		return released, time.Now(), nil, nil, fmt.Errorf("encoding admitted task: %w", err)
	}
	admittedRel, err := lock.Reserve(filepath.Dir(lockPath), admittedPattern(lockPath), raw)
	if err != nil {
		return released, time.Now(), nil, nil, err
	}
	release, since, end, stoppedAt, err := startReserve(lockPath, name, cores, cap, log)
	if err != nil {
		return released, since, nil, nil, errors.Join(err, admittedRel())
	}
	var once sync.Once
	var admittedErr error
	return func() error {
		err := release()
		once.Do(func() { admittedErr = admittedRel() })
		return errors.Join(err, admittedErr)
	}, since, end, stoppedAt, nil
}

func startReserve(lockPath, name string, cores float64, cap time.Duration, log io.Writer) (func() error, time.Time, func() string, func() time.Time, error) {
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
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(log, "graft: %s: panic in reserve poll: %v\n", name, r)
				setEnd("panic")
				drop()
			}
		}()
		type sample struct {
			at  time.Time
			cpu float64
		}
		samples := []sample{}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		deadline := time.NewTimer(cap)
		defer deadline.Stop()
		unread := false
		for {
			select {
			case <-done:
				return
			case <-deadline.C:
				setEnd("cap")
				drop()
				return
			case <-ticker.C:
				if unread {
					continue
				}
				cpu, err := readCPU(os.Getpid())
				if err != nil {
					fmt.Fprintf(log, "graft: %s: reading reserved CPU: %v\n", name, err)
					setEnd("unread")
					unread = true
					continue
				}
				at := time.Now()
				samples = append(samples, sample{at, cpu})
				for len(samples) > 1 && at.Sub(samples[1].at) >= window {
					samples = samples[1:]
				}
				if len(samples) > 1 && at.Sub(samples[0].at) >= window && (cpu-samples[0].cpu)/at.Sub(samples[0].at).Seconds() >= cores/2 {
					setEnd("cpu")
					drop()
					return
				}
			}
		}
	}()
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

const (
	loadSampleMaxGap = time.Second
	loadSampleStale  = 5500 * time.Millisecond
	loadFactor       = 1884.0 / 2048
)

// kernelSamples recovers kernel load samples from consecutive visible averages.
type kernelSamples struct {
	at, stableAt time.Time
	avg          float64
	have         bool

	values []float64
}

func (s *kernelSamples) observe(at time.Time, avg float64, valid bool) {
	if !valid {
		*s = kernelSamples{}
		return
	}
	if !s.have || at.Before(s.at) || at.Sub(s.at) > loadSampleMaxGap {
		s.values = nil
		s.at, s.avg, s.have = at, avg, true
		s.stableAt = at
		return
	}
	if avg != s.avg {
		delta := (avg - loadFactor*s.avg) / (1 - loadFactor)
		if delta >= 0 {
			s.append(delta)
		}
		s.avg, s.at, s.stableAt = avg, at, at
		return
	}
	if at.Sub(s.stableAt) > loadSampleStale {
		s.append(avg)
		s.stableAt = at
	}
	s.at = at
}

func (s *kernelSamples) append(sample float64) {
	s.values = append(s.values, sample)
	if len(s.values) > 2 {
		s.values = s.values[len(s.values)-2:]
	}
}

func (s *kernelSamples) lastTwo() (float64, float64, bool) {
	if len(s.values) < 2 {
		return 0, 0, false
	}
	return s.values[len(s.values)-2], s.values[len(s.values)-1], true
}

func (m *machine) observeKernelSample() (float64, error) {
	at := m.now()
	avg, err := pressure.ReadLoadAvg(m.procRoot)
	if err != nil {
		m.samples.observe(at, 0, false)
		return 0, fmt.Errorf("read kernel load sample: %w", err)
	}
	m.samples.observe(at, avg.Avg1, true)
	return avg.Avg1, nil
}

func (m *machine) warnKernelSampleError(err error) {
	if m.sampleReadWarned {
		return
	}
	fmt.Fprintf(m.log, "graft: %s: cannot read kernel load sample: %v; using load average 1m\n", m.name, err)
	m.sampleReadWarned = true
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
	admissionStart   time.Time
	procRoot         string
	now              func() time.Time
	sleep            func(context.Context, time.Duration) error
	samples          kernelSamples
	sampleReadWarned bool
	// runnable is the last count of the tasks ready to run.
	runnable      float64
	runnableLimit float64
	// avg1 is the last 1-minute load average, when avg1Known.
	avg1      float64
	avg1Known bool
	// unchecked holds the measures the system does not have, told once.
	unchecked map[error]bool
	// waits is how long admit waited, by why.
	waits Waits
}

func newMachine(l userconfig.MachineLoad, log io.Writer, name, kind string) *machine {
	proc := "/proc"
	if dir := os.Getenv(ProcEnv); dir != "" {
		proc = dir
	}
	limits, limitsErr := pressure.UserLimits()
	return &machine{l: l, limits: limits, limitsErr: limitsErr, log: log, name: name, kind: kind, procRoot: proc, now: time.Now, sleep: sleep, meter: pressureMeterFactory(proc), waits: Waits{}, admissionStart: time.Now()}
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
// controls whether a current queue holder also counts; a task with a
// priority, which Wait does not queue, leaves it out.
func BusyNowWithReserves(ctx context.Context, name, kind string, l userconfig.MachineLoad, lockPath string, queue bool, log io.Writer) (bool, error) {
	if queue {
		holders, err := lock.Holders(lockPath)
		if err != nil || len(holders) > 0 {
			return err == nil, err
		}
	}
	m := newMachine(l, log, name, kind)
	b, err := m.busy(ctx, true, false)
	if err != nil || b.why != "" || lockPath == "" {
		return b.why != "", err
	}
	b, err = m.reservesBusy(lockPath, false)
	return b.why != "", err
}

// BusyChecker keeps the pressure meter across readings, so repeated checks
// measure since the previous reading without waiting for another swap window.
type BusyChecker struct{ machine *machine }

// NewDiskBusyChecker checks disk, writeback, swap and quiet mode before the
// next batch of an already-running process. The batch mainly adds disk load,
// so CPU runnable and load-average limits do not apply to it.
func NewDiskBusyChecker(name, kind string, l userconfig.MachineLoad, log io.Writer) *BusyChecker {
	checker := &BusyChecker{machine: newMachine(l, log, name, kind)}
	checker.machine.limits.Runnable = math.Inf(1)
	return checker
}

// Why reports the current load rule that prevents work, or an empty string.
func (c *BusyChecker) Why(ctx context.Context) (string, error) {
	b, err := c.machine.busy(ctx, false, false)
	return b.why, err
}

// busyness is why the machine cannot take a heavy task, one of the Why
// constants, and the words that say it; a calm machine has neither.
type busyness struct{ why, text string }

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
func (m *machine) busy(ctx context.Context, fresh, priority bool) (busyness, error) {
	if m.limitsErr != nil {
		return busyness{}, fmt.Errorf("reading pressure limits: %w", m.limitsErr)
	}
	v, err := m.meter.Check(ctx, m.limits, m.kind, fresh)
	if err != nil {
		m.samples.observe(m.now(), 0, false)
		return busyness{}, fmt.Errorf("measuring the machine: %w", err)
	}
	// A look that the wait's end cut short is no measure: its disk source
	// failed for the deadline, not for the machine.
	if err := ended(ctx); err != nil {
		m.samples.observe(m.now(), 0, false)
		return busyness{}, err
	}
	if priority {
		avg, err := m.observeKernelSample()
		if err != nil {
			m.warnKernelSampleError(err)
		} else {
			v.Load.Avg1 = avg
		}
	}
	noPSI := slices.Contains(v.Skipped, pressure.ErrNoIOPressure)
	for _, none := range v.Skipped {
		m.skip(none, v.Load.DiskFault, noPSI)
	}
	m.runnable = float64(v.Load.Runnable)
	m.avg1, m.avg1Known = v.Load.Avg1, !slices.Contains(v.Skipped, pressure.ErrNoLoadAvg)
	m.runnableLimit = float64(v.Load.Threads)
	if m.limits.Runnable > 0 {
		m.runnableLimit = m.limits.Runnable
	}
	if v.Measure == pressure.CPU {
		m.runnableLimit = v.Limit
	}
	var sampleMean float64
	if priority {
		if a, b, ok := m.samples.lastTwo(); ok {
			sampleMean = (a + b) / 2
			v.Load.Avg1 = sampleMean
			v = pressure.Rule(v.Load, m.limits, v.Skipped)
		}
	}
	m.lastView, m.haveView = v, true
	var b busyness
	if v.Busy() {
		// A measure graft does not know yet still makes the machine busy.
		ms, known := measures[v.Measure]
		b = busyness{v.Measure, v.Why()}
		if priority && v.Measure == pressure.CPU && float64(v.Load.Runnable) < v.Limit {
			a, last, ok := m.samples.lastTwo()
			if ok {
				b.text = fmt.Sprintf("kernel load samples %.1f, %.1f reach %.0f", a, last, v.Limit)
			}
		}
		if known {
			b.why = ms.why
			if b.text == v.Why() {
				b.text = fmt.Sprintf("%s (%s)", v.Why(), ms.setting)
			}
		}
	}
	return b, nil
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
