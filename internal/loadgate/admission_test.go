package loadgate

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
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
	profile := json.RawMessage(`{"p90_write_mbs":42}`)
	oldFactory := pressureMeterFactory
	pressureMeterFactory = func(string) machineMeter {
		return &admissionMeter{verdicts: []pressure.Verdict{
			{Measure: pressure.Writeback, Value: 200, Limit: 64, Load: pressure.Load{Runnable: 1, Threads: 22, DirtyMB: 200, DirtyLimitMB: 64, Disk: &pressure.DiskNow{Profile: profile}}},
			{Measure: pressure.Writeback, Value: 200, Limit: 64, Load: pressure.Load{Runnable: 1, Threads: 22, DirtyMB: 200, DirtyLimitMB: 64, Disk: &pressure.DiskNow{Profile: profile}}},
			{Measure: pressure.IO, Value: 35, Limit: 10, Load: pressure.Load{Runnable: 1, Threads: 22, DirtyMB: 18, DirtyLimitMB: 64, Disk: &pressure.DiskNow{Profile: profile}}},
			{Load: pressure.Load{Runnable: 1, Threads: 22, DirtyMB: 12, DirtyLimitMB: 64, Disk: &pressure.DiskNow{Profile: profile}}},
		}}
	}
	defer func() { pressureMeterFactory = oldFactory }()

	l := testLoad
	l.Poll = time.Millisecond
	path := filepath.Join(t.TempDir(), "load.lock")
	_, release, err := WaitWithSettle(context.Background(), "synthetic", "projectA:exec:go", nil, l, path, "", 0, 0, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	journal := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "graft", "admissions.jsonl")
	lines := readAdmissionLines(t, journal)
	if len(lines) != 3 {
		t.Fatalf("got %d records, want 3: %+v", len(lines), lines)
	}
	if lines[0].Verdict != "wait" || lines[0].Measure != WhyWriteback || lines[1].Verdict != "wait" || lines[1].Measure != WhyIO || lines[2].Verdict != "admitted" {
		t.Fatalf("verdict sequence = %+v", lines)
	}
	for _, rec := range lines {
		if rec.Kind != "projectA:exec:go" || rec.Run != "run-synthetic" || string(rec.Profile) != string(profile) {
			t.Errorf("record identity/profile = %+v", rec)
		}
	}
	if lines[0].DirtyMB != 200 || lines[0].DirtyLimitMB != 64 || lines[1].DirtyMB != 18 || lines[1].DirtyLimitMB != 64 {
		t.Fatalf("dirty sample values = %+v", lines)
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
	_, _, err := WaitWithSettle(ctx, "synthetic", "projectA:exec:go", nil, l, filepath.Join(t.TempDir(), "load.lock"), "", 0, 0, io.Discard)
	if err == nil {
		t.Fatal("wait passed its context deadline")
	}
	lines := readAdmissionLines(t, filepath.Join(os.Getenv("XDG_CACHE_HOME"), "graft", "admissions.jsonl"))
	if len(lines) < 2 || lines[len(lines)-1].Verdict != "refused" || lines[len(lines)-1].Why == "" {
		t.Fatalf("deadline refusal missing: %+v", lines)
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
