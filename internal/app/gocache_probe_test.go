package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbeChildProcess(t *testing.T) {
	if path := os.Getenv("GRAFT_PROBE_CHILD_RELEASE"); path != "" {
		for {
			if _, err := os.Stat(path); err == nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestGocacheProbeStopsWhenWhileProcessExits(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProbeChildProcess$")
	cmd.Env = append(os.Environ(), "GRAFT_PROBE_CHILD_RELEASE="+release)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	done := make(chan error, 1)
	start, err := processStart(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{}, 1)
	ops := probeOps{
		truncate: func(f *os.File) error {
			select {
			case ready <- struct{}{}:
			default:
			}
			return f.Truncate(0)
		},
		pwrite: realProbeOps.pwrite,
	}
	probeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, runErr := runGocacheProbe(probeCtx, cmd.Process.Pid, start, t.TempDir(), 5*time.Second, ops, &out)
		done <- runErr
	}()
	<-ready
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("probe did not stop after --while exited")
	}
	if !strings.Contains(out.String(), "summary:") {
		t.Fatalf("probe output %q", out.String())
	}
}

func TestGocacheProbeStopsAtLifetimeAndPrintsSummary(t *testing.T) {
	var out bytes.Buffer
	started := time.Now()
	start, err := processStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runGocacheProbe(context.Background(), os.Getpid(), start, t.TempDir(), 25*time.Millisecond, realProbeOps, &out); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > time.Second || !strings.Contains(out.String(), "summary: 2 measurements") || !strings.Contains(out.String(), "p50 ") || !strings.Contains(out.String(), "p99 ") || !strings.Contains(out.String(), "max ") {
		t.Fatalf("duration %s, output %q", time.Since(started), out.String())
	}
}

func TestGocacheProbeExitsWhileWriteIsStuckAndWhileProcessDies(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProbeChildProcess$")
	cmd.Env = append(os.Environ(), "GRAFT_PROBE_CHILD_RELEASE="+release)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	start, err := processStart(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	blocked, started := make(chan struct{}), make(chan struct{}, 1)
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(blocked) }) }
	waited := false
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o600)
		unblock()
		if !waited {
			_ = cmd.Wait()
		}
	})
	ops := probeOps{
		truncate: func(*os.File) error { started <- struct{}{}; <-blocked; return nil },
		pwrite:   func(*os.File, []byte) error { return nil },
	}
	out := &lockedProbeBuffer{}
	type probeResult struct {
		outcome probeOutcome
		err     error
	}
	done := make(chan probeResult, 1)
	go func() {
		outcome, runErr := runGocacheProbe(context.Background(), cmd.Process.Pid, start, t.TempDir(), 10*time.Second, ops, out)
		done <- probeResult{outcome: outcome, err: runErr}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("probe write did not start")
	}
	waitProbeOutput(t, out, "write stuck 1s", 2*time.Second)
	releaseAt := time.Now()
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	waited = true
	var result probeResult
	select {
	case result = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not exit within 2s after --while exited")
	}
	if elapsed := time.Since(releaseAt); elapsed > 2*time.Second {
		t.Fatalf("probe exit took %s after --while exited", elapsed)
	}
	if result.err != nil || !result.outcome.writeActive || result.outcome.writeStage != "truncate" || result.outcome.writeFor < time.Second || !strings.Contains(out.String(), "summary:") || !strings.Contains(out.String(), "write stuck: truncate") {
		t.Fatalf("result %+v, error %v, output %q", result.outcome, result.err, out.String())
	}
	unblock()
	select {
	case <-result.outcome.workerDone:
	case <-time.After(time.Second):
		t.Fatal("unblocked probe worker did not stop")
	}
}

func TestGocacheProbeStopsWhenStartTicksDoNotMatch(t *testing.T) {
	start, err := processStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	outcome, err := runGocacheProbe(context.Background(), os.Getpid(), start+"0", t.TempDir(), time.Second, realProbeOps, &out)
	if err != nil || len(outcome.durations) != 0 || !strings.Contains(out.String(), "summary: 0 measurements") {
		t.Fatalf("outcome %+v, error %v, output %q", outcome, err, out.String())
	}
}

