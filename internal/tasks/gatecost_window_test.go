package tasks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/stats"
)

func TestGateStepCostsDeepensOnceForMissingPrice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	now := time.Now().UTC()
	rows := []stats.Record{
		{Time: now.Add(-time.Hour), Kind: stats.KindStep, Task: "gate", Project: "/repo/tree", Profile: "repo:gate", Step: "old", Run: "a", Exit: 0, WallS: 1},
		{Time: now.Add(-time.Minute), Kind: stats.KindStep, Task: "gate", Project: "/repo/tree", Profile: "repo:unit", Step: "new", Run: "b", Exit: 0, WallS: 3},
		{Time: now, Kind: stats.KindStep, Task: "gate", Project: "/repo/tree", Profile: "repo:gate", Step: "go vet", Run: "c", Host: "pc", Exit: 0, WallS: 40},
	}
	var data []byte
	for _, row := range rows {
		b, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		data = append(append(data, b...), '\n')
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	costs := &gateStepCosts{local: map[string]gateStepCost{}, host: "pc", path: path, project: "/repo/tree", now: now}
	costs.deepMatches = []stats.RecordMatch{{Profile: "repo:unit", Step: "new"}}
	if err := costs.deepen(); err != nil {
		t.Fatal(err)
	}
	if !costs.deepened || costs.local[gateCostKey("repo:unit", "new")] != (gateStepCost{here: 3, ok: true}) || !costs.hasFee || costs.fee != 40 {
		t.Fatalf("deepened prices = %+v", costs)
	}
	rows[1].WallS = 9
	data = nil
	for _, row := range rows {
		b, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		data = append(append(data, b...), '\n')
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := costs.deepen(); err != nil {
		t.Fatal(err)
	}
	if costs.local[gateCostKey("repo:unit", "new")].here != 3 {
		t.Fatal("second miss repeated deepening")
	}
}

func TestGateStepCostsDeepenOnlyMissingKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	now := time.Now().UTC()
	var data []byte
	appendRow := func(rec stats.Record) {
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		data = append(append(data, b...), '\n')
	}
	for i, wall := range []float64{20, 20, 20} {
		appendRow(stats.Record{Time: now.Add(-time.Hour), Kind: stats.KindStep, Task: "gate", Project: "/repo", Profile: "repo:gate", Step: "known", Run: fmt.Sprintf("old%d", i), WallS: wall, Exit: 0})
	}
	appendRow(stats.Record{Time: now, Kind: stats.KindStep, Task: "gate", Project: "/repo", Profile: "repo:gate", Step: "missing", Run: "miss", WallS: 3, Exit: 0})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	costs := &gateStepCosts{local: map[string]gateStepCost{"repo:gate\x00known": gateLocalPrice([]float64{1, 2})}, host: "pc", path: path, project: "/repo", now: now, deepMatches: []stats.RecordMatch{{Profile: "repo:gate", Step: "known"}, {Profile: "repo:gate", Step: "missing"}}}
	if err := costs.deepen(); err != nil {
		t.Fatal(err)
	}
	if got := costs.local[gateCostKey("repo:gate", "known")].here; got != 1.5 {
		t.Fatalf("tail price after deepening = %v, want 1.5", got)
	}
	remote, _ := gateCostPlacement(costs.local[gateCostKey("repo:gate", "known")], 10, "pc")
	if remote {
		t.Fatal("key with the fresh tail median was sent to the host after deepening")
	}
	if got := costs.local[gateCostKey("repo:gate", "missing")]; !got.ok || got.here != 3 {
		t.Fatalf("missing key was not filled: %+v", got)
	}
}

func TestMergeMissingGatePricesKeepsTailMedian(t *testing.T) {
	current := map[string]gateStepCost{"known": gateLocalPrice([]float64{1, 2})}
	mergeMissingGatePrices(current, map[string]gateStepCost{
		"known":  gateLocalPrice([]float64{20, 20, 20, 1, 2}),
		"missed": gateLocalPrice([]float64{3}),
	})
	if got := current["known"].here; got != 1.5 {
		t.Fatalf("deep window changed tail median to %v, want 1.5", got)
	}
	if got := current["missed"]; !got.ok || got.here != 3 {
		t.Fatalf("deep window did not fill missing key: %+v", got)
	}
	remote, _ := gateCostPlacement(current["known"], 10, "pc")
	if remote {
		t.Fatal("tail-priced key moved to the host after deepening")
	}
}

