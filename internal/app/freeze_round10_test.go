package app

import (
	"strings"
	"testing"
	"time"
)

func TestFreezeLogTimeIsUTC(t *testing.T) {
	frozen, err := time.Parse(time.RFC3339Nano, "2026-10-20T16:00:00.5+02:00")
	if err != nil {
		t.Fatal(err)
	}
	after, err := freezeLogTime("freeze-log/acc/v1-2-10/20261020T141000-abc1234-bypass")
	if err != nil {
		t.Fatal(err)
	}
	if after.Before(frozen.Truncate(time.Second)) {
		t.Fatalf("record at %s is judged before the freeze at %s", after, frozen)
	}
	sameSecond, err := freezeLogTime("freeze-log/acc/v1-2-10/20261020T140000-abc1234-bypass")
	if err != nil {
		t.Fatal(err)
	}
	if sameSecond.Before(frozen.Truncate(time.Second)) {
		t.Fatalf("record in the freeze's second is judged before it")
	}
	before, err := freezeLogTime("freeze-log/acc/v1-2-10/20261020T135759-abc1234-bypass")
	if err != nil {
		t.Fatal(err)
	}
	if !before.Before(frozen.Truncate(time.Second)) {
		t.Fatalf("record at %s is not judged before the freeze at %s", before, frozen)
	}
}

func TestFreezeJournalLineNamesUTC(t *testing.T) {
	line, err := freezeJournalLine("freeze-log/acc/v1-2-10/20261020T140000-abc1234-bypass", "object x\n\ntarget: acc\ncommit: abc1234def\nreason: urgent\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "2026-10-20 14:00:00 UTC") {
		t.Fatalf("journal line = %q, want the UTC time named", line)
	}
}