func TestGocacheProbeStopsWhenStartTicksChangeWhilePIDLives(t *testing.T) {
	start, err := processStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	var checks atomic.Int32
	identity := func(int, string) (bool, bool, error) {
		if checks.Add(1) == 1 {
			return true, true, nil
		}
		return true, false, nil
	}
	ops := probeOps{
		truncate: func(*os.File) error { return nil },
		pwrite:   func(*os.File, []byte) error { return nil },
	}
	var out bytes.Buffer
	started := time.Now()
	outcome, err := runGocacheProbeWithIdentity(context.Background(), os.Getpid(), start, t.TempDir(), 5*time.Second, ops, &out, identity)
	if err != nil || checks.Load() < 2 || len(outcome.durations) == 0 || time.Since(started) > 2*time.Second {
		t.Fatalf("outcome %+v, checks %d, elapsed %s, error %v", outcome, checks.Load(), time.Since(started), err)
	}
}

func TestProcStatParsersUseLastClosingParenthesisAndDetectZombie(t *testing.T) {
	stat := func(state string) []byte {
		return []byte("123 (name ) with space) " + state + strings.Repeat(" 0", 18) + " 987654")
	}
	start, err := procStatFields(stat("S"))
	if err != nil || start[19] != "987654" {
		t.Fatalf("fields %v, error %v", start, err)
	}
	alive, same, err := processIdentityFromStat(stat("Z"), "987654")
	if err != nil || alive || !same {
		t.Fatalf("zombie identity alive=%t same=%t error=%v", alive, same, err)
	}
}

func TestStartTrimProbeUsesItsOwnPID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GRAFT_GOCACHE_PROBE", "probe-stub")
	old := startProbeUnit
	t.Cleanup(func() { startProbeUnit = old })
	startProbeUnit = func(command, _ string, pid int, start, dir string) error {
		if command != "probe-stub" || pid != os.Getpid() || dir != filepath.Join(os.Getenv("HOME"), ".local", "state", "graft") {
			t.Errorf("launch command=%q pid=%d dir=%q", command, pid, dir)
		}
		want, err := processStart(os.Getpid())
		if err != nil || start != want {
			t.Errorf("start ticks %q, want %q (error %v)", start, want, err)
		}
		return nil
	}
	var out bytes.Buffer
	startTrimProbe(&out)
	if out.Len() != 0 {
		t.Fatalf("unexpected output %q", out.String())
	}
}

func TestStartTrimProbeOffPrintsOneLineWithoutLaunching(t *testing.T) {
	t.Setenv("GRAFT_GOCACHE_PROBE", "off")
	old := startProbeUnit
	t.Cleanup(func() { startProbeUnit = old })
	startProbeUnit = func(string, string, int, string, string) error {
		t.Fatal("probe launcher called while disabled")
		return nil
	}
	var out bytes.Buffer
	startTrimProbe(&out)
	if got, want := out.String(), "graft: gocache trim: probe off\n"; got != want {
		t.Fatalf("output %q, want %q", got, want)
	}
}

func TestProbeWriteDurationExcludesDelayedEventReceive(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "probe")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	called := make(chan struct{})
	ctx := t.Context()
	events := make(chan probeEvent)
	done := make(chan struct{})
	var active atomic.Pointer[activeProbeWrite]
	var stopping atomic.Bool
	ops := probeOps{
		truncate: func(*os.File) error { close(called); return nil },
		pwrite:   func(*os.File, []byte) error { return errors.New("stop worker") },
	}
	go runProbeWorker(ctx, f, nil, ops, events, done, &active, &stopping, io.Discard)
	<-called
	timer := time.NewTimer(300 * time.Millisecond)
	<-timer.C
	if active.Load() != nil {
		t.Fatal("completed truncate remained marked active while its event waited")
	}
	event := <-events
	if event.stage != "truncate" || event.duration >= trimProbeSlow {
		t.Fatalf("delayed event measured %s for %s", event.duration, event.stage)
	}
	var out bytes.Buffer
	supervisor := probeSupervisor{stderr: &out}
	if err := supervisor.recordFinished(event); err != nil || out.Len() != 0 || supervisor.outcome.durations[0] >= trimProbeSlow {
		t.Fatalf("delayed event was logged as slow: duration %s, error %v, output %q", supervisor.outcome.durations[0], err, out.String())
	}
	second := <-events
	if second.stage != "pwrite" || second.err == nil {
		t.Fatalf("worker result after delayed confirmation = %+v", second)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("probe worker did not stop")
	}
}

