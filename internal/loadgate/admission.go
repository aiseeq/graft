package loadgate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/aiseeq/akit/pressure"
	"github.com/aiseeq/graft/v2/internal/stats"
)

// admissionRecord records the decision and the exact disk and writeback
// sample that informed it. A decision taken before the first measure has no
// dirty fields and says so in ProfileNone.
type admissionRecord struct {
	Time         time.Time          `json:"time"`
	Run          string             `json:"run"`
	Kind         string             `json:"kind"`
	Verdict      string             `json:"verdict"`
	Measure      string             `json:"measure"`
	Why          string             `json:"why"`
	WaitedS      float64            `json:"waited_s"`
	Waits        map[string]float64 `json:"waits,omitempty"`
	Behind       string             `json:"behind,omitempty"`
	DirtyMB      *float64           `json:"dirty_mb,omitempty"`
	DirtyLimitMB *float64           `json:"dirty_limit_mb,omitempty"`
	Profile      json.RawMessage    `json:"profile,omitempty"`
	ProfileNone  string             `json:"profile_none,omitempty"`
	Priority     bool               `json:"priority"`
}

// notMeasured is the ProfileNone of a decision taken before any measure.
const notMeasured = "not measured"

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
		WaitedS: max(0, time.Since(m.admissionStart).Seconds()), Priority: priority}
	if !m.haveView {
		rec.ProfileNone = notMeasured
		return rec
	}
	load := m.lastView.Load
	rec.DirtyMB, rec.DirtyLimitMB = &load.DirtyMB, &load.DirtyLimitMB
	switch {
	case load.Disk != nil && len(load.Disk.Profile) > 0:
		rec.Profile = load.Disk.Profile
	case load.Disk != nil:
		rec.ProfileNone = "the disk state source gave no profile of the kind"
	case load.DiskFault == nil || errors.Is(load.DiskFault, pressure.ErrNoIOSource):
		rec.ProfileNone = "no disk state source"
	default:
		rec.ProfileNone = "the disk state source failed: " + load.DiskFault.Error()
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
