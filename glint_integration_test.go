package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// glintRepo is a deploy fixture whose test target holds its moves for glint
// findings of low and up, with a glint on PATH that reports the number in
// the file found of the state, or no report without one, and logs where and
// how it ran.
func glintRepo(t *testing.T) *deployFixture {
	t.Helper()
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		c := strings.Replace(deployConfig(state, q), "release_notes: {transitions: [{from: [In Progress], to: Testing}]}\n      status:", "release_notes: {transitions: [{from: [In Progress], to: Testing}], glint: low}\n      status:", 1)
		return strings.Replace(c, "{from: [Ready for release], to: Done}], subtasks: true}", "{from: [Ready for release], to: Done}], subtasks: true, sweep: {apply: true}, glint: low}", 1)
	})
	bin := t.TempDir()
	// With the file panicked a rule panicked: the report is whole, the
	// findings of that rule missing.
	writeFile(t, filepath.Join(bin, "glint"), `#!/bin/sh
echo "$PWD $*" >> "`+filepath.Join(d.state, "glint-runs")+`"
f="`+filepath.Join(d.state, "found")+`"
[ -e "$f" ] || { echo "no config" >&2; exit 3; }
if [ -e "`+filepath.Join(d.state, "panicked")+`" ]; then
  echo "{\"summary\": {\"total\": 0}, \"stats\": {\"rulesPanicked\": [\"long-function\"]}}"
  exit 1
fi
echo "{\"summary\": {\"total\": $(cat "$f")}}"
exit 1
`)
	if err := os.Chmod(filepath.Join(bin, "glint"), 0o755); err != nil {
		t.Fatal(err)
	}
	d.env = append(d.env, "PATH="+bin+string(os.PathListSeparator)+binDir+string(os.PathListSeparator)+os.Getenv("PATH"), "XDG_CACHE_HOME="+t.TempDir())
	d.mustGraft("deploy", "test")
	return d
}

func (d *deployFixture) moved() []string {
	_, moved := d.jira.snapshot()
	return moved
}

