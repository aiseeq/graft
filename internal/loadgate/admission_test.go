package loadgate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/aiseeq/akit/pressure"
	"github.com/aiseeq/graft/v2/internal/stats"
)

type admissionMeter struct {
	verdicts []pressure.Verdict
	index    int
}

func (m *admissionMeter) Check(context.Context, pressure.Limits, string, bool) (pressure.Verdict, error) {
	v := m.verdicts[min(m.index, len(m.verdicts)-1)]
	m.index++
	return v, nil
}

func TestAdmissionJournalMeasureChangesThenAdmits(t *testing.T) {
	fakeMachine(t)
	t.Setenv(stats.EnvRun, "run-synthetic")

	oldFactory := pressureMeterFactory
	pressureMeterFactory = func(string) machineMeter {
		return &admissionMeter{verdicts: []pressure.Verdict{
			{Measure: pressure.Writeback, Value: 200, Limit: 64, Load: pressure.Load{Runnable: 1, Blocked: 3, IOSome: 2.4, IOFull: 1.1, Threads: 22, DirtyMB: 200, DirtyLimitMB: 64}},
			{Measure: pressure.Writeback, Value: 200, Limit: 64, Load: pressure.Load{Runnable: 1, Blocked: 3, IOSome: 2.4, IOFull: 1.1, Threads: 22, DirtyMB: 200, DirtyLimitMB: 64}},
			{Measure: pressure.IO, Value: 35, Limit: 10, Load: pressure.Load{Runnable: 1, Blocked: 8, IOSome: 7.3, IOFull: 3.6, Threads: 22, DirtyMB: 18, DirtyLimitMB: 64}},
			{Load: pressure.Load{Runnable: 1, Blocked: 4, IOSome: 2.8, IOFull: 0.4, Threads: 22, DirtyMB: 12, DirtyLimitMB: 64}},
		}}
	}
	defer func() { pressureMeterFactory = oldFactory }()

	l := testLoad
	l.Poll = time.Millisecond
	path := filepath.Join(t.TempDir(), "load.lock")
	_, release, err := WaitWithSettle(context.Background(), "synthetic", "projectA:exec:go", nil, l, path, "", 0, 0, io.Discard, "")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	journal := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "graft", "admissions.jsonl")
	if raw, err := os.ReadFile(journal); err != nil || bytes.Contains(raw, []byte(`"profile`)) || bytes.Contains(raw, []byte(`"disk_fault"`)) {
		t.Errorf("journal of measures without a disk fault has a profile or a disk fault (%v):\n%s", err, raw)
	}
	lines := readAdmissionLines(t, journal)
	if len(lines) != 3 {
		t.Fatalf("got %d records, want 3: %+v", len(lines), lines)
	}
	if lines[0].Verdict != "wait" || lines[0].Measure != WhyWriteback || lines[1].Verdict != "wait" || lines[1].Measure != WhyIO || lines[2].Verdict != "admitted" {
		t.Fatalf("verdict sequence = %+v", lines)
	}
	for _, rec := range lines {
		if rec.Kind != "projectA:exec:go" || rec.Run != "run-synthetic" {
			t.Errorf("record identity = %+v", rec)
		}
	}
	if mb(lines[0].DirtyMB) != 200 || mb(lines[0].DirtyLimitMB) != 64 || mb(lines[1].DirtyMB) != 18 || mb(lines[1].DirtyLimitMB) != 64 {
		t.Fatalf("dirty sample values = %+v", lines)
	}
	for i, want := range []struct {
		blocked int
		ioSome  float64
		ioFull  float64
	}{{3, 2.4, 1.1}, {8, 7.3, 3.6}, {4, 2.8, 0.4}} {
		rec := lines[i]
		if rec.Blocked == nil || *rec.Blocked != want.blocked {
			t.Errorf("record %d blocked = %v, want %d", i, rec.Blocked, want.blocked)
		}
		if rec.IOSome == nil || *rec.IOSome != want.ioSome {
			t.Errorf("record %d io_some = %v, want %g", i, rec.IOSome, want.ioSome)
		}
		if rec.IOFull == nil || *rec.IOFull != want.ioFull || rec.IOFull != nil && *rec.IOFull == want.ioSome {
			t.Errorf("record %d io_full = %v, want %g distinct from io_some %g", i, rec.IOFull, want.ioFull, want.ioSome)
		}
	}
	if w := lines[2].Waits; w[WhyWriteback] <= 0 || w[WhyIO] <= 0 {
		t.Errorf("admitted line waits = %v, want writeback and io", w)
	}
}

