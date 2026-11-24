package loadgate

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAdmissionCountsCandidateBesideLiveReserve(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setRunnableLimit(t, 11)
	setLoadavg(t, 10)
	path := filepath.Join(t.TempDir(), "load.lock")
	release := admitOne(t, path, .5)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, _, err := WaitWithSettle(ctx, "candidate", "projectA:candidate", nil, testLoad, path, "", 0, 2, io.Discard, "")
	if err == nil {
		t.Fatal("candidate passed at LA1 10 + unseen reserve 0.5 + candidate 2, limit 11")
	}
	last := lastWaitRecord(t, filepath.Join(os.Getenv("XDG_CACHE_HOME"), "graft", "admissions.jsonl"))
	if last.Verdict != "wait" || field(last.Limit) != 11 || field(last.Candidate) != 2 || field(last.LiveReserve) != .5 || field(last.Unseen) < .49 || field(last.LA1) != 10 {
		t.Fatalf("recorded rejected decision = %+v", last)
	}
}

func TestAdmissionExitHatchWithoutOurReserves(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setRunnableLimit(t, 11)
	setLoadavg(t, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, release, err := WaitWithSettle(ctx, "oversized", "projectA:oversized", nil, testLoad, filepath.Join(t.TempDir(), "load.lock"), "", 0, 30, io.Discard, "")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	lines := readAdmissionLines(t, filepath.Join(os.Getenv("XDG_CACHE_HOME"), "graft", "admissions.jsonl"))
	last := lines[len(lines)-1]
	if last.Verdict != "admitted" || !last.ExitHatch || field(last.LA1) != 10 || field(last.Runnable) != 1 || field(last.Limit) != 11 || field(last.Unseen) != 0 || field(last.LiveReserve) != 0 || field(last.Candidate) != 30 || field(last.CandidateCapped) != 0 {
		t.Fatalf("exit-hatch decision = %+v", last)
	}
}

func TestAdmissionCountsCandidateAndBlocksAtEquality(t *testing.T) {
	for _, tc := range []struct {
		name      string
		candidate float64
		wantWait  bool
	}{
		{name: "below", candidate: 1.5},
		{name: "equal", candidate: 2, wantWait: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setRunnable, _, _ := fakeMachine(t)
			setRunnable(1)
			setRunnableLimit(t, 11)
			setLoadavg(t, 7)
			path := filepath.Join(t.TempDir(), "load.lock")
			release := admitOne(t, path, 2)
			defer release()
			setAdmissionFuture(t, path)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
			defer cancel()
			_, _, err := WaitWithSettle(ctx, "candidate", "projectA:candidate", nil, testLoad, path, "", 0, tc.candidate, io.Discard, "")
			if (err != nil) != tc.wantWait {
				t.Fatalf("candidate %.1f: err = %v, wantWait %v", tc.candidate, err, tc.wantWait)
			}
			if !tc.wantWait {
				return
			}
			last := lastWaitRecord(t, filepath.Join(os.Getenv("XDG_CACHE_HOME"), "graft", "admissions.jsonl"))
			if field(last.Candidate) != 2 || field(last.Unseen) < 1.99 || field(last.Limit) != 11 || math.Abs(field(last.LA1)-7) > .01 {
				t.Fatalf("equality decision = %+v", last)
			}
		})
	}
}

func TestReviewOversizedStarvation(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setRunnableLimit(t, 11)
	setLoadavg(t, 0)
	path := filepath.Join(t.TempDir(), "load.lock")
	release := admitOne(t, path, .5)
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, candidateRelease, err := WaitWithSettle(ctx, "oversized", "projectA:oversized", nil, testLoad, path, "", 0, 11, io.Discard, "")
	if err != nil {
		t.Fatalf("oversized candidate starved beside a small live task: %v", err)
	}
	defer candidateRelease()
	lines := readAdmissionLines(t, filepath.Join(os.Getenv("XDG_CACHE_HOME"), "graft", "admissions.jsonl"))
	last := lines[len(lines)-1]
	if field(last.Candidate) != 11 || field(last.CandidateCapped) != 0 || last.ExitHatch {
		t.Fatalf("oversized admission record = %+v", last)
	}
}

