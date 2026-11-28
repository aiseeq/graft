package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lineWith tells whether one line of out holds every part: a refusal and the
// task it names, not the two anywhere in the output.
func lineWith(out string, parts ...string) bool {
	for line := range strings.SplitSeq(out, "\n") {
		all := true
		for _, part := range parts {
			if !strings.Contains(line, part) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// windowGateConfig is prod with a window, its release version and an
// approved release that needs an approved Product version.
func windowGateConfig(state string, q func(string) string) string {
	return strings.Replace(versionConfig(state, q),
		"approved_release: {statuses: [Ready for release, Done], ignore_paths: [\"docs/**\"]}",
		`approved_release: {prefix: "Product", statuses: [Ready for release, Done], ignore_paths: ["docs/**"], version: {match: '^Product'}}`, 1)
}

func windowGateFixture(t *testing.T) *deployFixture {
	t.Helper()
	d := newDeployFixture(t, windowGateConfig)
	d.at("2026-10-22 15:30")
	prev := d.git("rev-parse", "HEAD")
	writeFile(t, filepath.Join(d.state, "prod-sha"), prev+"\n")
	writeFile(t, filepath.Join(d.state, "prod-version"), "1.0.1\n")
	writeFile(t, filepath.Join(d.state, "test-sha"), prev+"\n")
	writeFile(t, filepath.Join(d.state, "test-version"), "1.0.1\n")
	return d
}

// A release waiting for its approval goes out by exception with
// --unapproved; the items it ships by exception are the release's, so the
// window version that lists them refuses nothing. deploy check names the
// approval, and of the version's items only PROJ-3, which no commit of the
// release delivers; --unapproved does not check the version contents.
func TestDeployUnapprovedPendingApprovalNoFalseVersionContents(t *testing.T) {
	d := windowGateFixture(t)
	d.jira.setVersions([]map[string]any{{"id": "window", "name": "Product 22.10.26"}}, map[string][]string{
		"PROJ-1": {"window"}, "PROJ-2": {"window"}, "PROJ-3": {"window"},
	})
	d.jira.setApprovers(map[string][]string{"window": {"PENDING"}})
	d.jira.mu.Lock()
	d.jira.statuses["PROJ-1"] = "Ready for release"
	d.jira.statuses["PROJ-2"] = "Ready for release"
	d.jira.statuses["PROJ-3"] = "Ready for release"
	d.jira.manageVersions = true
	d.jira.mu.Unlock()
	d.write("one.txt", "synthetic task one\n")
	d.mustPlainCommit("feat: PROJ-1 synthetic work")
	d.write("two.txt", "synthetic task two\n")
	d.mustPlainCommit("feat: PROJ-2 synthetic work")
	d.git("push", "-q", "-u", "origin", "HEAD")
	head := d.git("rev-parse", "HEAD")

	out, code := d.graft("", "deploy", "check", "prod")
	if code == 0 || !lineWith(out, "version-approval:", "Product 22.10.26") {
		t.Fatalf("deploy check does not name the pending approval: exit %d\n%s", code, out)
	}
	if !lineWith(out, "lists PROJ-3, which this release does not take out") {
		t.Fatalf("deploy check does not name the only item the release leaves behind:\n%s", out)
	}

	out, code = d.graft("", "deploy", "--unapproved", "prod")
	if code != 0 || !strings.Contains(out, "shipped by exception (--unapproved)") || !strings.Contains(out, "deployed "+head[:12]+" to prod") {
		t.Fatalf("--unapproved does not ship the pending release: exit %d\n%s", code, out)
	}
	if strings.Contains(out, "does not take out") {
		t.Fatalf("--unapproved refuses the items it ships by exception:\n%s", out)
	}
}

// A target with confirm that already runs the commit: deploy check names the
// same-commit refusal and what graft deploy --redeploy would refuse next.
func TestDeployCheckSameCommitNamesRedeployBlockers(t *testing.T) {
	cfg := func(state string, q func(string) string) string {
		return strings.Replace(versionGateConfig(state, q),
			"      approved_release: {statuses: [Ready for release, Done]}",
			"      confirm: sudo\n      approved_release: {statuses: [Ready for release, Done], version: {match: '^Product'}}", 1)
	}
	d := newDeployFixture(t, cfg)
	prev := d.git("rev-parse", "HEAD")
	writeFile(t, filepath.Join(d.state, "gate-sha"), prev+"\n")
	d.git("switch", "-q", "-c", "withheld-work")
	d.write("withheld.txt", "synthetic withheld work\n")
	d.mustPlainCommit("feat: PROJ-1 withheld synthetic work")
	w := d.git("rev-parse", "HEAD")
	d.git("switch", "-q", "-c", "deploy-work", prev)
	d.git("push", "-q", "-u", "origin", "HEAD")
	writeFile(t, filepath.Join(d.state, "gate-sha")+".withheld", w+"\n")
	d.jira.setVersions([]map[string]any{{"id": "target", "name": "Product 1.0"}}, map[string][]string{"PROJ-1": {"target"}})
	d.jira.setApprovers(map[string][]string{"target": {"APPROVED"}})
	d.setStatus("PROJ-1", "In Progress")

	out, code := d.graft("", "deploy", "check", "prod")
	if code == 0 || !strings.Contains(out, "prod already runs "+prev[:12]+"; graft deploy --redeploy prod deploys it again") {
		t.Fatalf("missing the same-commit refusal: exit %d\n%s", code, out)
	}
	if !lineWith(out, "version-top:", "PROJ-1 is In Progress") {
		t.Fatalf("deploy check hides what --redeploy would refuse:\n%s", out)
	}
}

// A Jira read that fails for one commit refuses that commit and leaves the
// others checked, in deploy check and in the deploy.
func TestDeployJiraReadFailureOfOneCommitKeepsTheOthersChecked(t *testing.T) {
	for _, tc := range []struct {
		name, gate string
		want       []string
	}{
		// Without version, commit-items refuses PROJ-5 by its status.
		{"statuses", "approved_release: {statuses: [Ready for release, Done]}", []string{"feat: PROJ-5 synthetic work: PROJ-5"}},
		// With it, version-top does, though no version can be selected
		// without PROJ-9.
		{"version", "approved_release: {statuses: [Ready for release, Done], version: {match: '^Product'}}", []string{
			"version-top: PROJ-5 is Testing",
			"not checked: version-select, the version membership of version-top",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDeployFixture(t, func(state string, q func(string) string) string {
				return strings.Replace(deployConfig(state, q), "      approved_release: {statuses: [Ready for release, Done]}", "      "+tc.gate, 1)
			})
			writeFile(t, filepath.Join(d.state, "gate-sha"), d.git("rev-parse", "HEAD")+"\n")
			d.jira.mu.Lock()
			d.jira.failIssues = map[string]int{"PROJ-9": 500}
			d.jira.mu.Unlock()
			d.write("nine.txt", "synthetic unreadable work\n")
			d.mustPlainCommit("feat: PROJ-9 synthetic work")
			d.write("five.txt", "synthetic testing work\n")
			d.mustPlainCommit("feat: PROJ-5 synthetic work")
			d.git("push", "-q", "origin", "HEAD")
			for _, args := range [][]string{{"deploy", "check", "gate"}, {"deploy", "gate"}} {
				out, code := d.graft("", args...)
				if code == 0 {
					t.Fatalf("%v unexpectedly passed:\n%s", args, out)
				}
				if !lineWith(out, "feat: PROJ-9 synthetic work: PROJ-9: read Jira:") {
					t.Errorf("%v does not name the read failure of PROJ-9:\n%s", args, out)
				}
				for _, want := range tc.want {
					if !lineWith(out, want) {
						t.Errorf("%v missing %q:\n%s", args, want, out)
					}
				}
			}
		})
	}
}

// A target held by two windows names the refusal of each.
func TestDeployHoldNamesEveryHoldingWindow(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return strings.Replace(releaseConfig(state, q), "release_notes:\n  jira:", `    prod2:
      env: local
      run: [sh, deploy.sh]
      requires: test
      deployed_sha: {path: `+q("prod2-sha")+`}
      window: {days: [mon, thu], at: "15:00", until: "19:00", tz: Etc/GMT-3, cutoff: {days_before: 1, at: "18:00"}}
release_notes:
  jira:`, 1)
	})
	writeFile(t, filepath.Join(d.state, "prod2-sha"), d.git("rev-parse", "HEAD")+"\n")
	d.setStatus("PROJ-8", "Ready for release")
	d.at("2026-10-20 10:00")
	d.commitFile("checked.txt", "feat: PROJ-8 checked work")
	d.mustGraft("deploy", "test")
	candidate := d.git("rev-parse", "HEAD")

	d.at("2026-10-21 18:30")
	d.commitFile("more.txt", "feat: PROJ-1 more work")
	for _, args := range [][]string{{"deploy", "check", "test"}, {"deploy", "test"}} {
		out, code := d.graft("", args...)
		if code == 0 {
			t.Fatalf("%v unexpectedly passed:\n%s", args, out)
		}
		for _, window := range []string{"prod", "prod2"} {
			if !lineWith(out, "test holds the release candidate "+candidate[:12]+" of the "+window+" 22.10.26 window") {
				t.Errorf("%v does not name the hold of %s:\n%s", args, window, out)
			}
		}
	}
}

// The deploy names requires and the approved release in one refusal, as
// deploy check does.
func TestDeployNamesRequiresAndApprovedReleaseTogether(t *testing.T) {
	d := newDeployFixture(t, releaseConfig)
	d.at("2026-10-22 15:30")
	writeFile(t, filepath.Join(d.state, "prod-sha"), d.git("rev-parse", "HEAD")+"\n")
	writeFile(t, filepath.Join(d.state, "test-version"), "0.0.1\n")
	d.write("one.txt", "synthetic task one\n")
	d.mustPlainCommit("feat: PROJ-1 synthetic work")
	d.git("push", "-q", "origin", "HEAD")
	for _, args := range [][]string{{"deploy", "check", "prod"}, {"deploy", "prod"}} {
		out, code := d.graft("", args...)
		if code == 0 {
			t.Fatalf("%v unexpectedly passed:\n%s", args, out)
		}
		if !lineWith(out, "test runs version 0.0.1, not ") || !lineWith(out, "feat: PROJ-1 synthetic work: PROJ-1") {
			t.Errorf("%v does not name requires and the approved release together:\n%s", args, out)
		}
	}
}

// A failed Jira read for the outside_version advice is named with the
// refusals of the commits, not instead of them.
func TestDeployOutsideAdviceReadFailureKeepsCommitRefusals(t *testing.T) {
	d := liveFixture(t, entry60("Product v1.2.10"), map[string][]string{"PROJ-10": {"10010"}})
	d.jira.mu.Lock()
	d.jira.failIssues = map[string]int{"PROJ-60": 500}
	d.jira.mu.Unlock()
	d.commitFile("x.txt", "feat: PROJ-10 work of X")
	d.commitFile("three.txt", "feat: PROJ-3 work outside any version")
	for _, args := range [][]string{{"deploy", "check", "live"}, {"deploy", "live"}} {
		out, code := d.graft("", args...)
		if code == 0 {
			t.Fatalf("%v unexpectedly passed:\n%s", args, out)
		}
		if !lineWith(out, "outside_version advice: read Jira:", "PROJ-60") || !lineWith(out, "feat: PROJ-3 work outside any version: PROJ-3 not in an approved") {
			t.Errorf("%v does not name the read failure and the refused commit together:\n%s", args, out)
		}
	}
}

// integrationsFixture is prod requiring uat, both behind a fake ssh that
// runs the command with the host in FAKE_HOST: uat runs alpha and beta,
// prod configures alpha only.
func integrationsFixture(t *testing.T) (*fixture, func(string) string) {
	t.Helper()
	state := t.TempDir()
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "ssh"), "#!/bin/sh\nfor a in \"$@\"; do host=$line; line=$a; done\nFAKE_HOST=$host exec sh -c \"$line\"\n")
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
`,
		"VERSION":                     "1.0.0\n",
		"integration-exceptions.conf": "# integration|reason\n",
		"deploy.sh":                   "echo deploy >> " + list("runs") + "\n",
	}, "origin")
	for _, kv := range testEnv {
		if p, ok := strings.CutPrefix(kv, "PATH="); ok {
			f.env = append(f.env, "PATH="+bin+string(os.PathListSeparator)+p)
		}
	}
	f.write("VERSION", "1.0.1\n")
	f.mustPlainCommit("feat: base")
	f.git("push", "-q", "-u", "origin", "HEAD")
	writeFile(t, list("uat-version"), "1.0.1\n")
	writeFile(t, list("uat-list"), "alpha\nbeta\n")
	writeFile(t, list("prod-list"), "alpha\n")
	return f, list
}

// The integrations prod lacks refuse deploy check as they refuse the
// deploy, alone and together with requires.
func TestDeployCheckNamesMissingIntegrations(t *testing.T) {
	f, list := integrationsFixture(t)
	gap := "uat runs beta, which prod does not configure"
	for _, args := range [][]string{{"deploy", "check", "prod"}, {"deploy", "prod"}} {
		out, code := f.graft("", args...)
		if code == 0 || !lineWith(out, gap) {
			t.Fatalf("%v does not refuse the missing integration: exit %d\n%s", args, code, out)
		}
	}
	writeFile(t, list("uat-version"), "1.0.0\n")
	for _, args := range [][]string{{"deploy", "check", "prod"}, {"deploy", "prod"}} {
		out, code := f.graft("", args...)
		if code == 0 || !lineWith(out, "uat runs version 1.0.0, not 1.0.1") || !lineWith(out, gap) {
			t.Fatalf("%v does not name requires and the integrations together: exit %d\n%s", args, code, out)
		}
	}
	if _, err := os.Stat(list("runs")); !os.IsNotExist(err) {
		t.Fatalf("the deploy script ran: %v", err)
	}
}

// When the deploy script compares the integrations ({staged}), deploy check
// still reads the exceptions file and refuses a bad one, as the deploy does.
func TestDeployCheckReadsExceptionsOfStagedIntegrations(t *testing.T) {
	f, list := integrationsFixture(t)
	f.write(".graft.yaml", strings.Replace(f.read(".graft.yaml"), "-list', exceptions:", "-list {staged}', running: /srv/build, exceptions:", 1))
	f.write("integration-exceptions.conf", "beta\n")
	f.mustPlainCommit("chore: the deploy script compares the integrations")
	f.git("push", "-q", "origin", "HEAD")
	bad := `integration-exceptions.conf:1: expected "integration|reason", got "beta"`
	for _, args := range [][]string{{"deploy", "check", "prod"}, {"deploy", "prod"}} {
		out, code := f.graft("", args...)
		if code == 0 || !strings.Contains(out, bad) {
			t.Fatalf("%v does not refuse the bad exceptions file: exit %d\n%s", args, code, out)
		}
	}
	if _, err := os.Stat(list("runs")); !os.IsNotExist(err) {
		t.Fatalf("the deploy script ran: %v", err)
	}
}

// heldTestFixture is releaseConfig with test held by the prod window since
// the cutoff: test runs the commit deployed before it.
func heldTestFixture(t *testing.T) *deployFixture {
	t.Helper()
	d := newDeployFixture(t, releaseConfig)
	d.setStatus("PROJ-8", "Ready for release")
	d.at("2026-10-20 10:00")
	d.commitFile("checked.txt", "feat: PROJ-8 checked work")
	d.mustGraft("deploy", "test")
	d.at("2026-10-21 18:30")
	return d
}

// The candidate the hold records cannot be written: deploy check refuses as
// the deploy does, and writes nothing.
func TestDeployCheckNamesUnwritableCandidate(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a read-only file")
	}
	d := heldTestFixture(t)
	d.plainCommitFile("docs/note.md", "docs: a note the hold lets through")
	candidate := filepath.Join(d.state, "test-sha.candidate")
	writeFile(t, candidate, "")
	if err := os.Chmod(candidate, 0o444); err != nil {
		t.Fatal(err)
	}
	runs := d.stateFile("test-sha")
	for _, args := range [][]string{{"deploy", "check", "test"}, {"deploy", "test"}} {
		out, code := d.graft("", args...)
		if code == 0 || !strings.Contains(out, "test-sha.candidate") {
			t.Fatalf("%v does not refuse the unwritable candidate: exit %d\n%s", args, code, out)
		}
	}
	if got := d.stateFile("test-sha"); got != runs {
		t.Fatalf("the refused deploy recorded %q over %q", got, runs)
	}
}

// A deploy the hold refuses records no candidate: the next deploy reads the
// commit the target runs, not one a refused deploy left.
func TestDeployRefusedRecordsNoCandidate(t *testing.T) {
	d := heldTestFixture(t)
	d.commitFile("more.txt", "feat: PROJ-1 more work")
	out, code := d.graft("", "deploy", "test")
	if code == 0 || !strings.Contains(out, "test holds the release candidate") {
		t.Fatalf("the hold does not refuse: exit %d\n%s", code, out)
	}
	if d.stateExists("test-sha.candidate") {
		t.Fatalf("a refused deploy recorded the candidate %q", d.stateFile("test-sha.candidate"))
	}
}

// After requires refuses, the deploy reads the deployed commit kept under
// sudo without a password, as deploy check does: with sudo -n it checks the
// approved release too; when sudo -n needs the password, it names what it
// did not check.
func TestDeployAfterRefusalReadsWithoutSudoPassword(t *testing.T) {
	for _, tc := range []struct {
		name, sudo string
		want       string
	}{
		{"read", `[ "$1" = -n ] && shift; exec "$@"`, "feat: PROJ-2 synthetic work: PROJ-2"},
		{"password", `[ "$1" = -n ] && exit 1; exec "$@"`, "not checked: the deployed commit and the checks that need it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDeployFixture(t, func(state string, q func(string) string) string {
				return strings.Replace(releaseConfig(state, q), "deployed_sha: {path: "+q("prod-sha")+"}", "deployed_sha: {path: "+q("prod-sha")+", sudo: true}", 1)
			})
			calls := d.fakeSudoScript(t, tc.sudo)
			d.at("2026-10-22 15:30")
			writeFile(t, filepath.Join(d.state, "prod-sha"), d.git("rev-parse", "HEAD")+"\n")
			writeFile(t, filepath.Join(d.state, "test-version"), "0.0.1\n")
			d.write("one.txt", "synthetic task one\n")
			d.mustPlainCommit("feat: PROJ-2 synthetic work")
			d.git("push", "-q", "origin", "HEAD")
			out, code := d.graft("", "deploy", "prod")
			if code == 0 || !lineWith(out, "test runs version 0.0.1, not ") || !lineWith(out, tc.want) {
				t.Fatalf("deploy after the requires refusal: exit %d\n%s", code, out)
			}
			b, err := os.ReadFile(calls)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
				if line != "" && !strings.HasPrefix(line, "-n ") {
					t.Errorf("sudo asked for a password after the refusal: sudo %s\n%s", line, out)
				}
			}
		})
	}
}

// A redeploy takes nothing out: deploy check and the deploy leave the window
// version's contents unchecked and say so, whatever the version lists: an
// item the target runs, one tied to its commit by attribute, one a revert
// without its key took out, one no commit names.
func TestDeployRedeployLeavesVersionContentsUnchecked(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return strings.Replace(windowGateConfig(state, q), "      approved_release:", "      confirm: sudo\n      approved_release:", 1)
	})
	d.fakeSudoScript(t, `[ "$1" = -n ] && shift; [ "$1" = -v ] && exit 0; exec "$@"`)
	d.at("2026-10-22 15:30")
	d.jira.mu.Lock()
	d.jira.manageVersions = true
	d.jira.mu.Unlock()
	d.write("one.txt", "synthetic task one\n")
	d.mustPlainCommit("feat: PROJ-1 synthetic work")
	d.write("five.txt", "synthetic task five\n")
	d.mustPlainCommit("feat: PROJ-5 synthetic work")
	five := d.git("rev-parse", "HEAD")
	d.git("-c", "core.hooksPath=/dev/null", "revert", "--no-edit", "HEAD")
	d.git("-c", "core.hooksPath=/dev/null", "commit", "-q", "--amend", "-m", "revert: drop the synthetic work\n\nThis reverts commit "+five+".")
	d.write("seven.txt", "synthetic untagged work\n")
	d.mustPlainCommit("feat: synthetic untagged work")
	seven := d.git("rev-parse", "HEAD")
	d.write(".graft.yaml", strings.Replace(d.read(".graft.yaml"), "version: {match: '^Product'}}", "version: {match: '^Product'}, attribute: [{commit: "+seven+", item: PROJ-7, reason: synthetic}]}", 1))
	d.mustPlainCommit("chore: attribute the untagged work")
	d.git("push", "-q", "-u", "origin", "HEAD")
	head := d.git("rev-parse", "HEAD")
	for _, name := range []string{"prod", "test"} {
		writeFile(t, filepath.Join(d.state, name+"-sha"), head+"\n")
		writeFile(t, filepath.Join(d.state, name+"-version"), d.read("VERSION"))
	}
	d.jira.setVersions([]map[string]any{{"id": "window", "name": "Product 22.10.26"}}, map[string][]string{
		"PROJ-1": {"window"}, "PROJ-5": {"window"}, "PROJ-7": {"window"}, "PROJ-9": {"window"},
	})
	d.jira.setApprovers(map[string][]string{"window": {"APPROVED"}})
	for _, key := range []string{"PROJ-1", "PROJ-5", "PROJ-7", "PROJ-9"} {
		d.setStatus(key, "Ready for release")
	}
	unchecked := "not checked: version contents — the target already runs this commit"
	out, code := d.graft("", "deploy", "check", "prod")
	if code == 0 || !strings.Contains(out, "prod already runs "+head[:12]) || !lineWith(out, unchecked) || strings.Contains(out, "does not take out") {
		t.Fatalf("deploy check of the commit prod runs: exit %d\n%s", code, out)
	}
	out, code = d.graft("", "deploy", "--redeploy", "prod")
	if code != 0 || !lineWith(out, unchecked) || strings.Contains(out, "does not take out") {
		t.Fatalf("the redeploy: exit %d\n%s", code, out)
	}
}

// Without version, --unapproved warns that the approved release could not
// be collected when a Jira read fails, as the release page needs it.
func TestDeployUnapprovedWarnsOfJiraReadFailureWithoutVersion(t *testing.T) {
	d := newDeployFixture(t, deployConfig)
	writeFile(t, filepath.Join(d.state, "gate-sha"), d.git("rev-parse", "HEAD")+"\n")
	d.jira.mu.Lock()
	d.jira.failIssues = map[string]int{"PROJ-9": 500}
	d.jira.mu.Unlock()
	d.write("nine.txt", "synthetic unreadable work\n")
	d.mustPlainCommit("feat: PROJ-9 synthetic work")
	d.git("push", "-q", "origin", "HEAD")
	out, code := d.graft("", "deploy", "--unapproved", "gate")
	if code != 0 || !lineWith(out, "approved release collection failed (--unapproved)", "PROJ-9") {
		t.Fatalf("--unapproved does not warn of the failed read: exit %d\n%s", code, out)
	}
}

// A head that lacks the commit the target runs and a commit the approved
// release refuses are named together.
func TestDeployNamesMissingDeployedCommitAndApprovedReleaseTogether(t *testing.T) {
	d := newDeployFixture(t, releaseConfig)
	d.at("2026-10-22 15:30")
	d.git("switch", "-q", "-c", "hotfix/side")
	side := d.commitFile("side.txt", "fix: PROJ-4 side fix")
	d.git("push", "-q", "-u", "origin", "hotfix/side")
	d.git("switch", "-q", "-")
	d.write("one.txt", "synthetic task one\n")
	d.mustPlainCommit("feat: PROJ-1 synthetic work")
	d.git("push", "-q", "origin", "HEAD")
	writeFile(t, filepath.Join(d.state, "prod-sha"), side+"\n")
	writeFile(t, filepath.Join(d.state, "test-version"), d.read("VERSION"))
	for _, args := range [][]string{{"deploy", "check", "prod"}, {"deploy", "prod"}} {
		out, code := d.graft("", args...)
		if code == 0 || !lineWith(out, "prod runs "+side[:12]+" fix: PROJ-4 side fix, which") || !lineWith(out, "feat: PROJ-1 synthetic work: PROJ-1") {
			t.Errorf("%v does not name the missing deployed commit and the approved release together: exit %d\n%s", args, code, out)
		}
	}
}

// A hotfix refused by its label and a hold with no recorded commit are named
// together.
func TestDeployNamesHotfixAndHoldTogether(t *testing.T) {
	d := newDeployFixture(t, releaseConfig)
	writeFile(t, filepath.Join(d.state, "prod-sha"), d.git("rev-parse", "HEAD")+"\n")
	d.at("2026-10-21 18:30")
	d.git("switch", "-q", "-c", "hotfix/one")
	d.commitFile("fix.txt", "fix: PROJ-1 unlabelled fix")
	d.git("push", "-q", "-u", "origin", "hotfix/one")
	for _, args := range [][]string{{"deploy", "check", "test"}, {"deploy", "test"}} {
		out, code := d.graft("", args...)
		if code == 0 || !lineWith(out, "every commit must name an item labelled hotfix") || !lineWith(out, "runs no recorded commit to hold") {
			t.Errorf("%v does not name the hotfix and the hold together: exit %d\n%s", args, code, out)
		}
	}
}

// deploy check names the missing .env keys of the target and those of an
// advisory dep, as the deploy does before anything else.
func TestDeployCheckNamesMissingKeysOfTargetAndAdvisoryDeps(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := strings.Replace(freezeConfig(state, q), "run: [[sh, -c, '[ ! -e ", "run: [[sh, -c, 'echo ${SYNTH_LINT_KEY}; [ ! -e ", 1)
		return strings.Replace(cfg, "      deps: [lint]\n      requires: acc\n", "      deps: [lint]\n      requires: acc\n      dotenv: [SYNTH_TARGET_KEY]\n", 1)
	})
	out, code := d.graft("", "deploy", "check", "prod")
	if code == 0 || !lineWith(out, "SYNTH_TARGET_KEY is not set") || !lineWith(out, "SYNTH_LINT_KEY is not set") {
		t.Fatalf("deploy check does not name both missing keys: exit %d\n%s", code, out)
	}
}

// A deploy that went live while its merge into the trunk failed exits 3 and
// still shows the daily flag, as a complete deploy does.
func TestDeployTrunkMergeFailureStillShowsDailyFlag(t *testing.T) {
	f, _ := flagsRepo(t)
	sha := filepath.Join(t.TempDir(), "prod-sha")
	f.write(".graft.yaml", f.read(".graft.yaml")+`deploy:
  remote: origin
  targets:
    prod:
      env: local
      run: [sh, -c, 'true']
      deployed_sha: {path: `+yamlQuote(sha)+`}
      merge_into_trunk: true
