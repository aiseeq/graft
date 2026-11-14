package main

import (
	"strings"
	"testing"
)

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