func TestProbeOperationAt250msIsNotSlow(t *testing.T) {
	var out bytes.Buffer
	supervisor := probeSupervisor{stderr: &out}
	err := supervisor.recordFinished(probeEvent{stage: "truncate", started: time.Now(), duration: 250 * time.Millisecond})
	if err != nil || len(supervisor.outcome.durations) != 1 || out.Len() != 0 {
		t.Fatalf("recorded durations %v, error %v, output %q", supervisor.outcome.durations, err, out.String())
	}
}

func TestProbeSummaryDoesNotCallAnOrdinaryActiveWriteStuck(t *testing.T) {
	var out bytes.Buffer
	writeProbeSummary(&out, probeOutcome{writeActive: true, writeStage: "truncate", writeFor: 300 * time.Millisecond})
	if strings.Contains(out.String(), "write stuck") || !strings.Contains(out.String(), "write active: truncate") {
		t.Fatalf("summary %q", out.String())
	}
}

type lockedProbeBuffer struct {
	sync.Mutex
	bytes.Buffer
}

func (b *lockedProbeBuffer) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.Buffer.Write(p)
}

func (b *lockedProbeBuffer) String() string {
	b.Lock()
	defer b.Unlock()
	return b.Buffer.String()
}

func waitProbeOutput(t *testing.T, out *lockedProbeBuffer, text string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), text) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("did not see %q in probe output %q", text, out.String())
}

func TestGocacheProbeLogsSlowOperation(t *testing.T) {
	ops := probeOps{
		truncate: func(*os.File) error { time.Sleep(260 * time.Millisecond); return nil },
		pwrite:   func(*os.File, []byte) error { return nil },
	}
	var out bytes.Buffer
	start, err := processStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runGocacheProbe(context.Background(), os.Getpid(), start, t.TempDir(), 300*time.Millisecond, ops, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), " truncate") || !strings.Contains(out.String(), "summary:") {
		t.Fatalf("probe output %q", out.String())
	}
}

func TestGocacheTrimProbeLaunchFailureIsLoggedAndTrimContinues(t *testing.T) {
	dir, old := makeTrimCache(t)
	trimProc(t, 1, 1)
	serveTrimDisk(t, 0)
	quietConfigHome(t)
	t.Setenv("GOCACHE", dir)
	previous := startProbeUnit
	startProbeUnit = func(string, string, int, string, string) error { return errors.New("systemd-run unavailable") }
	t.Cleanup(func() { startProbeUnit = previous })
	var stderr bytes.Buffer
	err := (&App{Stdout: &bytes.Buffer{}, Stderr: &stderr}).GocacheTrim(context.Background(), 25*time.Hour, true, time.Second, 0)
	if err != nil {
		t.Fatalf("trim failed: %v; stderr %q", err, stderr.String())
	}
	if strings.Count(stderr.String(), "probe skipped:") != 1 || !strings.Contains(stderr.String(), "systemd-run unavailable") {
		t.Fatalf("stderr %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "batch 1: started ") || !strings.Contains(stderr.String(), "1 entries, 5 B, unlink ") || !strings.Contains(stderr.String(), "syncfs ") || !strings.Contains(stderr.String(), "waited ") {
		t.Fatalf("batch journal line missing: %q", stderr.String())
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("trim did not remove stale entry: %v", err)
	}
}