`)
	f.mustPlainCommit("feat: base")
	trunk := f.git("branch", "--show-current")
	f.git("push", "-q", "-u", "origin", "HEAD")
	f.git("remote", "set-head", "origin", trunk)
	f.git("switch", "-q", "-c", "release/1")
	f.write("fix.txt", "synthetic fix\n")
	f.mustPlainCommit("fix: found in the release")
	f.git("push", "-q", "-u", "origin", "release/1")
	out, code := f.graft("", "deploy", "prod")
	if code != 3 || !strings.Contains(out, "deployment succeeded, trunk merge failed") {
		t.Fatalf("the trunk merge does not fail as set up: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "graft: flag RAISED (local):") {
		t.Fatalf("the live release shows no daily flag:\n%s", out)
	}
}

// After a refusal the hold does not ask for the sudo password of the
// windowed target's record: when sudo -n cannot read it, the deploy names
// the refusal and what it did not check.
func TestDeployHoldAfterRefusalNamesSkippedSudoRead(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return strings.Replace(releaseConfig(state, q), "deployed_sha: {path: "+q("prod-sha")+"}", "deployed_sha: {path: "+q("prod-sha")+", sudo: true}", 1)
	})
	d.fakeSudoScript(t, `[ "$1" = -n ] && exit 1; exec "$@"`)
	base := d.git("rev-parse", "HEAD")
	writeFile(t, filepath.Join(d.state, "prod-sha"), base+"\n")
	writeFile(t, filepath.Join(d.state, "test-sha"), base+"\n")
	d.at("2026-10-21 18:30")
	d.git("switch", "-q", "-c", "hotfix/one")
	d.commitFile("fix.txt", "fix: PROJ-1 unlabelled fix")
	d.git("push", "-q", "-u", "origin", "hotfix/one")
	out, code := d.graft("", "deploy", "test")
	if code == 0 || !lineWith(out, "every commit must name an item labelled hotfix") ||
		!lineWith(out, "not checked: what needs "+filepath.Join(d.state, "prod-sha")+" on local — a refusal came first") {
		t.Fatalf("the hold after the hotfix refusal: exit %d\n%s", code, out)
	}
}

// sudoCallsWithoutN are the calls of the fake sudo, logged in calls, that
// could ask for the password: those without -n.
func sudoCallsWithoutN(t *testing.T, calls string) []string {
	t.Helper()
	b, err := os.ReadFile(calls)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var prompt []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		if line != "" && !strings.HasPrefix(line, "-n ") && !strings.HasPrefix(line, "-k") {
			prompt = append(prompt, line)
		}
	}
	return prompt
}

// sudoProdConfig is releaseConfig with prod's record under sudo.
func sudoProdConfig(confirm string) func(string, func(string) string) string {
	return func(state string, q func(string) string) string {
		cfg := strings.Replace(releaseConfig(state, q), "deployed_sha: {path: "+q("prod-sha")+"}", "deployed_sha: {path: "+q("prod-sha")+", sudo: true}", 1)
		return strings.Replace(cfg, "      approved_release:", confirm+"      approved_release:", 1)
	}
}

// Once the deploy refused, whether because its head lacks the commit prod
// runs or because prod runs it already, the approved release reads the
// .withheld record under sudo -n: only the read of the deployed commit,
// before any refusal, may ask for the password.
func TestDeployRefusalBeforeApprovedReleaseReadsWithheldWithoutPassword(t *testing.T) {
	const anySudo = `[ "$1" = -n ] && shift; [ "$1" = -v ] && exit 0; exec "$@"`
	lacksDeployed := func(d *deployFixture) string {
		d.git("switch", "-q", "-c", "hotfix/side")
		side := d.commitFile("side.txt", "fix: PROJ-4 side fix")
		d.git("push", "-q", "-u", "origin", "hotfix/side")
		d.git("switch", "-q", "-")
		d.write("one.txt", "synthetic task one\n")
		d.mustPlainCommit("feat: PROJ-1 synthetic work")
		d.git("push", "-q", "origin", "HEAD")
		return side
	}
	for _, tc := range []struct {
		name, confirm, sudo, want string
		setup                     func(d *deployFixture) string
	}{
		{"lacks the deployed commit", "", anySudo, "which", lacksDeployed},
		// sudo -n is refused the .withheld record: the approved release is
		// not checked, and that is no second refusal.
		{"lacks the deployed commit, .withheld needs the password", "", `case "$*" in "-n "*withheld*) echo "sudo: a password is required" >&2; exit 1;; esac
