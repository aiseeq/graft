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
	// WhyLoad, WhyIO and WhySwap are the waits for the load, the IO
	// pressure and the swapping to fall below their limits.
	WhyLoad = "load"
	WhyIO   = "io"
	WhySwap = "swap"
	// WhyReserve is waiting for live expected-core reservations.
	WhyReserve = "reserve"
	// WhyQuiet is the wait for the quiet mode akit's busy --quiet-until
	// sets to end.
	WhyQuiet = "quiet"
)

// Waits is how long a heavy task waited, by why.
type Waits map[string]time.Duration

// Admission is what a heavy task's admission saw: how long it waited, by
// why, and the profile the disk state source gave its kind at the last
// reading, nil without the source.
type Admission struct {
	Waits            Waits
	DiskProfile      json.RawMessage
	WaitedBehind     string
	ReserveCores     float64
	ReserveSince     time.Time
	ReserveEnd       func() string
	ReserveStoppedAt func() time.Time
}

type reserveData struct {
	Cores    float64 `json:"cores"`
	Admitted string  `json:"admitted"`
	Task     string  `json:"task"`
}

var reservePollInterval = 250 * time.Millisecond
var reserveWindow = 2 * time.Second
var reserveCPUReader = treeCPU

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

// spend takes d from the budget and returns what is left, under a lock: the
// heavy tasks of a deploy wait side by side, and their waits add up.
func (p *Priority) spend(d time.Duration) (left time.Duration, err error) {
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
	left = time.Duration(n) - d
	if d > 0 {
		if err := os.WriteFile(p.Budget, []byte(strconv.FormatInt(int64(max(left, 0)), 10)), 0o644); err != nil {
			return 0, fmt.Errorf("the budget for waiting: %w", err)
		}
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
	return WaitWithSettle(ctx, name, kind, p, l, lockPath, "", 0, log)
}

// WaitWithSettle admits a heavy task and, when settleLockPath is set,
// serializes tasks that requested their own settling period on that lock.
// The ordinary queue writes a live reservation before releasing its lock.
func WaitWithSettle(ctx context.Context, name, kind string, p *Priority, l userconfig.MachineLoad, lockPath, settleLockPath string, settleDuration time.Duration, log io.Writer) (Admission, func() error, error) {
	m := newMachine(l, log, name, kind)
	if p != nil && (settleLockPath == "" || settleDuration <= 0) {
		return reservePriority(ctx, m, name, kind, p, l, lockPath, log)
	}
	holder := "graft " + name + " (starting a heavy task)"
	var settleLock *lock.Lock
	start := time.Now()
	behind := ""
	if settleLockPath != "" && settleDuration > 0 {
		var err error
		settleLock, err = lock.AcquireFIFOObserved(ctx, settleLockPath, queueTimeout, holder+" with settle", log, nil)
		m.waits[WhyQueue] += time.Since(start)
		if err != nil {
			return m.admission(), released, err
		}
		behind = settleLock.WaitedBehind
		start = time.Now()
	}
	if p != nil {
		if err := m.admit(ctx, p); err != nil {
			var releaseErr error
			if settleLock != nil {
				releaseErr = settleLock.Release()
			}
			return m.behind(behind), released, errors.Join(err, releaseErr)
		}
		return m.behind(behind), settle(settleLock, settleDuration), nil
	}
	quiet, err := quietMode()
	if err != nil {
		if settleLock != nil {
			err = errors.Join(err, settleLock.Release())
		}
		return m.behind(behind), released, err
	}
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
	cores, err := stats.ExpectedCores(kind)
	if err != nil {
		if settleLock != nil {
			err = errors.Join(err, settleLock.Release())
		}
		return m.behind(behind), released, errors.Join(err, lk.Release())
	}
	release, since, end, stoppedAt, err := startReserve(lockPath, name, cores, l.Settle, log)
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

func (m *machine) admitWithReserves(ctx context.Context, p *Priority, lockPath string) error {
	start, mark := time.Now(), time.Now()
	spent := start
	var said time.Time
	waited := false
	var why string
	for {
		b, err := m.busy(ctx, !waited)
		now := time.Now()
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
			sum, err := liveReserveCores(lockPath)
			if err != nil {
				return err
			}
			if float64(m.runnable)+sum < m.runnableLimit {
				if waited {
					fmt.Fprintf(m.log, "graft: %s: runnable %.0f, starting after %s\n", m.name, m.runnable, time.Since(start).Round(time.Second))
				}
				return nil
			}
			b = busyness{why: WhyReserve, text: fmt.Sprintf("runnable %.0f plus reserves %.1f reaches %.0f", m.runnable, sum, m.runnableLimit)}
		}
		if p != nil && !waited {
			fmt.Fprintf(m.log, "graft: %s: %s, not queueing behind other heavy tasks: it runs for %s\n", m.name, b.text, p.For)
		}
		waited = true
		if p != nil {
			now := time.Now()
			left, err := p.spend(now.Sub(spent))
			if err != nil {
				return err
			}
			spent = now
			if left <= 0 {
				if p.Fail {
					return fmt.Errorf("%s: %s after %s, past the wait %s allows (deploy.load_wait)", m.name, b.text, time.Since(start).Round(time.Second), p.For)
				}
				fmt.Fprintf(m.log, "graft: %s: %s, starting after %s, all the wait %s allows (deploy.load_wait)\n", m.name, b.text, time.Since(start).Round(time.Second), p.For)
				return nil
			}
		}
		m.waits[b.why] += 0
		why = b.why
		if time.Since(said) >= time.Minute {
			fmt.Fprintf(m.log, "graft: %s: %s, waiting\n", m.name, b.text)
			said = time.Now()
		}
		pause := m.l.Poll
		if p != nil {
			left, err := p.spend(0)
			if err != nil {
				return err
			}
			pause = min(pause, left)
		}
		if err := sleep(ctx, pause); err != nil {
			if why != "" {
				m.waits[why] += time.Since(mark)
			}
			return err
		}
	}
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

func reservePriority(ctx context.Context, m *machine, name, kind string, p *Priority, l userconfig.MachineLoad, lockPath string, log io.Writer) (Admission, func() error, error) {
	if err := m.admitWithReserves(ctx, p, lockPath); err != nil {
		return m.admission(), released, err
	}
	cores, err := stats.ExpectedCores(kind)
	if err != nil {
		return m.admission(), released, err
	}
	rel, since, end, stoppedAt, err := startReserve(lockPath, name, cores, l.Settle, log)
	if err != nil {
		return m.admission(), released, err
	}
	a := m.admission()
	a.ReserveCores, a.ReserveSince, a.ReserveEnd, a.ReserveStoppedAt = cores, since, end, stoppedAt
	return a, rel, nil
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
	drop := func() {
		if err := rel(); err != nil {
			fmt.Fprintf(log, "graft: %s: releasing reserve: %v\n", name, err)
		}
		setStoppedAt()
	}
	go func() {
		type sample struct {
			at  time.Time
			cpu float64
		}
		samples := []sample{}
		ticker := time.NewTicker(reservePollInterval)
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
				cpu, err := reserveCPUReader(os.Getpid())
				if err != nil {
					fmt.Fprintf(log, "graft: %s: reading reserved CPU: %v\n", name, err)
					setEnd("unread")
					unread = true
					continue
				}
				at := time.Now()
				samples = append(samples, sample{at, cpu})
				for len(samples) > 1 && at.Sub(samples[1].at) >= reserveWindow {
					samples = samples[1:]
				}
				if len(samples) > 1 && at.Sub(samples[0].at) >= reserveWindow && (cpu-samples[0].cpu)/at.Sub(samples[0].at).Seconds() >= cores/2 {
					setEnd("cpu")
					drop()
					return
				}
			}
		}
	}()
	return func() error {
		once.Do(func() { setEnd("done"); close(done) })
		err := rel()
		setStoppedAt()
		return err
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

// machine reads what makes the machine busy for one task.
type machine struct {
	l          userconfig.MachineLoad
	limits     pressure.Limits
	limitsErr  error
	log        io.Writer
	name, kind string
	meter      *pressure.Meter
	// disk is the disk state source's profile of the kind at the last
	// reading.
	disk json.RawMessage
	// runnable is the last count of the tasks ready to run.
	runnable      float64
	runnableLimit float64
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
	return &machine{l: l, limits: limits, limitsErr: limitsErr, log: log, name: name, kind: kind, meter: pressure.NewMeter(proc), waits: Waits{}}
}

func (m *machine) admission() Admission { return Admission{Waits: m.waits, DiskProfile: m.disk} }

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
// the queue out, for a task with a priority, which Wait does not queue.
func BusyNow(ctx context.Context, name, kind string, l userconfig.MachineLoad, lockPath string, log io.Writer) (bool, error) {
	return BusyNowWithReserves(ctx, name, kind, l, lockPath, lockPath != "", log)
}

// BusyNowWithReserves checks the machine and its live reservations without
// queueing. queue controls whether a current queue holder also counts.
func BusyNowWithReserves(ctx context.Context, name, kind string, l userconfig.MachineLoad, lockPath string, queue bool, log io.Writer) (bool, error) {
	if queue {
		holders, err := lock.Holders(lockPath)
		if err != nil || len(holders) > 0 {
			return err == nil, err
		}
	}
	m := newMachine(l, log, name, kind)
	b, err := m.busy(ctx, true)
	if err != nil || b.why != "" {
		return b.why != "", err
	}
	sum, err := liveReserveCores(lockPath)
	return float64(m.runnable)+sum >= m.runnableLimit, err
}

// BusyChecker keeps the pressure meter across readings, so repeated checks
// measure since the previous reading without waiting for another swap window.
type BusyChecker struct{ machine *machine }

// NewBusyChecker makes an immediate machine-load checker for a task kind.
func NewBusyChecker(name, kind string, l userconfig.MachineLoad, log io.Writer) *BusyChecker {
	return &BusyChecker{machine: newMachine(l, log, name, kind)}
}

// Busy reports whether the machine currently fails the load rule.
func (c *BusyChecker) Busy(ctx context.Context) (bool, error) {
	b, err := c.machine.busy(ctx, false)
	return b.why != "", err
}

// busyness is why the machine cannot take a heavy task, one of the Why
// constants, and the words that say it; a calm machine has neither.
type busyness struct{ why, text string }

// measures are the rule's measures as the waits name them, with what sets
// each limit.
var measures = map[string]struct{ why, setting string }{
	pressure.CPU:   {WhyLoad, "pressure.yaml runnable"},
	pressure.IO:    {WhyIO, "pressure.yaml max_io_full"},
	pressure.Swap:  {WhySwap, "pressure.yaml max_mem_some, max_swap_kbs"},
	pressure.Quiet: {WhyQuiet, "busy --quiet-off ends it"},
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
	if m.limitsErr != nil {
		return busyness{}, fmt.Errorf("reading pressure limits: %w", m.limitsErr)
	}
	v, err := m.meter.Check(ctx, m.limits, m.kind, fresh)
	if err != nil {
		return busyness{}, fmt.Errorf("measuring the machine: %w", err)
	}
	// A look that the wait's end cut short is no measure: its disk source
	// failed for the deadline, not for the machine.
	if err := ctx.Err(); err != nil {
		return busyness{}, err
	}
	noPSI := slices.Contains(v.Skipped, pressure.ErrNoIOPressure)
	for _, none := range v.Skipped {
		m.skip(none, v.Load.DiskFault, noPSI)
	}
	m.runnable = float64(v.Load.Runnable)
	m.runnableLimit = float64(v.Load.Threads)
	if m.limits.Runnable > 0 {
		m.runnableLimit = m.limits.Runnable
	}
	if v.Measure == pressure.CPU {
		m.runnableLimit = v.Limit
	}
	m.disk = nil
	if v.Load.Disk != nil {
		m.disk = v.Load.Disk.Profile
	}
	var b busyness
	if v.Busy() {
		// A measure graft does not know yet still makes the machine busy.
		ms, known := measures[v.Measure]
		b = busyness{v.Measure, v.Why()}
		if known {
			b = busyness{ms.why, fmt.Sprintf("%s (%s)", v.Why(), ms.setting)}
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

// admit waits for the machine to be calm; a priority task waits no longer
// than its budget allows, spending it as it waits. The time between two
// readings goes to what the first of them found busy, the first reading's
// own to what it found.
func (m *machine) admit(ctx context.Context, p *Priority) error {
	start := time.Now()
	spent, mark := start, start
	var said time.Time
	waited := false
	why := ""
	for {
		b, err := m.busy(ctx, said.IsZero() && !waited)
		now := time.Now()
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
			if waited {
				fmt.Fprintf(m.log, "graft: %s: runnable %.0f, starting after %s\n", m.name, m.runnable, time.Since(start).Round(time.Second))
			}
			return nil
		}
		if p != nil && !waited {
			fmt.Fprintf(m.log, "graft: %s: %s, not queueing behind other heavy tasks: it runs for %s\n", m.name, b.text, p.For)
		}
		waited = true
		var left time.Duration
		if p != nil {
			now := time.Now()
			if left, err = p.spend(now.Sub(spent)); err != nil {
				return err
			}
			spent = now
		}
		if p != nil && left <= 0 {
			if p.Fail {
				return fmt.Errorf("%s: %s after %s, past the wait %s allows (deploy.load_wait)", m.name, b.text, time.Since(start).Round(time.Second), p.For)
			}
			fmt.Fprintf(m.log, "graft: %s: %s, starting after %s, all the wait %s allows (deploy.load_wait)\n", m.name, b.text, time.Since(start).Round(time.Second), p.For)
			return nil
		}
		if time.Since(said) >= time.Minute {
			fmt.Fprintf(m.log, "graft: %s: %s, waiting\n", m.name, b.text)
			said = time.Now()
		}
		pause := m.l.Poll
		if p != nil {
			pause = min(pause, left)
		}
		if err := sleep(ctx, pause); err != nil {
			return err
		}
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
