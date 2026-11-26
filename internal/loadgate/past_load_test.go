package loadgate

import (
	"bytes"
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

func TestPastLoadPassesCPUThresholdAndRecordsReason(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setLoadavg(t, 0)
	setRunnableLimit(t, 11)
	setRunnable(12)
	path := filepath.Join(t.TempDir(), "load.lock")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, _, err := Wait(ctx, "ordinary", "projectA:ordinary", nil, testLoad, path, io.Discard); err == nil {
		t.Fatal("ordinary admission passed the CPU limit")
	}

	admitCtx, admitCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer admitCancel()
	_, release, err := Wait(admitCtx, "authorized", "projectA:authorized", nil, testLoad, path, io.Discard, "tl dd4a9367:6208")
	if err != nil {
		t.Fatalf("authorized admission: %v", err)
	}
	defer release()
	recs := readAdmissionLines(t, filepath.Join(os.Getenv("XDG_CACHE_HOME"), "graft", "admissions.jsonl"))
	last := recs[len(recs)-1]
	if last.Verdict != "admitted" || last.PastLoad != "tl dd4a9367:6208" || field(last.LA1) != 0 || field(last.Runnable) != 12 {
		t.Fatalf("authorized admission journal = %+v", last)
	}
}

func TestPastLoadPriorityReservesBeforeOrdinaryWaiter(t *testing.T) {
	fakeMachine(t)
	setRunnableLimit(t, 5)
	setLoadavg(t, 12)
	path := filepath.Join(t.TempDir(), "load.lock")
	holder, err := lock.Acquire(context.Background(), path, time.Second, "current holder", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		name string
		adm  Admission
		rel  func() error
		err  error
	}
	got := make(chan result, 2)
	ordinaryCtx, ordinaryCancel := context.WithCancel(context.Background())
	defer ordinaryCancel()
	go func() {
		adm, rel, err := Wait(ordinaryCtx, "ordinary", "projectA:ordinary", nil, testLoad, path, io.Discard)
		got <- result{"ordinary", adm, rel, err}
	}()
	awaitFiles(t, path+".queue.*.lticket", 1)
	go func() {
		adm, rel, err := Wait(context.Background(), "authorized", "projectA:authorized", nil, testLoad, path, io.Discard, "tl dd4a9367:6208")
		got <- result{"authorized", adm, rel, err}
	}()
	awaitFiles(t, path+".queue.*.lticket", 2)
	if err := holder.Release(); err != nil {
		t.Fatal(err)
	}
	setLoadavg(t, 0)
	first := <-got
	if first.err != nil || first.name != "authorized" {
		t.Fatalf("first admission = %+v; want authorized", first)
	}
	if first.adm.ReserveCores <= 0 {
		t.Fatalf("authorized reservation = %.2f cores; want positive expected cores", first.adm.ReserveCores)
	}
	liveCores, err := liveReserveCores(path)
	if err != nil || liveCores < first.adm.ReserveCores {
		t.Fatalf("live authorized reservation = %.2f cores, %v; want at least %.2f", liveCores, err, first.adm.ReserveCores)
	}
	t.Logf("authorized reserve = %.2f, files=%v", first.adm.ReserveCores, func() []string { files, _ := filepath.Glob(admittedFilesPattern(path)); return files }())
	ordinaryMeter := newMachine(testLoad, io.Discard, "ordinary", "projectA:ordinary")
	ordinaryMeter.candidate, ordinaryMeter.candidateKnown = 4, true
	if _, err := ordinaryMeter.busy(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	reserveBusy, err := ordinaryMeter.reservesBusy(path)
	if err != nil || reserveBusy.why != WhyReserve {
		t.Fatalf("ordinary reserve check = %+v, %v; want reserve wait", reserveBusy, err)
	}
	ordinaryCancel()
	queued := <-got
	if queued.name != "ordinary" || queued.err == nil {
		if queued.rel != nil {
			_ = queued.rel()
		}
		t.Fatalf("ordinary queued result = %+v; want canceled queue waiter", queued)
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer waitCancel()
	ordinary, _, err := Wait(waitCtx, "ordinary-after", "projectA:ordinary", nil, testLoad, path, io.Discard)
	if err == nil || ordinary.Waits[WhyReserve] == 0 {
		t.Fatalf("ordinary admission after reservation = %+v, %v; want reserve wait", ordinary, err)
	}
	if err := first.rel(); err != nil {
		t.Fatal(err)
	}
}

func TestPastLoadWithCPUAndOtherPressureWaitsWithReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		load pressure.Load
		want string
	}{
		{"disk", pressure.Load{IOFull: 35}, "IO pressure"},
		{"writeout", pressure.Load{DirtyMB: 200, DirtyLimitMB: 64}, "write-out"},
		{"memory-swap", pressure.Load{MemSome: 30, SwapKBs: 4096}, "memory pressure"},
		{"quiet", pressure.Load{QuietUntil: time.Now().Add(time.Hour)}, "quiet mode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeMachine(t)
			var output bytes.Buffer
			m := newMachine(testLoad, &output, "authorized", "projectA:authorized")
			m.pastLoad = "tl dd4a9367:6208"
			m.limits = pressure.Limits{Runnable: 11, MaxIOFull: 10, MaxDirtyMB: 64, MaxMemSome: 10, MaxSwapKBs: 1024}
			tc.load.Runnable, tc.load.Threads = 12, 22
			if tc.name == "disk" {
				tc.load.IOWindow = 5 * time.Second
			}
			m.meter = &admissionMeter{verdicts: []pressure.Verdict{{Load: tc.load}}}
			m.sleep = func(context.Context, time.Duration) error { return context.Canceled }
			if err := m.admitWithReserves(context.Background(), nil, filepath.Join(t.TempDir(), "load.lock")); err == nil {
				t.Fatal("admitted while a non-CPU measure exceeded its limit")
			}
			if !strings.Contains(output.String(), tc.want) {
				t.Fatalf("wait output %q does not explain %s", output.String(), tc.want)
			}
		})
	}
}

