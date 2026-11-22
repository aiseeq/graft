package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
)

func staticcheckRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.org"}, args...)...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/test\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package test\nfunc f() {\n _ = 1\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git("init", "-q", "-b", "main")
	git("add", "-A")
	git("commit", "-qm", "base")
	return root
}

func fakeStaticcheck(t *testing.T, report string, exitCode int) string {
	return fakeStaticcheckOutput(t, report, "", exitCode)
}

func fakeStaticcheckOutput(t *testing.T, report, stderr string, exitCode int) string {
	t.Helper()
	bin := t.TempDir()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' '" + report + "'\n"
	if stderr != "" {
		script += "printf '%s\\n' '" + stderr + "' >&2\n"
	}
	script += "exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "staticcheck"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec '"+gitPath+"' \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return bin
}

func TestStaticcheckPureRenameDoesNotMarkLinesChanged(t *testing.T) {
	root := staticcheckRepo(t)
	cmd := exec.Command("git", "-C", root, "mv", "--", "a.go", "renamed.go")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git mv: %v\n%s", err, out)
	}
	fakeStaticcheck(t, `{"code":"SA4006","message":"old finding","location":{"file":"renamed.go","line":3}}`, 1)
	var out bytes.Buffer
	r := &Runner{Root: root, Config: &config.Config{}, Stdout: &out}
	if err := r.checkStaticcheck(context.Background(), "."); err != nil {
		t.Fatalf("pure rename must not make old finding new: %v; %q", err, out.String())
	}
}

func TestStaticcheckReportsChangedSAAndBuildFailureTogether(t *testing.T) {
	root := staticcheckRepo(t)
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package test\nfunc f() {\n _ = 2\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	report := `{"code":"SA4006","message":"changed finding","location":{"file":"a.go","line":3}}` + "\n" +
		`{"code":"compile","message":"undefined: missingName","location":{"file":"broken.go","line":1}}`
	fakeStaticcheckOutput(t, report, "compiler stderr detail", 1)
	var out bytes.Buffer
	r := &Runner{Root: root, Config: &config.Config{}, Stdout: &out}
	err := r.checkStaticcheck(context.Background(), ".")
	if err == nil {
		t.Fatal("mixed SA and compile diagnostics must fail")
	}
	for _, want := range []string{"a.go:3: SA4006", "broken.go:1: compile undefined: missingName", "compiler stderr detail"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q: %s", want, out.String())
		}
	}
}

