package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestFreezeFindsActiveLaterRequiredTarget(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := freezeConfig(state, q)
		prod := "    prod:\n"
		insert := "    aaa:\n      env: local\n      run: [sh, deploy.sh]\n      requires: acc\n      deployed_sha: {path: " + q("aaa-sha") + "}\n      approved_release: {prefix: \"Product v\"}\n      freeze: {types: [Bug], advisory: [lint]}\n"
		return strings.Replace(cfg, prod, insert+prod, 1)
	})
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-1": {"10010"}})
	d.jira.setApprovers(map[string][]string{"10010": {"APPROVED", "APPROVED"}})
	d.git("remote", "set-head", "origin", d.git("branch", "--show-current"))
	trunk := d.git("branch", "--show-current")
	d.mustGraft("deploy", "acc")
	d.commitFile("accepted.txt", "feat: PROJ-1 accepted")
	d.at("2026-10-20 10:00")
	d.mustGraft("deploy", "freeze", "prod", "Product v1.2.10")
	d.git("switch", "-q", trunk)
	out, code := d.graft("", "deploy", "acc")
	if code == 0 || !strings.Contains(out, "release/1.2.10") {
		t.Fatalf("prod freeze was ignored behind inactive aaa freeze: exit %d\n%s", code, out)
	}
}

func TestFreezeAdvisoryJournalFailureFailsCommitAfterCommit(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("accepted.txt", "feat: PROJ-1 accepted")
	d.freeze()
	d.write("advisory.txt", "change\n")
	writeFile(t, filepath.Join(d.state, "lint-fails"), "")
	hook := filepath.Join(d.remote("origin"), "hooks", "update")
	writeFile(t, hook, "#!/bin/sh\ncase \"$1\" in refs/tags/freeze-log/*) exit 1;; esac\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := d.graft("", "commit", "-m", "fix: PROJ-8 advisory failure")
	if code == 0 || !strings.Contains(out, "commit is made and pushed") || !strings.Contains(out, "freeze journal lacks the advisory record") {
		t.Fatalf("advisory journal push failure was not reported after commit: exit %d\n%s", code, out)
	}
	if !strings.Contains(d.git("log", "-1", "--format=%s"), "PROJ-8 advisory failure") {
		t.Error("the commit was not created")
	}
	tags := strings.Fields(d.git("tag", "--list", "freeze-log/prod/1.2.10/*"))
	if len(tags) == 0 {
		t.Fatal("the failed advisory push removed the local journal tag")
	}
	tag := tags[0]
	if !strings.Contains(out, "git push origin refs/tags/"+tag) {
		t.Fatalf("failure does not give the command to retry %s: %s", tag, out)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	d.git("push", "origin", "refs/tags/"+tag)
	if !slices.Contains(d.remoteTags("freeze-log/prod/1.2.10/"), tag) {
		t.Fatalf("retry did not publish advisory journal tag %s", tag)
	}
}

func TestFreezeAdvisoryGatePreflightIsAdvisory(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := freezeConfig(state, q)
		cfg = strings.Replace(cfg, "gate:\n  - task: lint", "gate:\n  - task: gate-lint", 1)
		cfg = strings.Replace(cfg, "tasks:\n  lint:", "tasks:\n  gate-lint:\n    run: [[sh, -c, 'echo ${NEED_KEY}']]\n  lint:", 1)
		cfg = strings.Replace(cfg, "advisory: [lint]", "advisory: [gate-lint]", 1)
		return cfg
	})
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-1": {"10010"}})
	d.jira.setApprovers(map[string][]string{"10010": {"APPROVED", "APPROVED"}})
	d.git("remote", "set-head", "origin", d.git("branch", "--show-current"))
	d.mustGraft("deploy", "acc")
	d.commitFile("accepted.txt", "feat: PROJ-1 accepted")
	d.at("2026-10-20 10:00")
	d.mustGraft("deploy", "freeze", "prod", "Product v1.2.10")
	d.write("new.txt", "change\n")
	out := d.mustGraft("commit", "-m", "fix: PROJ-8 advisory missing key")
	if !strings.Contains(out, "gate-lint failed, advisory while Product v1.2.10 is frozen") {
		t.Fatalf("missing task key did not become an advisory failure:\n%s", out)
	}
}

