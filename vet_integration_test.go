package main

import (
	"io/fs"
	"os"
	"path/filepath"
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

// TestBuiltinVetTrimpathShowsThisTreesPaths is the measurement the change
// stands on: two copies of one revision share a Go build cache, and go vet
// -trimpath in the second replays what the first cached, with
// ../<first>/pkg/p.go for its path. The gate of the second copy fails on its
// own path: the trimmed run reported, vet ran again without -trimpath.
func TestBuiltinVetTrimpathShowsThisTreesPaths(t *testing.T) {
	skipGoToolchainInShort(t)
	f := vetRepo(t, "", map[string]string{
		"go.mod":   "module example.com/projecta\n\ngo 1.26\n",
		"pkg/p.go": "package pkg\n\nimport \"fmt\"\n\nfunc F() { fmt.Printf(\"%d\", \"bad\") }\n",
	})
	second := vetCopy(t, f)
	f.env = append(f.env, "GOCACHE="+t.TempDir(), "GOFLAGS=")
	if out, code := f.graft("", "gate"); code == 0 || !strings.Contains(out, "pkg/p.go:5:") {
		t.Fatalf("first copy's vet did not fail on pkg/p.go (exit %d):\n%s", code, out)
	}
	// The scenario holds: go vet -trimpath in the second copy replays the
	// first copy's finding, with the first copy's path.
	first := "../" + filepath.Base(f.work) + "/"
	if out, code := f.exec(second, "", "go", "vet", "-trimpath", "./..."); code == 0 || !strings.Contains(out, first+"pkg/p.go:5:") {
		t.Fatalf("go vet -trimpath in the second copy did not show %spkg/p.go (exit %d):\n%s", first, code, out)
	}
	out, code := f.exec(second, "", graftBin, "gate")
	if code == 0 || !strings.Contains(out, "pkg/p.go:5:") || strings.Contains(out, first) {
		t.Fatalf("second copy's gate must fail on its own pkg/p.go, not %spkg/p.go (exit %d):\n%s", first, code, out)
	}
	if n := strings.Count(out, "graft: go vet reported; rerun without -trimpath for this tree's paths"); n != 1 {
		t.Fatalf("rerun notice said %d times, want once:\n%s", n, out)
	}
}

// TestBuiltinVetTrimpathSharesTheCache: a clean second copy of a revision
// adds next to nothing to the build cache its first copy filled, unless
// GOFLAGS turns -trimpath off, which graft keeps.
func TestBuiltinVetTrimpathSharesTheCache(t *testing.T) {
	skipGoToolchainInShort(t)
	growth := func(goflags string) int64 {
		f := vetRepo(t, "", map[string]string{
			"go.mod":        "module example.com/projecta\n\ngo 1.26\n",
			"pkg/p.go":      "package pkg\n\nimport \"fmt\"\n\nfunc F() string { return fmt.Sprint(1) }\n",
			"cmd/a/main.go": "package main\n\nimport (\n\t\"fmt\"\n\n\t\"example.com/projecta/pkg\"\n)\n\nfunc main() { fmt.Println(pkg.F()) }\n",
		})
		second := vetCopy(t, f)
		cache := t.TempDir()
		f.env = append(f.env, "GOCACHE="+cache, "GOFLAGS="+goflags)
		if out, code := f.graft("", "gate"); code != 0 {
			t.Fatalf("clean first copy, GOFLAGS=%q: exit %d\n%s", goflags, code, out)
		}
		before := vetCacheBytes(t, cache)
		out, code := f.exec(second, "", graftBin, "gate")
		if code != 0 {
			t.Fatalf("clean second copy, GOFLAGS=%q: exit %d\n%s", goflags, code, out)
		}
		if strings.Contains(out, "rerun without -trimpath") {
			t.Errorf("a clean vet was run again:\n%s", out)
		}
		return vetCacheBytes(t, cache) - before
	}
	trimmed, untrimmed := growth(""), growth("-trimpath=false")
	t.Logf("GOCACHE growth of the second copy's gate: %d bytes with -trimpath, %d bytes with GOFLAGS=-trimpath=false", trimmed, untrimmed)
	if trimmed >= 5<<20 {
		t.Errorf("the clean second copy grew the shared GOCACHE by %d bytes, want < 5 MiB", trimmed)
	}
	if untrimmed <= trimmed {
		t.Errorf("GOFLAGS=-trimpath=false grew the cache by %d bytes, no more than -trimpath's %d: was -trimpath added?", untrimmed, trimmed)
	}
}

// vetCopy copies f's work tree, .git included, next to it.
func vetCopy(t *testing.T, f *fixture) string {
	t.Helper()
	second := filepath.Join(f.base, "copy2")
	if err := os.CopyFS(second, os.DirFS(f.work)); err != nil {
		t.Fatalf("copying the project: %v", err)
	}
	return second
}

func vetCacheBytes(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err == nil {
			total += info.Size()
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

func TestGoProjectGatePassesCleanVetBeforeConfigSteps(t *testing.T) {
	f := vetRepo(t, "", map[string]string{"go.mod": vetGoMod, "main.go": vetGood})
	out := f.mustGraft("gate")
	vet, step := strings.Index(out, "gate 1/3: go vet ./..."), strings.Index(out, "gate 3/3:")
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
	for _, want := range []string{"gate 1/5: go vet ./...\n", "go vet ./... (deep/er/b)", "go vet ./... (tools/a)"} {
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
