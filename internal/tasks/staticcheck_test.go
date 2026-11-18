package tasks

import (
	"bytes"
	"context"
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
	t.Setenv("PATH", bin)
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
