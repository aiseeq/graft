package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"make smoke", []string{"make", "smoke"}},
		{"  go   test  -short ./... ", []string{"go", "test", "-short", "./..."}},
		{`bash scripts/smoke.sh`, []string{"bash", "scripts/smoke.sh"}},
		{`go test -run 'Test[A-Z]*'`, []string{"go", "test", "-run", "Test[A-Z]*"}},
		{`echo "a b" c`, []string{"echo", "a b", "c"}},
		{`echo "say \"hi\" \$x"`, []string{"echo", `say "hi" $x`}},
		{`echo "back\slash"`, []string{"echo", `back\slash`}},
		{`echo a\ b`, []string{"echo", "a b"}},
		{`echo ''`, []string{"echo", ""}},
		{`echo x~y`, []string{"echo", "x~y"}},
	}
	for _, c := range cases {
		got, err := SplitCommand(c.in)
		if err != nil {
			t.Errorf("SplitCommand(%q): %v", c.in, err)
			continue
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("SplitCommand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSplitCommandRejectsShellSyntax(t *testing.T) {
	for _, in := range []string{
		"make smoke && make lint",
		"go test | tee log",
		"go test > log",
		"echo $HOME",
		`echo "$HOME"`,
		"echo `date`",
		"echo $(date)",
		"rm *.tmp",
		"ls ~/bin",
		"CGO_ENABLED=0 go build",
		"a; b",
		`echo "open`,
		`echo 'open`,
		"",
		"   ",
	} {
		if got, err := SplitCommand(in); err == nil {
			t.Errorf("SplitCommand(%q) = %q, want an error", in, got)
		}
	}
}

const minimal = "schema: 1\nversion:\n  mode: none\n"

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse([]byte("schema: 1\nversion:\n  mode: file\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version.File != "VERSION" || cfg.Version.TagPrefix != "v" {
		t.Errorf("version defaults: %+v", cfg.Version)
	}
	if cfg.Hooks.Dir != ".githooks" || cfg.Lock.Timeout != 15*time.Minute {
		t.Errorf("hooks/lock defaults: %+v %+v", cfg.Hooks, cfg.Lock)
	}
	if !cfg.Checks.Secrets.IsEnabled() || !cfg.Checks.LargeFiles.IsEnabled() {
		t.Error("checks must be on by default")
	}
	if cfg.Checks.LargeFiles.MaxBinaryBytes != 1<<20 || cfg.Checks.LargeFiles.MaxTextBytes != 5<<20 {
		t.Errorf("size defaults: %+v", cfg.Checks.LargeFiles)
	}
	if cfg.Ticket != nil {
		t.Error("ticket must stay unset without a ticket section")
	}
}

func TestParseFull(t *testing.T) {
	cfg, err := Parse([]byte(`
schema: 1
gate:
  - make smoke
  - [sh, -c, 'go vet ./... && go test ./...']
version:
  mode: file
  file: meta/VERSION
  tag_on_commit: true
  sync:
    - path: web/package.json
      format: json
      key: [version]
    - path: config.yaml
      format: regex
      pattern: 'version:\s*"([^"]*)"'
    - path: web/VERSION
      format: plain
ticket:
  pattern: '[A-Z][A-Z]+-[0-9]+'
push:
  remotes: [origin]
checks:
  secrets:
    enabled: false
    extra_patterns: ['token_[a-z]{10}']
    exceptions:
      - path: 'testdata/**'
        reason: synthetic keys
  large_files:
    max_binary_bytes: 10
    binary_extensions: [PNG]
hooks:
  dir: tools/hooks
lock:
  timeout: 90s
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Gate[1].Argv; !slices.Equal(got, []string{"sh", "-c", "go vet ./... && go test ./..."}) {
		t.Errorf("list command: %q", got)
	}
	if cfg.Version.Sync[1].Regexp == nil || cfg.Ticket.Regexp == nil {
		t.Error("patterns must be compiled")
	}
	if cfg.Checks.Secrets.IsEnabled() {
		t.Error("secrets: enabled false ignored")
	}
	if !slices.Equal(cfg.Checks.LargeFiles.BinaryExtensions, []string{"png"}) {
		t.Errorf("extensions: %q", cfg.Checks.LargeFiles.BinaryExtensions)
	}
	if cfg.Lock.Timeout != 90*time.Second {
		t.Errorf("timeout: %v", cfg.Lock.Timeout)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"missing schema":         "version:\n  mode: none\n",
		"unknown key":            minimal + "gates: []\n",
		"missing mode":           "schema: 1\nversion: {}\n",
		"bad mode":               "schema: 1\nversion:\n  mode: semver\n",
		"sync outside file mode": "schema: 1\nversion:\n  mode: git-tag\n  sync: [{path: a, format: plain}]\n",
		"regex without group":    "schema: 1\nversion:\n  mode: file\n  sync: [{path: a, format: regex, pattern: 'v.*'}]\n",
		"regex two groups":       "schema: 1\nversion:\n  mode: file\n  sync: [{path: a, format: regex, pattern: '(a)(b)'}]\n",
		"json without key":       "schema: 1\nversion:\n  mode: file\n  sync: [{path: a, format: json}]\n",
		"unknown format":         "schema: 1\nversion:\n  mode: file\n  sync: [{path: a, format: toml}]\n",
		"path escapes tree":      "schema: 1\nversion:\n  mode: file\n  file: ../VERSION\n",
		"absolute path":          "schema: 1\nversion:\n  mode: file\n  file: /etc/VERSION\n",
		"exception w/o reason":   minimal + "checks:\n  secrets:\n    exceptions: [{path: a}]\n",
		"shell in gate":          minimal + "gate: ['make a && make b']\n",
		"empty ticket":           minimal + "ticket: {}\n",
		"duplicate remote":       minimal + "push:\n  remotes: [a, a]\n",
		"dotted extension":       minimal + "checks:\n  large_files:\n    binary_extensions: [.png]\n",
		"negative timeout":       minimal + "lock:\n  timeout: -1s\n",
	}
	for name, data := range cases {
		if _, err := Parse([]byte(data)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestCompileGlob(t *testing.T) {
	cases := []struct {
		glob  string
		match []string
		miss  []string
	}{
		{"docs/big.js", []string{"docs/big.js"}, []string{"docs/big.jsx", "x/docs/big.js"}},
		{"testdata/**", []string{"testdata/a", "testdata/a/b.go"}, []string{"testdata", "x/testdata/a"}},
		{"**/fixtures/*.pem", []string{"fixtures/a.pem", "a/b/fixtures/c.pem"}, []string{"fixtures/a/b.pem"}},
		{"*.min.js", []string{"app.min.js"}, []string{"web/app.min.js"}},
		{"file?.txt", []string{"file1.txt"}, []string{"file10.txt"}},
	}
	for _, c := range cases {
		re, err := CompileGlob(c.glob)
		if err != nil {
			t.Fatalf("%s: %v", c.glob, err)
		}
		for _, p := range c.match {
			if !re.MatchString(p) {
				t.Errorf("%s should match %s", c.glob, p)
			}
		}
		for _, p := range c.miss {
			if re.MatchString(p) {
				t.Errorf("%s should not match %s", c.glob, p)
			}
		}
	}
	if _, err := CompileGlob("../x"); err == nil || !strings.Contains(err.Error(), "relative") {
		t.Errorf("escaping glob: %v", err)
	}
}
