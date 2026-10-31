package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFreezeReportsEveryRefusedCommitAndJournalsAll(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("accepted.txt", "feat: PROJ-1 accepted work")
	d.freeze()
	d.commitFile("other.txt", "feat: PROJ-10 created late")
	d.commitFile("late.txt", "feat: PROJ-3 joined late")
	out, _ := d.graft("", "deploy", "check", "acc")
	if !strings.Contains(out, "PROJ-10") || !strings.Contains(out, "PROJ-3") {
		t.Fatalf("deploy check omitted one of the refused commits:\n%s", out)
	}
	d.mustGraft("deploy", "--past-freeze", "agreed", "acc")
	d.git("fetch", "-q", "origin", "refs/tags/freeze-log/*:refs/tags/freeze-log/*")
	journal := d.git("tag", "-l", "--format=%(contents)", "freeze-log/*")
	if !strings.Contains(journal, "PROJ-10") || !strings.Contains(journal, "PROJ-3") {
		t.Fatalf("bypass journal omitted one of the refused commits:\n%s", journal)
	}
}

func TestFreezeCommitPushFailureStillRecordsAdvisory(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("accepted.txt", "feat: PROJ-1 accepted work")
	d.freeze()
	writeFile(t, filepath.Join(d.state, "lint-fails"), "")
	hook := filepath.Join(d.remote("origin"), "hooks", "pre-receive")
	writeFile(t, hook, "#!/bin/sh\nwhile read old new ref; do case $ref in refs/heads/*) echo rejected >&2; exit 1;; esac; done\nexit 0\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	d.write("fix.txt", "fix\n")
	out, code := d.graft("", "commit", "-m", "fix: PROJ-8 push fails")
	if code == 0 || !strings.Contains(out, "recorded freeze advisory") || !strings.Contains(out, "lint failed, advisory") {
		t.Fatalf("push failure did not report the missing advisory journal record: exit %d\n%s", code, out)
	}
	if len(d.remoteTags("freeze-log/prod/1.2.10/")) == 0 {
		t.Fatal("advisory record was lost after the branch push failed")
	}
}

func TestFreezeThawRetainsFreezeWhenJournalDeletionFails(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("accepted.txt", "feat: PROJ-1 accepted work")
	d.freeze()
	writeFile(t, filepath.Join(d.state, "lint-fails"), "")
	d.write("fix.txt", "fix\n")
	d.mustGraft("commit", "-m", "fix: PROJ-8 advisory record")
	hook := filepath.Join(d.remote("origin"), "hooks", "update")
	writeFile(t, hook, "#!/bin/sh\ncase \"$1\" in refs/tags/freeze-log/*) exit 1;; esac\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := d.graft("", "deploy", "thaw", "prod", "--reason", "retry")
	if code == 0 || !strings.Contains(out, "freeze") {
		t.Fatalf("thaw did not report the failed journal deletion: exit %d\n%s", code, out)
	}
	if len(d.remoteTags("freeze/prod/")) == 0 {
		t.Fatal("freeze tag was deleted before the journal deletion completed")
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	d.mustGraft("deploy", "thaw", "prod", "--reason", "retry")
	if len(d.remoteTags("freeze/prod/")) != 0 || len(d.remoteTags("freeze-log/prod/1.2.10/")) != 0 {
		t.Fatal("second thaw did not remove the freeze and its journal")
	}
}

func TestFreezeAmendKeepsAdvisoryTagLocal(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("accepted.txt", "feat: PROJ-1 accepted work")
	d.freeze()
	writeFile(t, filepath.Join(d.state, "lint-fails"), "")
	hook := filepath.Join(d.remote("origin"), "hooks", "pre-receive")
	writeFile(t, hook, "#!/bin/sh\nwhile read old new ref; do case $ref in refs/heads/*) echo rejected >&2; exit 1;; esac; done\nexit 0\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	d.write("fix.txt", "first\n")
	if out, code := d.graft("", "commit", "-m", "fix: PROJ-8 first"); code == 0 || !strings.Contains(out, "push failed") {
		t.Fatalf("initial unpublished commit push unexpectedly succeeded: exit %d\n%s", code, out)
	}
	before := d.remoteTags("freeze-log/prod/1.2.10/")
	d.write("fix.txt", "second\n")
	d.git("add", "fix.txt")
	out := d.mustGraft("amend")
	if !strings.Contains(out, "not pushed") {
		t.Fatalf("amend output does not explain that the commit was not pushed:\n%s", out)
	}
	after := d.remoteTags("freeze-log/prod/1.2.10/")
	if len(after) != len(before)-1 {
		t.Fatalf("amend did not remove the previous commit's advisory tag: before=%v after=%v", before, after)
	}
	local := d.git("tag", "--list", "freeze-log/prod/1.2.10/*")
	short := d.git("rev-parse", "--short", "HEAD")
	if !strings.Contains(local, strings.TrimSpace(short)) {
		t.Fatalf("amend did not keep its advisory record for %s locally; before=%v after=%q output=%s", short, before, local, out)
	}
}

func TestCommitStillFollowsAnnotatedTagsWithoutFreeze(t *testing.T) {
	f := fileModeRepo(t, gateStep("ok"), "origin")
	f.mustGraft("commit", "-m", "feat: initial")
	f.git("config", "push.followTags", "true")
	f.git("tag", "-a", "companion", "HEAD", "-m", "companion")
	f.write("main.go", "package main // changed\n")
	f.mustGraft("commit", "-m", "feat: PROJ-42 update")
	if got := f.git("ls-remote", "--tags", "origin", "refs/tags/companion"); got == "" {
		t.Fatal("push without a configured freeze stopped following annotated tags")
	}
}

func TestFreezePreflightChecksAdvisoryOfInactiveFreeze(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := freezeConfig(state, q)
		prod := "    prod:\n"
		aaa := "    aaa:\n      env: local\n      run: [sh, deploy.sh]\n      requires: acc\n      deployed_sha: {path: " + q("aaa-sha") + "}\n      approved_release: {prefix: \"Product v\"}\n      freeze: {types: [Bug], advisory: [extra]}\n"
		cfg = strings.Replace(cfg, prod, aaa+prod, 1)
		cfg = strings.Replace(cfg, "deps: [lint]", "deps: [lint, extra]", 1)
		cfg = strings.Replace(cfg, "tasks:\n  lint:", "tasks:\n  extra:\n    run: [[sh, -c, 'echo ${NEED_KEY}']]\n  lint:", 1)
		return cfg
	})
	d.git("remote", "set-head", "origin", d.git("branch", "--show-current"))
	d.env = append(d.env, "NEED_KEY=present")
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-1": {"10010"}})
	d.jira.setApprovers(map[string][]string{"10010": {"APPROVED", "APPROVED"}})
	d.jira.mu.Lock()
	d.jira.created = map[string]time.Time{"PROJ-1": time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)}
	d.jira.fixChanges = map[string][]fakeFixChange{"PROJ-1": {{at: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC), id: "10010", added: true}}}
	d.jira.mu.Unlock()
	d.mustGraft("deploy", "acc")
	writeFile(t, filepath.Join(d.state, "prod-version"), d.stateFile("acc-version"))
	writeFile(t, filepath.Join(d.state, "prod-sha"), d.stateFile("acc-sha"))
	d.commitFile("accepted.txt", "feat: PROJ-1 accepted")
	d.at("2026-10-20 10:00")
	d.mustGraft("deploy", "freeze", "prod", "Product v1.2.10")
	d.env = withoutEnv(d.env, "NEED_KEY")
	out, code := d.graft("", "deploy", "acc")
	if code == 0 || !strings.Contains(out, "NEED_KEY") {
		t.Fatalf("missing key from inactive freeze advisory escaped early preflight: exit %d\n%s", code, out)
	}
}