func TestStaticcheckFailureOmitsUnchangedSA(t *testing.T) {
	root := staticcheckRepo(t)
	oldFile := filepath.Join(root, "old.go")
	if err := os.WriteFile(oldFile, []byte("package test\nfunc old() {\n _ = 1\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git := exec.Command("git", "-C", root, "-c", "user.name=t", "-c", "user.email=t@example.org", "add", "--", "old.go")
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	git = exec.Command("git", "-C", root, "-c", "user.name=t", "-c", "user.email=t@example.org", "commit", "-qm", "add old finding file")
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package test\nfunc f() {\n _ = 2\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	report := `{"code":"SA4006","message":"new finding","location":{"file":"a.go","line":3}}` + "\n" +
		`{"code":"SA4006","message":"old finding","location":{"file":"old.go","line":3}}`
	fakeStaticcheck(t, report, 1)
	var out bytes.Buffer
	r := &Runner{Root: root, Config: &config.Config{}, Stdout: &out}
	if err := r.checkStaticcheck(context.Background(), "."); err == nil {
		t.Fatal("changed SA finding must fail")
	}
	if !strings.Contains(out.String(), "a.go:3: SA4006 new finding") || strings.Contains(out.String(), "old.go:3: SA4006") {
		t.Fatalf("output must show only the changed SA finding: %q", out.String())
	}
}

func TestStaticcheckChangedLinesGate(t *testing.T) {
	for _, tc := range []struct {
		name, file, code string
		line             int
		changed          bool
		exitCode         int
		wantFail         bool
	}{
		{"changed SA", "a.go", "SA4006", 3, true, 1, true},
		{"unchanged line", "a.go", "SA4006", 1, true, 1, false},
		{"unchanged file", "other.go", "SA4006", 1, true, 1, false},
		{"non SA at exit zero", "a.go", "QF1001", 3, true, 0, false},
		{"compile error", "a.go", "compile", 3, true, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := staticcheckRepo(t)
			if tc.changed {
				if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package test\nfunc f() {\n _ = 2\n}\n"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			loc := tc.file
			report := `{"code":"` + tc.code + `","message":"undefined name","location":{"file":"` + loc + `","line":` + strconv.Itoa(tc.line) + `}}`
			fakeStaticcheck(t, report, tc.exitCode)
			var out bytes.Buffer
			r := &Runner{Root: root, Config: &config.Config{}, Stdout: &out}
			err := r.checkStaticcheck(context.Background(), ".")
			if (err != nil) != tc.wantFail {
				t.Fatalf("error = %v, output %q", err, out.String())
			}
			if tc.wantFail && tc.code == "SA4006" && !strings.Contains(out.String(), "a.go:3: SA4006") {
				t.Fatalf("missing diagnostic: %v; %q", err, out.String())
			}
			if tc.code == "compile" && (err == nil || !strings.Contains(out.String(), "compile undefined name")) {
				t.Fatalf("compile failure was not reported: %v; %q", err, out.String())
			}
		})
	}
}

func TestStaticcheckChangedPathsWithSpacesAndUnicode(t *testing.T) {
	for _, file := range []string{"has space.go", "тест.go"} {
		t.Run(file, func(t *testing.T) {
			root := staticcheckRepo(t)
			name := filepath.Join(root, file)
			base := []byte("package test\nfunc f() {\n _ = 1\n}\n")
			if err := os.WriteFile(name, base, 0644); err != nil {
				t.Fatal(err)
			}
			git := exec.Command("git", "-C", root, "-c", "user.name=t", "-c", "user.email=t@example.org", "add", "--", file)
			if out, err := git.CombinedOutput(); err != nil {
				t.Fatalf("git add: %v\n%s", err, out)
			}
			git = exec.Command("git", "-C", root, "-c", "user.name=t", "-c", "user.email=t@example.org", "commit", "-qm", "add named file")
			if out, err := git.CombinedOutput(); err != nil {
				t.Fatalf("git commit: %v\n%s", err, out)
			}
			if err := os.WriteFile(name, []byte("package test\nfunc f() {\n _ = 2\n}\n"), 0644); err != nil {
				t.Fatal(err)
			}
			fakeStaticcheck(t, `{"code":"SA4006","message":"changed","location":{"file":"`+file+`","line":3}}`, 1)
			var out bytes.Buffer
			r := &Runner{Root: root, Config: &config.Config{}, Stdout: &out}
			if err := r.checkStaticcheck(context.Background(), "."); err == nil || !strings.Contains(out.String(), file+":3: SA4006") {
				t.Fatalf("diagnostic for %q not matched: %v; %q", file, err, out.String())
			}
		})
	}
}

func TestStaticcheckFromModuleDirectoryIncludesUntrackedFile(t *testing.T) {
	root := staticcheckRepo(t)
	mod := filepath.Join(root, "mod")
	if err := os.MkdirAll(mod, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mod, "go.mod"), []byte("module example.com/mod\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	newFile := filepath.Join(mod, "new_test.go")
	if err := os.WriteFile(newFile, []byte("package mod\nfunc f() {\n _ = 2\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	fakeStaticcheck(t, `{"code":"SA4006","message":"new file","location":{"file":"new_test.go","line":3}}`, 1)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(mod); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	var out bytes.Buffer
	r := &Runner{Root: root, Config: &config.Config{}, Stdout: &out}
	if err := r.checkStaticcheck(context.Background(), "mod"); err == nil || !strings.Contains(out.String(), "mod/new_test.go:3: SA4006") {
		t.Fatalf("untracked module file was not included from subdirectory: %v; %q", err, out.String())
	}
}

func TestStaticcheckRealBinary(t *testing.T) {
	if _, err := exec.LookPath("staticcheck"); err != nil {
		t.Skip("staticcheck is not installed")
	}
	root := staticcheckRepo(t)
	r := &Runner{Root: root, Config: &config.Config{}}
	if err := r.checkStaticcheck(context.Background(), "."); err != nil {
		t.Fatal(err)
	}
}

func TestStaticcheckForeignPathTranslation(t *testing.T) {
	files := map[string]bool{"x/f.go": true, "a/x/f.go": true, "svc/p.go": true}
	root := t.TempDir()
	if got, own := diagnosticLineForTest(t, "/old/tree/a/x/f.go", "mod", root, files, nil); !own || got != "mod/a/x/f.go:3: SA4006 changed" {
		t.Fatalf("longest suffix translation = %q own=%v", got, own)
	}
	if got, own := diagnosticLineForTest(t, "/old/tree/.claude/worktrees/x/svc/p.go", "mod", root, files, nil); !own || got != "mod/svc/p.go:3: SA4006 changed" {
		t.Fatalf("nested worktree translation = %q own=%v", got, own)
	}
	if got, own := diagnosticLineForTest(t, "/elsewhere/q/f.go", "mod", root, files, nil); own || got != "/elsewhere/q/f.go:3: SA4006 changed" {
		t.Fatalf("basename-only path translated: %q own=%v", got, own)
	}
	goRootOutput, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Fatal(err)
	}
	goRoot := strings.TrimSpace(string(goRootOutput))
	if isExternalGoPath(filepath.Join(goRoot, "src", "fmt", "print.go"), []string{goRoot}) != true {
		t.Fatal("GOROOT source path must be external")
	}
	moduleCache := t.TempDir()
	if isExternalGoPath(filepath.Join(moduleCache, "example.com", "x", "f.go"), []string{moduleCache}) != true {
		t.Fatal("module cache source path must be external")
	}
	for _, external := range []string{
		filepath.Join(goRoot, "src", "fmt", "print.go"),
		filepath.Join(moduleCache, "example.com", "x", "f.go"),
	} {
		input, err := json.Marshal(staticcheckDiagnosticAt(external))
		if err != nil {
			t.Fatal(err)
		}
		parsed, retry, err := parseStaticcheckOutput(input, "mod", root, files, nil, []string{goRoot, moduleCache})
		if err != nil || retry || len(parsed.findings) != 0 || !strings.Contains(parsed.diagnosticLines[0], external) {
			t.Fatalf("external related path %q was translated: %+v retry=%v err=%v", external, parsed, retry, err)
		}
	}
	if got := staticcheckPathCandidates("/old/tree/x/f.go", "mod", files, nil); len(got) != 2 || got[0] != "a/x/f.go" || got[1] != "x/f.go" {
		t.Fatalf("ambiguous suffix candidates = %v; want both colliding files", got)
	}
	if got := staticcheckPathCandidates("/elsewhere/q/f.go", "mod", files, nil); len(got) != 0 {
		t.Fatalf("single-name suffix was translated: %v", got)
	}
	rootFiles := map[string]bool{"p.go": true, "sub/p.go": true}
	rootDiagnostic := []byte(`{"code":"SA4006","message":"root file","location":{"file":"/old/copy/svc/p.go","line":3}}`)
	parsed, retry, err := parseStaticcheckOutput(rootDiagnostic, "svc", root, rootFiles, map[string]map[int]bool{"svc/p.go": {3: true}}, nil)
	if err != nil || retry || len(parsed.findings) != 1 || !strings.Contains(parsed.findings[0], "svc/p.go:3: SA4006") {
		t.Fatalf("module-root file translation = %+v retry=%v err=%v", parsed, retry, err)
	}
	if got := staticcheckPathCandidates("/old/copy/svc/sub/p.go", "svc", rootFiles, nil); len(got) != 1 || got[0] != "sub/p.go" {
		t.Fatalf("root file translation lost nested-path precedence: %v", got)
	}
	rootModuleDiagnostic := []byte(`{"code":"SA4006","message":"root module file","location":{"file":"/old/copy/p.go","line":3}}`)
	parsed, retry, err = parseStaticcheckOutput(rootModuleDiagnostic, ".", root, rootFiles, map[string]map[int]bool{"p.go": {3: true}}, nil)
	if err != nil || retry || len(parsed.findings) != 1 || !strings.Contains(parsed.findings[0], "p.go:3: SA4006") {
		t.Fatalf("root module file translation = %+v retry=%v err=%v", parsed, retry, err)
	}
	diagnostic := []byte(`{"code":"SA4006","message":"ambiguous","location":{"file":"/old/tree/x/f.go","line":3}}`)
	for _, candidate := range []string{"mod/x/f.go", "mod/a/x/f.go"} {
		parsed, retry, err := parseStaticcheckOutput(diagnostic, "mod", root, files, map[string]map[int]bool{candidate: {3: true}}, nil)
		if err != nil || retry || len(parsed.findings) != 1 {
			t.Fatalf("ambiguous candidate %s: findings=%v retry=%v err=%v", candidate, parsed.findings, retry, err)
		}
	}
	moduleRoot := filepath.Join(root, "svc")
	if err := os.MkdirAll(moduleRoot, 0755); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(moduleRoot, "x", "f.go")
	got, _, own, _, err := staticcheckDiagnosticLine(staticcheckDiagnosticAt(localPath), "svc", root, files, nil)
	if err != nil || !own || !strings.HasPrefix(got, "svc/x/f.go:3:") {
		t.Fatalf("own collision path = %q own=%v err=%v", got, own, err)
	}
}

func TestStaticcheckLocalModulePathSkipsGoIgnoredDirectories(t *testing.T) {
	moduleDir := t.TempDir()
	for _, tc := range []struct {
		name string
		own  bool
	}{
		{"visible/pkg.go", true},
		{".claude/worktrees/x/pkg.go", false},
		{"_generated/pkg.go", false},
		{"testdata/pkg.go", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.MkdirAll(filepath.Join(moduleDir, filepath.Dir(tc.name)), 0755); err != nil {
				t.Fatal(err)
			}
			got, own, err := staticcheckLocalModuleFile(filepath.Join(moduleDir, tc.name), moduleDir)
			if err != nil {
				t.Fatal(err)
			}
			if own != tc.own || (own && filepath.ToSlash(got) != tc.name) {
				t.Fatalf("staticcheckLocalModuleFile() = %q, %v; want path %q, own %v", got, own, tc.name, tc.own)
			}
		})
	}
	nested := filepath.Join(moduleDir, "nested")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "go.mod"), []byte("module example.com/nested\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got, own, err := staticcheckLocalModuleFile(filepath.Join(nested, "pkg.go"), moduleDir); err != nil || own {
		t.Fatalf("nested module path %q classified as own, err=%v", got, err)
	}
}

func TestStaticcheckModuleFilesFiltersNestedModuleInSubdirectory(t *testing.T) {
	root := t.TempDir()
	moduleDir := filepath.Join(root, "svc")
	nestedDir := filepath.Join(moduleDir, "nested")
	if err := os.MkdirAll(nestedDir, 0755); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	var packageJSON strings.Builder
	for _, pkg := range []map[string]any{
		{"Dir": moduleDir, "GoFiles": []string{"p.go"}, "Module": map[string]bool{"Main": true}},
		{"Dir": nestedDir, "GoFiles": []string{"n.go"}, "Module": map[string]bool{"Main": false}},
	} {
		encoded, err := json.Marshal(pkg)
		if err != nil {
			t.Fatal(err)
		}
		packageJSON.Write(encoded)
		packageJSON.WriteByte('\n')
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$STATICCHECK_TEST_JSON\"\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STATICCHECK_TEST_JSON", packageJSON.String())
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	files, err := (&Runner{Root: root, Config: &config.Config{}}).staticcheckModuleFiles(context.Background(), "svc")
	if err != nil {
		t.Fatal(err)
	}
	if !files["p.go"] || files["nested/n.go"] {
		t.Fatalf("module file set = %v; want only main module files", files)
	}
}

func TestStaticcheckModuleFilesReportsGoListStderr(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "svc"), 0755); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\ntest \"$1 $2\" = 'list -e' || exit 3\necho broken-import >&2\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	_, err := (&Runner{Root: root, Config: &config.Config{}}).staticcheckModuleFiles(context.Background(), "svc")
	if err == nil || !strings.Contains(err.Error(), "broken-import") {
		t.Fatalf("go list error = %v; want captured stderr", err)
	}
	combined := staticcheckWithListingError(err, errors.New("staticcheck exit 1"), "staticcheck stderr")
	if !strings.Contains(combined.Error(), "staticcheck exit 1") || !strings.Contains(combined.Error(), "staticcheck stderr") {
		t.Fatalf("listing error masked staticcheck failure: %v", combined)
	}
}

func diagnosticLineForTest(t *testing.T, file, module, root string, files map[string]bool, external []string) (string, bool) {
	t.Helper()
	line, _, own, _, err := staticcheckDiagnosticLine(staticcheckDiagnosticAt(file), module, root, files, external)
	if err != nil {
		t.Fatal(err)
	}
	return line, own
}

func staticcheckDiagnosticAt(file string) staticcheckDiagnostic {
	d := staticcheckDiagnostic{Code: "SA4006", Message: "changed"}
	d.Location.File, d.Location.Line = file, 3
	return d
}

func TestStaticcheckUntranslatedPathRequestsRetry(t *testing.T) {
	diagnostic := staticcheckDiagnostic{}
	diagnostic.Code = "SA4006"
	diagnostic.Message = "finding"
	diagnostic.Location.File = "/old/tree/other/missing.go"
	diagnostic.Location.Line = 3
	data, err := json.Marshal(diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	_, retry, err := parseStaticcheckOutput(data, "mod", t.TempDir(), map[string]bool{"svc/p.go": true}, map[string]map[int]bool{}, nil)
	if err != nil || !retry {
		t.Fatalf("parseStaticcheckOutput retry = %v, err = %v", retry, err)
	}
}

func TestStaticcheckRerunsForUntranslatedPath(t *testing.T) {
	root := staticcheckRepo(t)
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package test\nfunc f() {\n _ = 2\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	countFile := filepath.Join(bin, "count")
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nn=0\n[ ! -f '" + countFile + "' ] || read n < '" + countFile + "'\nn=$((n + 1))\nprintf '%s\\n' \"$n\" > '" + countFile + "'\n"
	script += "if [ \"$n\" -eq 1 ]; then printf '%s\\n' '{\"code\":\"SA4006\",\"message\":\"foreign\",\"location\":{\"file\":\"/old/copy/missing.go\",\"line\":3}}'; else printf '%s\\n' '{\"code\":\"SA4006\",\"message\":\"local\",\"location\":{\"file\":\"a.go\",\"line\":3}}'; fi\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "staticcheck"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec '"+gitPath+"' \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(goPath, filepath.Join(bin, "go")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	var out, stderr bytes.Buffer
	r := &Runner{Root: root, Config: &config.Config{}, Stdout: &out, Stderr: &stderr}
	if err := r.checkStaticcheck(context.Background(), "."); err == nil || !strings.Contains(out.String(), "a.go:3: SA4006 local") {
		t.Fatalf("rerun result was not gated: %v; %q", err, out.String())
	}
	count, err := os.ReadFile(countFile)
	if err != nil || strings.TrimSpace(string(count)) != "2" {
		t.Fatalf("staticcheck run count = %q, err = %v; want 2 runs", count, err)
	}
	if !strings.Contains(stderr.String(), staticcheckRerunNotice) {
		t.Fatalf("missing rerun notice: %q", stderr.String())
	}
}

func TestStaticcheckRootFileTranslationDoesNotRetry(t *testing.T) {
	root := staticcheckRepo(t)
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package test\nfunc f() {\n _ = 2\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"git": gitPath, "go": goPath} {
		if err := os.Symlink(target, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	diagnostic, err := json.Marshal(staticcheckDiagnosticAt("/old/copy/a.go"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "staticcheck"), []byte("#!/bin/sh\nprintf '%s\\n' '"+string(diagnostic)+"'\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	var out, stderr bytes.Buffer
	r := &Runner{Root: root, Config: &config.Config{}, Stdout: &out, Stderr: &stderr}
	if err := r.checkStaticcheck(context.Background(), "."); err == nil || !strings.Contains(out.String(), "a.go:3: SA4006") {
		t.Fatalf("root finding was not translated and gated: %v; %q", err, out.String())
	}
	if strings.Contains(stderr.String(), staticcheckRerunNotice) {
		t.Fatalf("translated root finding unexpectedly reran: %q", stderr.String())
	}
}

func TestStaticcheckRetryForcesTrimpathFalseAndRejectsForeignPaths(t *testing.T) {
	for _, tc := range []struct {
		name, secondFile string
		wantErr          string
	}{
		{"local path retains finding", "a.go", "running staticcheck"},
		{"foreign path fails clearly", "/still/foreign/no-match.go", staticcheckRerunUntranslatedError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := staticcheckRepo(t)
			if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package test\nfunc f() {\n _ = 2\n}\n"), 0644); err != nil {
				t.Fatal(err)
			}
			goPath, err := exec.LookPath("go")
			if err != nil {
				t.Fatal(err)
			}
			gitPath, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			countFile := filepath.Join(bin, "count")
			flagsFile := filepath.Join(bin, "retry-flags")
			script := "#!/bin/sh\nn=0\n[ ! -f '" + countFile + "' ] || read n < '" + countFile + "'\nn=$((n + 1))\nprintf '%s\\n' \"$n\" > '" + countFile + "'\n"
			script += "if [ \"$n\" -eq 1 ]; then file='/old/copy/no-match.go'; else file='" + tc.secondFile + "'; go env GOFLAGS > '" + flagsFile + "'; fi\n"
			script += "printf '{\"code\":\"SA4006\",\"message\":\"cached\",\"location\":{\"file\":\"%s\",\"line\":3}}\\n' \"$file\"\nexit 1\n"
			if err := os.WriteFile(filepath.Join(bin, "staticcheck"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec '"+gitPath+"' \"$@\"\n"), 0755); err != nil {
				t.Fatal(err)
			}
			goenv := filepath.Join(t.TempDir(), "goenv")
			oldFlags, hadFlags := os.LookupEnv("GOFLAGS")
			if err := os.Unsetenv("GOFLAGS"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if hadFlags {
					_ = os.Setenv("GOFLAGS", oldFlags)
				} else {
					_ = os.Unsetenv("GOFLAGS")
				}
			})
			t.Setenv("GOENV", goenv)
			writeEnv := exec.Command(goPath, "env", "-w", "GOFLAGS=-trimpath")
			writeEnv.Dir = root
			if out, err := writeEnv.CombinedOutput(); err != nil {
				t.Fatalf("write isolated GOENV: %v\n%s", err, out)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			var out, stderr bytes.Buffer
			r := &Runner{Root: root, Config: &config.Config{}, Stdout: &out, Stderr: &stderr}
			err = r.checkStaticcheck(context.Background(), ".")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("retry error = %v, want substring %q; stdout=%q stderr=%q", err, tc.wantErr, out.String(), stderr.String())
			}
			flags, err := os.ReadFile(flagsFile)
			if err != nil || strings.TrimSpace(string(flags)) != "-trimpath -trimpath=false" {
				t.Fatalf("retry GOFLAGS = %q, err=%v; want GOENV value overridden by -trimpath=false", flags, err)
			}
			if tc.secondFile == "a.go" && !strings.Contains(out.String(), "a.go:3: SA4006 cached") {
				t.Fatalf("retry finding missing from gate output: %q", out.String())
			}
			if tc.secondFile != "a.go" && !strings.Contains(stderr.String(), staticcheckRerunUntranslatedError) {
				t.Fatalf("missing untranslated retry reason: %q", stderr.String())
			}
		})
	}
}

func TestStaticcheckSkipsGoListWithoutChangedLines(t *testing.T) {
	root := staticcheckRepo(t)
	bin := t.TempDir()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	listMarker := filepath.Join(bin, "listed")
	goScript := "#!/bin/sh\nif [ \"$1\" = env ]; then printf '\\n'; exit 0; fi\nif [ \"$1\" = list ]; then touch '" + listMarker + "'; exit 0; fi\nexec '" + goPath + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(goScript), 0755); err != nil {
		t.Fatal(err)
	}
	staticScript := "#!/bin/sh\nprintf '%s\\n' '{\"code\":\"SA4006\",\"message\":\"old cached\",\"location\":{\"file\":\"/old/copy/no-match.go\",\"line\":3}}'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "staticcheck"), []byte(staticScript), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec '"+gitPath+"' \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := (&Runner{Root: root, Config: &config.Config{}}).checkStaticcheck(context.Background(), "."); err != nil {
		t.Fatalf("unchanged cached finding should pass: %v", err)
	}
	if _, err := os.Stat(listMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("go list ran with no changed lines: stat err=%v", err)
	}
}

func TestStaticcheckAddsTrimPathToGoFlags(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"append to existing flags", "-mod=readonly", "-mod=readonly -trimpath"},
		{"preserve explicit false", "-trimpath=false", "-trimpath=false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := staticcheckRepo(t)
			bin := t.TempDir()
			flagsFile := filepath.Join(bin, "flags")
			gitPath, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "staticcheck"), []byte("#!/bin/sh\nprintf '%s' \"$GOFLAGS\" > '"+flagsFile+"'\nexit 0\n"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec '"+gitPath+"' \"$@\"\n"), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("GOFLAGS", tc.input)
			if err := (&Runner{Root: root, Config: &config.Config{}}).checkStaticcheck(context.Background(), "."); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(flagsFile)
			if err != nil || strings.TrimSpace(string(got)) != tc.want {
				t.Fatalf("staticcheck GOFLAGS = %q, err = %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestStaticcheckAddsTrimPathToGoEnvFlags(t *testing.T) {
	root := staticcheckRepo(t)
	bin := t.TempDir()
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	flagsFile := filepath.Join(bin, "flags")
	if err := os.WriteFile(filepath.Join(bin, "staticcheck"), []byte("#!/bin/sh\nprintf '%s' \"$GOFLAGS\" > '"+flagsFile+"'\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec '"+gitPath+"' \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	goenv := filepath.Join(t.TempDir(), "goenv")
	oldFlags, hadFlags := os.LookupEnv("GOFLAGS")
	if err := os.Unsetenv("GOFLAGS"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadFlags {
			_ = os.Setenv("GOFLAGS", oldFlags)
		} else {
			_ = os.Unsetenv("GOFLAGS")
		}
	})
	t.Setenv("GOENV", goenv)
	writeEnv := exec.Command(goPath, "env", "-w", "GOFLAGS=-tags=x")
	writeEnv.Dir = root
	if out, err := writeEnv.CombinedOutput(); err != nil {
		t.Fatalf("write isolated GOENV: %v\n%s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := (&Runner{Root: root, Config: &config.Config{}}).checkStaticcheck(context.Background(), "."); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(flagsFile)
	if err != nil || strings.TrimSpace(string(got)) != "-tags=x -trimpath" {
		t.Fatalf("staticcheck GOFLAGS = %q, err = %v; want GOENV flags and -trimpath", got, err)
	}
}

func TestStaticcheckSharedCacheAcrossCopies(t *testing.T) {
	if _, err := exec.LookPath("staticcheck"); err != nil {
		t.Skip("staticcheck is not installed")
	}
	base := t.TempDir()
	gitRoot := filepath.Join(base, "repo")
	if err := os.MkdirAll(filepath.Join(gitRoot, "svc"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitRoot, "svc", "go.mod"), []byte("module example.com/projA\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitRoot, "svc", "p.go"), []byte("package projectA\nfunc f() {\n _ = 1\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cachedDir := filepath.Join(gitRoot, "svc", "cached")
	if err := os.MkdirAll(cachedDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cachedDir, "c.go"), []byte("package cached\nfunc gen() int { println(); return 0 }\nfunc old() {\n y := gen()\n y = gen()\n println(y)\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git := func(root string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.org"}, args...)...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(gitRoot, "init", "-q", "-b", "main")
	git(gitRoot, "add", "-A")
	git(gitRoot, "commit", "-qm", "base")
	b := filepath.Join(base, "B")
	if out, err := exec.Command("git", "clone", "-q", gitRoot, b).CombinedOutput(); err != nil {
		t.Fatalf("clone B: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(b, ".git", "info", "exclude"), []byte("svc/.claude/\n"), 0644); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(b, "svc", ".claude", "worktrees", "x")
	if err := os.MkdirAll(filepath.Dir(a), 0755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "clone", "-q", gitRoot, a).CombinedOutput(); err != nil {
		t.Fatalf("clone hidden worktree A: %v\n%s", err, out)
	}
	withFinding := []byte("package projectA\nfunc gen() int { println(); return 0 }\nfunc f() {\n x := gen()\n x = gen()\n println(x)\n}\n")
	if err := os.WriteFile(filepath.Join(a, "svc", "p.go"), withFinding, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "svc", "p.go"), withFinding, 0644); err != nil {
		t.Fatal(err)
	}
	staticCache, goCache := filepath.Join(base, "staticcheck-cache"), filepath.Join(base, "go-cache")
	if err := os.MkdirAll(staticCache, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(goCache, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STATICCHECK_CACHE", staticCache)
	t.Setenv("GOCACHE", goCache)
	var warmOut bytes.Buffer
	if err := (&Runner{Root: a, Config: &config.Config{}, Stdout: &warmOut}).checkStaticcheck(context.Background(), "svc"); err == nil || !strings.Contains(warmOut.String(), "SA4006") {
		t.Fatalf("warm copy A did not cache the changed SA finding: %v; %q", err, warmOut.String())
	}
	baseEnv := append(os.Environ(), "GOFLAGS=-trimpath")
	baseRaw := exec.Command("staticcheck", "-checks", "SA*", "-f", "json", "./...")
	baseRaw.Dir, baseRaw.Env = filepath.Join(a, "svc"), baseEnv
	baseOutput, baseErr := baseRaw.Output()
	if baseErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(baseErr, &exitErr) || exitErr.ExitCode() != 1 {
			t.Fatalf("raw staticcheck A: %v", baseErr)
		}
	}
	if !bytes.Contains(baseOutput, []byte("SA4006")) {
		var stderr []byte
		if exitErr, ok := baseErr.(*exec.ExitError); ok {
			stderr = exitErr.Stderr
		}
		t.Fatalf("warm copy A did not produce the cache marker: output=%s err=%v stderr=%s", baseOutput, baseErr, stderr)
	}
	env := baseEnv
	raw := exec.Command("staticcheck", "-checks", "SA*", "-f", "json", "./...")
	raw.Dir, raw.Env = filepath.Join(b, "svc"), env
	output, err := raw.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Fatalf("raw staticcheck B: %v", err)
		}
	}
	if !bytes.Contains(output, []byte(filepath.Join(a, "svc", "cached", "c.go"))) {
		t.Fatalf("cache did not replay A path in B output: %s", output)
	}
	if !bytes.Contains(output, []byte(filepath.Join(a, "svc", "p.go"))) {
		t.Fatalf("cached finding on B's changed line did not replay A path: %s", output)
	}
	foreign, err := staticcheckHasForeignAbsolutePath(output, "svc", b)
	if err != nil || !foreign {
		t.Fatalf("hidden worktree cache path was classified as local: %s err=%v", output, err)
	}
	var out, stderr bytes.Buffer
	r := &Runner{Root: b, Config: &config.Config{}, Stdout: &out, Stderr: &stderr}
	if err := r.checkStaticcheck(context.Background(), "svc"); err == nil || !strings.Contains(out.String(), "svc/p.go:4: SA4006") {
		t.Fatalf("changed SA finding not retained: %v; %q", err, out.String())
	}
	if strings.Contains(stderr.String(), staticcheckRerunNotice) {
		t.Fatalf("translated cached finding unexpectedly reran: %q", stderr.String())
	}
	if err := exec.Command("git", "-C", b, "-c", "user.name=t", "-c", "user.email=t@example.org", "add", "svc/p.go").Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", b, "-c", "user.name=t", "-c", "user.email=t@example.org", "commit", "-qm", "add finding baseline").Run(); err != nil {
		t.Fatal(err)
	}
	// The SA4006 at line 4 is now unchanged relative to HEAD.
	if err := (&Runner{Root: b, Config: &config.Config{}}).checkStaticcheck(context.Background(), "svc"); err != nil {
		t.Fatalf("unchanged SA finding should pass: %v", err)
	}
}

func TestStaticcheckMissingBinary(t *testing.T) {
	root := staticcheckRepo(t)
	bin := t.TempDir()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(gitPath, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	r := &Runner{Root: root, Config: &config.Config{}}
	err = r.checkStaticcheck(context.Background(), ".")
	if err == nil || !strings.Contains(err.Error(), "go install honnef.co/go/tools/cmd/staticcheck@latest") {
		t.Fatalf("error = %v", err)
	}
}

func TestStaticcheckDisabledHasNoGateStep(t *testing.T) {
	root := staticcheckRepo(t)
	r := &Runner{Root: root, Config: &config.Config{GoVet: config.GoVet{Enabled: new(false)}, GoFmt: config.GoFmt{Enabled: new(false)}}}
	steps, err := r.gateSteps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.step.Source == StaticcheckSource {
			t.Fatal("staticcheck step present while disabled")
		}
	}
}
