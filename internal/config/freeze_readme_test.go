package config

import (
	"os"
	"strings"
	"testing"
)

// freezeTestConfig is a project whose prod target holds the freeze line given
// and whose smoke gate step depends on nested.
func freezeTestConfig(freeze string) []byte {
	return []byte(`schema: 1
version: {mode: file}
envs:
  local: {}
tasks:
  lint: {run: [[sh, lint.sh]]}
  nested: {run: [[sh, nested.sh]]}
  smoke: {deps: [nested], run: [[sh, smoke.sh]]}
gate:
  - task: smoke
  - task: lint
deploy:
  remote: origin
  targets:
    acc:
      env: local
      run: [sh, deploy.sh]
      version: 'cat acc-version'
      deployed_sha: {path: acc-sha}
    prod:
      env: local
      run: [sh, deploy.sh]
      deps: [nested]
      requires: acc
      version: 'cat prod-version'
      deployed_sha: {path: prod-sha}
      approved_release: {prefix: "Product "}
      ` + freeze + `
release_notes:
  jira:
    project_keys: [PROJ]
`)
}

func TestReadmeFreezeExampleParses(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	var example string
	for line := range strings.Lines(string(readme)) {
		if strings.HasPrefix(line, "freeze: {") {
			example = strings.TrimSpace(line)
			break
		}
	}
	if example == "" {
		t.Fatal("README has no line starting with freeze: {")
	}
	if _, err := Parse(freezeTestConfig(example)); err != nil {
		t.Fatalf("README example %q: %v", example, err)
	}
}

func TestFreezeAdvisoryNestedSaysWhatToDo(t *testing.T) {
	_, err := Parse(freezeTestConfig(`freeze: {types: [Bug], advisory: [nested]}`))
	if err == nil {
		t.Fatal("an advisory task nested in a gate step was accepted")
	}
	for _, want := range []string{`"nested" cannot be a dependency of "smoke"`, `take it out of "smoke"`, "gate step"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
}
