package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

const (
	trimProbeInterval = 200 * time.Millisecond
	trimProbeSlow     = 250 * time.Millisecond
	trimProbeLifetime = 2 * time.Hour
)

var startProbeUnit = func(command, binary string, pid int, start, dir string) error {
	unit := fmt.Sprintf("graft-gocache-trim-probe-%d", pid)
	args := []string{"--user", "--collect", "--unit", unit, "--", binary, "gocache", "probe",
		"--while", fmt.Sprintf("%d:%s", pid, start), "--dir", dir}
	out, err := exec.Command(command, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", command, err, strings.Join(strings.Fields(string(out)), " "))
	}
	return nil
}

func startTrimProbe(stderr io.Writer) {
	command := os.Getenv("GRAFT_GOCACHE_PROBE")
	if command == "off" {
		fmt.Fprintln(stderr, "graft: gocache trim: probe off")
		return
	}
	if command == "" {
		command = "systemd-run"
	}
	binary, err := os.Executable()
	if err == nil {
		var home string
		home, err = os.UserHomeDir()
		if err == nil {
			pid := os.Getpid()
			var start string
			start, err = processStart(pid)
			if err == nil {
				err = startProbeUnit(command, binary, pid, start, filepath.Join(home, ".local", "state", "graft"))
			}
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "graft: gocache trim: probe skipped: %v\n", err)
	}
}

type probeOps struct {
	truncate func(*os.File) error
	pwrite   func(*os.File, []byte) error
}

var realProbeOps = probeOps{
	truncate: func(f *os.File) error { return f.Truncate(0) },
	pwrite: func(f *os.File, data []byte) error {
		n, err := unix.Pwrite(int(f.Fd()), data, 0)
		if err == nil && n != len(data) {
			err = fmt.Errorf("short pwrite: %d of %d bytes", n, len(data))
		}
		return err
	},
}

// GocacheProbe measures small writes until trim exits or the lifetime ends.
func (a *App) GocacheProbe(ctx context.Context, pid int, start, dir string) error {
	outcome, err := runGocacheProbe(ctx, pid, start, dir, trimProbeLifetime, realProbeOps, a.Stderr)
	if err != nil {
		return err
	}
	if outcome.writeActive {
		return ErrGocacheProbeWriteStuck
	}
	return nil
}

// ErrGocacheProbeWriteStuck asks the probe command to exit immediately, even
// though its write worker is still blocked.
var ErrGocacheProbeWriteStuck = errors.New("gocache probe write remains active")

type probeOutcome struct {
	durations   []time.Duration
	writeActive bool
	writeStage  string
	writeFor    time.Duration
	workerDone  <-chan struct{}
}

type probeEvent struct {
	stage    string
	started  time.Time
	duration time.Duration
	err      error
}

type activeProbeWrite struct {
	stage   string
	started time.Time
}

type probeIdentity func(pid int, start string) (alive, same bool, err error)

func runGocacheProbe(ctx context.Context, pid int, start, dir string, lifetime time.Duration, ops probeOps, stderr io.Writer) (probeOutcome, error) {
	return runGocacheProbeWithIdentity(ctx, pid, start, dir, lifetime, ops, stderr, processIdentityAlive)
}

