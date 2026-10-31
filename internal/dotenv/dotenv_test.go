package dotenv

import (
	"os"
	"path/filepath"
	"runtime"
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
HASH=abc # a comment
HASH_TAB=abc	# a comment
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
		"HASH":           "abc",
		"HASH_TAB":       "abc",
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
		"comment as value": "K=  # required\n",
		"bare hash value":  "K=#abc\n",
		"hash in a word":   "K=abc#def\n",
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

func TestSetRoundTrip(t *testing.T) {
	p := writeEnv(t, "# keep me\nA=1\nB=old\n\nB=dup\nC=3")
	values := []string{"new", "postgres://x:y@h:1/db?sslmode=disable", "has space", "it's", "multi\nline $x \"q\"", ""}
	for _, v := range values {
		if err := Set(p, "B", v); err != nil {
			t.Fatal(err)
		}
		f, err := Load(p)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := f.Get("B"); got != v {
			t.Errorf("round trip of %q gave %q", v, got)
		}
		if a, _ := f.Get("A"); a != "1" {
			t.Errorf("A changed: %q", a)
		}
	}
	if err := Set(p, "NEW", "v"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if want := "# keep me\nA=1\nB=\n\nC=3\nNEW=v\n"; string(data) != want {
		t.Errorf("file:\n%s", data)
	}
	if err := os.WriteFile(p, []byte("A=1\nB=  # set by graft\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Set(p, "B", "v"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(p); string(data) != "A=1\nB=v\n" {
		t.Errorf("a line of the key whose value Load refuses must be replaced:\n%s", data)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(p)
		if info.Mode().Perm() != 0o600 {
			t.Errorf("mode changed to %v", info.Mode())
		}
	}
	broken := writeEnv(t, "A=1\nJUSTTEXT\n")
	if err := Set(broken, "B", "v"); err == nil {
		t.Error("Set on a file Load refuses must fail")
	}
	if data, _ := os.ReadFile(broken); string(data) != "A=1\nJUSTTEXT\n" {
		t.Errorf("a refused file must stay as it was:\n%s", data)
	}
	if err := Set(filepath.Join(t.TempDir(), "missing"), "K", "v"); err == nil {
		t.Error("Set on a missing file must fail")
	}
}

// TestShellRefs: in a command's text a ${...} that is no key is the shell's
// own expansion, and the keys around it are still references.
func TestShellRefs(t *testing.T) {
	for s, want := range map[string][]string{
		`w="a|b"; echo ${w%|*}`:              nil,
		`echo ${item%%:*} ${API_KEY} ${open`: {"API_KEY"},
		`echo ${x:-${TOKEN}} $$HOME ${1}`:    {"TOKEN"},
		`echo ${A} ${B}`:                     {"A", "B"},
	} {
		if got := ShellRefs(s); !slices.Equal(got, want) {
			t.Errorf("ShellRefs(%q) = %q, want %q", s, got, want)
		}
	}
	if _, err := Refs(`echo ${w%|*}`); err == nil {
		t.Error("Refs of a text graft expands must still fail on a ${ that is no key")
	}
}