// TestDeployGlintHoldsItems: with release_notes.glint an item moves only
// when glint finds nothing in the deployed commit, whatever glint's exit
// status; graft deploy handover checks the deployed commit again, in a work
// tree of its own when the work tree is elsewhere, and --glint-ticket names
// an open item that takes the findings.
func TestDeployGlintHoldsItems(t *testing.T) {
	d := glintRepo(t)
	writeFile(t, filepath.Join(d.state, "found"), "2\n")
	d.write("a.txt", "a\n")
	d.mustGraft("commit", "-m", "feat: PROJ-1 the form")
	sha := strings.TrimSpace(d.git("rev-parse", "--short=12", "HEAD"))
	out := d.mustGraft("deploy", "test")
	if want := "graft: PROJ-1: NOT MOVED to Testing: glint: 2 findings of low and above in " + sha + " (release_notes.glint); once glint finds nothing in the deployed commit, the next deploy to test or graft deploy handover test moves it"; !strings.Contains(out, want) || len(d.moved()) != 0 {
		t.Errorf("moved %v, want none and %q:\n%s", d.moved(), want, out)
	}
	if runs := readFile(t, filepath.Join(d.state, "glint-runs")); runs != d.work+" check . -s low -o json\n" {
		t.Errorf("glint ran: %q", runs)
	}

	// The work tree moved on: handover checks the deployed commit apart.
	d.write("b.txt", "b\n")
	if out, code := d.graft("", "deploy", "handover", "test"); code == 0 || !strings.Contains(out, "graft: PROJ-1: NOT MOVED to Testing: glint: 2 findings") || !strings.Contains(out, "1 of test are still held") {
		t.Errorf("handover with findings: exit %d\n%s", code, out)
	}
	runs := strings.Split(strings.TrimSpace(readFile(t, filepath.Join(d.state, "glint-runs"))), "\n")
	if len(runs) != 2 || strings.HasPrefix(runs[1], d.work+" ") {
		t.Errorf("handover ran glint in the work tree: %q", runs)
	}
	if out, code := d.graft("", "deploy", "handover", "test", "--glint-ticket", "PROJ-2"); code == 0 || !strings.Contains(out, "--glint-ticket PROJ-2 is Done: the findings need an item still to be done") {
		t.Errorf("a done ticket: exit %d\n%s", code, out)
	}
	if out, code := d.graft("", "deploy", "handover", "test", "--glint-ticket", "OTHER-1"); code == 0 || !strings.Contains(out, `"OTHER-1" is not an item of release_notes.jira.project_keys`) {
		t.Errorf("a ticket of another project: exit %d\n%s", code, out)
	}
	out = d.mustGraft("deploy", "handover", "test", "--glint-ticket", "PROJ-3")
	if want := "PROJ-1 moved to Testing with 2 glint findings in " + sha + ": PROJ-3 takes them (--glint-ticket)"; !strings.Contains(out, want) || !slices.Equal(d.moved(), []string{"PROJ-1"}) {
		t.Errorf("moved %v, want PROJ-1 and %q:\n%s", d.moved(), want, out)
	}

	// Clean, the next item moves; glint without a report holds it.
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-8"], d.jira.statuses["PROJ-9"] = "In Progress", "In Progress"
	d.jira.mu.Unlock()
	writeFile(t, filepath.Join(d.state, "found"), "0\n")
	d.mustGraft("commit", "-m", "feat: PROJ-8 the list")
	if out := d.mustGraft("deploy", "test"); !strings.Contains(out, "PROJ-8 moved to Testing\n") {
		t.Errorf("clean:\n%s", out)
	}
	if err := os.Remove(filepath.Join(d.state, "found")); err != nil {
		t.Fatal(err)
	}
	d.write("c.txt", "c\n")
	d.mustGraft("commit", "-m", "feat: PROJ-9 the menu")
	if out := d.mustGraft("deploy", "test"); !strings.Contains(out, "graft: PROJ-9: NOT MOVED to Testing: glint: glint check gave no report") || !strings.Contains(out, "no config") {
		t.Errorf("no report:\n%s", out)
	}
	if out, code := d.graft("", "deploy", "--glint-ticket", "PROJ-3", "uat"); code != 1 || !strings.Contains(out, "deploy.targets.uat.release_notes.glint is not set") {
		t.Errorf("--glint-ticket on a target without glint: exit %d\n%s", code, out)
	}
	// A ticket that cannot take the findings refuses the deploy before it.
	before := d.stateFile("test-run")
	d.write("d.txt", "d\n")
	d.mustGraft("commit", "-m", "feat: PROJ-9 more")
	if out, code := d.graft("", "deploy", "--glint-ticket", "PROJ-2", "test"); code == 0 || !strings.Contains(out, "--glint-ticket PROJ-2 is Done") || d.stateFile("test-run") != before {
		t.Errorf("a done ticket on the deploy: exit %d\n%s", code, out)
	}
	writeFile(t, filepath.Join(d.state, "found"), "0\n")
	writeFile(t, filepath.Join(d.state, "panicked"), "")
	if out := d.mustGraft("deploy", "test"); !strings.Contains(out, "graft: PROJ-9: NOT MOVED to Testing: glint: glint rules panicked: long-function") {
		t.Errorf("a panicked rule:\n%s", out)
	}
}

// TestSweepGlintHoldsItems: the sweep moves nothing to its done status
// while glint finds anything in the commit the target runs, listing the
// items instead.
func TestSweepGlintHoldsItems(t *testing.T) {
	d := glintRepo(t)
	d.git("remote", "set-head", "origin", d.git("branch", "--show-current"))
	d.mustGraft("deploy", "sub")
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-70"] = "Ready for release"
	d.jira.mu.Unlock()
	writeFile(t, filepath.Join(d.state, "found"), "3\n")
	d.write("a.txt", "a\n")
	d.mustGraft("commit", "-m", "feat: PROJ-70 the report")
	out := d.mustGraft("deploy", "sub")
	for _, want := range []string{
		"graft: PROJ-70: NOT MOVED to Done: glint: 3 findings of low and above",
		"PROJ-70 stays Ready for release though its work of the last 30 days is on sub (commits: 1): glint: 3 findings of low and above",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}
	if len(d.moved()) != 0 {
		t.Errorf("moved %v:\n%s", d.moved(), out)
	}
}