func withoutEnv(env []string, key string) []string {
	filtered := env[:0]
	for _, item := range env {
		if !strings.HasPrefix(item, key+"=") {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func TestFreezeRefusesMultipleTagsForTarget(t *testing.T) {
	d := freezeRepo(t)
	d.freeze()
	d.git("tag", "-a", "freeze/prod/9.9.9", "HEAD", "-m", "version: Product v9.9.9", "-m", "branch: release/9.9.9", "-m", "time: 2026-10-20T10:00:00Z")
	d.git("push", "origin", "refs/tags/freeze/prod/9.9.9")
	out, code := d.graft("", "deploy", "freeze", "prod", "Product v1.3.0")
	if code == 0 || !strings.Contains(out, "freeze/prod/1.2.10") || !strings.Contains(out, "freeze/prod/9.9.9") {
		t.Fatalf("multiple remote freeze tags were not named and refused: exit %d\n%s", code, out)
	}
}

func TestFreezeRefusesSecondTargetRequiringFrozenTarget(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := freezeConfig(state, q)
		prod := "    prod:\n"
		prod2 := "    prod2:\n      env: local\n      run: [sh, deploy.sh]\n      deps: [lint]\n      requires: acc\n      deployed_sha: {path: " + q("prod2-sha") + "}\n      approved_release: {prefix: \"Product v\"}\n      freeze: {types: [Bug], advisory: [lint]}\n"
		return strings.Replace(cfg, prod, prod2+prod, 1)
	})
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-1": {"10010"}})
	d.jira.setApprovers(map[string][]string{"10010": {"APPROVED", "APPROVED"}})
	d.git("remote", "set-head", "origin", d.git("branch", "--show-current"))
	trunk := d.git("branch", "--show-current")
	d.mustGraft("deploy", "freeze", "prod", "Product v1.2.10")
	d.git("switch", "-q", trunk)
	out, code := d.graft("", "deploy", "freeze", "prod2", "Product v1.2.10")
	if code == 0 || !strings.Contains(out, "prod") || !strings.Contains(out, "prod2") {
		t.Fatalf("second freeze over the same required target was not refused with both names: exit %d\n%s", code, out)
	}
}
