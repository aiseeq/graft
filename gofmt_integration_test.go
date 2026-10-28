package main

import (
	"strings"
	"testing"
)

// fmtBad is a file gofmt would change.
const fmtBad = "package main\n\nfunc main()  {}\n"

// TestGofmtFailsTheGateWithTheFiles: after the vet steps the gate checks that
// every Go file git lists in the modules is as gofmt leaves it, and fails
// with the files and what to do. Files under testdata and ignored files are
// not checked; line endings are not formatting.
func TestGofmtFailsTheGateWithTheFiles(t *testing.T) {
	f := vetRepo(t, "", map[string]string{
		"go.mod": vetGoMod, "main.go": fmtBad,
		"crlf.go":            "package main\r\n\r\nfunc f() {}\r\n",
		"testdata/bad.go":    fmtBad,
		".gitignore":         "gen/\n",
		"gen/x.go":           fmtBad,
		"tools/a/go.mod":     vetGoMod,
		"tools/a/x.go":       fmtBad,
		"tools/a/formed.go":  "package main\n\nfunc g() {}\n",
		"notes/plain.go.txt": fmtBad,
	})
	out, code := f.graft("", "commit", "-m", "feat: x")
	if code == 0 {
		t.Fatalf("unformatted files must fail the commit:\n%s", out)
	}
	for _, want := range []string{"gate 1/4: go vet ./...", "gate 2/4: go vet ./... (tools/a)", "gate 3/4: gofmt -l",
		`gate failed at "gofmt -l"`, "main.go", "tools/a/x.go", "gofmt -w"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"testdata/bad.go", "gen/x.go", "crlf.go", "formed.go", "plain.go.txt"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("%s listed:\n%s", unwanted, out)
		}
	}
	if n := f.commitCount(); n != 0 {
		t.Errorf("%d commits made", n)
	}
	f.run(f.work, "gofmt", "-w", "main.go", "tools/a/x.go")
	f.mustGraft("commit", "-m", "feat: x")
}

// TestGofmtRunsWithoutVetAndSkipsExcludedModules: the check takes the
// modules go vet would, go_vet.exclude included, and runs with vet off.
func TestGofmtRunsWithoutVetAndSkipsExcludedModules(t *testing.T) {
	f := vetRepo(t, "go_vet: {enabled: false, reason: synthetic test project}\n", map[string]string{
		"go.mod": vetGoMod, "main.go": fmtBad,
	})
	out, code := f.graft("", "gate")
	if code == 0 || strings.Contains(out, "go vet") || !strings.Contains(out, "gate 1/2: gofmt -l\n") || !strings.Contains(out, "main.go") {
		t.Errorf("exit %d:\n%s", code, out)
	}

	f = vetRepo(t, "go_vet: {exclude: [tools/a], reason: generated code}\n", map[string]string{
		"go.mod": vetGoMod, "main.go": vetGood,
		"tools/a/go.mod": vetGoMod, "tools/a/x.go": fmtBad,
	})
	if out := f.mustGraft("gate"); !strings.Contains(out, "gofmt -l") || strings.Contains(out, "x.go") {
		t.Errorf("an excluded module checked, or no check:\n%s", out)
	}
}

// TestGofmtCanBeTurnedOffWithAReason: go_fmt.enabled: false needs a reason,
// as go_vet's does; a project without go.mod has no check.
func TestGofmtCanBeTurnedOffWithAReason(t *testing.T) {
	f := vetRepo(t, "go_fmt: {enabled: false, reason: synthetic test project}\n", map[string]string{"go.mod": vetGoMod, "main.go": fmtBad})
	if out := f.mustGraft("gate"); strings.Contains(out, "gofmt") {
		t.Errorf("go_fmt.enabled: false must skip the check:\n%s", out)
	}
	f = vetRepo(t, "go_fmt: {enabled: false}\n", map[string]string{"go.mod": vetGoMod, "main.go": vetGood})
	if out, code := f.graft("", "gate"); code == 0 || !strings.Contains(out, "go_fmt.reason") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	f = vetRepo(t, "", map[string]string{"main.go": fmtBad})
	if out := f.mustGraft("gate"); strings.Contains(out, "gofmt") {
		t.Errorf("a project without go.mod must not be checked:\n%s", out)
	}
}
