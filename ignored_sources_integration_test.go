package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIgnoredSourcesAlternateIndex(t *testing.T) {
	f := newRepo(t, map[string]string{".graft.yaml": ignoredConfig})
	f.git("add", ".graft.yaml")
	f.git("commit", "-m", "initial")
	f.write("cmd/jwt-helper/main.go", "package main\n")
	f.write(".gitignore", "jwt-*\n")
	index, err := os.ReadFile(filepath.Join(f.work, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	alt := filepath.Join(f.base, "alternate-index")
	if err := os.WriteFile(alt, index, 0o600); err != nil {
		t.Fatal(err)
	}
	f.env = append(f.env, "GIT_INDEX_FILE="+alt)
	if out, code := f.tryGit("add", ".gitignore"); code != 0 {
		t.Fatalf("add to alternate index: %d: %s", code, out)
	}
	f.git("add", "-f", "cmd/jwt-helper/main.go")
	out, code := f.graft("", "check")
	if code == 0 || !strings.Contains(out, "cmd/jwt-helper/main.go") || !strings.Contains(out, ".gitignore:1: jwt-*") {
		t.Fatalf("alternate index: exit %d: %s", code, out)
	}
}

func TestIgnoredSourcesUntrackedIgnoreCancelsRule(t *testing.T) {
	f := newRepo(t, map[string]string{".graft.yaml": ignoredConfig})
	f.git("add", ".graft.yaml")
	f.git("commit", "-m", "initial")
	f.write("cmd/jwt-helper/main.go", "package main\n")
	f.write(".gitignore", "jwt-*\n")
	f.write("cmd/.gitignore", "!jwt-*\n")
	f.git("add", ".gitignore")
	out, code := f.graft("", "check")
	if code == 0 || !strings.Contains(out, "cmd/.gitignore") || !strings.Contains(out, ".gitignore staged differs from the work tree") {
		t.Fatalf("untracked ignore: exit %d: %s", code, out)
	}
}

func TestIgnoredSourcesOtherWorktreeIgnores(t *testing.T) {
	for _, tc := range []struct {
		name, initial, rootRule, nested string
		stageNested                     bool
	}{
		{"ignored file", "", "cmd/.gitignore\njwt-*\n", "!jwt-*\n", false},
		{"tracked file modified", "original\n", "jwt-*\n", "!jwt-*\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepo(t, map[string]string{".graft.yaml": ignoredConfig})
			if tc.stageNested {
				f.write("cmd/.gitignore", tc.initial)
			}
			f.git("add", ".")
			f.git("commit", "-m", "initial")
			f.write("cmd/.gitignore", tc.nested)
			f.write("cmd/jwt-helper/main.go", "source\n")
			f.write(".gitignore", tc.rootRule)
			f.git("add", ".gitignore")
			if out, code := f.graft("", "check"); code == 0 || !strings.Contains(out, "cmd/.gitignore: .gitignore staged differs from the work tree") {
				t.Fatalf("exit %d: %s", code, out)
			}
		})
	}
}

func TestIgnoredSourcesSkipsExcludedIgnoreFiles(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": ignoredConfig + "checks: {ignored_sources: {exceptions: [{path: 'generated/**', reason: generated}]}}\n",
	})
	f.git("add", ".graft.yaml")
	f.git("commit", "-m", "initial")
	f.write("node_modules/pkg/.gitignore", "*.go\n")
	f.write("generated/.gitignore", "*.go\n")
	f.write("nested/.gitignore", "*.go\n")
	f.run(filepath.Join(f.work, "nested"), "git", "init", "-q")
	f.write(".gitignore", "*.go\n")
	f.git("add", ".gitignore")
	if out, code := f.graft("", "check"); code != 0 {
		t.Fatalf("excluded directories: exit %d: %s", code, out)
	}
}

func TestIgnoredSourcesDeletedIgnoreTriggersComparison(t *testing.T) {
	f := newRepo(t, map[string]string{".graft.yaml": ignoredConfig, ".gitignore": "*.log\n"})
	f.git("add", ".")
	f.git("commit", "-m", "initial")
	f.git("rm", ".gitignore")
	f.write("nested/.gitignore", "*.go\n")
	if out, code := f.graft("", "check"); code == 0 || !strings.Contains(out, "nested/.gitignore: .gitignore staged differs from the work tree") {
		t.Fatalf("deleted ignore: exit %d: %s", code, out)
	}
}

func TestIgnoredSourcesMissingTrackedIgnore(t *testing.T) {
	f := newRepo(t, map[string]string{".graft.yaml": ignoredConfig, "cmd/.gitignore": "*.go\n"})
	f.git("add", ".")
	f.git("commit", "-m", "initial")
	if err := os.Remove(filepath.Join(f.work, "cmd", ".gitignore")); err != nil {
		t.Fatal(err)
	}
	f.write(".gitignore", "jwt-*\n")
	f.git("add", ".gitignore")
	if out, code := f.graft("", "check"); code == 0 || !strings.Contains(out, "cmd/.gitignore: .gitignore staged differs from the work tree") {
		t.Fatalf("missing tracked ignore: exit %d: %s", code, out)
	}
}

func TestIgnoredSourcesStagedIgnoreInExcludedDirectory(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml":          ignoredConfig + "checks: {ignored_sources: {exceptions: [{path: 'generated/**', reason: generated}]}}\n",
		"generated/.gitignore": "old\n",
	})
	f.git("add", ".")
	f.git("commit", "-m", "initial")
	f.write("generated/.gitignore", "new\n")
	f.git("add", "generated/.gitignore")
	f.write("generated/.gitignore", "local\n")
	if out, code := f.graft("", "check"); code != 0 {
		t.Fatalf("excluded staged ignore: exit %d: %s", code, out)
	}
}

