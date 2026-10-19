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