func runGocacheProbeWithIdentity(ctx context.Context, pid int, start, dir string, lifetime time.Duration, ops probeOps, stderr io.Writer, identity probeIdentity) (probeOutcome, error) {
	var outcome probeOutcome
	alive, same, err := identity(pid, start)
	if err != nil {
		return outcome, fmt.Errorf("check trim process %d: %w", pid, err)
	}
	if !alive || !same {
		writeProbeSummary(stderr, outcome)
		return outcome, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return outcome, fmt.Errorf("create probe directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "trim-probe"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return outcome, fmt.Errorf("open probe file: %w", err)
	}
	data := make([]byte, 4096)
	events := make(chan probeEvent, 4)
	workerCtx, cancelWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	var active atomic.Pointer[activeProbeWrite]
	var stopping atomic.Bool
	go runProbeWorker(workerCtx, f, data, ops, events, workerDone, &active, &stopping, stderr)
	defer func() {
		cancelWorker()
		if !outcome.writeActive {
			<-workerDone
		}
		if err := f.Close(); err != nil {
			// The probe is diagnostic; report close failures with its summary.
			fmt.Fprintf(stderr, "graft: gocache probe: close probe file: %v\n", err)
		}
	}()
	supervisor := probeSupervisor{pid: pid, start: start, deadline: time.Now().Add(lifetime), stderr: stderr, events: events, active: &active, identity: identity}
	stopErr := supervisor.run(ctx)
	stopping.Store(true)
	cancelWorker()
	supervisor.drain(&outcome, &stopErr, workerDone)
	outcome.workerDone = workerDone
	writeProbeSummary(stderr, outcome)
	return outcome, stopErr
}

func runProbeWorker(ctx context.Context, f *os.File, data []byte, ops probeOps, events chan<- probeEvent, done chan<- struct{}, active *atomic.Pointer[activeProbeWrite], stopping *atomic.Bool, stderr io.Writer) {
	stage := "write"
	var started time.Time
	defer close(done)
	defer func() {
		if recovered := recover(); recovered != nil {
			panicErr := fmt.Errorf("probe worker panic: %v", recovered)
			fmt.Fprintf(stderr, "graft: gocache probe: %v\n", panicErr)
			if current := active.Load(); current != nil {
				stage, started = current.stage, current.started
			}
			active.Store(nil)
			events <- probeEvent{stage: stage, started: started, err: panicErr}
		}
	}()
	operations := []struct {
		name string
		fn   func() error
	}{{"truncate", func() error { return ops.truncate(f) }}, {"pwrite", func() error { return ops.pwrite(f, data) }}}
	for {
		for _, operation := range operations {
			if ctx.Err() != nil || stopping.Load() {
				return
			}
			stage = operation.name
			current := &activeProbeWrite{stage: stage, started: time.Now()}
			active.Store(current)
			if stopping.Load() {
				active.CompareAndSwap(current, nil)
				return
			}
			began := time.Now()
			started = began
			opErr := operation.fn()
			elapsed := time.Since(began)
			active.Store(nil)
			events <- probeEvent{stage: stage, started: began, duration: elapsed, err: opErr}
			if opErr != nil {
				fmt.Fprintf(stderr, "graft: gocache probe: %s failed: %v\n", stage, opErr)
				return
			}
		}
		timer := time.NewTimer(trimProbeInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

type probeSupervisor struct {
	pid                 int
	start               string
	deadline, nextCheck time.Time
	stderr              io.Writer
	events              <-chan probeEvent
	outcome             probeOutcome
	active              *atomic.Pointer[activeProbeWrite]
	identity            probeIdentity
	lastStuckSeconds    int
	lastStuckWrite      *activeProbeWrite
}

func (s *probeSupervisor) run(ctx context.Context) error {
	s.nextCheck = time.Now().Add(time.Second)
	for {
		now := time.Now()
		if err := ctx.Err(); err != nil {
			return err
		}
		if !now.Before(s.deadline) {
			return nil
		}
		timer := time.NewTimer(time.Until(s.nextWake()))
		select {
		case event := <-s.events:
			timer.Stop()
			if err := s.onEvent(event); err != nil {
				return err
			}
		case <-timer.C:
			if done, err := s.onTimer(time.Now()); done || err != nil {
				return err
			}
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

func (s *probeSupervisor) nextWake() time.Time {
	wake := s.nextCheck
	if s.deadline.Before(wake) {
		wake = s.deadline
	}
	active := s.active.Load()
	if active != s.lastStuckWrite {
		s.lastStuckWrite, s.lastStuckSeconds = active, 0
	}
	if active != nil {
		notice := active.started.Add(time.Duration(s.lastStuckSeconds+1) * time.Second)
		if notice.Before(wake) {
			wake = notice
		}
	}
	return wake
}

func (s *probeSupervisor) onEvent(event probeEvent) error {
	return s.recordFinished(event)
}

func (s *probeSupervisor) recordFinished(event probeEvent) error {
	s.outcome.durations = append(s.outcome.durations, event.duration)
	if event.duration > trimProbeSlow {
		fmt.Fprintf(s.stderr, "graft: gocache probe: %s: %s %s\n", event.started.Format(time.RFC3339Nano), event.duration.Round(time.Millisecond), event.stage)
	}
	if event.err != nil {
		return fmt.Errorf("probe %s: %w", event.stage, event.err)
	}
	return nil
}

func (s *probeSupervisor) onTimer(now time.Time) (bool, error) {
	active := s.active.Load()
	if active == nil {
		s.lastStuckSeconds, s.lastStuckWrite = 0, nil
	} else {
		if active != s.lastStuckWrite {
			s.lastStuckWrite, s.lastStuckSeconds = active, 0
		}
		seconds := int(now.Sub(active.started) / time.Second)
		for seconds > 0 && s.lastStuckSeconds < seconds {
			s.lastStuckSeconds++
			fmt.Fprintf(s.stderr, "graft: gocache probe: write stuck %ds (%s)\n", s.lastStuckSeconds, active.stage)
		}
	}
	if !now.Before(s.nextCheck) {
		alive, same, err := s.identity(s.pid, s.start)
		if err != nil || !alive || !same {
			return true, err
		}
		s.nextCheck = now.Add(time.Second)
	}
	return false, nil
}

func (s *probeSupervisor) drain(outcome *probeOutcome, stopErr *error, workerDone <-chan struct{}) {
	for {
		select {
		case event := <-s.events:
			s.recordDrainEvent(event, stopErr)
		default:
			if s.active.Load() == nil {
				select {
				case event := <-s.events:
					s.recordDrainEvent(event, stopErr)
					continue
				case <-workerDone:
					goto drained
				}
			}
			goto drained
		}
	}
drained:
	for {
		select {
		case event := <-s.events:
			s.recordDrainEvent(event, stopErr)
		default:
			outcome.durations = s.outcome.durations
			active := s.active.Load()
			outcome.writeActive = active != nil
			if outcome.writeActive {
				outcome.writeStage = active.stage
				outcome.writeFor = time.Since(active.started)
			}
			return
		}
	}
}

func (s *probeSupervisor) recordDrainEvent(event probeEvent, stopErr *error) {
	if err := s.recordFinished(event); err != nil {
		*stopErr = errors.Join(*stopErr, err)
	}
}

func writeProbeSummary(w io.Writer, result probeOutcome) {
	sorted := append([]time.Duration(nil), result.durations...)
	slices.Sort(sorted)
	var p50, p99, max time.Duration
	if len(sorted) != 0 {
		p50 = sorted[percentileIndex(len(sorted), 50)]
		p99 = sorted[percentileIndex(len(sorted), 99)]
		max = sorted[len(sorted)-1]
	}
	fmt.Fprintf(w, "graft: gocache probe: summary: %d measurements, p50 %s, p99 %s, max %s",
		len(sorted), formatProbeDuration(p50), formatProbeDuration(p99), formatProbeDuration(max))
	if result.writeActive {
		state := "write active"
		if result.writeFor >= time.Second {
			state = "write stuck"
		}
		fmt.Fprintf(w, "; %s: %s %s", state, result.writeStage, result.writeFor.Round(time.Millisecond))
	}
	fmt.Fprintln(w)
}

func percentileIndex(count, percentile int) int {
	return (count*percentile+99)/100 - 1
}

func formatProbeDuration(d time.Duration) string {
	return fmt.Sprintf("%.3fms", float64(d)/float64(time.Millisecond))
}

func processStart(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", fmt.Errorf("read /proc/%d/stat: %w", pid, err)
	}
	fields, err := procStatFields(data)
	if err != nil {
		return "", err
	}
	if len(fields) <= 19 {
		return "", errors.New("proc stat has no start time")
	}
	return fields[19], nil
}

func procStatFields(data []byte) ([]string, error) {
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 || end+2 >= len(data) {
		return nil, errors.New("malformed proc stat")
	}
	return strings.Fields(string(data[end+2:])), nil // starts at field 3; start time is field 22.
}

func processIdentityAlive(pid int, expected string) (alive, same bool, err error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("read /proc/%d/stat: %w", pid, err)
	}
	return processIdentityFromStat(data, expected)
}

func processIdentityFromStat(data []byte, expected string) (alive, same bool, err error) {
	fields, err := procStatFields(data)
	if err != nil {
		return false, false, err
	}
	if len(fields) <= 19 {
		return false, false, errors.New("proc stat has no start time")
	}
	return fields[0] != "Z" && fields[0] != "X", fields[19] == expected, nil
}
