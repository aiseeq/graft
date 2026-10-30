package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRound10StaleJournalDoesNotReturnAfterRefreeze(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("one.txt", "feat: PROJ-1 accepted work")
	trunk := d.freeze()
	d.commitFile("other.txt", "feat: PROJ-6 another product")
	d.mustGraft("deploy", "--past-freeze", "OLD cancelled freeze", "acc")
	old := d.remoteTags("freeze-log/prod/1.2.10/")
	if len(old) != 1 {
		t.Fatalf("old journal = %q", old)
	}
	d.git("fetch", "-q", "origin")
	d.git("push", "-q", "origin", ":refs/tags/freeze/prod/1.2.10", ":refs/tags/"+old[0])
	d.git("switch", "-q", trunk)
	d.at("2026-10-21 10:00")
	d.mustGraft("deploy", "freeze", "prod", "Product v1.2.10")
	d.git("switch", "-q", "release/1.2.10")
	d.at("2026-10-21 14:00")
	d.mustGraft("deploy", "--redeploy", "--past-freeze", "NEW", "acc")
	for _, tag := range d.remoteTags("freeze-log/prod/1.2.10/") {
		if tag == old[0] {
			t.Errorf("cancelled freeze's bypass %s returned in the new journal", tag)
		}
	}
}

func TestRound10FailedJournalRemoteCheckKeepsLocalRecord(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("accepted.txt", "feat: PROJ-1 accepted work")
	d.freeze()
	d.commitFile("late.txt", "feat: PROJ-3 late item")
	marker := filepath.Join(d.state, "acc-run")
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapperDir := t.TempDir()
	wrapper := filepath.Join(wrapperDir, "git")
	writeFile(t, wrapper, "#!/bin/sh\nif [ -e "+yamlQuote(marker)+" ] && [ \"$1\" = ls-remote ]; then\n  for arg do case \"$arg\" in refs/tags/freeze-log/*) echo 'remote check failed' >&2; exit 1;; esac; done\nfi\nexec "+yamlQuote(gitPath)+" \"$@\"\n")
	if err := os.Chmod(wrapper, 0o755); err != nil {
		t.Fatal(err)
	}
	d.env = append(d.env, "PATH="+wrapperDir+string(os.PathListSeparator)+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, code := d.graft("", "deploy", "--past-freeze", "accepted exception", "acc")
	if code == 0 {
		t.Fatalf("deploy hid the journal lookup failure:\n%s", out)
	}
	if tags := d.git("tag", "-l", "freeze-log/prod/1.2.10/*"); strings.TrimSpace(tags) == "" {
		t.Errorf("remote check failure left no local freeze journal tag:\n%s", out)
	}
}

func TestRound10CheckHeadFailureStillJournalsBypass(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("accepted.txt", "feat: PROJ-1 accepted work")
	d.freeze()
	d.commitFile("late.txt", "feat: PROJ-3 late item")
	d.env = append(d.env, "MOVE_HEAD=1")
	out, code := d.graft("", "deploy", "--past-freeze", "accepted exception", "acc")
	if code == 0 || !strings.Contains(out, "the deployed commit is not recorded") {
		t.Fatalf("expected the post-script check-head error, got exit %d:\n%s", code, out)
	}
	if tags := d.remoteTags("freeze-log/prod/1.2.10/"); len(tags) == 0 {
		t.Errorf("post-script check-head error lost its bypass record:\n%s", out)
	}
}
