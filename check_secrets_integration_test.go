package main

import (
	"strings"
	"testing"
)

// graft check --secrets scans only the staged text for secrets and works in a
// repository without .graft.yaml, as a pre-commit hook of any repository.
func TestCheckSecretsWithoutConfig(t *testing.T) {
	token := "gh" + "p_" + strings.Repeat("aB3", 12)
	f := newRepo(t, map[string]string{"notes.md": "one\n"})
	f.git("add", "notes.md")
	if out, code := f.graft("", "check", "--secrets"); code != 0 || !strings.Contains(out, "no secrets") {
		t.Fatalf("clean staged files: exit %d\n%s", code, out)
	}
	f.write("notes.md", "one\ntoken: "+token+"\n")
	f.write("big.bin", "\x00"+strings.Repeat("x", 2<<20))
	f.git("add", "notes.md", "big.bin")
	out, code := f.graft("", "check", "--secrets")
	if code == 0 || !strings.Contains(out, "notes.md:2") || !strings.Contains(out, "GitHub token") {
		t.Fatalf("staged token: exit %d\n%s", code, out)
	}
	if strings.Contains(out, token) || strings.Contains(out, "big.bin") {
		t.Errorf("the output shows the value or checks more than secrets:\n%s", out)
	}
	// The secret is looked for in the staged text, not in the file on disk.
	f.write("notes.md", "one\n")
	if _, code := f.graft("", "check", "--secrets"); code == 0 {
		t.Error("a token staged but gone from disk passed")
	}
}

// With .graft.yaml, --secrets takes its exceptions and extra patterns.
func TestCheckSecretsTakesConfig(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml":      "schema: 1\nversion: {mode: none, reason: 'no version'}\ngate: []\nchecks:\n  secrets:\n    extra_patterns: ['projectA_[0-9]{6}']\n    exceptions: [{path: 'testdata/**', reason: fixtures}]\n",
		"a.txt":            "id projectA_123456\n",
		"testdata/key.txt": "gh" + "p_" + strings.Repeat("aB3", 12) + "\n",
	})
	f.git("add", ".")
	out, code := f.graft("", "check", "--secrets")
	if code == 0 || !strings.Contains(out, "a.txt:1") || strings.Contains(out, "testdata/key.txt") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}
