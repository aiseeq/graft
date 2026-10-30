package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/jira"
)

func TestFreezeUsesTheIssueVersionID(t *testing.T) {
	issue := jira.Issue{FixVersions: []jira.Version{
		{ID: "other-project-id", Name: "Product v1.2.10"},
	}}
	if got := issue.FixVersionID("Product v1.2.10"); got != "other-project-id" {
		t.Fatalf("freeze issue version id = %q, want item's own id", got)
	}
}

func TestReplaceFreezeJournalWithinBuildSection(t *testing.T) {
	first := "<h2>Product v1.2.10</h2><ul><li>items</li></ul><h3>Freeze journal</h3><ul><li>old</li></ul>"
	second := "<h2>Product v1.2.11</h2><ul><li>other items</li></ul><h3>Freeze journal</h3><ul><li>keep</li></ul>"
	body := first + second
	build := "<h2>Product v1.2.10</h2>"
	journal := "<h3>Freeze journal</h3><ul><li>new</li></ul>"
	body = replaceFreezeJournal(body, build, "<h3>Freeze journal</h3>", journal)
	body = replaceFreezeJournal(body, build, "<h3>Freeze journal</h3>", journal)
	if got := strings.Count(body, "<h3>Freeze journal</h3>"); got != 2 {
		t.Fatalf("freeze journal headings = %d, want one for each build: %s", got, body)
	}
	firstBuild := body[:strings.Index(body, "<h2>Product v1.2.11</h2>")]
	if !strings.Contains(firstBuild, "<li>new</li>") || strings.Contains(firstBuild, "<li>old</li>") {
		t.Fatalf("first build journal was not replaced in place: %s", firstBuild)
	}
	if !strings.Contains(body, "<li>keep</li>") {
		t.Fatalf("second build journal changed: %s", body)
	}
}

func TestFreezeJournalLineOmitsTagHeadersAndSignature(t *testing.T) {
	dir := t.TempDir()
	gitTest := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	gitTest("init", "-q")
	gitTest("config", "user.name", "Test User")
	gitTest("config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest("add", "file")
	gitTest("commit", "-q", "-m", "base")
	for name, signed := range map[string]bool{"plain": false, "signed": true} {
		tag := "freeze-log/prod/1.2.10/20261020T100000-abcdef1-advisory-" + name
		args := []string{"tag", "-a", tag, "-m", "target: prod", "-m", "commit: abcdef1234567890", "-m", "reason: review"}
		if signed {
			args = append(args, "-m", "-----BEGIN PGP SIGNATURE-----\nsynthetic\n-----END PGP SIGNATURE-----")
		}
		gitTest(args...)
		contents := gitTest("cat-file", "tag", tag)
		line, err := freezeJournalLine(tag, contents)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"tagger", "@", "object ", "PGP"} {
			if strings.Contains(line, forbidden) {
				t.Errorf("%s journal line includes %q: %s", name, forbidden, line)
			}
		}
	}
}

func TestFreezeJournalListsRetriedCommitAdvisory(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	remote := filepath.Join(root, "remote.git")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	gitTest := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	gitTest(root, "init", "--bare", "-q", remote)
	gitTest(work, "init", "-q")
	gitTest(work, "config", "user.name", "Test User")
	gitTest(work, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(work, "file"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(work, "add", "file")
	gitTest(work, "commit", "-q", "-m", "base")
	gitTest(work, "remote", "add", "origin", remote)
	tag := "freeze-log/prod/1.2.10/20261020T100000-abcdef1-advisory-lint"
	gitTest(work, "tag", "-a", tag, "-m", "target: prod", "-m", "commit: abcdef1234567890", "-m", "reason: lint failed")
	gitTest(work, "push", "origin", "refs/tags/"+tag)
	repo, err := gitx.Open(work)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Deploy: &config.Deploy{Remote: "origin"}}
	app := &App{}
	journal, err := app.freezeJournal(repo, cfg, &activeFreeze{target: "prod", version: "1.2.10"}, &config.ReleasePage{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(journal, "lint failed") {
		t.Fatalf("freeze journal omitted retried advisory record: %s", journal)
	}
}
