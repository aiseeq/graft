package tasks

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gittest"
)

// TestGateFilesListsAsEachPathspecAlone: the one git ls-files of gateFiles
// gives the Markdown files *.md lists and the go.mod files */go.mod lists,
// tracked, untracked and ignored alike, with GIT_ICASE_PATHSPECS too.
func TestGateFilesListsAsEachPathspecAlone(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, ".none"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	gittest.Run(t, root, "init", "-q", "-b", "main")
	for _, name := range []string{
		"README.md", "docs/guide.md", "docs/NOTES.MD", "go.mod", "sub/go.mod",
		"sub/deep/go.mod", "x/GO.MOD", "notes.txt", "md.go", "go.mod.md",
		"docs/go.mod.txt", "ignored.md", "gone/go.mod",
	} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "README.md", "sub/go.mod", "gone/go.mod", ".gitignore")
	gittest.Run(t, root, "commit", "-q", "-m", "base")
	if err := os.Remove(filepath.Join(root, "gone", "go.mod")); err != nil {
		t.Fatal(err)
	}
	alone := func(pathspec string) []string {
		out := gittest.Run(t, root, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", pathspec)
		return slices.DeleteFunc(strings.Split(out, "\x00"), func(s string) bool { return s == "" })
	}
	for _, icase := range []string{"", "1"} {
		t.Run("icase="+icase, func(t *testing.T) {
			t.Setenv("GIT_ICASE_PATHSPECS", icase)
			markdown, goMods, err := NewRunner(root, &config.Config{}, io.Discard, io.Discard).gateFiles()
			if err != nil {
				t.Fatal(err)
			}
			if want := alone("*.md"); !slices.Equal(markdown, want) {
				t.Errorf("Markdown %q, want %q", markdown, want)
			}
			if want := alone("*/go.mod"); !slices.Equal(goMods, want) {
				t.Errorf("go.mod %q, want %q", goMods, want)
			}
		})
	}
}