func TestAdmissionRecordUsesIndependentAvailabilityForBlockedAndIO(t *testing.T) {
	for _, tc := range []struct {
		name    string
		skipped []error
		disk    *pressure.DiskNow
		blocked bool
		ioSome  bool
		ioFull  bool
	}{
		{name: "only stat skipped", skipped: []error{pressure.ErrNoRunnable}, ioSome: true, ioFull: true},
		{name: "only PSI skipped", skipped: []error{pressure.ErrNoIOPressure}, blocked: true},
		{name: "disk source without PSI", skipped: []error{pressure.ErrNoIOPressure}, disk: &pressure.DiskNow{}, blocked: true, ioFull: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &machine{haveView: true, lastView: pressure.Verdict{
				Load: pressure.Load{Blocked: 6, IOSome: 3.25, IOFull: 1.25, Disk: tc.disk}, Skipped: tc.skipped,
			}}
			rec := m.admissionRecord(false)
			if (rec.Blocked != nil) != tc.blocked {
				t.Errorf("blocked = %v, want present %t", rec.Blocked, tc.blocked)
			}
			if (rec.IOSome != nil) != tc.ioSome {
				t.Errorf("io_some = %v, want present %t", rec.IOSome, tc.ioSome)
			}
			if (rec.IOFull != nil) != tc.ioFull {
				t.Errorf("io_full = %v, want present %t", rec.IOFull, tc.ioFull)
			}
			if rec.Blocked != nil && *rec.Blocked != 6 {
				t.Errorf("blocked = %d, want 6", *rec.Blocked)
			}
			if rec.IOSome != nil && *rec.IOSome != 3.25 {
				t.Errorf("io_some = %g, want 3.25", *rec.IOSome)
			}
			if rec.IOFull != nil && *rec.IOFull != 1.25 {
				t.Errorf("io_full = %g, want 1.25", *rec.IOFull)
			}
			line, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []struct {
				name    string
				present bool
			}{{`"blocked"`, tc.blocked}, {`"io_some"`, tc.ioSome}, {`"io_full"`, tc.ioFull}} {
				if got := bytes.Contains(line, []byte(field.name)); got != field.present {
					t.Errorf("JSON field %s present = %t, want %t: %s", field.name, got, field.present, line)
				}
			}
		})
	}
}

// refusingMeter fails the test if the machine is measured at all.
type refusingMeter struct{ t *testing.T }

func (m refusingMeter) Check(context.Context, pressure.Limits, string, bool) (pressure.Verdict, error) {
	m.t.Error("measured after the queue refused")
	return pressure.Verdict{}, nil
}

// mb reads a dirty field, -1 when the line has none.
func mb(v *float64) float64 {
	if v == nil {
		return -1
	}
	return *v
}

func TestAdmissionJournalRefusedBeforeMeasure(t *testing.T) {
	fakeMachine(t)
	oldFactory := pressureMeterFactory
	pressureMeterFactory = func(string) machineMeter { return refusingMeter{t} }
	defer func() { pressureMeterFactory = oldFactory }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := WaitWithSettle(ctx, "synthetic", "projectA:exec:go", nil, testLoad, filepath.Join(t.TempDir(), "load.lock"), "", 0, 0, io.Discard, ""); err == nil {
		t.Fatal("a cancelled wait was admitted")
	}
	lines := readAdmissionLines(t, filepath.Join(os.Getenv("XDG_CACHE_HOME"), "graft", "admissions.jsonl"))
	last := lines[len(lines)-1]
	if last.Verdict != "refused" || last.DirtyMB != nil || last.DirtyLimitMB != nil {
		t.Fatalf("refusal before any measure = %+v", last)
	}
}

