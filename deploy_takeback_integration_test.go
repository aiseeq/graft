package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// takeBackFixture leaves the published branch at head while recording a
// later deployed commit on the target.
func takeBackFixture(t *testing.T, target, stateSHA string) (*deployFixture, string, string) {
	t.Helper()
	d := deployRepo(t)
	head := d.git("rev-parse", "HEAD")
	d.write("deployed-later.txt", "already deployed\n")
	d.mustPlainCommit("feat: PROJ-9 deployed later")
	prev := d.git("rev-parse", "HEAD")
	d.git("push", "-q", "origin", prev+":refs/heads/hotfix/PROJ-9")
	writeFile(t, filepath.Join(d.state, stateSHA), prev+"\n")
	d.git("reset", "--hard", head)
	return d, head, prev
}

func readTakeBackJournal(t *testing.T, d *deployFixture, shaFile string) string {
	t.Helper()
	path := filepath.Join(d.state, shaFile+".journal")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat take-back journal %s: %v", path, err)
	}
	return readFile(t, path)
}

func TestDeployTakeBackRefusalDoesNotRunTarget(t *testing.T) {
	d, _, prev := takeBackFixture(t, "uat", "uat-sha")
	out, code := d.graft("", "deploy", "uat")
	if code == 0 || !strings.Contains(out, "takes back 1 deployed commits") || !strings.Contains(out, prev[:7]+" feat: PROJ-9 deployed later") ||
		!strings.Contains(out, "merge the deployed commits into this one (a rebase or cherry-pick leaves their SHAs out and is refused again), or pass --take-back --reason TEXT") {
		t.Fatalf("take-back refusal: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(d.state, "uat-run")); !os.IsNotExist(err) {
		t.Fatalf("target deploy command ran after refusal: %v", err)
	}
}

func TestDeployTakeBackRequiresReason(t *testing.T) {
	d, _, _ := takeBackFixture(t, "uat", "uat-sha")
	out, code := d.graft("", "deploy", "--take-back", "uat")
	if code == 0 || !strings.Contains(out, "--take-back requires --reason TEXT") {
		t.Fatalf("missing reason: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(d.state, "uat-run")); !os.IsNotExist(err) {
		t.Fatalf("target deploy command ran without a reason: %v", err)
	}
}

func TestDeployTakeBackRejectsWhitespaceReason(t *testing.T) {
	d, _, _ := takeBackFixture(t, "uat", "uat-sha")
	out, code := d.graft("", "deploy", "--take-back", "--reason", "   ", "uat")
	if code == 0 || !strings.Contains(out, "--take-back requires --reason TEXT") {
		t.Fatalf("blank reason: exit %d\n%s", code, out)
	}
}

func TestDeployCheckReportsTakeBackRefusal(t *testing.T) {
	d, _, prev := takeBackFixture(t, "uat", "uat-sha")
	out, code := d.graft("", "deploy", "check", "uat")
	if code == 0 || !strings.Contains(out, "takes back 1 deployed commits") || !strings.Contains(out, prev[:7]+" feat: PROJ-9 deployed later") {
		t.Fatalf("deploy check: exit %d\n%s", code, out)
	}
}

func TestDeployTakeBackRecordsReasonWithJiraComment(t *testing.T) {
	d, _, _ := takeBackFixture(t, "test", "test-sha")
	d.write("new-work.txt", "delivered work\n")
	d.mustFixtureCommit("feat: PROJ-1 new work")
	reason := "fix {missing} {target}"
	d.mustGraft("deploy", "--take-back", "--reason", reason, "test")
	journal := readTakeBackJournal(t, d, "test-sha")
	if !strings.Contains(journal, `"take_back_reason":"`+reason+`"`) {
		t.Fatalf("deployment journal = %q", journal)
	}
	comments, _ := d.jira.snapshot()
	if len(comments["PROJ-1"]) != 1 || !strings.Contains(comments["PROJ-1"][0], "Take-back reason: "+reason) {
		t.Fatalf("Jira comments = %#v", comments["PROJ-1"])
	}
}

func TestDeployTakeBackCleanRollbackRecordsReason(t *testing.T) {
	d, head, _ := takeBackFixture(t, "test", "test-sha")
	const reason = "intentional clean rollback"
	d.mustGraft("deploy", "--take-back", "--reason", reason, "test")
	if strings.TrimSpace(d.stateFile("test-sha")) != head {
		t.Fatalf("recorded deployed SHA = %q, want %q", d.stateFile("test-sha"), head)
	}
	if journal := readTakeBackJournal(t, d, "test-sha"); !strings.Contains(journal, reason) {
		t.Fatalf("clean rollback journal = %q", journal)
	}
	if !d.stateExists("test-run") {
		t.Fatal("deploy command did not run for the clean rollback")
	}
	comments, _ := d.jira.snapshot()
	if len(comments) != 0 {
		t.Fatalf("clean rollback unexpectedly posted Jira comments: %#v", comments)
	}
}

func TestDeployTakeBackJournalMustOpenBeforeTargetRuns(t *testing.T) {
	d, _, prev := takeBackFixture(t, "test", "test-sha")
	journalPath := filepath.Join(d.state, "test-sha.journal")
	if err := os.Mkdir(journalPath, 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := d.graft("", "deploy", "--take-back", "--reason", "audit", "test")
	if code == 0 || !strings.Contains(out, "opening take-back journal "+journalPath) {
		t.Fatalf("journal open failure: exit %d\n%s", code, out)
	}
	if got := strings.TrimSpace(d.stateFile("test-sha")); got != prev {
		t.Errorf("deployed SHA changed before journal opened: got %s want %s", got, prev)
	}
	if _, err := os.Stat(filepath.Join(d.state, "test-run")); !os.IsNotExist(err) {
		t.Errorf("target script ran despite journal open failure: %v", err)
	}
}

func TestDeployTakeBackNoEffectDoesNotOpenJournal(t *testing.T) {
	for _, target := range []string{"test", "live"} {
		t.Run(target, func(t *testing.T) {
			d := deployRepo(t)
			sha := d.git("rev-parse", "HEAD")
			stateSHA := target + "-sha"
			journalPath := filepath.Join(d.state, stateSHA+".journal")
			if err := os.Mkdir(journalPath, 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(d.state, stateSHA), sha+"\n")
			out, code := d.graft("", "deploy", "--take-back", "--reason", "not needed", target)
			if code != 0 || !strings.Contains(out, "graft: nothing is taken back; --take-back has no effect") {
				t.Fatalf("no-op take-back: exit %d\n%s", code, out)
			}
			if _, err := os.Stat(filepath.Join(d.state, target+"-run")); err != nil {
				t.Errorf("target script did not run: %v", err)
			}
			if info, err := os.Stat(journalPath); err != nil || !info.IsDir() {
				t.Errorf("journal path was changed: stat = %v, err = %v", info, err)
			}
			comments, _ := d.jira.snapshot()
			if len(comments) != 0 {
				t.Errorf("no-op take-back posted Jira comments: %#v", comments)
			}
		})
	}
}

func TestDeployTakeBackShallowCloneWarnsAndContinues(t *testing.T) {
	d := deployRepo(t)
	prev := d.git("rev-parse", "HEAD")
	d.write("next.txt", "next\n")
	d.mustFixtureCommit("feat: PROJ-1 next")
	head := d.git("rev-parse", "HEAD")
	writeFile(t, filepath.Join(d.state, "test-sha"), prev+"\n")
	writeFile(t, filepath.Join(d.work, ".git", "shallow"), head+"\n")
	out := d.mustGraft("deploy", "test")
	if !strings.Contains(out, "shallow clone: cannot tell whether this deploy takes commits back; git fetch --unshallow") {
		t.Fatalf("shallow warning missing:\n%s", out)
	}
	if strings.Contains(out, "takes back 1 deployed commits") {
		t.Fatalf("shallow history caused a false take-back refusal:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(d.state, "test-run")); err != nil {
		t.Fatalf("deploy command did not run: %v", err)
	}
}

func TestDeployShallowCloneWarnsWhenPreviousSHAIsUnknown(t *testing.T) {
	d := deployRepo(t)
	head := d.git("rev-parse", "HEAD")
	writeFile(t, filepath.Join(d.work, ".git", "shallow"), head+"\n")
	writeFile(t, filepath.Join(d.state, "test-sha"), strings.Repeat("f", 40)+"\n")
	out := d.mustGraft("deploy", "test")
	if !strings.Contains(out, "commit "+strings.Repeat("f", 12)+", not in the local repository (git fetch?)") ||
		!strings.Contains(out, "shallow clone: cannot tell whether this deploy takes commits back; git fetch --unshallow") {
		t.Fatalf("unknown previous SHA warning:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(d.state, "test-run")); err != nil {
		t.Fatalf("deploy did not continue: %v", err)
	}
}

func TestDeployTakeBackRecordsReasonWhenShallowComparisonIsUnavailable(t *testing.T) {
	d, head, _ := takeBackFixture(t, "test", "test-sha")
	writeFile(t, filepath.Join(d.work, ".git", "shallow"), head+"\n")
	const reason = "audit shallow"
	out := d.mustGraft("deploy", "--take-back", "--reason", reason, "test")
	for _, want := range []string{
		"shallow clone: cannot tell whether this deploy takes commits back; git fetch --unshallow",
		"take-back reason recorded: audit shallow (comparison unavailable: shallow clone)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if journal := readTakeBackJournal(t, d, "test-sha"); !strings.Contains(journal, "comparison unavailable: shallow clone") || !strings.Contains(journal, reason) {
		t.Fatalf("shallow take-back journal = %q", journal)
	}
}

func TestDeployTakeBackRecordsReasonWhenPreviousSHAIsUnknown(t *testing.T) {
	d := deployRepo(t)
	unknown := strings.Repeat("f", 40)
	writeFile(t, filepath.Join(d.state, "test-sha"), unknown+"\n")
	const reason = "audit missing SHA"
	out := d.mustGraft("deploy", "--take-back", "--reason", reason, "test")
	for _, want := range []string{
		"commit " + unknown[:12] + ", not in the local repository (git fetch?)",
		"take-back reason recorded: audit missing SHA (comparison unavailable: " + unknown[:12] + " not in the local repository)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if journal := readTakeBackJournal(t, d, "test-sha"); !strings.Contains(journal, "comparison unavailable: "+unknown[:12]+" not in the local repository") || !strings.Contains(journal, reason) {
		t.Fatalf("unknown-prev journal = %q", journal)
	}
}

func TestDeployWithNoTakeBackKeepsExistingSummaryAndRuns(t *testing.T) {
	d := deployRepo(t)
	prev := d.git("rev-parse", "HEAD")
	writeFile(t, filepath.Join(d.state, "test-sha"), prev+"\n")
	d.write("next.txt", "next\n")
	d.mustFixtureCommit("feat: PROJ-1 next")
	out := d.mustGraft("deploy", "test")
	if !strings.Contains(out, "brings 1 commit, takes none back") {
		t.Fatalf("existing summary missing:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(d.state, "test-run")); err != nil {
		t.Fatalf("target deploy command did not run: %v", err)
	}
}

func TestDeployTakeBackDoesNotOverrideApprovedReleaseRefusal(t *testing.T) {
	d, _, prev := takeBackFixture(t, "live", "live-sha")
	out, code := d.graft("", "deploy", "--take-back", "--reason", "intentional", "live")
	if code == 0 || !strings.Contains(out, "runs "+prev[:7]) || !strings.Contains(out, "which feature/PROJ-42-thing lacks; merge it first") {
		t.Fatalf("approved_release take-back refusal: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(d.state, "live-run")); !os.IsNotExist(err) {
		t.Fatalf("target deploy command ran after approved_release refusal: %v", err)
	}
}