func TestPastLoadStillWaitsForNonCPUMeasures(t *testing.T) {
	for _, tc := range []struct {
		name, why string
		load      pressure.Load
	}{
		{"io", WhyIO, pressure.Load{IOFull: 35, IOWindow: 5 * time.Second}},
		{"writeback", WhyWriteback, pressure.Load{DirtyMB: 200, DirtyLimitMB: 64}},
		{"swap", WhySwap, pressure.Load{MemSome: 30, SwapKBs: 4096}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeMachine(t)
			var output bytes.Buffer
			m := newMachine(testLoad, &output, "authorized", "projectA:authorized")
			m.pastLoad = "tl dd4a9367:6208"
			m.limits = pressure.Limits{Runnable: 11, MaxIOFull: 10, MaxDirtyMB: 64, MaxMemSome: 10, MaxSwapKBs: 1024}
			tc.load.Runnable, tc.load.Threads = 12, 22
			m.meter = &admissionMeter{verdicts: []pressure.Verdict{{Load: tc.load}}}
			m.sleep = func(context.Context, time.Duration) error { return context.Canceled }
			err := m.admitWithReserves(context.Background(), nil, filepath.Join(t.TempDir(), "load.lock"))
			if err == nil || m.waits[tc.why] == 0 {
				t.Fatalf("waits = %v, %v; want wait for %s", m.waits, err, tc.why)
			}
		})
	}

	t.Run("disk-freeze", func(t *testing.T) {
		fakeMachine(t)
		var output bytes.Buffer
		m := newMachine(testLoad, &output, "authorized", "projectA:authorized")
		m.pastLoad = "tl dd4a9367:6208"
		m.limits = pressure.Limits{Runnable: 11, MaxIOFull: 10, MaxDirtyMB: 64, MaxMemSome: 10, MaxSwapKBs: 1024}
		m.meter = &admissionMeter{verdicts: []pressure.Verdict{{Load: pressure.Load{
			Runnable: 12, Threads: 22, IOFull: 0, IOWindow: 5 * time.Second,
			Disk: &pressure.DiskNow{Open: true, OpenMs: 37},
		}}}}
		m.sleep = func(context.Context, time.Duration) error { return context.Canceled }
		err := m.admitWithReserves(context.Background(), nil, filepath.Join(t.TempDir(), "load.lock"))
		if err == nil || m.waits[WhyIO] == 0 {
			t.Fatalf("waits = %v, %v; want wait for disk freeze", m.waits, err)
		}
		if !strings.Contains(output.String(), "disk freeze") {
			t.Fatalf("wait output %q does not explain disk freeze", output.String())
		}
	})

	t.Run("quiet", func(t *testing.T) {
		fakeMachine(t)
		var output bytes.Buffer
		m := newMachine(testLoad, &output, "authorized", "projectA:authorized")
		m.pastLoad = "tl dd4a9367:6208"
		m.limits = pressure.Limits{Runnable: 11}
		m.meter = &admissionMeter{verdicts: []pressure.Verdict{{Load: pressure.Load{Runnable: 12, Threads: 22, QuietUntil: time.Now().Add(time.Hour)}}}}
		m.sleep = func(context.Context, time.Duration) error { return context.Canceled }
		err := m.admitWithReserves(context.Background(), nil, filepath.Join(t.TempDir(), "load.lock"))
		if err == nil || m.waits[WhyQuiet] == 0 {
			t.Fatalf("waits = %v, %v; want quiet wait", m.waits, err)
		}
	})
}
