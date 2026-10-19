package main

import (
	"strings"
	"testing"
)

const vetGoMod = "module example.com/vetcase\n\ngo 1.21\n"

// vetBad has a Printf verb that does not fit its argument: only go vet finds it.
const vetBad = "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Printf(\"%d\\n\", \"x\") }\n"

const vetGood = "package main\n\nfunc main() {}\n"

func vetRepo(t *testing.T, config string, files map[string]string) *fixture {
	files[".graft.yaml"] = fileModeConfig + gateStep("ok") + config
	files["VERSION"] = "1.2.3\n"
	files["web/package.json"] = "{\n  \"version\": \"1.2.3\"\n}\n"
	files["app.yaml"] = "name: app\nversion: \"1.2.3\"\n"
	return newRepo(t, files, "origin")
}

func TestGoProjectGateRunsVetAndItsFailureFailsTheGate(t *testing.T) {
	f := vetRepo(t, "", map[string]string{"go.mod": vetGoMod, "main.go": vetBad})
	out, code := f.graft("", "commit", "-m", "feat: x")
	if code == 0 {
		t.Fatalf("vet failure must fail the commit:\n%s", out)
	}
	if !strings.Contains(out, "go vet ./...") || !strings.Contains(out, "gate failed") {
		t.Errorf("output:\n%s", out)
	}
	if n := f.commitCount(); n != 0 {
		t.Errorf("%d commits made", n)
	}
}

func TestGoProjectGatePassesCleanVetBeforeConfigSteps(t *testing.T) {
	f := vetRepo(t, "", map[string]string{"go.mod": vetGoMod, "main.go": vetGood})
	out := f.mustGraft("gate")
	vet, step := strings.Index(out, "gate 1/2: go vet ./..."), strings.Index(out, "gate 2/2:")
	if vet < 0 || step < vet {
		t.Errorf("vet must be the first of two steps:\n%s", out)
	}
	f.mustGraft("commit", "-m", "feat: x")
}

func TestGateWithoutGoModSkipsVet(t *testing.T) {
	f := vetRepo(t, "", map[string]string{"main.go": vetBad})
	out := f.mustGraft("gate")
	if strings.Contains(out, "go vet") {
		t.Errorf("a project without go.mod must not be vetted:\n%s", out)
	}
}

func TestGoVetCanBeTurnedOffWithAReason(t *testing.T) {
	f := vetRepo(t, "go_vet:\n  enabled: false\n  reason: synthetic test project\n", map[string]string{"go.mod": vetGoMod, "main.go": vetBad})
	out := f.mustGraft("gate")
	if strings.Contains(out, "go vet") {
		t.Errorf("go_vet.enabled: false must skip vet:\n%s", out)
	}
	f.mustGraft("commit", "-m", "feat: x")
}

