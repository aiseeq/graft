package message

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var keyRe = regexp.MustCompile(`[A-Z][A-Z]+-[0-9]+`)

func TestBranchKey(t *testing.T) {
	cases := map[string]string{
		"feature/PROJ-123-add-thing": "PROJ-123",
		"PROJ-7":                     "PROJ-7",
		"fix/ab-12":                  "",
		"main":                       "",
		"x/PROJ-1-and-PROJ-2":        "PROJ-1",
	}
	for branch, want := range cases {
		if got := BranchKey(branch, keyRe); got != want {
			t.Errorf("BranchKey(%q) = %q, want %q", branch, got, want)
		}
	}
	if got := BranchKey("feature/PROJ-1", nil); got != "" {
		t.Errorf("no pattern must mean no key, got %q", got)
	}
}

func TestWithKey(t *testing.T) {
	cases := []struct {
		name, msg, key, want string
	}{
		{"no key", "fix: x", "", "fix: x\n"},
		{"appended", "fix: x", "PROJ-1", "fix: x\n\nPROJ-1\n"},
		{"after body", "fix: x\n\nbody", "PROJ-1", "fix: x\n\nbody\n\nPROJ-1\n"},
		{"already in subject", "fix(PROJ-1): x", "PROJ-1", "fix(PROJ-1): x\n"},
		{"already in body", "fix: x\n\nsee PROJ-1", "PROJ-1", "fix: x\n\nsee PROJ-1\n"},
		{"longer key is not a mention", "fix: PROJ-12", "PROJ-1", "fix: PROJ-12\n\nPROJ-1\n"},
		{"prefixed key is not a mention", "fix: XPROJ-1", "PROJ-1", "fix: XPROJ-1\n\nPROJ-1\n"},
		{
			"before trailers", "feat: y\n\nbody\n\nCo-Authored-By: A <a@b>\nSigned-off-by: B <b@c>", "PROJ-9",
			"feat: y\n\nbody\n\nPROJ-9\n\nCo-Authored-By: A <a@b>\nSigned-off-by: B <b@c>\n",
		},
		{"subject is never a trailer block", "fix: x", "PROJ-2", "fix: x\n\nPROJ-2\n"},
		{
			"prose paragraph is not a trailer block", "fix: x\n\nNote: this\nand more", "PROJ-3",
			"fix: x\n\nNote: this\nand more\n\nPROJ-3\n",
		},
	}
	for _, c := range cases {
		if got := WithKey(c.msg, c.key); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRead(t *testing.T) {
	tricky := "feat: $(x) $$HOME \"q\" 'a' `b` \\n\n\n# kept\n\n\nbody\r\n"
	file := filepath.Join(t.TempDir(), "msg")
	if err := os.WriteFile(file, []byte(tricky), 0o644); err != nil {
		t.Fatal(err)
	}
	want := "feat: $(x) $$HOME \"q\" 'a' `b` \\n\n\n# kept\n\n\nbody"

	got, err := Read(Source{File: file})
	if err != nil || got != want {
		t.Errorf("file: %q, %v", got, err)
	}
	got, err = Read(Source{File: "-", Stdin: strings.NewReader(tricky)})
	if err != nil || got != want {
		t.Errorf("stdin: %q, %v", got, err)
	}
	got, err = Read(Source{Parts: []string{"fix: a", "body $x"}})
	if err != nil || got != "fix: a\n\nbody $x" {
		t.Errorf("parts: %q, %v", got, err)
	}

	for name, src := range map[string]Source{
		"nothing":     {Stdin: strings.NewReader("fix: ignored")},
		"both":        {Parts: []string{"a"}, File: file},
		"empty":       {Parts: []string{"  \n"}},
		"empty stdin": {File: "-", Stdin: strings.NewReader("\n\n")},
		"no file":     {File: filepath.Join(t.TempDir(), "missing")},
	} {
		if _, err := Read(src); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
