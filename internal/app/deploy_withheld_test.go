package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

type withheldRepo struct {
	t    *testing.T
	dir  string
	repo *gitx.Repo
}

func newWithheldRepo(t *testing.T, initial string) *withheldRepo {
	t.Helper()
	r := newEmptyWithheldRepo(t)
	r.commitText("work.txt", initial, "base")
	return r
}

func newEmptyWithheldRepo(t *testing.T) *withheldRepo {
	t.Helper()
	dir := t.TempDir()
	r := &withheldRepo{t: t, dir: dir}
	r.git("init", "-q")
	r.git("config", "user.name", "Test")
	r.git("config", "user.email", "test@example.invalid")
	r.git("config", "commit.gpgsign", "false")
	var err error
	r.repo, err = gitx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *withheldRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *withheldRepo) writePath(name string, content *string) {
	r.t.Helper()
	file := filepath.Join(r.dir, name)
	if content == nil {
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			r.t.Fatal(err)
		}
		return
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(*content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *withheldRepo) commitPath(name string, content *string, subject string) string {
	r.t.Helper()
	r.writePath(name, content)
	r.git("add", "-A")
	r.git("commit", "-qm", subject)
	return r.head()
}

func (r *withheldRepo) commitText(name, content, subject string) string {
	r.t.Helper()
	return r.commitPath(name, &content, subject)
}

func (r *withheldRepo) commitAll(subject string) string {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-qm", subject)
	return r.head()
}

func (r *withheldRepo) revert(sha string) {
	r.t.Helper()
	cmd := exec.Command("git", "-c", "core.hooksPath=/dev/null", "revert", "--no-edit", sha)
	cmd.Dir = r.dir
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git revert %s: %v\n%s", sha, err, out)
	}
}

func (r *withheldRepo) head() string {
	r.t.Helper()
	return r.git("rev-parse", "HEAD")
}

func partitionForTest(t *testing.T, r *withheldRepo, prev, head, w string, versionFiles ...string) ([]string, []string) {
	t.Helper()
	paths, err := repoPaths(r.repo, prev, head)
	if err != nil {
		t.Fatal(err)
	}
	active, held, err := partitionWithheld(r.repo, prev, head, []string{w}, versionFiles, paths)
	if err != nil {
		t.Fatal(err)
	}
	return active, held
}

func TestPartitionWithheldUsesChangedFiles(t *testing.T) {
	t.Run("hotfix does not touch overlaid withheld work", func(t *testing.T) {
		r := newWithheldRepo(t, "base\n")
		w := r.commitText("work.txt", "original\n", "feat: PROJ-123 work")
		r.commitText("work.txt", "base\n", "fix: PROJ-123 cancel work")
		r.commitText("work.txt", "reworked\n", "feat: PROJ-123 followup")
		prev := r.head()
		head := r.commitText("hotfix.txt", "hotfix\n", "fix: PROJ-123 hotfix")
		active, held := partitionForTest(t, r, prev, head, w)
		if len(active) != 0 || len(held) != 1 || held[0] != w {
			t.Fatalf("active %v, held %v; want W retained and unchecked", active, held)
		}
	})

	t.Run("hotfix does not recheck work already shipped unapproved", func(t *testing.T) {
		r := newWithheldRepo(t, "base\n")
		w := r.commitText("work.txt", "original\n", "feat: PROJ-123 work")
		prev := w
		head := r.commitText("hotfix.txt", "hotfix\n", "fix: PROJ-123 hotfix")
		active, held := partitionForTest(t, r, prev, head, w)
		if len(active) != 0 || len(held) != 1 || held[0] != w {
			t.Fatalf("active %v, held %v; want W retained and unchecked", active, held)
		}
	})

	for _, tc := range []struct {
		name    string
		prepare func(*withheldRepo) (w, prev, head string)
	}{
		{
			name: "partial restoration",
			prepare: func(r *withheldRepo) (w, prev, head string) {
				w = r.commitText("work.txt", "new1\nmiddle\nnew2\n", "feat: PROJ-123 work")
				r.revert(w)
				prev = r.head()
				head = r.commitText("work.txt", "new1\nmiddle\nold2\n", "fix: PROJ-123 partial restore")
				return
			},
		},
		{
			name: "independent replacement",
			prepare: func(r *withheldRepo) (w, prev, head string) {
				w = r.commitText("work.txt", "secret-feature\n", "feat: PROJ-123 work")
				r.revert(w)
				prev = r.commitText("work.txt", "unrelated\n", "fix: independent replacement")
				head = r.commitText("work.txt", "secret-feature\n", "fix: replace again")
				return
			},
		},
		{
			name: "file deletion",
			prepare: func(r *withheldRepo) (w, prev, head string) {
				w = r.commitPath("work.txt", nil, "feat: PROJ-123 delete work file")
				r.revert(w)
				prev = r.commitText("work.txt", "other\n", "fix: restore independently")
				head = r.commitPath("work.txt", nil, "fix: delete file")
				return
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newWithheldRepo(t, "old1\nmiddle\nold2\n")
			w, prev, head := tc.prepare(r)
			active, held := partitionForTest(t, r, prev, head, w)
			if len(active) != 1 || active[0] != w || len(held) != 0 {
				t.Fatalf("active %v, held %v; want W checked", active, held)
			}
		})
	}

	t.Run("version-only commit uses its real change", func(t *testing.T) {
		r := newWithheldRepo(t, "base\n")
		w := r.commitText("VERSION", "1\n", "feat: PROJ-123 version change")
		prev := r.head()
		r.revert(w)
		head := r.head()
		active, held := partitionForTest(t, r, prev, head, w, "./VERSION")
		if len(active) != 0 || len(held) != 1 || held[0] != w {
			t.Fatalf("active %v, held %v; want reverted version change retained", active, held)
		}
	})

	t.Run("root withheld commit paths", func(t *testing.T) {
		r := newEmptyWithheldRepo(t)
		w := r.commitText("work.txt", "root change\n", "feat: PROJ-123 root work")
		paths, err := withheldPaths(r.repo, w)
		if err != nil {
			t.Fatal(err)
		}
		if len(paths) != 1 || paths[0] != "work.txt" {
			t.Fatalf("withheld paths %v; want [work.txt]", paths)
		}
		active, held, err := partitionWithheld(r.repo, w, w, []string{w}, nil, map[string]bool{})
		if err != nil {
			t.Fatal(err)
		}
		if len(active) != 0 || len(held) != 1 || held[0] != w {
			t.Fatalf("active %v, held %v; want root W retained without error", active, held)
		}
	})

	t.Run("merge withheld commit paths use first parent", func(t *testing.T) {
		r := newWithheldRepo(t, "base\n")
		mainBranch := r.git("branch", "--show-current")
		r.git("checkout", "-qb", "feature")
		r.commitText("work.txt", "merged work\n", "feat: PROJ-123 merge side")
		r.git("checkout", "-q", mainBranch)
		r.commitText("hotfix.txt", "hotfix\n", "fix: PROJ-123 mainline")
		r.git("merge", "--no-ff", "-qm", "merge feature", "feature")
		w := r.head()
		paths, err := withheldPaths(r.repo, w)
		if err != nil {
			t.Fatal(err)
		}
		if len(paths) != 1 || paths[0] != "work.txt" {
			t.Fatalf("withheld paths %v; want [work.txt]", paths)
		}
	})

}

