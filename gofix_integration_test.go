package main

import (
	"path/filepath"
	"strings"
	"testing"
)

const fixGoMod = "module example.com/fixcase\n\ngo 1.26\n"

// fixDue has a loop go fix rewrites to a range over an int.
const fixDue = "package main\n\nfunc main() {\n\tfor i := 0; i < 3; i++ {\n\t\tprintln(i)\n\t}\n}\n"

// TestGoFixIsOffByDefault: a gate without go_fix leaves go fix's rewrites
// alone.
func TestGoFixIsOffByDefault(t *testing.T) {
	f := vetRepo(t, "", map[string]string{"go.mod": fixGoMod, "main.go": fixDue})
	if out := f.mustGraft("gate"); strings.Contains(out, "go fix") {
		t.Errorf("go fix ran:\n%s", out)
	}
}

// TestGoFixFailsTheGateWithItsDiff: with go_fix, rewrites go fix would make
// fail the gate after vet, with the diff and what to do, a Go upgrade named.
func TestGoFixFailsTheGateWithItsDiff(t *testing.T) {
	f := vetRepo(t, "go_fix: {enabled: true}\n", map[string]string{"go.mod": fixGoMod, "main.go": fixDue})
	out, code := f.graft("", "commit", "-m", "feat: x")
	if code == 0 {
		t.Fatalf("go fix's rewrites must fail the commit:\n%s", out)
	}
	for _, want := range []string{"gate 1/4: go vet ./...", "gate 2/4: go fix -diff ./...", "+\tfor i := range 3 {",
		`gate failed at "go fix -diff ./..."`, "run go fix ./... in the module, again until go fix -diff ./... is empty",
		"after a Go upgrade this step fails in every project"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if n := f.commitCount(); n != 0 {
		t.Errorf("%d commits made", n)
	}
	f.run(f.work, "go", "fix", "./...")
	f.mustGraft("commit", "-m", "feat: x")
}

// TestGoFixRunsInEveryModuleWithoutVet: go_fix takes the modules go vet
// would, and runs with vet turned off.
func TestGoFixRunsInEveryModuleWithoutVet(t *testing.T) {
	f := vetRepo(t, "go_vet: {enabled: false, reason: synthetic test project}\ngo_fix: {enabled: true}\n", map[string]string{
		"go.mod": fixGoMod, "main.go": vetBad,
		"tools/a/go.mod": fixGoMod, "tools/a/main.go": fixDue,
	})
	out, code := f.graft("", "gate")
	if code == 0 || strings.Contains(out, "go vet") || !strings.Contains(out, "gate 1/4: go fix -diff ./...\n") ||
		!strings.Contains(out, `gate failed at "go fix -diff ./... (tools/a)"`) {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

// TestBuiltinGoFixTrimpathShowsThisTreesPaths checks the same cached analyzer
// result across two copies of one module: the gate retries a reported diff
// without -trimpath so the displayed patch names this tree.
func TestBuiltinGoFixTrimpathShowsThisTreesPaths(t *testing.T) {
	skipGoToolchainInShort(t)
	f := vetRepo(t, "go_vet: {enabled: false, reason: synthetic test project}\ngo_fix: {enabled: true}\n", map[string]string{
		"go.mod":   fixGoMod,
		"pkg/p.go": "package pkg\n\nfunc F() { for i := 0; i < 3; i++ { println(i) } }\n",
	})
	second := vetCopy(t, f)
	f.env = append(f.env, "GOCACHE="+t.TempDir(), "GOFLAGS=")
	if out, code := f.graft("", "gate"); code == 0 || !strings.Contains(out, "pkg/p.go") {
		t.Fatalf("first copy's go fix did not report pkg/p.go (exit %d):\n%s", code, out)
	}
	firstPath := filepath.ToSlash(filepath.Join(f.work, "pkg/p.go"))
	firstRelative, err := filepath.Rel(second, filepath.Join(f.work, "pkg/p.go"))
	if err != nil {
		t.Fatal(err)
	}
	firstRelative = filepath.ToSlash(firstRelative)
	// Ask Go directly to expose the trimmed analyzer result before the gate
	// suppresses it. This also verifies that the second tree actually reuses
	// the first tree's cached result.
	trimmedOut, _ := f.exec(second, "", "go", "fix", "-trimpath", "-diff", "./...")
	if !strings.Contains(trimmedOut, firstPath) && !strings.Contains(trimmedOut, firstRelative) {
		t.Fatalf("trimmed go fix in second tree did not expose the first tree's cached path %q or %q:\n%s", firstPath, firstRelative, trimmedOut)
	}
	secondPath := filepath.ToSlash(filepath.Join(second, "pkg/p.go"))
	out, code := f.exec(second, "", graftBin, "gate")
	if code == 0 || !strings.Contains(out, secondPath) || strings.Contains(out, firstPath) || strings.Contains(out, firstRelative) {
		t.Fatalf("second copy's gate must show %q and neither %q nor %q (exit %d):\n%s", secondPath, firstPath, firstRelative, code, out)
	}
	if n := strings.Count(out, "graft: go fix -diff reported; rerun without -trimpath for this tree's paths"); n != 1 {
		t.Fatalf("rerun notice said %d times, want once:\n%s", n, out)
	}
}