` + anySudo, "prod-sha.withheld on local — a refusal came first", lacksDeployed},
		{"runs it already", "      confirm: sudo\n", anySudo, "pass --redeploy to deploy it again", func(d *deployFixture) string {
			return d.git("rev-parse", "HEAD")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDeployFixture(t, sudoProdConfig(tc.confirm))
			calls := d.fakeSudoScript(t, tc.sudo)
			d.at("2026-10-22 15:30")
			writeFile(t, filepath.Join(d.state, "prod-sha"), tc.setup(d)+"\n")
			writeFile(t, filepath.Join(d.state, "test-version"), d.read("VERSION"))
			out, code := d.graft("", "deploy", "prod")
			if code == 0 || !lineWith(out, tc.want) || strings.Contains(out, "approved release: reading") {
				t.Fatalf("the refusal: exit %d\n%s", code, out)
			}
			for _, call := range sudoCallsWithoutN(t, calls) {
				if strings.Contains(call, "prod-sha.withheld") {
					t.Errorf("the .withheld record was read with a password prompt after the refusal: sudo %s\n%s", call, out)
				}
			}
		})
	}
}

// The hold of one window refused, the next window's record is read under
// sudo -n.
func TestDeployHoldRefusalReadsNextWindowWithoutPassword(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return strings.Replace(releaseConfig(state, q), "release_notes:\n  jira:", `    prod2:
      env: local
      run: [sh, deploy.sh]
      requires: test
      deployed_sha: {path: `+q("prod2-sha")+`, sudo: true}
      window: {days: [mon, thu], at: "15:00", until: "19:00", tz: Etc/GMT-3, cutoff: {days_before: 1, at: "18:00"}}
