package taskcache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestChangedParts(t *testing.T) {
	parts := map[string]string{"tree": "tree-1", "definition": "definition-1", "inputs": "inputs-1", "graft": "graft-1"}
	tests := []struct {
		name string
		prev map[string]string
		curr map[string]string
		want []string
	}{
		{name: "unchanged", prev: parts, curr: parts},
		{name: "tree", prev: parts, curr: map[string]string{"tree": "tree-2", "definition": "definition-1", "inputs": "inputs-1", "graft": "graft-1"}, want: []string{"tree"}},
		{name: "definition", prev: parts, curr: map[string]string{"tree": "tree-1", "definition": "definition-2", "inputs": "inputs-1", "graft": "graft-1"}, want: []string{"definition"}},
		{name: "inputs", prev: parts, curr: map[string]string{"tree": "tree-1", "definition": "definition-1", "inputs": "inputs-2", "graft": "graft-1"}, want: []string{"inputs"}},
		{name: "graft", prev: parts, curr: map[string]string{"tree": "tree-1", "definition": "definition-1", "inputs": "inputs-1", "graft": "graft-2"}, want: []string{"graft"}},
		{name: "missing current part", prev: parts, curr: map[string]string{"tree": "tree-1", "definition": "definition-1", "graft": "graft-1"}, want: []string{"inputs"}},
		{name: "missing previous part", prev: map[string]string{"tree": "tree-1", "definition": "definition-1", "graft": "graft-1"}, curr: parts, want: []string{"inputs"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ChangedParts(tt.prev, tt.curr); !slices.Equal(got, tt.want) {
				t.Errorf("ChangedParts = %v, want %v", got, tt.want)
			}
		})
	}
}

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
