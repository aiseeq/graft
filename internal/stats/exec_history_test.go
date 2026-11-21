package stats

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadExecHistoryRejectsMalformedLine(t *testing.T) {
	for _, line := range []string{
		"not-json",
		"{}",
		`{"time":"2026-11-21T00:00:00Z","cmd_key":"projectA-key","tree_mem_mb":null,"exit":0,"cached":false,"host":false}`,
	} {
		t.Run(line, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "exec-history.jsonl")
			if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadExecHistory(path, time.Time{}); err == nil {
				t.Fatal("LoadExecHistory accepted an incomplete or malformed line")
			}
		})
	}
}

func TestExecHistoryAllowsNullTreeMemoryButBarsTheRun(t *testing.T) {
	statsPath := filepath.Join(t.TempDir(), "stats.jsonl")
	key := "projectA-key"
	for range CheapHistoryRuns {
		if err := AppendExecHistory(statsPath, ExecHistoryRecord{Time: time.Now(), CmdKey: key, CPUS: 1, TreeMemMB: new(20.0), Exit: 0}); err != nil {
			t.Fatal(err)
		}
	}
	if err := AppendExecHistory(statsPath, ExecHistoryRecord{Time: time.Now(), CmdKey: key, CPUS: 1, TreeMemMB: nil, Exit: 0}); err != nil {
		t.Fatal(err)
	}
	records, err := LoadExecHistory(ExecHistoryPath(statsPath), time.Now().Add(-CheapHistoryWindow))
	if err != nil {
		t.Fatal(err)
	}
	if WouldCheap(records, key, time.Now().Add(-CheapHistoryWindow)) {
		t.Fatal("null tree memory should be valid but not cheap")
	}
}

func TestPruneExecHistoryDropsOnlyMalformedRowsWithOldReadableTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exec-history.jsonl")
	old := `{"time":"2026-01-01T00:00:00Z","cmd_key":`
	unknownAge := `{"time":"not-a-time","cmd_key":`
	if err := os.WriteFile(path, []byte(old+"\n"+unknownAge+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if err := pruneExecHistory(path, since); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != unknownAge+"\n" {
		t.Fatalf("pruned contents = %q, want unreadable-age row retained", got)
	}
	_, err = LoadExecHistory(path, since)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatal("retained malformed row should still make the decision unavailable")
	}
}

func TestExecHistoryDecisionWithTenThousandRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exec-history.jsonl")
	now := time.Now()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(file)
	for i := range 10_000 {
		rec := ExecHistoryRecord{Time: now.Add(-time.Duration(i) * time.Second), CmdKey: "projectA-key", CPUS: 1, TreeMemMB: new(20.0), Exit: 0}
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	since := now.Add(-CheapHistoryWindow)
	start := time.Now()
	history, err := LoadExecHistory(path, since)
	if err != nil {
		t.Fatal(err)
	}
	got := WouldCheap(history, "projectA-key", since)
	elapsed := time.Since(start)
	t.Logf("would_cheap on %d compact rows: %s", len(history), elapsed)
	if elapsed > time.Second {
		t.Fatalf("history decision took %s, want at most 1s", elapsed)
	}
	if !got {
		t.Fatal("10,000 successful local rows should be cheap")
	}
}

func TestExecHistoryCacheReadOnly(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	statsPath, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	path := ExecHistoryPath(statsPath)
	since := time.Now().Add(-CheapHistoryWindow)
	start := time.Now()
	history, err := LoadExecHistory(path, since)
	if err != nil {
		t.Fatal(err)
	}
	key := ""
	if len(history) > 0 {
		key = history[0].CmdKey
	}
	_ = WouldCheap(history, key, since)
	elapsed := time.Since(start)
	t.Logf("read-only real exec history: %d rows, decision %s", len(history), elapsed)
	if elapsed > time.Second {
		t.Fatalf("real exec history decision took %s, want at most 1s", elapsed)
	}
}

func TestAppendExecHistoryPrunesOldRowsAfterOneMiB(t *testing.T) {
	statsPath := filepath.Join(t.TempDir(), "stats.jsonl")
	path := ExecHistoryPath(statsPath)
	old := ExecHistoryRecord{Time: time.Now().Add(-2 * CheapHistoryWindow), CmdKey: "projectA-old", CPUS: 1, TreeMemMB: new(20.0), Exit: 0}
	line, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 0, execHistoryMaxSize+len(line)+1)
	for len(data) <= execHistoryMaxSize {
		data = append(data, line...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	recent := ExecHistoryRecord{Time: time.Now(), CmdKey: "projectA-recent", CPUS: 1, TreeMemMB: new(20.0), Exit: 0}
	if err := AppendExecHistory(statsPath, recent); err != nil {
		t.Fatal(err)
	}
	history, err := LoadExecHistory(path, time.Now().Add(-CheapHistoryWindow))
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].CmdKey != recent.CmdKey {
		t.Fatalf("pruned history = %+v, want only recent row", history)
	}
}