release_notes:
  jira:`, 1)
	})
	calls := d.fakeSudoScript(t, `[ "$1" = -n ] && shift; exec "$@"`)
	writeFile(t, filepath.Join(d.state, "prod2-sha"), d.git("rev-parse", "HEAD")+"\n")
	d.setStatus("PROJ-8", "Ready for release")
	d.at("2026-10-20 10:00")
	d.commitFile("checked.txt", "feat: PROJ-8 checked work")
	d.mustGraft("deploy", "test")
	d.at("2026-10-21 18:30")
	d.commitFile("more.txt", "feat: PROJ-1 more work")
	if err := os.Remove(calls); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	out, code := d.graft("", "deploy", "test")
	if code == 0 || !lineWith(out, "of the prod 22.10.26 window") || !lineWith(out, "of the prod2 22.10.26 window") {
		t.Fatalf("the holds: exit %d\n%s", code, out)
	}
	if prompt := sudoCallsWithoutN(t, calls); len(prompt) > 0 {
		t.Errorf("the second window's record was read with a password prompt after the first refused: %q\n%s", prompt, out)
	}
}

// After a hotfix refusal the hold reads the target's .candidate record under
// sudo -n; when that wants the password, the deploy names it not checked
// rather than as a second refusal.
func TestDeploySkippedCandidateReadIsNotChecked(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return strings.Replace(releaseConfig(state, q), "deployed_sha: {path: "+q("test-sha")+"}", "deployed_sha: {path: "+q("test-sha")+", sudo: true}", 1)
	})
	d.fakeSudoScript(t, `[ "$1" = -n ] && { echo "sudo: a password is required" >&2; exit 1; }; exec "$@"`)
	base := d.git("rev-parse", "HEAD")
	writeFile(t, filepath.Join(d.state, "prod-sha"), base+"\n")
	writeFile(t, filepath.Join(d.state, "test-sha"), base+"\n")
	d.at("2026-10-21 18:30")
	d.git("switch", "-q", "-c", "hotfix/one")
	d.commitFile("fix.txt", "fix: PROJ-1 unlabelled fix")
	d.git("push", "-q", "-u", "origin", "hotfix/one")
	out, code := d.graft("", "deploy", "test")
	if code == 0 || !lineWith(out, "every commit must name an item labelled hotfix") ||
		!lineWith(out, "not checked: what needs "+filepath.Join(d.state, "test-sha.candidate")+" on local — a refusal came first") {
		t.Fatalf("the hold after the hotfix refusal: exit %d\n%s", code, out)
	}
	if strings.Contains(out, "nothing was deployed: reading") {
		t.Fatalf("the skipped read is a refusal:\n%s", out)
	}
}

// heldSudoTestFixture is heldTestFixture with test's record under sudo and
// the fake sudo given by script, past the cutoff with a commit the hold lets
// through.
func heldSudoTestFixture(t *testing.T, script string) *deployFixture {
	t.Helper()
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		return strings.Replace(releaseConfig(state, q), "deployed_sha: {path: "+q("test-sha")+"}", "deployed_sha: {path: "+q("test-sha")+", sudo: true}", 1)
	})
	d.fakeSudoScript(t, script)
	d.setStatus("PROJ-8", "Ready for release")
	d.at("2026-10-20 10:00")
	d.commitFile("checked.txt", "feat: PROJ-8 checked work")
	d.mustGraft("deploy", "test")
	d.at("2026-10-21 18:30")
	d.plainCommitFile("docs/note.md", "docs: a note the hold lets through")
	return d
}

// sudo reads the records without a password but wants it when asked about
// the write: deploy check cannot tell whether the candidate can be written,
// and says so instead of refusing.
func TestDeployCheckCandidateWriteNeedingPasswordIsNotChecked(t *testing.T) {
	d := heldSudoTestFixture(t, `case "$*" in "-n -l "*) echo "sudo: a password is required" >&2; exit 1;; esac
