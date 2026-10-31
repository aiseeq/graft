package main

import (
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/stats"
)

func TestStatsFailures(t *testing.T) {
	f, _ := statsRepo(t, "tasks:\n  broken:\n    run: ["+helperStep("fail")+"]\n")
	if _, code := f.graft("", "broken"); code == 0 {
		t.Fatal("broken task passed")
	}
	out := f.mustGraft("stats", "--failures", "--task", "broken", "--since", "1d")
	for _, want := range []string{"PROJECT/TREE", "broken", "failing on purpose", "1", "\n    gatehelper: failing on purpose"} {
		if !strings.Contains(out, want) {
			t.Errorf("graft stats --failures missing %q:\n%s", want, out)
		}
	}
}

func TestGateSaysTheSameReason(t *testing.T) {
	f := tasksRepo(t, "gate:\n  - task: broken\ntasks:\n  broken:\n    run: ["+helperStep("fail")+"]\n", nil)
	for attempt := 1; attempt <= 2; attempt++ {
		out, code := f.graft("", "gate")
		if code == 0 || !strings.Contains(out, "see graft stats --failures") {
			t.Fatalf("gate attempt %d: exit %d\n%s", attempt, code, out)
		}
		if attempt == 2 && (!strings.Contains(out, "same reason as") || !strings.Contains(out, "2 times in a row")) {
			t.Errorf("second gate failure did not identify the repeat:\n%s", out)
		}
	}
}

func TestReasonSkipsGraftAndWarnings(t *testing.T) {
	got := stats.CleanReason("graft: running step\nwarning: result cache off\nsource.go:8: failed check\n")
	if got != "source.go:8: failed check" {
		t.Fatalf("CleanReason() = %q", got)
	}
}
