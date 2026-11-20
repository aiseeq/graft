package stats

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"testing"
	"time"
)

func benchmarkHistoryGZ(b *testing.B, singlePass bool) {
	b.Helper()
	record := Record{
		Time: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC),
		Kind: KindTask, Project: "/synthetic/project", Task: "test", Top: "gate",
		WallS: 12.5, UserS: 8.25, SysS: 1.5, Step: "go test -count=1 ./...",
	}
	line, err := json.Marshal(record)
	if err != nil {
		b.Fatal(err)
	}
	line = append(line, '\n')
	plain := bytes.Repeat(line, 150_000)
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(plain); err != nil {
		b.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		b.Fatal(err)
	}
	gzData := compressed.Bytes()
	b.SetBytes(int64(len(plain)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		input := bytes.NewReader(gzData)
		zr, err := gzip.NewReader(input)
		if err != nil {
			b.Fatal(err)
		}
		if singlePass {
			buffer := bytes.NewBuffer(make([]byte, 0, len(plain)))
			_, err := io.Copy(buffer, zr)
			if err == nil {
				err = zr.Close()
			} else {
				zr.Close()
			}
			if err != nil {
				b.Fatal(err)
			}
			if _, malformed, err := decodeRecords(bytes.NewReader(buffer.Bytes())); err != nil || malformed != 0 {
				b.Fatalf("decode buffered JSONL: malformed=%d err=%v", malformed, err)
			}
			continue
		}
		if _, err := io.Copy(io.Discard, zr); err != nil {
			b.Fatal(err)
		}
		if err := zr.Close(); err != nil {
			b.Fatal(err)
		}
		if _, err := input.Seek(0, io.SeekStart); err != nil {
			b.Fatal(err)
		}
		zr, err = gzip.NewReader(input)
		if err != nil {
			b.Fatal(err)
		}
		if _, malformed, err := decodeRecords(zr); err != nil || malformed != 0 {
			b.Fatalf("decode two-pass JSONL: malformed=%d err=%v", malformed, err)
		}
		if err := zr.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHistoryGZTwoPass(b *testing.B) { benchmarkHistoryGZ(b, false) }

func BenchmarkHistoryGZSinglePass(b *testing.B) { benchmarkHistoryGZ(b, true) }
