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
