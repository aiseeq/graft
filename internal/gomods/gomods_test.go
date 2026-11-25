package gomods

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/userconfig"
)

func TestEnvWithNameCaseByOS(t *testing.T) {
	env := []string{"GOWORK=old", "gowork=lower", "PATH=/bin"}
	for _, tc := range []struct {
		goos string
		want []string
	}{
		{"linux", []string{"gowork=lower", "PATH=/bin", "GOWORK=off"}},
		{"windows", []string{"PATH=/bin", "GOWORK=off"}},
	} {
		if got := envWithForOS(env, "GOWORK", "off", tc.goos); !slices.Equal(got, tc.want) {
			t.Errorf("envWithForOS on %s = %#v, want %#v", tc.goos, got, tc.want)
		}
	}
}

func TestForeignGoGetFailureRestoresExactFiles(t *testing.T) {
	dir := t.TempDir()
	modPath := filepath.Join(dir, "go.mod")
	sumPath := filepath.Join(dir, "go.sum")
	mod := "module example.com/consumer\n\ngo 1.21\n\nrequire example.com/foreign v1.0.0\n"
	sum := "example.com/foreign v1.0.0 h1:old\n"
	if err := os.WriteFile(modPath, []byte(mod), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sumPath, []byte(sum), 0o600); err != nil {
		t.Fatal(err)
	}
	goPath := filepath.Join(dir, "go")
	script := `#!/bin/sh
if [ "$1" = list ]; then
  printf '%s\n' '{"Path":"example.com/foreign","Version":"v1.1.0"}'
  exit 0
fi
if [ "$1" = get ]; then
  printf '%s\n' 'module example.com/consumer' '' 'go 1.21' '' 'require example.com/foreign v1.1.0' > go.mod
  printf '%s\n' 'example.com/foreign v1.1.0 h1:new' > go.sum
  echo 'simulated API incompatibility' >&2
  exit 1
fi
exit 0
`
	if err := os.WriteFile(goPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var warnings []string
	warn := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	if err := raiseForeignFile(context.Background(), modPath, "go.mod", goPath, []string{}, func(string) bool { return false }, &strings.Builder{}, warn); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(modPath); string(got) != mod {
		t.Errorf("go.mod not restored byte for byte: %q", got)
	}
	if got, _ := os.ReadFile(sumPath); string(got) != sum {
		t.Errorf("go.sum not restored byte for byte: %q", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "example.com/foreign") || !strings.Contains(warnings[0], "simulated API incompatibility") {
		t.Errorf("warning = %#v", warnings)
	}
}

func TestForeignGoTidyFailureRestoresPreListSnapshot(t *testing.T) {
	dir := t.TempDir()
	modPath := filepath.Join(dir, "go.mod")
	sumPath := filepath.Join(dir, "go.sum")
	mod := "module example.com/consumer\n\nrequire example.com/foreign v1.0.0\n\n// user's uncommitted edit\n"
	sum := "user's go.sum bytes\n"
	if err := os.WriteFile(modPath, []byte(mod), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sumPath, []byte(sum), 0o600); err != nil {
		t.Fatal(err)
	}
	goPath := filepath.Join(dir, "go")
	script := `#!/bin/sh
if [ "$1" = list ]; then
  printf '%s\n' 'module example.com/consumer' '' 'go 1.21' '' 'require example.com/foreign v1.0.0' > go.mod
  printf '%s\n' 'transient from list' > go.sum
  printf '%s\n' '{"Path":"example.com/foreign","Version":"v1.1.0"}'
  exit 0
fi
if [ "$1" = get ]; then
  printf '%s\n' 'module example.com/consumer' '' 'go 1.21' '' 'require example.com/foreign v1.1.0' > go.mod
  printf '%s\n' 'transient from get' > go.sum
  exit 0
fi
if [ "$1" = mod ] && [ "$2" = tidy ]; then
  printf '%s\n' 'partial tidy changed go.mod' > go.mod
  printf '%s\n' 'partial tidy changed go.sum' > go.sum
  echo 'tidy failed after partial edits' >&2
  exit 1
fi
exit 0
`
	if err := os.WriteFile(goPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var warnings []string
	warn := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	if err := raiseForeignFile(context.Background(), modPath, "go.mod", goPath, []string{}, func(string) bool { return false }, &strings.Builder{}, warn); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(modPath); string(got) != mod {
		t.Errorf("go.mod did not restore pre-list bytes: %q", got)
	}
	if got, _ := os.ReadFile(sumPath); string(got) != sum {
		t.Errorf("go.sum did not restore pre-list bytes: %q", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "tidy failed after partial edits") {
		t.Errorf("warning = %#v", warnings)
	}
}

func TestForeignGoDoesNotRewriteUnchangedReadOnlyFiles(t *testing.T) {
	for _, offline := range []bool{false, true} {
		name := "no update"
		if offline {
			name = "offline"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			modPath := filepath.Join(dir, "go.mod")
			sumPath := filepath.Join(dir, "go.sum")
			mod := "module example.com/consumer\n\ngo 1.21\n\nrequire example.com/foreign v1.0.0\n"
			sum := "example.com/foreign v1.0.0 h1:old\n"
			if err := os.WriteFile(modPath, []byte(mod), 0o444); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sumPath, []byte(sum), 0o444); err != nil {
				t.Fatal(err)
			}
			stamp := time.Unix(946684800, 0)
			for _, path := range []string{modPath, sumPath} {
				if err := os.Chtimes(path, stamp, stamp); err != nil {
					t.Fatal(err)
				}
			}
			goPath := filepath.Join(dir, "go")
			script := `#!/bin/sh
if [ "$1" = list ]; then
  if [ "$GO_FAKE_OFFLINE" = 1 ]; then
    echo 'proxy unavailable' >&2
    exit 1
  fi
  printf '%s\n' '{"Path":"example.com/foreign","Version":"v1.0.0"}'
fi
exit 0
`
			if err := os.WriteFile(goPath, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			env := []string{}
			if offline {
				env = append(env, "GO_FAKE_OFFLINE=1")
			}
			var warnings []string
			warn := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
			err := raiseForeignFile(context.Background(), modPath, "go.mod", goPath, env, func(string) bool { return false }, &strings.Builder{}, warn)
			if offline {
				if err != nil {
					t.Fatalf("offline foreign step: %v", err)
				}
				if len(warnings) != 1 || !strings.Contains(warnings[0], "proxy unavailable") {
					t.Fatalf("offline warning = %#v", warnings)
				}
			} else if err != nil {
				t.Fatalf("no-update foreign step: %v", err)
			}
			for _, path := range []string{modPath, sumPath} {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if !info.ModTime().Equal(stamp) {
					t.Errorf("%s was rewritten: mtime %v, want %v", filepath.Base(path), info.ModTime(), stamp)
				}
				if info.Mode().Perm() != 0o444 {
					t.Errorf("%s mode = %o, want read-only 444", filepath.Base(path), info.Mode().Perm())
				}
			}
		})
	}
}

func TestForeignGoWithoutGoDirectiveCanRaise(t *testing.T) {
	dir := t.TempDir()
	modPath := filepath.Join(dir, "go.mod")
	mod := "module example.com/consumer\n\nrequire example.com/foreign v1.0.0\n"
	if err := os.WriteFile(modPath, []byte(mod), 0o600); err != nil {
		t.Fatal(err)
	}
	goPath := filepath.Join(dir, "go")
	script := `#!/bin/sh
if [ "$1" = list ]; then
  printf '%s\n' '{"Path":"example.com/foreign","Version":"v1.1.0"}'
elif [ "$1" = get ]; then
  printf '%s\n' 'module example.com/consumer' '' 'require example.com/foreign v1.1.0' > go.mod
fi
exit 0
`
	if err := os.WriteFile(goPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	warn := func(string, ...any) {}
	if err := raiseForeignFile(context.Background(), modPath, "go.mod", goPath, []string{}, func(string) bool { return false }, &strings.Builder{}, warn); err != nil {
		t.Fatalf("raise without go directive: %v", err)
	}
	if got, _ := os.ReadFile(modPath); !strings.Contains(string(got), "v1.1.0") {
		t.Errorf("raise did not happen: %q", got)
	}
}

func TestForeignGoQueriesOnlyNonOwnModules(t *testing.T) {
	dir := t.TempDir()
	modPath := filepath.Join(dir, "go.mod")
	mod := "module example.com/consumer\n\ngo 1.21\n\nrequire (\n\texample.com/own/lib v1.0.0\n\texample.com/foreign/lib v1.0.0\n)\n"
	if err := os.WriteFile(modPath, []byte(mod), 0o600); err != nil {
		t.Fatal(err)
	}
	goPath := filepath.Join(dir, "go")
	logPath := filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf '%s %s\n' "$1" "$*" >> calls
if [ "$1" = list ]; then
  printf '%s\n' '{"Path":"example.com/foreign/lib","Version":"v1.1.0"}'
fi
exit 0
`
	if err := os.WriteFile(goPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var warnings []string
	warn := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	own := func(path string) bool { return strings.HasPrefix(path, "example.com/own/") }
	if err := raiseForeignFile(context.Background(), modPath, "go.mod", goPath, []string{}, own, &strings.Builder{}, warn); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), "example.com/own/lib@latest") || !strings.Contains(string(calls), "example.com/foreign/lib@latest") {
		t.Errorf("foreign pass queried wrong modules: %s", calls)
	}
	if !strings.Contains(string(calls), "get get example.com/foreign/lib@v1.1.0") || strings.Contains(string(calls), "own/lib@") {
		t.Errorf("foreign pass sent wrong upgrade request: %s", calls)
	}
}

func TestForeignGoToolchainOnlyChangeRollsBack(t *testing.T) {
	dir := t.TempDir()
	modPath := filepath.Join(dir, "go.mod")
	mod := "module example.com/consumer\n\ngo 1.21\n\nrequire example.com/foreign v1.0.0\n"
	if err := os.WriteFile(modPath, []byte(mod), 0o600); err != nil {
		t.Fatal(err)
	}
	goPath := filepath.Join(dir, "go")
	script := `#!/bin/sh
if [ "$1" = list ]; then
  printf '%s\n' '{"Path":"example.com/foreign","Version":"v1.1.0"}'
elif [ "$1" = get ]; then
  printf '%s\n' 'module example.com/consumer' '' 'go 1.21' 'toolchain go1.99.0' '' 'require example.com/foreign v1.1.0' > go.mod
fi
exit 0
`
	if err := os.WriteFile(goPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var warnings []string
	warn := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	if err := raiseForeignFile(context.Background(), modPath, "go.mod", goPath, []string{}, func(string) bool { return false }, &strings.Builder{}, warn); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(modPath); string(got) != mod {
		t.Errorf("toolchain-only change was not rolled back: %q", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "go/toolchain directive changed") {
		t.Errorf("warning = %#v", warnings)
	}
}

func TestSelectCandidates(t *testing.T) {
	const gomod = `module example.com/own/app

go 1.21

require (
	example.com/own/app v1.0.0
	example.com/own/lib v1.2.0
	example.com/own/local v1.0.0
	example.com/own/replaced v1.0.0
	example.com/ownish/lib v1.0.0
	example.com/other/lib v1.0.0
)

replace example.com/own/local v1.0.0 => ../local
replace example.com/own/replaced => ../replaced
`
	got, err := selectCandidates([]byte(gomod), userconfig.Modules{Latest: []string{"example.com/own"}}.IsLatest)
	if err != nil {
		t.Fatal(err)
	}
	want := []candidate{{"example.com/own/lib", "v1.2.0"}}
	if len(got) != len(want) || (len(got) > 0 && got[0] != want[0]) {
		t.Fatalf("candidates: %#v, want %#v", got, want)
	}
}

func TestFormatRaisedModules(t *testing.T) {
	got := formatRaisedModules(
		[]candidate{{path: "example.com/akit", version: "v1.0.0"}, {path: "example.com/graft/v2", version: "v2.0.0"}},
		map[string]LatestModule{"example.com/akit": {Version: "v1.1.0"}, "example.com/graft/v2": {Version: "v2.1.0"}},
	)
	const want = "example.com/akit v1.0.0 -> v1.1.0, example.com/graft/v2 v2.0.0 -> v2.1.0"
	if got != want || strings.Contains(got, "\n") {
		t.Fatalf("formatRaisedModules = %q, want one line %q", got, want)
	}
}

func TestForeignUpgradeKeepsMajorAndVersionShape(t *testing.T) {
	for _, tc := range []struct {
		current, latest string
		want            bool
	}{
		{"v1.2.0", "v1.3.0", true},
		{"v1.2.0", "v2.0.0", false},
		{"v1.2.0", "v2.0.0+incompatible", false},
		{"v1.2.0", "v1.3.1-0.20260102000000-222222222222", false},
		{"v0.0.0-20260101000000-111111111111", "v0.0.0-20260102000000-222222222222", true},
	} {
		if got := foreignUpgrade(tc.current, tc.latest); got != tc.want {
			t.Errorf("foreignUpgrade(%q, %q) = %t, want %t", tc.current, tc.latest, got, tc.want)
		}
	}
}
