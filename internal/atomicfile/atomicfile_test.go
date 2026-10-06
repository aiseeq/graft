package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReplacesAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state")
	for _, content := range []string{"one\n", "two\n"} {
		if err := Write(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "two\n" {
		t.Fatalf("%q %v", data, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Errorf("leftovers: %v %v", entries, err)
	}
	if err := Write(filepath.Join(dir, "missing", "x"), nil, 0o600); err == nil {
		t.Error("writing into a missing directory passed")
	}
}