func TestGateDeepeningDoesNotCountHostRowsForLocalPrice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	now := time.Now().UTC()
	encode := func(rec stats.Record) []byte {
		t.Helper()
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		return append(data, '\n')
	}
	local := stats.Record{Time: now.Add(-time.Hour), Kind: stats.KindStep, Task: "gate", Project: "/repo", Profile: "repo:gate", Step: "go vet ./...", Run: "local", Exit: 0, WallS: 1.25}
	if err := os.WriteFile(path+".1", encode(local), 0o600); err != nil {
		t.Fatal(err)
	}
	var newest []byte
	for i := range 6 {
		newest = append(newest, encode(stats.Record{Time: now.Add(time.Duration(i) * time.Second), Kind: stats.KindStep, Task: "gate", Project: "/repo", Profile: "repo:gate", Step: "go vet ./...", Run: fmt.Sprintf("host-%d", i), Host: "pc", Exit: 0, WallS: 40 + float64(i)})...)
	}
	if err := os.WriteFile(path, newest, 0o600); err != nil {
		t.Fatal(err)
	}
	costs := &gateStepCosts{local: map[string]gateStepCost{}, host: "pc", project: "/repo", path: path, now: now, hasFee: true, fee: 40, deepMatches: []stats.RecordMatch{{Profile: "repo:gate", Step: "go vet ./..."}}}
	if err := costs.deepen(); err != nil {
		t.Fatal(err)
	}
	if got := costs.local[gateCostKey("repo:gate", "go vet ./...")]; !got.ok || got.here != 1.25 {
		t.Fatalf("deep scan failed to find old local price behind host rows: %+v", got)
	}
}

func TestGateCostPlacementRules(t *testing.T) {
	if gateStatsDeepBytes != 256<<20 {
		t.Fatalf("deep scan cap = %d, want 256 MiB", gateStatsDeepBytes)
	}
	if got := gateLocalPrice([]float64{2}); got != (gateStepCost{here: 2, ok: true}) {
		t.Fatalf("one fresh local value = %+v", got)
	}
	remote, why := gateCostPlacement(gateStepCost{}, 10, "pc")
	if remote || why != "price probe here: no fresh local price, 10.0s on pc" {
		t.Fatalf("unknown local price placement = %v, %q", remote, why)
	}
	remote, why = gateCostPlacement(gateStepCost{here: 2, ok: true}, 10, "pc")
	if remote || why != "cheaper here: 2.0s here, 10.0s on pc" {
		t.Fatalf("cheap local price placement = %v, %q", remote, why)
	}
	remote, why = gateCostPlacement(gateStepCost{here: 12, ok: true}, 10, "pc")
	if !remote || why != "" {
		t.Fatalf("expensive local price placement = %v, %q", remote, why)
	}
}

func TestGateCostFreshnessUsesSevenDays(t *testing.T) {
	now := time.Now()
	const sevenDays = 7 * 24 * time.Hour
	fresh := stats.Record{Time: now.Add(-sevenDays + time.Nanosecond)}
	if !freshGateCostRecord(&fresh, now) {
		t.Fatal("value just younger than seven days expired")
	}
	old := stats.Record{Time: now.Add(-sevenDays - time.Nanosecond)}
	if freshGateCostRecord(&old, now) {
		t.Fatal("value just older than seven days remained fresh")
	}
}

func TestGateCostWindowDeduplicatesRepeatedRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	now := time.Now().UTC()
	rec := stats.Record{Time: now, Kind: stats.KindStep, Task: "gate", Project: "/repo", Profile: "repo:gate", Step: "check", Run: "same", Exit: 0, WallS: 1}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(append(append([]byte{}, b...), '\n'), append(b, '\n')...), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, _, err := stats.LoadWindow(path, time.Time{}, "repo:", int64(len(b)*2+2))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("duplicate records = %d, want one", len(rows))
	}
}