[ "$1" = -n ] && shift; exec "$@"`)
	out, code := d.graft("", "deploy", "check", "test")
	if code != 0 || !lineWith(out, "not checked: writing "+filepath.Join(d.state, "test-sha.candidate")+" — needs the sudo password") {
		t.Fatalf("deploy check: exit %d\n%s", code, out)
	}
}

// The candidate does not exist yet and its directory is not writable:
// deploy check refuses, as the write would fail.
func TestDeployCheckNamesCandidateDirectoryNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a read-only directory")
	}
	d := heldTestFixture(t)
	d.plainCommitFile("docs/note.md", "docs: a note the hold lets through")
	if err := os.Chmod(d.state, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(d.state, 0o755); err != nil {
			t.Error(err)
		}
	})
	out, code := d.graft("", "deploy", "check", "test")
	if code == 0 || !lineWith(out, "test-sha.candidate on local cannot be written") {
		t.Fatalf("deploy check does not refuse the unwritable directory: exit %d\n%s", code, out)
	}
}

// deploy check takes --reviewer-agreed as the deploy does: the pairs that
// cover the guard pass it, others do not.
func TestDeployCheckTakesReviewerAgreed(t *testing.T) {
	d := reviewGuardFixture(t, []string{"In QA"})
	setupGuardVersion(d, map[string][]string{"PROJ-1": {"10010"}})
	d.setStatus("PROJ-1", "In QA")
	out, code := d.graft("", "deploy", "check", "--reviewer-agreed", "PROJ-1@anna", "uat")
	if code != 0 || strings.Contains(out, "review guard:") {
		t.Fatalf("deploy check with the agreement: exit %d\n%s", code, out)
	}
	out, code = d.graft("", "deploy", "check", "--reviewer-agreed", "PROJ-1@bob", "uat")
	if code == 0 || !strings.Contains(out, "--reviewer-agreed PROJ-1@anna") {
		t.Fatalf("deploy check with another reviewer's agreement: exit %d\n%s", code, out)
	}
	if out, code := d.graft("", "deploy", "status", "--reviewer-agreed", "PROJ-1@anna", "uat"); code == 0 {
		t.Fatalf("deploy status took --reviewer-agreed:\n%s", out)
	}
}

// sudo rules may allow a shell and not tee, the command the candidate is
// written with: deploy check and the deploy ask sudo about tee and refuse
// alike, and the deploy runs no tee.
func TestDeployCheckRefusesCandidateWriteSudoDoesNotAllow(t *testing.T) {
	d := heldSudoTestFixture(t, `[ "$1" = -n ] && shift
