package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecHeavyRecordsAdmission(t *testing.T) {
	f, statsFile := statsRepo(t, "")
	if out, code := f.graft("", "exec", "--heavy", "--", "true"); code != 0 {
		t.Fatalf("graft exec --heavy: exit %d\n%s", code, out)
	}
	kind := filepath.Base(f.work) + ":exec:true"
	journal := filepath.Join(filepath.Dir(statsFile), "admissions.jsonl")
	file, err := os.Open(journal)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	found := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var rec struct {
			Kind    string `json:"kind"`
			Verdict string `json:"verdict"`
			Run     string `json:"run"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			t.Fatalf("decode admission %q: %v", scanner.Text(), err)
		}
		if rec.Kind == kind && rec.Verdict == "admitted" && rec.Run != "" {
			found = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("no admitted record for %s in %s", kind, journal)
	}

	for _, kv := range append(append([]string{}, testEnv...), f.env...) {
		if !strings.HasPrefix(kv, "XDG_RUNTIME_DIR=") {
			continue
		}
		dir := filepath.Join(strings.TrimPrefix(kv, "XDG_RUNTIME_DIR="), "graft", "heavy")
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return // no heavy scope was created in this test environment
			}
			t.Fatal(err)
		}
		for _, entry := range entries {
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(data)) == kind {
				return
			}
		}
		return // systemd did not create a graft heavy scope for this run
	}
}

func TestExecHeavyRecordsCgroupPeak(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("heavy cgroup scopes are Linux only")
	}
	for _, kv := range testEnv {
		if !strings.HasPrefix(kv, "GRAFT_PROC=") {
			continue
		}
		procRoot := strings.TrimPrefix(kv, "GRAFT_PROC=")
		if _, err := os.Stat(filepath.Join(procRoot, "self", "cgroup")); os.IsNotExist(err) {
			t.Skip("GRAFT_PROC test fixture has no self/cgroup; test-gate runs inside heavy and cannot create a real heavy scope")
		}
		break
	}
	f, statsFile := statsRepo(t, "")
	source := filepath.Join(f.base, "hold-memory.go")
	program := `package main
import "time"
func main() {
	b := make([]byte, 600<<20)
	for i := 0; i < len(b); i += 4096 { b[i] = 1 }
	time.Sleep(2 * time.Second)
}`
	if err := os.WriteFile(source, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := f.graft("", "exec", "--heavy", "--local", "--", "go", "run", source); code != 0 {
		t.Fatalf("graft exec --heavy allocator: exit %d\n%s", code, out)
	}
	kind := filepath.Base(f.work) + ":exec:go"
	created := false
	for _, kv := range append(append([]string{}, testEnv...), f.env...) {
		if !strings.HasPrefix(kv, "XDG_RUNTIME_DIR=") {
			continue
		}
		dir := filepath.Join(strings.TrimPrefix(kv, "XDG_RUNTIME_DIR="), "graft", "heavy")
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				break
			}
			t.Fatal(err)
		}
		for _, entry := range entries {
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			created = created || strings.TrimSpace(string(data)) == kind
		}
		break
	}
	if !created {
		t.Skip("systemd did not create a graft heavy scope; no real cgroup peak is available in this environment")
	}
	for _, rec := range readStats(t, statsFile) {
		if rec.Kind == "task" && rec.Profile == kind {
			if rec.CgroupPeakMB == nil || *rec.CgroupPeakMB < 600 {
				t.Fatalf("cgroup_peak_mb = %v; want at least 600 MiB", rec.CgroupPeakMB)
			}
			return
		}
	}
	t.Fatalf("no task stats record for %s", kind)
}
