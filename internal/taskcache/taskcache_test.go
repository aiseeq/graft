package taskcache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPreviousReadsOnlyLastPass(t *testing.T) {
	dir := t.TempDir()
	for i := range 1000 {
		entry := Entry{Project: "unrelated", Task: fmt.Sprintf("task-%d", i), Time: time.Now(), Parts: map[string]string{"tree": "x"}}
		data, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("pass-%04d.json", i)), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want := Entry{Project: "projectA", Task: "verify", Time: time.Now(), Parts: map[string]string{"tree": "tree-hash"}}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	name := lastName(want.Project, want.Task)
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}

	reads := 0
	got, ok, err := previous(dir, want.Project, want.Task, func(path string) ([]byte, error) {
		reads++
		if filepath.Base(path) != name {
			t.Errorf("read unrelated pass %q", filepath.Base(path))
		}
		return os.ReadFile(path)
	})
	if err != nil || !ok {
		t.Fatalf("Previous = (%+v, %t, %v)", got, ok, err)
	}
	if reads != 1 {
		t.Errorf("read %d files, want only last-pass file", reads)
	}
	if got.Parts["tree"] != want.Parts["tree"] {
		t.Errorf("Previous parts = %v, want %v", got.Parts, want.Parts)
	}
}
