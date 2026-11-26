package loadgate

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"

	"github.com/aiseeq/akit/pressure"
	"github.com/aiseeq/graft/v2/internal/stats"
)

// admissionRecord records the decision and the writeback sample that
// informed it, and why the disk state source gave no reading when it gave
// none. A decision taken before the first measure has no dirty fields.
type admissionRecord struct {
	Time            time.Time          `json:"time"`
	Run             string             `json:"run"`
	Kind            string             `json:"kind"`
	Verdict         string             `json:"verdict"`
	Measure         string             `json:"measure"`
	Why             string             `json:"why"`
	WaitedS         float64            `json:"waited_s"`
	Waits           map[string]float64 `json:"waits,omitempty"`
	Behind          string             `json:"behind,omitempty"`
	DirtyMB         *float64           `json:"dirty_mb,omitempty"`
	DirtyLimitMB    *float64           `json:"dirty_limit_mb,omitempty"`
	DiskFault       string             `json:"disk_fault,omitempty"`
	Priority        bool               `json:"priority"`
	PastLoad        string             `json:"past_load,omitempty"`
	LA1             *float64           `json:"la1,omitempty"`
	Runnable        *float64           `json:"runnable,omitempty"`
	Blocked         *int               `json:"blocked,omitempty"`
	IOSome          *float64           `json:"io_some,omitempty"`
	IOFull          *float64           `json:"io_full,omitempty"`
	Limit           *float64           `json:"limit,omitempty"`
	Unseen          *float64           `json:"unseen,omitempty"`
	LiveReserve     *float64           `json:"live_reserve,omitempty"`
	Candidate       *float64           `json:"candidate,omitempty"`
	CandidateCapped *float64           `json:"candidate_capped,omitempty"`
	ExitHatch       bool               `json:"exit_hatch"`
}

type admissionDecision struct {
	LA1, Runnable, Limit, Unseen, LiveReserve, Candidate, CandidateCapped *float64
}

var admissionWriteWarning atomic.Bool

func writeAdmission(rec admissionRecord) error {
	path, err := stats.Path()
	if err != nil {
		return err
	}
	return stats.Append(filepath.Join(filepath.Dir(path), "admissions.jsonl"), rec, stats.MaxSize)
}

func (m *machine) appendAdmission(rec admissionRecord) {
	if err := writeAdmission(rec); err != nil && admissionWriteWarning.CompareAndSwap(false, true) {
		fmt.Fprintf(os.Stderr, "graft: admissions not recorded (the task goes on): %v\n", err)
	}
}

// admissionRecord is a line for the decision now: waited_s counts from the
// start of the admission, the queue for load.lock included, for every verdict.
func (m *machine) admissionRecord(priority bool) admissionRecord {
	rec := admissionRecord{Time: time.Now(), Run: os.Getenv(stats.EnvRun), Kind: m.kind,
		WaitedS: max(0, time.Since(m.admissionStart).Seconds()), Priority: priority,
		LA1: m.decision.LA1, Runnable: m.decision.Runnable, Limit: m.decision.Limit,
		Unseen: m.decision.Unseen, LiveReserve: m.decision.LiveReserve,
		Candidate: m.decision.Candidate, CandidateCapped: m.decision.CandidateCapped, ExitHatch: m.exitHatch}
	if m.pastLoad != "" {
		rec.PastLoad = m.pastLoad
	}
	if !m.haveView {
		return rec
	}
	load := m.lastView.Load
	if !slices.Contains(m.lastView.Skipped, pressure.ErrNoMemInfo) {
		rec.DirtyMB = &load.DirtyMB
	}
	if load.DirtyLimitMB > 0 &&
		!slices.Contains(m.lastView.Skipped, pressure.ErrNoMemInfo) &&
		!slices.Contains(m.lastView.Skipped, pressure.ErrNoDirtyLimit) {
		rec.DirtyLimitMB = &load.DirtyLimitMB
	}
	if !slices.Contains(m.lastView.Skipped, pressure.ErrNoRunnable) {
		rec.Blocked = &load.Blocked
	}
	if !slices.Contains(m.lastView.Skipped, pressure.ErrNoIOPressure) {
		rec.IOSome = &load.IOSome
		rec.IOFull = &load.IOFull
	} else if load.Disk != nil {
		rec.IOFull = &load.IOFull
	}
	if load.DiskFault != nil {
		rec.DiskFault = load.DiskFault.Error()
	}
	return rec
}

// withWaits adds what the whole admission waited for, by why, and the holder
// it queued behind.
func withWaits(rec admissionRecord, adm Admission) admissionRecord {
	for why, d := range adm.Waits {
		if d > 0 {
			if rec.Waits == nil {
				rec.Waits = map[string]float64{}
			}
			rec.Waits[why] = d.Seconds()
		}
	}
	rec.Behind = adm.WaitedBehind
	return rec
}

func (m *machine) recordAdmitted(adm Admission, priority bool) {
	rec := withWaits(m.admissionRecord(priority), adm)
	rec.Verdict = "admitted"
	m.appendAdmission(rec)
}

func (m *machine) recordRefused(why string, adm Admission, priority bool) {
	rec := withWaits(m.admissionRecord(priority), adm)
	rec.Verdict, rec.Why = "refused", why
	if m.haveView && m.lastView.Busy() {
		rec.Measure = m.lastView.Measure
	}
	m.appendAdmission(rec)
}

func (m *machine) recordWait(b busyness, priority bool) {
	if b.why == "" || b.why == m.lastLoggedMeasure {
		return
	}
	rec := m.admissionRecord(priority)
	rec.Verdict, rec.Measure, rec.Why = "wait", b.why, b.text
	m.appendAdmission(rec)
	m.lastLoggedMeasure = b.why
}