func TestCandidateCountsAgainstRunnableWithLiveReserve(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(10)
	setRunnableLimit(t, 11)
	setLoadavg(t, 0)
	path := filepath.Join(t.TempDir(), "load.lock")
	release := admitOne(t, path, .5)
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, _, err := WaitWithSettle(ctx, "runnable-boundary", "projectA:runnable-boundary", nil, testLoad, path, "", 0, 1, io.Discard, "")
	if err == nil {
		t.Fatal("candidate passed runnable 10 + live reserve 0.5 + candidate 1 at limit 11")
	}
}

func TestReviewBusyJournal(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setRunnableLimit(t, 11)
	setLoadavg(t, 12)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, _, err := WaitWithSettle(ctx, "busy-journal", "projectA:busy-journal", nil, testLoad, filepath.Join(t.TempDir(), "load.lock"), "", 0, 2, io.Discard, "")
	if err == nil {
		t.Fatal("busy load unexpectedly admitted")
	}
	lines := readAdmissionLines(t, filepath.Join(os.Getenv("XDG_CACHE_HOME"), "graft", "admissions.jsonl"))
	for _, rec := range lines {
		if rec.Verdict != "wait" {
			continue
		}
		if field(rec.LA1) != 12 || field(rec.Runnable) != 1 || field(rec.Limit) != 11 || field(rec.Candidate) != 2 || rec.Unseen != nil || rec.LiveReserve != nil {
			t.Fatalf("busy wait record = %+v", rec)
		}
		return
	}
	t.Fatalf("no wait record for busy load: %+v", lines)
}

func TestAdmissionJournalFirstCPUWindow(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setRunnableLimit(t, 11)
	setLoadavg(t, 0)
	path := filepath.Join(t.TempDir(), "load.lock")
	release, _, _, _, err := startAdmission(path, "first-measure", .5, time.Hour, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, _, err = WaitWithSettle(ctx, "waiting", "projectA:waiting", nil, testLoad, path, "", 0, 2, io.Discard, "")
	if err == nil {
		t.Fatal("wait for first CPU measure unexpectedly admitted")
	}
	lines := readAdmissionLines(t, filepath.Join(os.Getenv("XDG_CACHE_HOME"), "graft", "admissions.jsonl"))
	for _, rec := range lines {
		if rec.Verdict != "wait" || rec.Measure != WhyReserve {
			continue
		}
		if field(rec.LA1) != 0 || field(rec.Runnable) != 1 || field(rec.Limit) != 11 || field(rec.Unseen) < .49 || field(rec.LiveReserve) != .5 || field(rec.Candidate) != 2 || field(rec.CandidateCapped) != 2 {
			t.Fatalf("first CPU window record = %+v", rec)
		}
		return
	}
	t.Fatalf("no first-CPU-window wait record: %+v", lines)
}

func field(v *float64) float64 {
	if v == nil {
		return math.NaN()
	}
	return *v
}

func lastWaitRecord(t *testing.T, path string) admissionRecord {
	t.Helper()
	lines := readAdmissionLines(t, path)
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i].Verdict == "wait" {
			return lines[i]
		}
	}
	t.Fatalf("no wait record: %+v", lines)
	return admissionRecord{}
}

func setAdmissionFuture(t *testing.T, lockPath string) {
	t.Helper()
	files, err := filepath.Glob(admittedFilesPattern(lockPath))
	if err != nil || len(files) == 0 {
		t.Fatalf("admitted files = %v, %v", files, err)
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var record reserveData
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		future := time.Now().Add(time.Minute).Format(time.RFC3339Nano)
		record.Admitted = future
		if record.At != "" {
			record.At = future
		}
		if record.ObservedAt != "" {
			record.ObservedAt = future
		}
		data, err = json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