case "$*" in *tee*test-sha.candidate*) [ "$1" = -l ] && exit 1; echo "sudo: command not allowed" >&2; exit 1;; esac
[ "$1" = -l ] && { shift; echo "/usr/bin/$*"; exit 0; }
exec "$@"`)
	for _, args := range [][]string{{"deploy", "check", "test"}, {"deploy", "test"}} {
		out, code := d.graft("", args...)
		if code == 0 || !lineWith(out, "test-sha.candidate on local cannot be written", "sudo does not allow tee") {
			t.Fatalf("graft %s: exit %d\n%s", strings.Join(args, " "), code, out)
		}
	}
	if _, err := os.Stat(filepath.Join(d.state, "test-sha.candidate")); !os.IsNotExist(err) {
		t.Fatalf("the candidate was written: %v", err)
	}
}

// The reverse rules, tee allowed and a shell only for the reads: deploy
// check passes, as the deploy does, and the deploy records the candidate.
func TestDeployCheckPassesCandidateWriteSudoAllowsWithoutShell(t *testing.T) {
	d := heldSudoTestFixture(t, `[ "$1" = -n ] && shift
case "$*" in "sh -c if [ -e "*"; then cat "*"; fi") ;; "sh "*) echo "sudo: command not allowed" >&2; exit 1;; esac
[ "$1" = -l ] && { shift; echo "/usr/bin/$*"; exit 0; }
exec "$@"`)
	out, code := d.graft("", "deploy", "check", "test")
	if code != 0 || !lineWith(out, "graft deploy test passes its checks now") {
		t.Fatalf("deploy check: exit %d\n%s", code, out)
	}
	out, code = d.graft("", "deploy", "test")
	if code != 0 || !strings.Contains(d.stateFile("test-sha.candidate"), " ") {
		t.Fatalf("the deploy: exit %d\n%s", code, out)
	}
}

// The first read of the deployed commit failed: that refusal marks the run
// at once, so the review guard beside it reads the record under sudo -n.
func TestDeployReadRefusalReadsGuardRecordWithoutPassword(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := deployConfig(state, q)
		cfg = strings.Replace(cfg, "    uat:\n      env: local\n", "    uat:\n      env: local\n      version: 'echo 1.2.10'\n      review_guard: {jira_version: 'Product v{version}', statuses: [Testing]}\n", 1)
		return strings.Replace(cfg, "deployed_sha: {path: "+q("uat-sha")+"}", "deployed_sha: {path: "+q("uat-sha")+", sudo: true}", 1)
	})
	setupGuardVersion(d, map[string][]string{"PROJ-1": {"10010"}})
	calls := d.fakeSudoScript(t, `case "$*" in *uat-sha*) [ "$(grep -c uat-sha "$(dirname "$0")/calls")" = 1 ] && { echo 'synthetic read error' >&2; exit 1; };; esac