func TestReview3OtherBranchRenamedRestore(t *testing.T) {
	r := newWithheldRepo(t, "base\n")
	mainBranch := r.git("branch", "--show-current")
	r.git("checkout", "-qb", "withheld")
	w := r.commitText("work.txt", "secret\n", "feat: PROJ-123 withheld work")
	r.git("checkout", "-q", mainBranch)
	r.git("mv", "work.txt", "renamed.txt")
	prev := r.commitAll("refactor: rename work file")
	head := r.commitText("renamed.txt", "secret\n", "feat: PROJ-456 restore work")
	active, held := partitionForTest(t, r, prev, head, w)
	if len(active) != 1 || active[0] != w || len(held) != 0 {
		t.Fatalf("active %v, held %v; want non-ancestor W checked", active, held)
	}
}

func TestReview3OtherBranchRenamedRestoreMixedVersionFileCode(t *testing.T) {
	r := newWithheldRepo(t, "base\n")
	r.writePath("VERSION", new("1.0.0\n"))
	r.writePath("version.go", new("// version: 1.0.0\nconst policy = \"old\"\n"))
	r.commitAll("chore: add version files")
	r.writePath("work.txt", new("secret\n"))
	r.writePath("version.go", new("// version: 1.0.1\nconst policy = \"secret\"\n"))
	w := r.commitAll("feat: PROJ-123 mixed work")
	prev := w
	head := r.commitText("version.go", "// version: 1.0.1\nconst policy = \"restored\"\n", "feat: PROJ-456 restore policy")
	versionCfg := config.Version{
		Mode: config.ModeFile,
		File: "VERSION",
		Sync: []config.SyncFile{{Path: "version.go", Format: config.FormatRegex, Pattern: `version: ([0-9]+\.[0-9]+\.[0-9]+)`, Regexp: regexp.MustCompile(`version: ([0-9]+\.[0-9]+\.[0-9]+)`)}},
	}
	paths := pathsForVersionDeploy(t, r, prev, head, versionCfg)
	active, held, err := partitionWithheld(r.repo, prev, head, []string{w}, []string{"VERSION", "version.go"}, paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0] != w || len(held) != 0 {
		t.Fatalf("active %v, held %v; want W checked for code changed in version.go", active, held)
	}
}

func TestReview3VersionPaths(t *testing.T) {
	r := newWithheldRepo(t, "base\n")
	w := r.commitText("VERSION", "1.0.0\n", "feat: PROJ-123 version work")
	prev := w
	r.revert(w)
	head := r.head()
	active, held := partitionForTest(t, r, prev, head, w, "./VERSION")
	if len(active) != 0 || len(held) != 1 || held[0] != w {
		t.Fatalf("active %v, held %v; want normalized version-only path retained", active, held)
	}
}

func TestReview3AncestorWithVersionBumpOnlyStaysHeld(t *testing.T) {
	r := newWithheldRepo(t, "base\n")
	r.commitText("VERSION", "1.0.0\n", "chore: initial version")
	w := r.commitText("work.txt", "secret\n", "feat: PROJ-123 withheld work")
	r.revert(w)
	prev := r.head()
	head := r.commitText("VERSION", "1.0.1\n", "chore: version bump")
	paths := pathsForVersionDeploy(t, r, prev, head, config.Version{Mode: config.ModeFile, File: "VERSION"})
	active, held, err := partitionWithheld(r.repo, prev, head, []string{w}, []string{"VERSION"}, paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 || len(held) != 1 || held[0] != w {
		t.Fatalf("active %v, held %v; want ancestor W retained and unchecked", active, held)
	}
}

func pathsForVersionDeploy(t *testing.T, r *withheldRepo, prev, head string, v config.Version) map[string]bool {
	t.Helper()
	paths, err := repoPaths(r.repo, prev, head)
	if err != nil {
		t.Fatal(err)
	}
	paths, err = excludeVersionOnlyBumps(r.repo, prev, head, paths, v)
	if err != nil {
		t.Fatal(err)
	}
	return paths
}
