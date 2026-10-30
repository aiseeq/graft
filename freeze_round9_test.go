package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRound9BypassCollectsBranchAndItemRefusals(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("accepted.txt", "feat: PROJ-1 accepted work")
	trunk := d.freeze()
	d.git("switch", "-q", trunk)
	d.commitFile("late.txt", "feat: PROJ-3 PROJ-10 both late")

	out, _ := d.graft("", "deploy", "check", "acc")
	if !strings.Contains(out, "frozen at Product v1.2.10") || !strings.Contains(out, "PROJ-3") || !strings.Contains(out, "PROJ-10") {
		t.Errorf("check omitted branch or item refusal:\n%s", out)
	}
	out = d.mustGraft("deploy", "--past-freeze", "agreed", "acc")
	d.git("fetch", "-q", "origin", "refs/tags/freeze-log/*:refs/tags/freeze-log/*")
	journal := d.git("tag", "-l", "--format=%(contents)", "freeze-log/*")
	if !strings.Contains(journal, "PROJ-3") || !strings.Contains(journal, "PROJ-10") || !strings.Contains(out, "PROJ-3") || !strings.Contains(out, "PROJ-10") {
		t.Errorf("bypass did not name all refusals; output:\n%s\njournal:\n%s", out, journal)
	}
}

func TestRound9FreezeTagKeepsSubsecondTime(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("accepted.txt", "feat: PROJ-1 accepted work")
	d.env = append(d.env, "GRAFT_NOW=2026-10-20T10:00:00.500000000+03:00")
	d.mustGraft("deploy", "freeze", "prod", "Product v1.2.10")
	tag := "freeze/prod/1.2.10"
	d.git("fetch", "-q", "origin", "refs/tags/"+tag+":refs/tags/"+tag)
	if contents := d.git("tag", "-l", "--format=%(contents)", tag); !strings.Contains(contents, "2026-10-20T10:00:00.5+03:00") {
		t.Errorf("freeze tag lost its subsecond time:\n%s", contents)
	}
}

func TestRound9TwoLateKeysInOneCommit(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("accepted.txt", "feat: PROJ-1 accepted work")
	d.freeze()
	d.commitFile("late.txt", "feat: PROJ-3 PROJ-10 both late")
	out, _ := d.graft("", "deploy", "check", "acc")
	if !strings.Contains(out, "PROJ-3") || !strings.Contains(out, "PROJ-10") {
		t.Errorf("check omitted one refused item from a commit:\n%s", out)
	}
	out = d.mustGraft("deploy", "--past-freeze", "agreed", "acc")
	d.git("fetch", "-q", "origin", "refs/tags/freeze-log/*:refs/tags/freeze-log/*")
	journal := d.git("tag", "-l", "--format=%(contents)", "freeze-log/*")
	if !strings.Contains(out, "PROJ-3") || !strings.Contains(out, "PROJ-10") || !strings.Contains(journal, "PROJ-3") || !strings.Contains(journal, "PROJ-10") {
		t.Errorf("bypass omitted one refusal; output:\n%s\njournal:\n%s", out, journal)
	}
}

