package checks

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

func TestIgnoredSources(t *testing.T) {
	for _, tc := range []struct {
		name, ignore, file, extra, exceptions, want string
	}{
		{"directory mask", "jwt-*", "cmd/jwt-helper/main.go", "", "", "jwt-*"},
		{"root anchor", "/jwt-helper", "cmd/jwt-helper/main.go", "", "", ""},
		{"dependencies", "node_modules/", "node_modules/x/index.ts", "", "", ""},
		{"exception", "generated/", "generated/x/index.ts", "", "generated/**", ""},
		{"go build", "build", "internal/build/x.go", "", "", "build"},
		{"web build", "build", "web/build/app.js", "web/package.json", "", ""},
		{"report source", "e2e-results/", "e2e-results/capture.spec.ts", "", "", "e2e-results/"},
		{"next generated", "next-env.d.ts", "web/next-env.d.ts", "", "", ""},
		{"exact file", "web/types/generated.ts", "web/types/generated.ts", "", "", ""},
		{"exact basename elsewhere", "generated.ts", "web/types/generated.ts", "", "", "generated.ts"},
		{"escaped exact file", `web/a\*.ts`, "web/a*.ts", "", "", ""},
		{"glob over a starred name", "web/a*.ts", "web/a*.ts", "", "", "web/a*.ts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			initIgnoredRepo(t, root)
			writeIgnoredFile(t, root, ".gitignore", tc.ignore+"\n")
			writeIgnoredFile(t, root, tc.file, "source")
			if tc.extra != "" {
				writeIgnoredFile(t, root, tc.extra, "{}")
			}
			yaml := "schema: 1\nversion: {mode: none}\n"
			if tc.exceptions != "" {
				yaml += "checks:\n  ignored_sources:\n    exceptions:\n      - path: '" + tc.exceptions + "'\n        reason: generated\n"
			}
			cfg, err := config.Parse([]byte(yaml))
			if err != nil {
				t.Fatal(err)
			}
			got, err := IgnoredSources(&gitx.Repo{Root: root}, cfg.Checks.IgnoredSources)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" && len(got) != 0 {
				t.Fatalf("unexpected findings: %v", got)
			}
			if tc.want != "" && (len(got) != 1 || got[0].Path != tc.file || !strings.Contains(got[0].Reason, ".gitignore:1: "+tc.want)) {
				t.Fatalf("findings = %v", got)
			}
		})
	}
}

func TestIgnoredSourcesNestedRepository(t *testing.T) {
	root := t.TempDir()
	initIgnoredRepo(t, root)
	writeIgnoredFile(t, root, ".gitignore", "hidden/\n")
	writeIgnoredFile(t, root, "hidden/repo/main.go", "source")
	initIgnoredRepo(t, filepath.Join(root, "hidden/repo"))
	cfg, err := config.Parse([]byte("schema: 1\nversion: {mode: none}\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := IgnoredSources(&gitx.Repo{Root: root}, cfg.Checks.IgnoredSources)
	if err != nil || len(got) != 0 {
		t.Fatalf("findings = %v, error = %v", got, err)
	}
}

func TestNewIgnoredSourcesSkipsTreeWithoutIgnoreChanges(t *testing.T) {
	root := t.TempDir()
	initIgnoredRepo(t, root)
	writeIgnoredFile(t, root, "nested/.gitignore", "*.go\n")
	got, err := NewIgnoredSources(&gitx.Repo{Root: root}, config.IgnoredSources{}, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("findings = %v, error = %v", got, err)
	}
}

// A full path in info/exclude names the file from the top of the work tree,
// as a root .gitignore would: not a finding.
func TestIgnoredSourcesExactPathInInfoExclude(t *testing.T) {
	root := t.TempDir()
	initIgnoredRepo(t, root)
	writeIgnoredFile(t, root, "web/generated.ts", "source")
	writeIgnoredFile(t, root, "web/other.ts", "source")
	writeIgnoredFile(t, root, ".git/info/exclude", "/web/generated.ts\nother.ts\n")
	cfg, err := config.Parse([]byte("schema: 1\nversion: {mode: none}\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := IgnoredSources(&gitx.Repo{Root: root}, cfg.Checks.IgnoredSources)
	if err != nil || len(got) != 1 || got[0].Path != "web/other.ts" {
		t.Fatalf("findings = %v, error = %v; want only the mask's web/other.ts", got, err)
	}
}

// A git failure other than "no HEAD yet" is an error, not an empty set of
// HEAD's rules: the commit's rules would all look new, or none would.
func TestWriteHeadIgnoresGitError(t *testing.T) {
	root := t.TempDir()
	writeIgnoredFile(t, root, ".git", "gitdir: "+filepath.Join(root, "missing")+"\n")
	if err := writeHeadIgnores(&gitx.Repo{Root: root}, t.TempDir()); err == nil {
		t.Fatal("broken repository: no error")
	}
	unborn := t.TempDir()
	initIgnoredRepo(t, unborn)
	if err := writeHeadIgnores(&gitx.Repo{Root: unborn}, t.TempDir()); err != nil {
		t.Fatalf("unborn HEAD: %v", err)
	}
}

func initIgnoredRepo(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "init", "-q", dir)
	drop, err := gitx.LocalEnvVars()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = gitx.Without(os.Environ(), drop)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %s: %v", out, err)
	}
}

func writeIgnoredFile(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
