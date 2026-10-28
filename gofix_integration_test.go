package main

import (
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
