package tasks

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// removeOld removes the programs a Windows install moved aside, by name
// prefix: a name with glob characters in it is matched literally, and the
// other files stay.
func TestRemoveOld(t *testing.T) {
	dir := t.TempDir()
	names := []string{".tool[1].old-1", ".tool[1].old-2", ".tool1.old-3", "tool[1]", ".tool[1].oldish"}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeOld(dir, ".tool[1].old-"); err != nil {
		t.Fatal(err)
	}
	var left []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if want := []string{".tool1.old-3", ".tool[1].oldish", "tool[1]"}; !slices.Equal(left, want) {
		t.Errorf("left %q, want %q", left, want)
	}
	if err := removeOld(filepath.Join(dir, "missing"), ".x.old-"); err == nil {
		t.Error("a directory that cannot be listed is no error")
	}
}
