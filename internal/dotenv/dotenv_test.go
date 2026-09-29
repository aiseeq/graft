package dotenv

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeEnv(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	p := writeEnv(t, `# comment
PLAIN=postgres://u:p@127.0.0.1:5433/app_test?sslmode=disable
export EXPORTED=yes
  SPACED = value with spaces  
SINGLE='a "b" $c \n'
DOUBLE="say \"hi\" \$HOME \\ line\nnext"
HASH=abc # not a comment
EMPTY=
QUOTED_COMMENT='x' # trailing comment
DUP=first
DUP=second
`)
	f, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"PLAIN":          "postgres://u:p@127.0.0.1:5433/app_test?sslmode=disable",
		"EXPORTED":       "yes",
		"SPACED":         "value with spaces",
		"SINGLE":         `a "b" $c \n`,
		"DOUBLE":         "say \"hi\" $HOME \\ line\nnext",
		"HASH":           "abc # not a comment",
		"EMPTY":          "",
		"QUOTED_COMMENT": "x",
		"DUP":            "second",
	}
	for k, v := range want {
		if got, ok := f.Get(k); !ok || got != v {
			t.Errorf("%s = %q (%v), want %q", k, got, ok, v)
		}
	}
	if _, ok := f.Get("MISSING"); ok {
		t.Error("MISSING found")
	}
}

func TestLoadErrors(t *testing.T) {
	for name, content := range map[string]string{
		"no equals":        "JUSTTEXT\n",
		"bad key":          "1KEY=x\n",
		"open single":      "K='abc\n",
		"open double":      "K=\"abc\n",
		"text after quote": "K='a' b\n",
	} {
		if _, err := Load(writeEnv(t, content)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestLookupAndExpand(t *testing.T) {
	p := writeEnv(t, "FROM_FILE=file\nBOTH=file\n")
	t.Setenv("BOTH", "env")
	l := NewLookup(p)
	got, err := ExpandAll([]string{"${FROM_FILE}", "x=${BOTH}", "$$HOME", "$PLAIN", "cost $5"}, l)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"file", "x=env", "$HOME", "$PLAIN", "cost $5"}; !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	for _, bad := range []string{"${MISSING}", "${open", "${1X}"} {
		if _, err := Expand(bad, l); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
	if _, err := NewLookup(filepath.Join(t.TempDir(), "none")).Value("NOPE_NOT_SET"); err == nil {
		t.Error("missing file and key must fail")
	}
}
