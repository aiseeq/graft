package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDeployIntegrations: a release goes out only where every integration the
// target from requires runs is configured or excused with a reason; a command
// that asks the uploaded build is the deploy script's to call, and graft
// records the deploy only when that call passed.
func TestDeployIntegrations(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("deploy scripts use sh")
	}
	state := t.TempDir()
	// The stand-in ssh runs the command here, with the host it was sent to.
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "ssh"), `#!/bin/sh
for a in "$@"; do host=$line; line=$a; done
FAKE_HOST=$host exec sh -c "$line"
`)
	if err := os.Chmod(filepath.Join(bin, "ssh"), 0o755); err != nil {
		t.Fatal(err)
	}
	list := func(name string) string { return filepath.Join(state, name) }
	f := newRepo(t, map[string]string{
		".graft.yaml": `schema: 1
version: {mode: file}
envs:
  uat: {ssh: {host: uat}}
  prod: {ssh: {host: prod}}
deploy:
  remote: origin
  targets:
    uat:
      env: uat
      run: [sh, deploy.sh]
      version: 'cat ` + list("uat-version") + `'
    prod:
      env: prod
      requires: uat
      run: [sh, deploy.sh]
      integrations: {command: 'cat ` + state + `/$FAKE_HOST-list', exceptions: integration-exceptions.conf}
    staged:
      env: prod
      requires: uat
      run: [sh, deploy.sh]
      deployed_sha: {path: ` + yamlQuote(list("staged-sha")) + `}
      integrations: {command: 'cat {staged}', running: ` + yamlQuote(list("uat-list")) + `}
`,
		"VERSION":                     "1.0.0\n",
		"integration-exceptions.conf": "# integration|reason\n",
		"deploy.sh": `set -e
echo "deploy $GRAFT_DEPLOY_TARGET" >> ` + list("runs") + `
if [ "$GRAFT_DEPLOY_TARGET" = staged ] && [ ! -e ` + list("skip-check") + ` ]; then
  graft deploy check-integrations ` + list("staged-build") + `
fi
`,
	}, "origin")
	for _, kv := range testEnv {
		if p, ok := strings.CutPrefix(kv, "PATH="); ok {
			f.env = append(f.env, "PATH="+bin+string(os.PathListSeparator)+p)
		}
	}
	f.mustGraft("commit", "-m", "feat: base")
	writeFile(t, list("uat-version"), "1.0.1\n")
	writeFile(t, list("uat-list"), "alpha\nbeta\n")
	writeFile(t, list("prod-list"), "alpha\n")
	exceptions := func(content string) {
		f.write("integration-exceptions.conf", content)
		f.mustGraft("commit", "-m", "chore: integration exceptions")
		writeFile(t, list("uat-version"), f.read("VERSION"))
	}
	runs := func() string {
		b, _ := os.ReadFile(list("runs"))
		_ = os.Remove(list("runs"))
		return string(b)
	}

	out, code := f.graft("", "deploy", "prod")
	if code == 0 || !strings.Contains(out, "uat runs beta, which prod does not configure: set it up on prod or list it in integration-exceptions.conf with a reason") || runs() != "" {
		t.Errorf("a missing integration: exit %d\n%s", code, out)
	}

	exceptions("beta\n")
	if out, code := f.graft("", "deploy", "prod"); code == 0 || !strings.Contains(out, `integration-exceptions.conf:1: expected "integration|reason", got "beta"`) {
		t.Errorf("an exception without its reason: exit %d\n%s", code, out)
	}

	exceptions("beta|not sold there yet\nalpha|old line\n")
	out = f.mustGraft("deploy", "prod")
	if !strings.Contains(out, "uat runs beta, prod leaves it off: not sold there yet") || !strings.Contains(out, "the exception for alpha in integration-exceptions.conf is not needed any more") || runs() != "deploy prod\n" {
		t.Errorf("an excused integration:\n%s", out)
	}

	// The uploaded build lacks beta: the script's check stops it.
	writeFile(t, list("staged-build"), "alpha\n")
	out, code = f.graft("", "deploy", "staged")
	if code == 0 || !strings.Contains(out, "uat runs beta, which staged does not configure") || !strings.Contains(out, "the deploy script failed") {
		t.Errorf("the staged build lacks an integration: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(list("staged-sha")); !os.IsNotExist(err) {
		t.Errorf("a failed check recorded the deploy: %v", err)
	}

	// A script that never calls the check is not recorded either.
	writeFile(t, list("staged-build"), "alpha\nbeta\ngamma\n")
	writeFile(t, list("skip-check"), "")
	out, code = f.graft("", "deploy", "staged")
	if code == 0 || !strings.Contains(out, "it never passed graft deploy check-integrations") {
		t.Errorf("no check: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(list("staged-sha")); !os.IsNotExist(err) {
		t.Errorf("an unchecked deploy was recorded: %v", err)
	}

	if err := os.Remove(list("skip-check")); err != nil {
		t.Fatal(err)
	}
	out = f.mustGraft("deploy", "staged")
	if !strings.Contains(out, "staged configures every integration uat runs (staged: alpha, beta, gamma)") {
		t.Errorf("a checked deploy:\n%s", out)
	}
	if _, err := os.Stat(list("staged-sha")); err != nil {
		t.Errorf("a checked deploy is not recorded: %v", err)
	}

	if out, code := f.graft("", "deploy", "check-integrations", "x"); code == 0 || !strings.Contains(out, "check-integrations runs inside graft deploy") {
		t.Errorf("outside graft deploy: exit %d\n%s", code, out)
	}
}