func TestGoVetOffWithoutReasonIsRefused(t *testing.T) {
	f := vetRepo(t, "go_vet:\n  enabled: false\n", map[string]string{"go.mod": vetGoMod, "main.go": vetGood})
	out, code := f.graft("", "gate")
	if code == 0 || !strings.Contains(out, "go_vet.reason") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

func TestGateVetsAModuleBelowTheRootWithoutRootGoMod(t *testing.T) {
	f := vetRepo(t, "", map[string]string{"backend/go.mod": vetGoMod, "backend/main.go": vetBad})
	out, code := f.graft("", "commit", "-m", "feat: x")
	if code == 0 || !strings.Contains(out, "go vet ./... (backend)") || !strings.Contains(out, "gate failed") {
		t.Errorf("a failing vet in backend/ must fail the gate: exit %d\n%s", code, out)
	}
	if n := f.commitCount(); n != 0 {
		t.Errorf("%d commits made", n)
	}
}

func TestGateVetsEveryModuleAndPassesWhenClean(t *testing.T) {
	f := vetRepo(t, "", map[string]string{
		"go.mod": vetGoMod, "main.go": vetGood,
		"tools/a/go.mod": vetGoMod, "tools/a/main.go": vetGood,
		"deep/er/b/go.mod": vetGoMod, "deep/er/b/main.go": vetGood,
	})
	out := f.mustGraft("gate")
	for _, want := range []string{"gate 1/4: go vet ./...\n", "go vet ./... (deep/er/b)", "go vet ./... (tools/a)"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
}

func TestGateSkipsTestdataVendorAndNodeModulesModules(t *testing.T) {
	f := vetRepo(t, "", map[string]string{
		"testdata/go.mod": vetGoMod, "testdata/main.go": vetBad,
		"pkg/testdata/x/go.mod": vetGoMod, "pkg/testdata/x/main.go": vetBad,
		"vendor/v/go.mod": vetGoMod, "vendor/v/main.go": vetBad,
		"web/node_modules/m/go.mod": vetGoMod, "web/node_modules/m/main.go": vetBad,
	})
	out := f.mustGraft("gate")
	if strings.Contains(out, "go vet") {
		t.Errorf("modules under testdata, vendor, node_modules must not be vetted:\n%s", out)
	}
}

func TestGoVetExcludeNeedsAReasonAndSkipsTheDirectory(t *testing.T) {
	files := func() map[string]string {
		return map[string]string{"backend/go.mod": vetGoMod, "backend/main.go": vetBad, "tools/go.mod": vetGoMod, "tools/main.go": vetGood}
	}
	f := vetRepo(t, "go_vet:\n  exclude: [backend]\n", files())
	if out, code := f.graft("", "gate"); code == 0 || !strings.Contains(out, "go_vet.reason") {
		t.Errorf("exclude without a reason: exit %d\n%s", code, out)
	}
	f = vetRepo(t, "go_vet:\n  exclude: [backend]\n  reason: generated code\n", files())
	out := f.mustGraft("gate")
	if strings.Contains(out, "(backend)") || !strings.Contains(out, "go vet ./... (tools)") {
		t.Errorf("only tools/ must be vetted:\n%s", out)
	}
}

func TestGateVetsOnlyTheModulesOfGoWork(t *testing.T) {
	f := vetRepo(t, "", map[string]string{
		"go.work":        "go 1.21\n\nuse ./app\n",
		"app/go.mod":     vetGoMod,
		"app/main.go":    vetGood,
		"legacy/go.mod":  vetGoMod,
		"legacy/main.go": vetBad,
	})
	out := f.mustGraft("gate")
	if !strings.Contains(out, "go vet ./... (app)") || strings.Contains(out, "(legacy)") {
		t.Errorf("only app, the module of go.work use, must be vetted:\n%s", out)
	}
	f.mustGraft("commit", "-m", "feat: x")
}

func TestGateWithGoWorkStillFailsOnVetOfAUsedModule(t *testing.T) {
	f := vetRepo(t, "", map[string]string{
		"go.work":     "go 1.21\n\nuse (\n\t.\n\t./app\n)\n",
		"go.mod":      vetGoMod,
		"main.go":     vetGood,
		"app/go.mod":  "module example.com/vetcase/app\n\ngo 1.21\n",
		"app/main.go": vetBad,
	})
	out, code := f.graft("", "gate")
	if code == 0 || !strings.Contains(out, "go vet ./... (app)") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

func TestGateVetsAGoWorkModuleWithASpaceInItsPath(t *testing.T) {
	f := vetRepo(t, "", map[string]string{
		"go.work":        "go 1.21\n\nuse \"./my app\"\n",
		"my app/go.mod":  vetGoMod,
		"my app/main.go": vetGood,
		"legacy/go.mod":  vetGoMod,
		"legacy/main.go": vetBad,
	})
	out := f.mustGraft("gate")
	if !strings.Contains(out, "go vet ./... (my app)") || strings.Contains(out, "(legacy)") {
		t.Errorf("only my app, the module of go.work use, must be vetted:\n%s", out)
	}
}

// TestGoVetEnvGivesTheBuildFlags: a file behind a build tag is vetted only
// when go_vet.env gives the tag.
func TestGoVetEnvGivesTheBuildFlags(t *testing.T) {
	files := func() map[string]string {
		return map[string]string{"go.mod": vetGoMod, "main.go": vetGood, "tagged.go": "//go:build special\n\n" + strings.Replace(vetBad, "func main()", "func init()", 1)}
	}
	f := vetRepo(t, "", files())
	f.mustGraft("gate")
	f = vetRepo(t, "go_vet:\n  env: {GOFLAGS: -tags=special}\n", files())
	if out, code := f.graft("", "gate"); code == 0 || !strings.Contains(out, "tagged.go") {
		t.Errorf("the tagged file is not vetted with go_vet.env: exit %d\n%s", code, out)
	}
}