func TestRound9TrunkMergeDoesNotPushStaleFreezeTag(t *testing.T) {
	d := freezeRepo(t)
	configPath := filepath.Join(d.work, ".graft.yaml")
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	prodSHA := yamlQuote(filepath.Join(d.state, "prod-sha"))
	updated := strings.Replace(string(configData), "      deployed_sha: {path: "+prodSHA+"}\n      approved_release:",
		"      deployed_sha: {path: "+prodSHA+"}\n      merge_into_trunk: true\n      approved_release:", 1)
	if updated == string(configData) {
		t.Fatal("could not enable trunk merge in fixture")
	}
	if err := os.WriteFile(configPath, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	d.mustPlainCommit("feat: PROJ-1 enable trunk merge")
	d.git("push", "-q", "origin", "HEAD")
	d.commitFile("accepted.txt", "feat: PROJ-1 accepted work")
	d.freeze()
	tag := "freeze/prod/1.2.10"
	d.mustGraft("deploy", "thaw", "prod", "--reason", "test cleanup")
	d.git("tag", "-a", tag, "-m", "stale local tag")
	d.git("config", "push.followTags", "true")
	d.at("2026-10-20 14:00")
	d.mustGraft("deploy", "acc")
	d.mustGraft("deploy", "prod")
	if refs := d.git("ls-remote", "--tags", "origin", "refs/tags/"+tag); strings.TrimSpace(refs) != "" {
		t.Errorf("merge_into_trunk pushed stale freeze tag: %s", refs)
	}
}

func TestRound9FreezeRetryAfterBranchPushFailure(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("accepted.txt", "feat: PROJ-1 accepted work")
	hook := filepath.Join(d.remote("origin"), "hooks", "pre-receive")
	writeFile(t, hook, "#!/bin/sh\nwhile read old new ref; do case $ref in refs/heads/release/*) echo rejected >&2; exit 1;; esac; done\nexit 0\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	d.at("2026-10-20 10:00")
	_, code := d.graft("", "deploy", "freeze", "prod", "Product v1.2.10")
	if code == 0 {
		t.Fatal("freeze succeeded despite rejected branch push")
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if branches := d.git("branch", "--list", "release/1.2.10"); strings.TrimSpace(branches) != "" {
		t.Errorf("failed freeze left a local release branch: %q", branches)
	}
	out, code := d.graft("", "deploy", "freeze", "prod", "Product v1.2.10")
	if code != 0 {
		t.Errorf("freeze retry failed: exit %d\n%s", code, out)
	}
}

func TestRound9FailedDeployJournalIsRetriedBeforeFreezeEnds(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("accepted.txt", "feat: PROJ-1 accepted work")
	d.freeze()
	d.plainCommitFile("advisory.txt", "feat: PROJ-1 advisory failure")
	d.mustGraft("deploy", "acc")
	if err := os.WriteFile(filepath.Join(d.state, "lint-fails"), []byte("fail\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(d.remote("origin"), "hooks", "pre-receive")
	writeFile(t, hook, "#!/bin/sh\nwhile read old new ref; do case $ref in refs/tags/freeze-log/*) echo rejected >&2; exit 1;; esac; done\nexit 0\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := d.graft("", "deploy", "prod")
	if code == 0 {
		t.Fatal("deploy succeeded despite rejected advisory journal push")
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	local := d.git("tag", "-l", "freeze-log/prod/1.2.10/*")
	if strings.TrimSpace(local) == "" {
		t.Fatalf("failed journal record was not kept locally:\n%s", out)
	}
	if err := os.Remove(filepath.Join(d.state, "lint-fails")); err != nil {
		t.Fatal(err)
	}
	d.plainCommitFile("followup.txt", "feat: PROJ-1 follow-up")
	d.mustGraft("deploy", "acc")
	d.mustGraft("deploy", "prod")
	if tags := d.remoteTags("freeze/"); len(tags) != 0 {
		t.Errorf("freeze ended with tags still on the remote: %v", tags)
	}
	if body := d.jira.page("Product v1.2.10: release notes"); !strings.Contains(body, "advisory prod") {
		t.Errorf("release page missed the retried advisory record:\n%s", body)
	}
}

func TestRound9CommitTagFailureStillRecordsAdvisory(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("accepted.txt", "feat: PROJ-1 accepted work")
	d.freeze()
	configPath := filepath.Join(d.work, ".graft.yaml")
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(configData), "version: {mode: file, build_branches:", "version: {mode: file, tag_on_commit: true, build_branches:", 1)
	if updated == string(configData) {
		t.Fatal("could not enable version tags in fixture")
	}
	if err := os.WriteFile(configPath, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.state, "lint-fails"), []byte("fail\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapperDir := t.TempDir()
	wrapper := filepath.Join(wrapperDir, "git")
	writeFile(t, wrapper, "#!/bin/sh\nif [ \"$1 $2 $3\" = 'tag -a v1.0.2.1' ]; then echo 'forced tag failure' >&2; exit 1; fi\nexec "+yamlQuote(gitPath)+" \"$@\"\n")
	if err := os.Chmod(wrapper, 0o755); err != nil {
		t.Fatal(err)
	}
	d.env = append(d.env, "PATH="+wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, code := d.graft("", "commit", "-m", "feat: PROJ-1 commit with advisory")
	if code == 0 {
		t.Fatal("commit succeeded despite failed version tag")
	}
	if tags := d.remoteTags("freeze-log/"); len(tags) == 0 {
		t.Errorf("commit tag failure lost the advisory record; tags:\n%s\noutput:\n%s", tags, out)
	}
}