func TestFreezeReplacesStaleLocalFreezeTag(t *testing.T) {
	d := freezeRepo(t)
	d.at("2026-10-20 10:00")
	d.git("tag", "-a", "freeze/prod/1.2.10", "HEAD", "-m", "stale local tag")
	out, code := d.graft("", "deploy", "freeze", "prod", "Product v1.2.10")
	if code != 0 {
		t.Fatalf("freeze could not replace a stale local tag: exit %d\n%s", code, out)
	}
}

func TestFreezeDoesNotRestoreDeletedRemoteBranchFromStaleRef(t *testing.T) {
	d := freezeRepo(t)
	trunk := d.git("branch", "--show-current")
	d.at("2026-10-20 10:00")
	d.mustGraft("deploy", "freeze", "prod", "Product v1.2.10")
	d.mustGraft("deploy", "thaw", "prod", "--reason", "test")
	d.git("push", "origin", ":refs/heads/release/1.2.10")
	d.git("switch", "-q", trunk)
	d.git("branch", "-D", "release/1.2.10")
	head := d.git("rev-parse", "HEAD")
	d.mustGraft("deploy", "freeze", "prod", "Product v1.2.10")
	got := d.git("rev-parse", "release/1.2.10")
	if got != head {
		t.Fatalf("freeze reused stale release branch at %s, want trunk %s", got, head)
	}
}

func TestFreezeCommitDoesNotFollowPushFollowTags(t *testing.T) {
	d := freezeRepo(t)
	trunk := d.freeze()
	d.mustGraft("deploy", "thaw", "prod", "--reason", "test")
	d.git("config", "push.followTags", "true")
	d.git("tag", "-a", "freeze/prod/1.2.10", "HEAD", "-m", "stale local freeze")
	d.git("switch", "-q", trunk)
	d.write("after-thaw.txt", "change\n")
	d.mustGraft("commit", "-m", "fix: PROJ-8 after thaw")
	if refs := d.git("ls-remote", "--tags", "origin", "refs/tags/freeze/prod/1.2.10"); refs != "" {
		t.Fatalf("graft commit followed a stale freeze tag: %s", refs)
	}
}

func TestFreezeRefusesTwoActiveFreezesForOneRequiredTarget(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := freezeConfig(state, q)
		prod := "    prod:\n"
		target := "    aaa:\n      env: local\n      run: [sh, deploy.sh]\n      deps: [lint]\n      requires: acc\n      deployed_sha: {path: " + q("aaa-sha") + "}\n      approved_release: {prefix: \"Product v\"}\n      freeze: {types: [Bug], advisory: [lint]}\n"
		return strings.Replace(cfg, prod, target+prod, 1)
	})
	d.jira.setVersions(productVersions(), map[string][]string{"PROJ-1": {"10010"}})
	d.jira.setApprovers(map[string][]string{"10010": {"APPROVED", "APPROVED"}})
	d.git("remote", "set-head", "origin", d.git("branch", "--show-current"))
	trunk := d.git("branch", "--show-current")
	d.mustGraft("deploy", "acc")
	d.at("2026-10-20 10:00")
	d.mustGraft("deploy", "freeze", "aaa", "Product v1.2.10")
	d.git("switch", "-q", trunk)
	d.mustGraft("deploy", "freeze", "prod", "Product v1.2.10")
	d.git("switch", "-q", trunk)
	out, code := d.graft("", "deploy", "acc")
	if code == 0 || !strings.Contains(out, "aaa") || !strings.Contains(out, "prod") {
		t.Fatalf("two active freezes did not refuse acc with both targets named: exit %d\n%s", code, out)
	}
}