[ "$1" = -n ] && shift; exec "$@"`)
	out, code := d.graft("", "deploy", "uat")
	if code == 0 || !lineWith(out, "synthetic read error") || !lineWith(out, "review guard: uat Jira version Product v1.2.10 has work under review") {
		t.Fatalf("the read refusal: exit %d\n%s", code, out)
	}
	if prompt := sudoCallsWithoutN(t, calls); len(prompt) != 1 {
		t.Fatalf("sudo calls that may ask for the password, want only the first read: %q\n%s", prompt, out)
	}
}

// The branch refusal of a freeze marks the run before the freeze issue check
// reads the frozen target's record: that read goes under sudo -n, and when it
// wants the password the check is named not checked beside the refusal.
func TestDeployFreezeBranchRefusalReadsRecordWithoutPassword(t *testing.T) {
	d := freezeRepoWith(t, func(state string, q func(string) string) string {
		return strings.Replace(freezeConfig(state, q), "deployed_sha: {path: "+q("prod-sha")+"}", "deployed_sha: {path: "+q("prod-sha")+", sudo: true}", 1)
	})
	d.commitFile("accepted.txt", "feat: PROJ-1 accepted work")
	d.fakeSudoScript(t, `[ "$1" = -n ] && shift; [ "$1" = -v ] && exit 0; exec "$@"`)
	trunk := d.freeze()
	calls := d.fakeSudoScript(t, `case "$*" in "-n "*prod-sha*) echo "sudo: a password is required" >&2; exit 1;; esac
[ "$1" = -n ] && shift; [ "$1" = -v ] && exit 0; exec "$@"`)
	d.git("switch", "-q", trunk)
	d.commitFile("late.txt", "feat: PROJ-1 late work")
	out, code := d.graft("", "deploy", "acc")
	if code == 0 || !lineWith(out, "is frozen at Product v1.2.10") || !lineWith(out, "not checked: what needs "+filepath.Join(d.state, "prod-sha")+" on local") {
		t.Fatalf("the branch refusal: exit %d\n%s", code, out)
	}
	if prompt := sudoCallsWithoutN(t, calls); len(prompt) > 0 {
		t.Errorf("sudo may ask for the password after the branch refusal: %q\n%s", prompt, out)
	}
	// Passed by --past-freeze the branch refusal is none: the record is read
	// as without a refusal and the freeze issue check runs.
	out, code = d.graft("", "deploy", "--past-freeze", "agreed", "acc")
	if code != 0 || strings.Contains(out, "not checked") {
		t.Fatalf("the bypassed deploy: exit %d\n%s", code, out)
	}
}

// test's record lies in a directory inside one the deploy user cannot enter,
// which sudo opens: whether the candidate's directory exists the check cannot
// see, and it says so instead of refusing.
func TestDeployCheckNamesCandidateDirectoryNotVisible(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root enters any directory")
	}
	var outer string
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		outer = filepath.Join(state, "outer")
		if err := os.MkdirAll(filepath.Join(outer, "inner"), 0o755); err != nil {
			t.Fatal(err)
		}
		return strings.Replace(releaseConfig(state, q), "deployed_sha: {path: "+q("test-sha")+"}", "deployed_sha: {path: "+q("outer/inner/test-sha")+", sudo: true}", 1)
	})
	d.fakeSudoScript(t, `[ "$1" = -n ] && shift
[ "$1" = -l ] && { shift; echo "/usr/bin/$*"; exit 0; }
chmod 755 `+yamlQuote(outer)+`; "$@"; rc=$?; chmod 000 `+yamlQuote(outer)+`; exit $rc`)
	d.setStatus("PROJ-8", "Ready for release")
	d.at("2026-10-20 10:00")
	d.commitFile("checked.txt", "feat: PROJ-8 checked work")
	d.mustGraft("deploy", "test")
	if err := os.Chmod(outer, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(outer, 0o755); err != nil {
			t.Error(err)
		}
	})
	d.at("2026-10-21 18:30")
	d.plainCommitFile("docs/note.md", "docs: a note the hold lets through")
	out, code := d.graft("", "deploy", "check", "test")
	if code != 0 || !lineWith(out, "not checked: writing "+filepath.Join(outer, "inner", "test-sha.candidate")+" — its directory is not visible to the deploy user") {
		t.Fatalf("deploy check: exit %d\n%s", code, out)
	}
}

// test runs a commit the deploy would take back: that refusal marks the run
// before the hold reads prod's record, under sudo -n.
func TestDeployTakeBackRefusalReadsHoldRecordWithoutPassword(t *testing.T) {
	d := newDeployFixture(t, sudoProdConfig(""))
	calls := d.fakeSudoScript(t, `[ "$1" = -n ] && shift; [ "$1" = -v ] && exit 0; exec "$@"`)
	d.setStatus("PROJ-8", "Ready for release")
	d.at("2026-10-20 10:00")
	d.commitFile("checked.txt", "feat: PROJ-8 checked work")
	d.mustGraft("deploy", "test")
	d.git("switch", "-q", "-c", "side")
	side := d.commitFile("side.txt", "feat: PROJ-8 side work")
	d.git("switch", "-q", "-")
	writeFile(t, filepath.Join(d.state, "test-sha"), side+"\n")
	d.at("2026-10-21 18:30")
	d.plainCommitFile("docs/note.md", "docs: a note the hold lets through")
	if err := os.Remove(calls); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	out, code := d.graft("", "deploy", "test")
	if code == 0 || !lineWith(out, "--take-back") || !lineWith(out, "holds the release candidate") {
		t.Fatalf("the take-back refusal: exit %d\n%s", code, out)
	}
	if prompt := sudoCallsWithoutN(t, calls); len(prompt) > 0 {
		t.Errorf("sudo may ask for the password after the take-back refusal: %q\n%s", prompt, out)
	}
}

// The review guard refused: the freeze issue check after it reads the frozen
// target's record under sudo -n.
func TestDeployReviewGuardRefusalReadsFreezeRecordWithoutPassword(t *testing.T) {
	d := freezeRepoWith(t, func(state string, q func(string) string) string {
		return strings.Replace(freezeConfig(state, q), "deployed_sha: {path: "+q("prod-sha")+"}", "deployed_sha: {path: "+q("prod-sha")+", sudo: true}", 1)
	})
	d.commitFile("accepted.txt", "feat: PROJ-1 accepted work")
	d.fakeSudoScript(t, `[ "$1" = -n ] && shift; [ "$1" = -v ] && exit 0; exec "$@"`)
	d.freeze()
	accSHA := "deployed_sha: {path: " + yamlQuote(filepath.Join(d.state, "acc-sha")) + "}\n"
	d.write(".graft.yaml", strings.Replace(d.read(".graft.yaml"), accSHA, accSHA+"      review_guard: {jira_version: 'Product v{version}', statuses: [Testing]}\n", 1))
	d.mustPlainCommit("chore: PROJ-1 guard acc")
	calls := d.fakeSudoScript(t, `case "$*" in "-n "*prod-sha*) echo "sudo: a password is required" >&2; exit 1;; esac
