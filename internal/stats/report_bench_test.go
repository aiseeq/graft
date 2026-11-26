package stats

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func BenchmarkDecodeRecords50K(b *testing.B) {
	line, err := json.Marshal(Record{
		Time:    time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC),
		Kind:    KindStep,
		Task:    "gate",
		Project: "projectA",
		Profile: "projectA:gate",
		Step:    "check",
	})
	if err != nil {
		b.Fatal(err)
	}
	journal := bytes.Repeat(append(line, '\n'), 50_000)
	b.SetBytes(int64(len(journal)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		rows, malformed, err := decodeRecords(bytes.NewReader(journal))
		if err != nil || malformed != 0 || len(rows) != 50_000 {
			b.Fatalf("decode = %d records, %d malformed, %v", len(rows), malformed, err)
		}
	}
}
