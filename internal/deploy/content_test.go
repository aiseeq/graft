package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/gitx"
)

func TestChangePresentUsesExactChangedLines(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.com")
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(text, subject string) string {
		t.Helper()
		write(text)
		git("add", "file.txt")
		git("commit", "-q", "-m", subject)
		return git("rev-parse", "HEAD")
	}
	commit("alpha\nbeta\ngamma\ndelta\nepsilon\nzeta\n", "base")
	git("checkout", "-q", "-b", "mirror")
	mirror := commit("alpha\nbeta\ngamma\ndelta\nEPSILON\nzeta\n", "mirror change")
	git("checkout", "-q", "main")
	git("checkout", "-q", "-b", "origin")
	commit("alpha\nBETA\ngamma\ndelta\nepsilon\nzeta\n", "origin change")
	git("cherry-pick", mirror)
	deployed := git("rev-parse", "HEAD")

	repo, err := gitx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	present, err := ChangePresent(repo, deployed, mirror)
	if err != nil || !present {
		t.Fatalf("cherry-picked change with changed context: present=%v, err=%v", present, err)
	}

	git("checkout", "-q", "main")
	git("checkout", "-q", "-b", "whitespace")
	git("checkout", "-q", "-b", "mirror-indent")
	indented := commit("def allowed(admin):\n    if admin:\n        pass\n        return True\n", "indent return")
	git("checkout", "-q", "whitespace")
	git("checkout", "-q", "-b", "origin-space")
	spaceBase := commit("def allowed(admin):\n    if admin:\n        pass\n    return  True\n", "extra space")
	present, err = ChangePresent(repo, spaceBase, indented)
	if present || err != nil {
		t.Fatalf("indentation change = present %v, err %v; want absent", present, err)
	}
}

func TestChangePresentRecognizesRemovedLinesAndWholeFiles(t *testing.T) {
	testCases := []struct {
		name        string
		base        map[string]string
		mirror      map[string]string
		deployed    map[string]string
		wantPresent bool
		// orphan makes the mirror's change a root commit of its own.
		orphan bool
	}{
		{
			name:        "text change present, binary change beside it missing",
			base:        map[string]string{"file.txt": "a\n", "img.bin": "\x00old"},
			mirror:      map[string]string{"file.txt": "b\n", "img.bin": "\x00new"},
			deployed:    map[string]string{"file.txt": "b\n", "img.bin": "\x00old"},
			wantPresent: false,
		},
		{
			name:        "text and binary changes both present",
			base:        map[string]string{"file.txt": "a\n", "img.bin": "\x00old"},
			mirror:      map[string]string{"file.txt": "b\n", "img.bin": "\x00new"},
			deployed:    map[string]string{"file.txt": "b\n", "img.bin": "\x00new"},
			wantPresent: true,
		},
		{
			name:        "root commit of the mirror absent",
			base:        map[string]string{"file.txt": "a\n"},
			mirror:      map[string]string{"other.txt": "o\n"},
			deployed:    map[string]string{"file.txt": "a\n"},
			wantPresent: false,
			orphan:      true,
		},
		{
			name:        "removed lines still in deployed branch",
			base:        map[string]string{"file.txt": "keep\nremove me\nmore\n"},
			mirror:      map[string]string{"file.txt": "keep\nmore\n"},
			deployed:    map[string]string{"file.txt": "keep\nremove me\nmore\n"},
			wantPresent: false,
		},
		{
			name:        "removed lines already removed in deployed branch",
			base:        map[string]string{"file.txt": "keep\nremove me\nmore\n"},
			mirror:      map[string]string{"file.txt": "keep\nmore\n"},
			deployed:    map[string]string{"file.txt": "keep\nmore\n"},
			wantPresent: true,
		},
		{
			name:        "deleted file still in deployed branch",
			base:        map[string]string{"file.txt": "remove the file\n"},
			mirror:      map[string]string{},
			deployed:    map[string]string{"file.txt": "remove the file\n"},
			wantPresent: false,
		},
		{
			name:        "deleted file already absent from deployed branch",
			base:        map[string]string{"file.txt": "remove the file\n"},
			mirror:      map[string]string{},
			deployed:    map[string]string{},
			wantPresent: true,
		},
		{
			name:        "created file absent from deployed branch",
			base:        map[string]string{},
			mirror:      map[string]string{"file.txt": "new content\n"},
			deployed:    map[string]string{},
			wantPresent: false,
		},
		{
			name:        "created file already in deployed branch",
			base:        map[string]string{},
			mirror:      map[string]string{"file.txt": "new content\n"},
			deployed:    map[string]string{"file.txt": "new content\n"},
			wantPresent: true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LC_ALL", "en_US.UTF-8")
			dir := t.TempDir()
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
				}
				return strings.TrimSpace(string(out))
			}
			writeFiles := func(files map[string]string) {
				t.Helper()
				for path, content := range files {
					if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			commit := func(files map[string]string, subject string) string {
				t.Helper()
				git("rm", "-q", "-r", "-f", "--ignore-unmatch", "--", ".")
				writeFiles(files)
				git("add", "-A")
				git("commit", "-q", "--allow-empty", "-m", subject)
				return git("rev-parse", "HEAD")
			}

			git("init", "-q", "-b", "main")
			git("config", "user.name", "Test")
			git("config", "user.email", "test@example.com")
			commit(tc.base, "base")
			if tc.orphan {
				git("checkout", "-q", "--orphan", "mirror")
			} else {
				git("checkout", "-q", "-b", "mirror")
			}
			mirror := commit(tc.mirror, "mirror change")
			git("checkout", "-q", "main")
			git("checkout", "-q", "-b", "deployed")
			deployed := commit(tc.deployed, "deployed content")

			repo, err := gitx.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			present, err := ChangePresent(repo, deployed, mirror)
			if tc.wantPresent && (err != nil || !present) {
				t.Errorf("ChangePresent() = %v, %v; want present", present, err)
			}
			if !tc.wantPresent && (present || err != nil) {
				t.Errorf("ChangePresent() = %v, %v; want absent", present, err)
			}
		})
	}
}