[ "$1" = -n ] && shift; [ "$1" = -v ] && exit 0; exec "$@"`)
	d.commitFile("late.txt", "feat: PROJ-1 late work")
	out, code := d.graft("", "deploy", "acc")
	if code == 0 || !lineWith(out, "review guard:") || !lineWith(out, "not checked: what needs "+filepath.Join(d.state, "prod-sha")+" on local") {
		t.Fatalf("the review guard refusal: exit %d\n%s", code, out)
	}
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	var last string
	for line := range strings.SplitSeq(string(b), "\n") {
		if strings.Contains(line, "prod-sha") {
			last = line
		}
	}
	if !strings.HasPrefix(last, "-n ") {
		t.Fatalf("the freeze issue check read prod's record with a password prompt after the guard refused: sudo %s\n%s", last, out)
	}
}

// A link where the candidate goes, with sudo or without, dangling, to /proc
// or to a writable file: graft writes through no link, and deploy check and
// the deploy refuse it alike, as both ask the same CheckWritable. Nothing is
// written through the link.
func TestDeployAndCheckRefuseCandidateLinkAlike(t *testing.T) {
	const sudo = `[ "$1" = -n ] && shift; [ "$1" = -l ] && { shift; echo "/usr/bin/$*"; exit 0; }; exec "$@"`
	for _, tc := range []struct {
		name    string
		fixture func(t *testing.T) *deployFixture
		target  func(d *deployFixture) string
		old     string
	}{
		{"dangling link without sudo", func(t *testing.T) *deployFixture {
			d := heldTestFixture(t)
			d.plainCommitFile("docs/note.md", "docs: a note the hold lets through")
			return d
		}, func(d *deployFixture) string { return filepath.Join(d.state, "missing", "record") }, ""},
		{"link to a writable file under sudo", func(t *testing.T) *deployFixture { return heldSudoTestFixture(t, sudo) },
			func(d *deployFixture) string { return filepath.Join(d.state, "actual-candidate") }, "old\n"},
		{"link to /proc under sudo", func(t *testing.T) *deployFixture { return heldSudoTestFixture(t, sudo) },
			func(*deployFixture) string { return "/proc/version" }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.fixture(t)
			target := tc.target(d)
			if tc.old != "" {
				writeFile(t, target, tc.old)
			}
			if err := os.Symlink(target, filepath.Join(d.state, "test-sha.candidate")); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"deploy", "check", "test"}, {"deploy", "test"}} {
				out, code := d.graft("", args...)
				if code == 0 || !lineWith(out, "test-sha.candidate on local cannot be written", "it is a symbolic link") {
					t.Fatalf("graft %s: exit %d\n%s", strings.Join(args, " "), code, out)
				}
			}
			if tc.old != "" {
				if b, err := os.ReadFile(target); err != nil || string(b) != tc.old {
					t.Fatalf("the link's target: %q, %v", b, err)
				}
			} else if _, err := os.Lstat(target); target != "/proc/version" && !os.IsNotExist(err) {
				t.Fatalf("the dangling link's target was made: %v", err)
			}
		})
	}
}

// The deploy asks CheckWritable, sudo -n -l about the very tee of the
// candidate, before it writes the candidate with that tee.
func TestDeployAsksCheckWritableBeforeTheCandidateWrite(t *testing.T) {
	d := heldSudoTestFixture(t, `[ "$1" = -n ] && shift; [ "$1" = -l ] && { shift; echo "/usr/bin/$*"; exit 0; }; exec "$@"`)
	calls := d.fakeSudoScript(t, `[ "$1" = -n ] && shift; [ "$1" = -l ] && { shift; echo "/usr/bin/$*"; exit 0; }; exec "$@"`)
	candidate := filepath.Join(d.state, "test-sha.candidate")
	if out, code := d.graft("", "deploy", "test"); code != 0 {
		t.Fatalf("the deploy: exit %d\n%s", code, out)
	}
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	asked, wrote := -1, -1
	for i, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		switch {
		case line == "-n -l tee "+candidate && asked < 0:
			asked = i
		case line == "tee "+candidate && wrote < 0:
			wrote = i
		}
	}
	if asked < 0 || wrote < 0 || asked > wrote {
		t.Fatalf("sudo -n -l tee at %d, the tee at %d:\n%s", asked, wrote, b)
	}
}

// heldSudoFindmntFixture is heldSudoTestFixture with a fake findmnt, given
// by script, beside the fake sudo, on the PATH the fixture's commands get.
func heldSudoFindmntFixture(t *testing.T, script string) *deployFixture {
	t.Helper()
	const sudo = `[ "$1" = -n ] && shift; [ "$1" = -l ] && { shift; echo "/usr/bin/$*"; exit 0; }; exec "$@"`
	d := heldSudoTestFixture(t, sudo)
	findmnt := filepath.Join(filepath.Dir(d.fakeSudoScript(t, sudo)), "findmnt")
	writeFile(t, findmnt, "#!/bin/sh\n"+script+"\n")
	if err := os.Chmod(findmnt, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

// The candidate's directory lies on a read-only mount, as findmnt tells:
// deploy check refuses, and the deploy alike, before it writes the candidate.
func TestDeployCheckRefusesCandidateOnReadOnlyMount(t *testing.T) {
	d := heldSudoFindmntFixture(t, `[ "$*" = "-n -o OPTIONS -T $STATE" ] && { echo ro,relatime; exit 0; }; exit 1`)
	state := d.state
	d.env = append(d.env, "STATE="+state)
	for _, args := range [][]string{{"deploy", "check", "test"}, {"deploy", "test"}} {
		out, code := d.graft("", args...)
		if code == 0 || !lineWith(out, "test-sha.candidate on local cannot be written", state+" is on a file system mounted read-only") {
			t.Fatalf("graft %s: exit %d\n%s", strings.Join(args, " "), code, out)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "test-sha.candidate")); !os.IsNotExist(err) {
		t.Fatalf("the candidate was written: %v", err)
	}
}

// findmnt cannot tell the mount: deploy check names the write not checked
// instead of refusing, and the deploy leaves it to the write, which passes.
func TestDeployCheckNamesCandidateMountUnknown(t *testing.T) {
	d := heldSudoFindmntFixture(t, `echo "findmnt: synthetic failure" >&2; exit 1`)
	out, code := d.graft("", "deploy", "check", "test")
	if code != 0 || !lineWith(out, "not checked: writing "+filepath.Join(d.state, "test-sha.candidate")+" — findmnt cannot tell the mount of its directory") {
		t.Fatalf("deploy check: exit %d\n%s", code, out)
	}
	if out, code := d.graft("", "deploy", "test"); code != 0 {
		t.Fatalf("the deploy: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(d.state, "test-sha.candidate")); err != nil {
		t.Fatalf("the candidate: %v", err)
	}
}