const ignoredConfig = "schema: 1\nversion: {mode: none, reason: 'no version'}\ngate: []\n"

func TestIgnoredSourcesNewRules(t *testing.T) {
	for _, tc := range []struct {
		name, rule, file string
		blocked          bool
	}{
		{"glob directory", "jwt-*", "cmd/jwt-helper/main.go", true},
		{"anchored directory", "/jwt-helper", "cmd/jwt-helper/main.go", false},
		{"exact file", "web/types/generated.ts", "web/types/generated.ts", false},
		{"anchored exact file", "/web/types/generated.ts", "web/types/generated.ts", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepo(t, map[string]string{".graft.yaml": ignoredConfig, "README.md": "initial\n"})
			f.git("add", ".")
			f.git("commit", "-m", "initial")
			f.write(".gitignore", tc.rule+"\n")
			f.write(tc.file, "source\n")
			f.git("add", ".gitignore")
			out, code := f.graft("", "commit", "-m", "feat: ignore")
			if tc.blocked {
				if code == 0 || !strings.Contains(out, tc.file) || !strings.Contains(out, ".gitignore:1: "+tc.rule) {
					t.Fatalf("exit %d: %s", code, out)
				}
			} else if code != 0 {
				t.Fatalf("exit %d: %s", code, out)
			}
			if out, code := f.graft("", "check", "--ignored-sources"); (code != 0) != tc.blocked {
				t.Fatalf("audit exit %d: %s", code, out)
			}
		})
	}
}

func TestIgnoredSourcesOldRuleAndAudit(t *testing.T) {
	f := newRepo(t, map[string]string{".graft.yaml": ignoredConfig, ".gitignore": "jwt-*\n", "README.md": "initial\n"})
	f.git("add", ".")
	f.git("commit", "-m", "initial")
	f.write("cmd/jwt-helper/main.go", "package main\n")
	f.write("README.md", "changed\n")
	f.git("add", "README.md")
	if out, code := f.graft("", "commit", "-m", "docs: change"); code != 0 {
		t.Fatalf("old rule blocked commit: %d: %s", code, out)
	}
	if out, code := f.graft("", "check", "--ignored-sources"); code == 0 || !strings.Contains(out, "cmd/jwt-helper/main.go") || !strings.Contains(out, ".gitignore:1: jwt-*") {
		t.Fatalf("audit exit %d: %s", code, out)
	}
}

func TestIgnoredSourcesStagedDiffers(t *testing.T) {
	f := newRepo(t, map[string]string{".graft.yaml": ignoredConfig, ".gitignore": "jwt-*\n"})
	f.write("cmd/jwt-helper/main.go", "package main\n")
	f.git("add", ".graft.yaml", ".gitignore")
	f.write(".gitignore", "/jwt-helper\n")
	if out, code := f.graft("", "check"); code == 0 || !strings.Contains(out, ".gitignore staged differs from the work tree") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestIgnoredSourcesChangedNestedRule(t *testing.T) {
	f := newRepo(t, map[string]string{".graft.yaml": ignoredConfig, "web/.gitignore": "old-*\n"})
	f.git("add", ".")
	f.git("commit", "-m", "initial")
	f.write("web/cmd/old-helper/main.go", "source\n")
	f.write("web/cmd/jwt-helper/main.go", "source\n")
	f.write("web/.gitignore", "old-*\njwt-*\n")
	f.git("add", "web/.gitignore")
	out, code := f.graft("", "check")
	if code == 0 || !strings.Contains(out, "web/cmd/jwt-helper/main.go") || !strings.Contains(out, "web/.gitignore:2: jwt-*") || strings.Contains(out, "web/cmd/old-helper/main.go") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestIgnoredSourcesAuditWithoutConfig(t *testing.T) {
	f := newRepo(t, map[string]string{".gitignore": "jwt-*\n"})
	f.write("cmd/jwt-helper/main.go", "package main\n")
	if out, code := f.graft("", "check", "--ignored-sources"); code == 0 || !strings.Contains(out, "cmd/jwt-helper/main.go") || strings.Contains(out, "add an exception") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestIgnoredSourcesAuditWhenDisabled(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": ignoredConfig + "checks: {ignored_sources: {enabled: false}}\n",
		".gitignore":  "jwt-*\n",
	})
	f.write("cmd/jwt-helper/main.go", "source\n")
	f.git("add", ".graft.yaml", ".gitignore")
	if out, code := f.graft("", "check"); code != 0 {
		t.Fatalf("disabled check: %d: %s", code, out)
	}
	if out, code := f.graft("", "check", "--ignored-sources"); code == 0 || !strings.Contains(out, "cmd/jwt-helper/main.go") {
		t.Fatalf("audit: %d: %s", code, out)
	}
}

// diff.interHunkContext joins near hunks with context lines even under
// --unified=0: the new rule's line number counts them, or the finding is
// dropped as an old rule's.
func TestIgnoredSourcesInterHunkContext(t *testing.T) {
	f := newRepo(t, map[string]string{".graft.yaml": ignoredConfig, ".gitignore": "old-a\nold-b\nold-c\n"})
	f.git("config", "diff.interHunkContext", "10")
	f.git("add", ".")
	f.git("commit", "-m", "initial")
	f.write(".gitignore", "added-a\nold-a\nold-b\nold-c\njwt-*\n")
	f.write("cmd/jwt-helper/main.go", "package main\n")
	f.git("add", ".gitignore")
	out, code := f.graft("", "check")
	if code == 0 || !strings.Contains(out, "cmd/jwt-helper/main.go") || !strings.Contains(out, ".gitignore:5: jwt-*") {
		t.Fatalf("joined hunks: exit %d: %s", code, out)
	}
}
