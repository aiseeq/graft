package stats

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func historyFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	path := filepath.Join(root, "stats.jsonl")
	at := time.Now().UTC().Truncate(time.Second)
	records := []Record{
		{Time: at, Kind: KindTask, Task: "build", Top: "gate", Project: "/p", WallS: 3},
		{Time: at.Add(time.Second), Kind: KindTask, Task: "test", Top: "gate", Project: "/p", WallS: 5},
	}
	var data bytes.Buffer
	for _, r := range records {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		data.Write(b)
		data.WriteByte('\n')
	}
	data.WriteString("malformed stats line\n")
	if err := os.WriteFile(path+".1", data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []PackageRun{
		{Time: at, Project: "/p", Task: "test", Package: "example/a", Result: ResultPass, Tests: []TestResult{{Name: "TestA", Result: ResultPass}}},
		{Time: at.Add(time.Second), Project: "/p", Task: "test", Package: "example/b", Result: ResultFail, Tests: []TestResult{{Name: "TestB", Result: ResultFail, Reason: "oops"}}},
	}
	data.Reset()
	for _, r := range tests {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		data.Write(b)
		data.WriteByte('\n')
	}
	data.WriteString("malformed tests line\n")
	if err := os.WriteFile(TestsPath(path)+".1", data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, filepath.Join(root, "cache", "graft", "history-gz")
}

func reportOutput(t *testing.T, records []Record) string {
	t.Helper()
	rows, _ := Summarize(records, Filter{})
	var b strings.Builder
	if err := Render(&b, rows, "TASK", false); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func testsOutput(t *testing.T, path string) string {
	t.Helper()
	var runs []PackageRun
	malformed, err := ReadTests(path, time.Time{}, func(r PackageRun) error { runs = append(runs, r); return nil })
	if err != nil || malformed != 1 {
		t.Fatalf("ReadTests: malformed=%d err=%v", malformed, err)
	}
	rep := summarizeTests(runs, TestFilter{}, 20)
	var b strings.Builder
	if err := RenderTests(&b, rep, false); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestHistoryGZMatchesUncompressedReports(t *testing.T) {
	path, dir := historyFixture(t)
	got, bad, err := Load(path, time.Time{})
	if err != nil || bad != 1 {
		t.Fatalf("Load: malformed=%d err=%v", bad, err)
	}
	want, wantMalformed, err := loadFile(path + ".1")
	if err != nil || wantMalformed != bad || reportOutput(t, got) != reportOutput(t, want) {
		t.Fatalf("stats output/malformed differs from JSONL: malformed=%d err=%v", wantMalformed, err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("gzip cache was not created: %v", err)
	}
	if got, want := testsOutput(t, TestsPath(path)), testsOutputDirect(t, TestsPath(path)+".1"); got != want {
		t.Fatalf("tests output differs from JSONL\n got: %s\nwant: %s", got, want)
	}
}

func testsOutputDirect(t *testing.T, path string) string {
	t.Helper()
	runs, malformed, err := decodePackageRuns(mustOpen(t, path))
	if err != nil || malformed != 1 {
		t.Fatalf("decode tests JSONL: malformed=%d err=%v", malformed, err)
	}
	var b strings.Builder
	if err := RenderTests(&b, summarizeTests(runs, TestFilter{}, 20), false); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestHistoryGZInvalidationAndCorruptionFallback(t *testing.T) {
	t.Run("append", func(t *testing.T) {
		path, _ := historyFixture(t)
		if _, _, err := Load(path, time.Time{}); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(path+".1", os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.WriteString(f, "{\"kind\":\"task\",\"task\":\"new\",\"top\":\"gate\"}\n")
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			t.Fatal(err)
		}
		recs, bad, err := Load(path, time.Time{})
		if err != nil || bad != 1 || len(recs) != 3 || recs[2].Task != "new" {
			t.Fatalf("stale gzip used: records=%+v malformed=%d err=%v", recs, bad, err)
		}
	})
	for _, mode := range []string{"truncated", "crc"} {
		t.Run(mode, func(t *testing.T) {
			path, dir := historyFixture(t)
			if _, _, err := Load(path, time.Time{}); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			gzPath := filepath.Join(dir, entries[0].Name())
			data, err := os.ReadFile(gzPath)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "truncated" {
				data = data[:len(data)-5]
			} else {
				source, err := os.Open(path + ".1")
				if err != nil {
					t.Fatal(err)
				}
				key, keyErr := historyStat(source)
				source.Close()
				if keyErr != nil {
					t.Fatal(keyErr)
				}
				var poisoned bytes.Buffer
				writer := gzip.NewWriter(&poisoned)
				encoded, _ := json.Marshal(key)
				writer.Comment = string(encoded)
				_, _ = io.WriteString(writer, "{\"kind\":\"task\",\"task\":\"poisoned\",\"top\":\"gate\"}\n")
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				data = poisoned.Bytes()
				data[len(data)-8] ^= 0xff
			}
			if err := os.WriteFile(gzPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			recs, bad, err := Load(path, time.Time{})
			if err != nil || bad != 1 || len(recs) != 2 {
				t.Fatalf("fallback duplicated or lost lines: records=%d malformed=%d err=%v", len(recs), bad, err)
			}
		})
	}
}

func TestHistoryGZRejectsSourceAppendBeforeAcceptance(t *testing.T) {
	path, _ := historyFixture(t)
	sourcePath := path + ".1"
	source, err := os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	key, err := historyStat(source)
	source.Close()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	gzPath, err := historyGZPath(sourcePath, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(gzPath), 0o700); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	encoded, err := json.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}
	writer.Comment = string(encoded)
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gzPath, compressed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	records, malformed, err := readHistoryWithHook(sourcePath, decodeRecords, func() {
		f, err := os.OpenFile(sourcePath, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := io.WriteString(f, "{\"kind\":\"task\",\"task\":\"arrived\",\"top\":\"gate\"}\n")
		if closeErr := f.Close(); writeErr == nil {
			writeErr = closeErr
		}
		if writeErr != nil {
			t.Fatal(writeErr)
		}
	})
	if err != nil || malformed != 1 || len(records) != 3 || records[2].Task != "arrived" {
		t.Fatalf("gzip accepted after source append: records=%+v malformed=%d err=%v", records, malformed, err)
	}
}

func TestHistoryGZRejectsWrongHeaderKey(t *testing.T) {
	path, dir := historyFixture(t)
	if _, _, err := Load(path, time.Time{}); err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	key, err := historyStat(source)
	source.Close()
	if err != nil {
		t.Fatal(err)
	}
	gzPath, err := historyGZPath(path+".1", key)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(payload, []byte(`"task":"build"`)) {
		t.Fatal("fixture does not contain expected task")
	}
	payload = bytes.Replace(payload, []byte(`"task":"build"`), []byte(`"task":"evil!"`), 1)
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	gz.Comment = "wrong source key"
	if _, err := gz.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gzPath, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	recs, malformed, err := Load(path, time.Time{})
	if err != nil || malformed != 1 || len(recs) != 2 || recs[0].Task == "evil!" {
		t.Fatalf("wrong gzip key accepted: records=%+v malformed=%d err=%v", recs, malformed, err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryGZChecksCRCBeforeDecode(t *testing.T) {
	path, dir := historyFixture(t)
	source, err := os.Open(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	key, err := historyStat(source)
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	writer := gzip.NewWriter(&data)
	encoded, _ := json.Marshal(key)
	writer.Comment = string(encoded)
	_, _ = writer.Write(bytes.Repeat([]byte("x"), int(key.Size)))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	compressed := data.Bytes()
	compressed[len(compressed)-8] ^= 0xff
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	gzPath := filepath.Join(dir, "candidate.gz")
	if err := os.WriteFile(gzPath, compressed, 0o600); err != nil {
		t.Fatal(err)
	}
	gz, err := os.Open(gzPath)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	called := false
	_, _, readErr := readValidGZ(gz, source, key, func(r io.Reader) (struct{}, int, error) {
		called = true
		_, err := io.ReadAll(r)
		return struct{}{}, 0, err
	})
	if readErr == nil || called {
		t.Fatalf("bad CRC reached decoder: err=%v decoder_called=%v", readErr, called)
	}
}

func TestReadTestsCorruptGZFallsBackWithoutPartialRuns(t *testing.T) {
	for _, mode := range []string{"truncated", "crc"} {
		t.Run(mode, func(t *testing.T) {
			statsPath, dir := historyFixture(t)
			testsPath := TestsPath(statsPath)
			testsOutput(t, testsPath)
			source, err := os.Open(testsPath + ".1")
			if err != nil {
				t.Fatal(err)
			}
			key, err := historyStat(source)
			source.Close()
			if err != nil {
				t.Fatal(err)
			}
			gzPath, err := historyGZPath(testsPath+".1", key)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(gzPath)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "truncated" {
				data = data[:len(data)-5]
			} else {
				var poisoned bytes.Buffer
				writer := gzip.NewWriter(&poisoned)
				encoded, _ := json.Marshal(key)
				writer.Comment = string(encoded)
				_, _ = io.WriteString(writer, "{\"package\":\"poisoned\",\"result\":\"pass\"}\n")
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				data = poisoned.Bytes()
				data[len(data)-8] ^= 0xff
			}
			if err := os.WriteFile(gzPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			var runs []PackageRun
			malformed, err := ReadTests(testsPath, time.Time{}, func(r PackageRun) error {
				runs = append(runs, r)
				return nil
			})
			if err != nil || malformed != 1 || len(runs) != 2 || runs[0].Package == "poisoned" {
				t.Fatalf("corrupt gzip leaked runs: runs=%+v malformed=%d err=%v", runs, malformed, err)
			}
			if _, err := os.Stat(dir); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHistoryGZSweepRemovesMissingCopy(t *testing.T) {
	path, dir := historyFixture(t)
	if _, _, err := Load(path, time.Time{}); err != nil {
		t.Fatal(err)
	}
	_ = testsOutput(t, TestsPath(path))
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		names := make([]string, len(entries))
		for i := range entries {
			names[i] = entries[i].Name()
		}
		t.Fatalf("gzip files=%q err=%v", names, err)
	}
	if err := os.Remove(path + ".1"); err != nil {
		t.Fatal(err)
	}
	sweepHistoryGZ(path)
	entries, err = os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "tests.jsonl-") {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("sweep left unexpected files: %q", names)
	}
}

func TestHistoryGZHeaderContainsKeyAndValidCRC(t *testing.T) {
	path, dir := historyFixture(t)
	if _, _, err := Load(path, time.Time{}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(gz.Comment, `"sha256_edges"`) {
			t.Errorf("gzip header lacks identity key: %s", gz.Comment)
		}
		if _, err := io.Copy(io.Discard, gz); err != nil {
			t.Errorf("gzip checksum: %v", err)
		}
		gz.Close()
		f.Close()
	}
}
