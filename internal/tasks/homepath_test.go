package tasks

import (
	"context"
	"path/filepath"
	"testing"
)

// TestHomePaths: a cache.files entry and an install target starting with ~/
// are under the home directory; a bare ~ is the home directory as an install
// target and a work-tree path as a cache input.
func TestHomePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	root := t.TempDir()
	r := &Runner{Root: root}
	for _, tc := range []struct{ in, want string }{
		{"~/.config/tool.yaml", filepath.Join(home, ".config", "tool.yaml")},
		{"~", filepath.Join(root, "~")},
		{"go.sum", filepath.Join(root, "go.sum")},
	} {
		if got, err := r.inputPath(tc.in); err != nil || got != tc.want {
			t.Errorf("inputPath(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{"~/bin", filepath.Join(home, "bin")},
		{"~", home},
	} {
		if got, err := installDestination(context.Background(), tc.in); err != nil || got != tc.want {
			t.Errorf("installDestination(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}