func TestAdmissionJournalRefusesAtDeadline(t *testing.T) {
	fakeMachine(t)
	oldFactory := pressureMeterFactory
	pressureMeterFactory = func(string) machineMeter {
		return &admissionMeter{verdicts: []pressure.Verdict{{
			Measure: pressure.Writeback, Value: 200, Limit: 64,
			Load: pressure.Load{Runnable: 1, Threads: 22, DirtyMB: 200, DirtyLimitMB: 64},
		}}}
	}
	defer func() { pressureMeterFactory = oldFactory }()
	l := testLoad
	l.Poll = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _, err := WaitWithSettle(ctx, "synthetic", "projectA:exec:go", nil, l, filepath.Join(t.TempDir(), "load.lock"), "", 0, 0, io.Discard, "")
	if err == nil {
		t.Fatal("wait passed its context deadline")
	}
	lines := readAdmissionLines(t, filepath.Join(os.Getenv("XDG_CACHE_HOME"), "graft", "admissions.jsonl"))
	if len(lines) < 2 || lines[len(lines)-1].Verdict != "refused" || lines[len(lines)-1].Why == "" {
		t.Fatalf("deadline refusal missing: %+v", lines)
	}
}

func TestAdmissionRecordNamesFailingDiskSource(t *testing.T) {
	fault := fmt.Errorf("%w: connect: refused", pressure.ErrIOSourceFailed)
	for _, c := range []struct {
		fault error
		want  string
	}{
		{fault, `"disk_fault":"disk state source failed: connect: refused"`},
		{pressure.ErrNoIOSource, `"disk_fault":` + strconv.Quote(pressure.ErrNoIOSource.Error())},
	} {
		m := &machine{haveView: true, lastView: pressure.Verdict{Load: pressure.Load{DiskFault: c.fault}}}
		line, err := json.Marshal(m.admissionRecord(false))
		if err != nil || !bytes.Contains(line, []byte(c.want)) {
			t.Errorf("journal line %s (%v), want %s", line, err, c.want)
		}
	}
	ok := &machine{haveView: true, lastView: pressure.Verdict{Load: pressure.Load{Disk: &pressure.DiskNow{}}}}
	if line, _ := json.Marshal(ok.admissionRecord(false)); bytes.Contains(line, []byte("disk_fault")) {
		t.Errorf("journal line with a disk reading names a fault: %s", line)
	}
}

func TestAdmissionRecordOmitsUnavailableDirtyMeasures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		limits  pressure.Limits
		skipped []error
		present bool
		dirty   bool
		limit   float64
	}{
		{name: "no meminfo", skipped: []error{pressure.ErrNoMemInfo}},
		{name: "no machine dirty limit", skipped: []error{pressure.ErrNoDirtyLimit}, dirty: true, limit: 64},
		{name: "custom limit without measured machine limit", limits: pressure.Limits{MaxDirtyMB: 32}, skipped: []error{pressure.ErrNoDirtyLimit}, dirty: true, limit: 64},
		{name: "custom limit but no measured value", limits: pressure.Limits{MaxDirtyMB: 32}, dirty: true},
		{name: "measured limit", present: true, dirty: true, limit: 64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &machine{limits: tc.limits, haveView: true, lastView: pressure.Verdict{
				Load: pressure.Load{DirtyMB: 99, DirtyLimitMB: tc.limit}, Skipped: tc.skipped,
			}}
			line, err := json.Marshal(m.admissionRecord(false))
			if err != nil {
				t.Fatal(err)
			}
			gotLimit := bytes.Contains(line, []byte(`"dirty_limit_mb"`))
			if gotLimit != tc.present {
				t.Fatalf("dirty_limit_mb present = %t, want %t: %s", gotLimit, tc.present, line)
			}
			gotDirty := bytes.Contains(line, []byte(`"dirty_mb"`))
			if gotDirty != tc.dirty {
				t.Fatalf("dirty_mb present = %t, want %t: %s", gotDirty, tc.dirty, line)
			}
			if tc.present && (m.admissionRecord(false).DirtyLimitMB == nil || *m.admissionRecord(false).DirtyLimitMB != tc.limit) {
				t.Fatalf("measured dirty limit = %v, want %g", m.admissionRecord(false).DirtyLimitMB, tc.limit)
			}
		})
	}
}

func readAdmissionLines(t *testing.T, path string) []admissionRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []admissionRecord
	for line := range bytes.SplitSeq(bytes.TrimSpace(data), []byte("\n")) {
		var rec admissionRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("invalid admission record %q: %v", line, err)
		}
		lines = append(lines, rec)
	}
	return lines
}
