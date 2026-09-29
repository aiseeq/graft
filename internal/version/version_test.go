package version

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aiseeq/graft/internal/config"
)

func TestParse(t *testing.T) {
	for _, ok := range []string{"0.0.0", "1.2.3", "10.20.30"} {
		if v, err := Parse(ok); err != nil || v.String() != ok {
			t.Errorf("Parse(%q) = %v, %v", ok, v, err)
		}
	}
	for _, bad := range []string{"", "1.2", "v1.2.3", "1.2.3-rc1", "01.2.3", "1.2.3\n", "1.2.x", "99999999999999999999.0.0"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}

func TestBump(t *testing.T) {
	v := Semver{1, 2, 3}
	for level, want := range map[Level]string{Patch: "1.2.4", Minor: "1.3.0", Major: "2.0.0"} {
		if got := v.Bump(level).String(); got != want {
			t.Errorf("%s bump = %s, want %s", level, got, want)
		}
	}
	if !(Semver{1, 9, 9}).Less(Semver{2, 0, 0}) || (Semver{1, 2, 3}).Less(Semver{1, 2, 3}) {
		t.Error("Less ordering")
	}
}

// syncTarget builds the target for one version.sync entry written as YAML.
func syncTarget(t *testing.T, entry string) Target {
	t.Helper()
	cfg, err := config.Parse([]byte("schema: 1\nversion:\n  mode: file\n  sync: [" + entry + "]\n"))
	if err != nil {
		t.Fatal(err)
	}
	return Targets(cfg.Version)[1]
}

func TestReplacePlain(t *testing.T) {
	tg := Targets(config.Version{File: "VERSION"})[0]
	got, err := tg.Replace([]byte("1.2.3\n"), "1.2.4")
	if err != nil || string(got) != "1.2.4\n" {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := tg.Extract([]byte("  \n")); err == nil {
		t.Error("empty file must fail")
	}
}

func TestReplaceJSONKeepsFormatting(t *testing.T) {
	in := `{
  "name": "app",
  "version": "1.2.3",
  "nested": {"version": "7.7.7"},
  "list": [{"version": "8.8.8"}],
  "packages": {
    "": {
      "name": "app",
      "version": "1.2.3"
    },
    "node_modules/x": {"version": "9.9.9"}
  }
}
`
	top := syncTarget(t, `{path: package.json, format: json, key: [version]}`)
	got, err := top.Replace([]byte(in), "2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "name": "app",
  "version": "2.0.0",
  "nested": {"version": "7.7.7"},
  "list": [{"version": "8.8.8"}],
  "packages": {
    "": {
      "name": "app",
      "version": "1.2.3"
    },
    "node_modules/x": {"version": "9.9.9"}
  }
}
`
	if string(got) != want {
		t.Errorf("top-level replace:\n%s", got)
	}

	lock := syncTarget(t, `{path: package-lock.json, format: json, key: [packages, "", version]}`)
	v, err := lock.Extract([]byte(in))
	if err != nil || v != "1.2.3" {
		t.Errorf("nested extract = %q, %v", v, err)
	}
}

// Escaped strings before the target must not shift the replacement: the
// standard decoder's InputOffset drifts after them.
func TestReplaceJSONAfterEscapedStrings(t *testing.T) {
	tg := syncTarget(t, `{path: package.json, format: json, key: [version]}`)
	in := `{"description": "say \"hi\" \u00e9té", "version": "1.2.3", "tail": "x"}`
	got, err := tg.Replace([]byte(in), "1.2.4")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"description": "say \"hi\" \u00e9té", "version": "1.2.4", "tail": "x"}`
	if string(got) != want {
		t.Errorf("got %s", got)
	}
}

func TestJSONErrors(t *testing.T) {
	tg := syncTarget(t, `{path: p.json, format: json, key: [version]}`)
	for name, in := range map[string]string{
		"missing":   `{"name": "x"}`,
		"duplicate": `{"version": "1.0.0", "version": "1.0.1"}`,
		"number":    `{"version": 1}`,
		"escaped":   `{"version": "1.0\u002e0"}`,
		"invalid":   `{"version": "1.0.0"`,
		"trailing":  `{"version": "1.0.0"} {}`,
	} {
		if _, err := tg.Extract([]byte(in)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestReplaceRegex(t *testing.T) {
	tg := syncTarget(t, `{path: config.yaml, format: regex, pattern: 'version:\s*"([^"]*)"'}`)
	in := "app:\n  version: \"1.2.3\"\nother:\n  version:   \"1.2.3\"\n"
	got, err := tg.Replace([]byte(in), "1.3.0")
	if err != nil {
		t.Fatal(err)
	}
	if want := "app:\n  version: \"1.3.0\"\nother:\n  version:   \"1.3.0\"\n"; string(got) != want {
		t.Errorf("got %q", got)
	}
	if _, err := tg.Extract([]byte("app:\n  version: \"1.2.3\"\nx:\n  version: \"1.2.4\"\n")); err == nil {
		t.Error("disagreeing occurrences must fail")
	}
	if _, err := tg.Extract([]byte("nothing here")); err == nil {
		t.Error("no match must fail")
	}
}

func TestWriteAndRestore(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("VERSION", "1.2.3\n", 0o644)
	write("gen.yaml", "version: \"1.2.3\"\n", 0o444)
	cfg, err := config.Parse([]byte("schema: 1\nversion:\n  mode: file\n  sync: [{path: gen.yaml, format: regex, pattern: 'version: \"([^\"]*)\"'}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	cur, err := Current(root, cfg.Version)
	if err != nil || cur.String() != "1.2.3" {
		t.Fatalf("Current = %v, %v", cur, err)
	}
	backup, err := Write(root, cfg.Version, cur.Bump(Minor))
	if err != nil {
		t.Fatal(err)
	}
	gen, _ := os.ReadFile(filepath.Join(root, "gen.yaml"))
	if string(gen) != "version: \"1.3.0\"\n" {
		t.Errorf("gen.yaml = %q", gen)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(filepath.Join(root, "gen.yaml"))
		if info.Mode().Perm() != 0o444 {
			t.Errorf("read-only mode lost: %v", info.Mode())
		}
	}
	if err := backup.Restore(); err != nil {
		t.Fatal(err)
	}
	v, _ := os.ReadFile(filepath.Join(root, "VERSION"))
	gen, _ = os.ReadFile(filepath.Join(root, "gen.yaml"))
	if string(v) != "1.2.3\n" || string(gen) != "version: \"1.2.3\"\n" {
		t.Errorf("restore: %q %q", v, gen)
	}
}

func TestWriteTouchesNothingWhenATargetIsBroken(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte("1.2.3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte("schema: 1\nversion:\n  mode: file\n  sync: [{path: missing.json, format: json, key: [version]}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Write(root, cfg.Version, Semver{1, 2, 4}); err == nil {
		t.Fatal("missing sync file must fail")
	}
	if v, _ := os.ReadFile(filepath.Join(root, "VERSION")); string(v) != "1.2.3\n" {
		t.Errorf("VERSION changed to %q", v)
	}
}

func TestWriteSeveralTargetsInOneFile(t *testing.T) {
	root := t.TempDir()
	lock := "{\n  \"name\": \"web\",\n  \"version\": \"1.2.3\",\n  \"packages\": {\n    \"\": {\n      \"name\": \"web\",\n      \"version\": \"1.2.3\"\n    }\n  }\n}\n"
	for name, content := range map[string]string{"VERSION": "1.2.3\n", "package-lock.json": lock} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Parse([]byte(`schema: 1
version:
  mode: file
  sync:
    - {path: package-lock.json, format: json, key: [version]}
    - {path: package-lock.json, format: json, key: [packages, "", version]}
`))
	if err != nil {
		t.Fatal(err)
	}
	backup, err := Write(root, cfg.Version, Semver{1, 2, 4})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(root, "package-lock.json"))
	if want := strings.ReplaceAll(lock, "1.2.3", "1.2.4"); string(got) != want {
		t.Errorf("package-lock.json:\n%s\nwant:\n%s", got, want)
	}
	if err := backup.Restore(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "package-lock.json")); string(got) != lock {
		t.Errorf("restore:\n%s", got)
	}
}
