package main

// Ticket requirement regression cases use synthetic repositories.

import (
	"maps"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
)

const ticketDeploy = `deploy:
  remote: origin
  targets:
    prod: {env: local, run: [sh, d.sh], deployed_sha: {path: /x/prod}, approved_release: {statuses: [Done], ignore_paths: ["docs/**"]}}
release_notes: {jira: {project_keys: [PROJ]}}
`

func ticketRepo(t *testing.T, ticket, extraVersion, deploy, gate string, files map[string]string) *fixture {
	t.Helper()
	all := map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file" + extraVersion + "}\nticket: " + ticket + "\nenvs: {local: {}}\n" + deploy + "gate: " + gate + "\n",
		"VERSION":     "1.0.0\n",
	}
	maps.Copy(all, files)
	f := newRepo(t, all, "origin")
	f.git("checkout", "-q", "-b", "main")
	f.mustGraft("commit", "-m", "feat: PROJ-1 base")
	return f
}

const ticketReqProj = "{pattern: 'PROJ-[0-9]+', required: true}"

// F1: a work-tree rename (" R") of a short path out of ignore_paths: the
// source "a.c" (3 chars) is dropped, the commit passes without a key though
// it deletes a.c outside docs/.
func TestTicketRequiredF1WorktreeRenameShortSource(t *testing.T) {
	f := ticketRepo(t, ticketReqProj, "", ticketDeploy, "[]", map[string]string{"a.c": "int x;\n"})
	f.run(f.work, "sh", "-c", "mkdir docs && mv a.c docs/a.c")
	f.git("add", "-N", "docs/a.c")
	t.Logf("status: %q", f.git("status", "--porcelain"))
	out, code := f.graft("", "commit", "-m", "chore: move")
	t.Logf("OBSERVED exit %d\n%s", code, out)
	t.Logf("committed files: %s", f.git("show", "--name-status", "--format=", "HEAD"))
	if code == 0 {
		t.Errorf("a commit deleting a.c (outside docs/**) passed without a work item")
	}
}

// F7: a docs-only work-tree rename is refused: the source path is parsed as
// an entry of its own and cut to "s/a.md".
func TestTicketRequiredF7WorktreeRenameDocsOnly(t *testing.T) {
	f := ticketRepo(t, ticketReqProj, "", ticketDeploy, "[]", map[string]string{"docs/a.md": "a\n"})
	f.git("mv", "docs/a.md", "docs/b.md")
	f.git("reset", "-q")
	f.git("add", "-N", "docs/b.md")
	t.Logf("status: %q", f.git("status", "--porcelain"))
	out, code := f.graft("", "commit", "-m", "docs: rename")
	t.Logf("OBSERVED exit %d\n%s", code, out)
	if code != 0 {
		t.Errorf("a docs-only rename was refused")
	}
}

// F2: an anchored ticket pattern finds the key in the branch but is matched
// again against the whole message, where ^...$ fails.
func TestTicketRequiredF2AnchoredPattern(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: file}\nticket: {pattern: '^PROJ-[0-9]+$', required: true}\nenvs: {local: {}}\n" + ticketDeploy + "gate: []\n",
		"VERSION":     "1.0.0\n",
	}, "origin")
	f.git("checkout", "-q", "-b", "main")
	out, code := f.graft("", "commit", "-m", "feat: PROJ-1 base")
	t.Logf("on main, message 'feat: PROJ-1 base': OBSERVED exit %d\n%s", code, out)
	f.git("checkout", "-q", "-b", "PROJ-42")
	f.write("change.txt", "change\n")
	out, code = f.graft("", "commit", "-m", "fix: thing")
	t.Logf("branch PROJ-42, 'fix: thing': OBSERVED exit %d\n%s", code, out)
	if code != 0 {
		t.Errorf("the branch PROJ-42 names the item, but the commit was refused")
	}
}

func TestTicketRequiredFirstCommitWithoutHead(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\nticket: {pattern: 'PROJ-[0-9]+', required: true}\ngate: []\n",
		"base.txt":    "base\n",
	}, "origin")
	f.git("checkout", "-q", "-b", "main")
	if out, code := f.graft("", "commit", "-m", "chore: base"); code == 0 || !strings.Contains(out, "ticket.required") {
		t.Fatalf("first commit without a key passed: %s", out)
	}
	f.mustGraft("commit", "-m", "chore: PROJ-1 base")
}

func TestTicketRequiredHashPattern(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\nticket: {pattern: '#[0-9]+', required: true}\ngate: []\n",
		"base.txt":    "base\n",
	}, "origin")
	f.git("checkout", "-q", "-b", "main")
	f.mustGraft("commit", "-m", "chore: #1 base")
	f.write("change.txt", "change\n")
	f.mustGraft("commit", "-m", "feat: #123 task")
}

// F8: the unanchored pattern counts SHA-256 / XPROJ-1 as a work item.
func TestTicketRequiredF8FalseKey(t *testing.T) {
	f := ticketRepo(t, "{pattern: '[A-Z][A-Z]+-[0-9]+', required: true}", "", ticketDeploy, "[]", nil)
	f.write("a.txt", "a\n")
	out, code := f.graft("", "commit", "-m", "fix: SHA-256 digest")
	t.Logf("default pattern, 'fix: SHA-256 digest': OBSERVED exit %d\n%s", code, out)
	if code == 0 {
		t.Errorf("SHA-256 passed as a work item")
	}
	g := ticketRepo(t, ticketReqProj, "", ticketDeploy, "[]", nil)
	g.write("a.txt", "a\n")
	out, code = g.graft("", "commit", "-m", "fix: XPROJ-1 thing")
	t.Logf("pattern PROJ-[0-9]+, 'fix: XPROJ-1 thing': OBSERVED exit %d\n%s", code, out)
	if code == 0 {
		t.Errorf("XPROJ-1 passed as a PROJ work item")
	}
}

// F3/F9: a version.sync file is dropped whole: a dependency change in
// package.json plus a doc passes without a key.
func TestTicketRequiredF3SyncFileHidesEdits(t *testing.T) {
	sync := ", sync: [{path: package.json, format: json, key: [version]}]"
	f := ticketRepo(t, ticketReqProj, sync, ticketDeploy, "[]", map[string]string{
		"package.json": "{\n  \"version\": \"1.0.0\",\n  \"dependencies\": {}\n}\n",
	})
	f.write("package.json", "{\n  \"version\": \"1.0.1\",\n  \"dependencies\": {\"left-pad\": \"1.0.0\"}\n}\n")
	f.write("docs/x.md", "x\n")
	out, code := f.graft("", "commit", "-m", "chore: deps")
	t.Logf("OBSERVED exit %d\n%s", code, out)
	if code == 0 {
		t.Logf("committed package.json diff:\n%s", f.git("show", "--format=", "HEAD", "--", "package.json"))
		t.Errorf("a dependency change passed without a work item")
	}
}

// F4: the check runs before the gate; files the gate writes are added by
// git add -A and never checked.
func TestTicketRequiredF4GateWritesFiles(t *testing.T) {
	f := ticketRepo(t, ticketReqProj, "", ticketDeploy, "[[sh, -c, 'ls docs > gen.go 2>/dev/null; true']]", nil)
	f.write("docs/guide.md", "g\n")
	out, code := f.graft("", "commit", "-m", "docs: guide")
	t.Logf("OBSERVED exit %d\n%s", code, out)
	files := f.git("show", "--name-only", "--format=", "HEAD")
	t.Logf("committed files: %s", files)
	if code == 0 && strings.Contains(files, "gen.go") {
		t.Errorf("gen.go (outside docs/**) went in without a work item")
	}
}

// F5: a staged change undone in the work tree (MM) is listed though git add
// -A drops it: a docs-only commit is refused.
func TestTicketRequiredF5StagedThenReverted(t *testing.T) {
	f := ticketRepo(t, ticketReqProj, "", ticketDeploy, "[]", map[string]string{"code.txt": "c\n"})
	f.write("code.txt", "c2\n")
	f.git("add", "code.txt")
	f.write("code.txt", "c\n")
	f.write("docs/guide.md", "g\n")
	t.Logf("status: %q", f.git("status", "--porcelain"))
	out, code := f.graft("", "commit", "-m", "docs: guide")
	t.Logf("OBSERVED exit %d\n%s", code, out)
	if code != 0 {
		f.git("add", "-A")
		t.Logf("what git add -A would commit: %s", f.git("diff", "--cached", "--name-only"))
		t.Errorf("a commit that would change only docs/ was refused")
	}
}

// F6: ignore_paths of all targets are merged; the deploy gate checks each
// target against its own list.
func TestTicketRequiredF6UnionOfTargets(t *testing.T) {
	cases := []struct {
		name, deploy string
		files        map[string]string
	}{
		{"stage without ignore_paths", `deploy:
  remote: origin
  targets:
    prod: {env: local, run: [sh, d.sh], deployed_sha: {path: /x/prod}, approved_release: {statuses: [Done], ignore_paths: ["docs/**"]}}
    stage: {env: local, run: [sh, d.sh], deployed_sha: {path: /x/stage}, approved_release: {statuses: [Done]}}
release_notes: {jira: {project_keys: [PROJ]}}
`, map[string]string{"docs/x.md": "x\n"}},
		{"split between targets", `deploy:
  remote: origin
  targets:
    prod: {env: local, run: [sh, d.sh], deployed_sha: {path: /x/prod}, approved_release: {statuses: [Done], ignore_paths: ["docs/**"]}}
    stage: {env: local, run: [sh, d.sh], deployed_sha: {path: /x/stage}, approved_release: {statuses: [Done], ignore_paths: ["README.md"]}}
release_notes: {jira: {project_keys: [PROJ]}}
`, map[string]string{"docs/a.txt": "a\n", "README.md": "r\n"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := ticketRepo(t, ticketReqProj, "", c.deploy, "[]", nil)
			for k, v := range c.files {
				f.write(k, v)
			}
			out, code := f.graft("", "commit", "-m", "docs: no item")
			t.Logf("OBSERVED exit %d\n%s", code, out)
			if code != 0 {
				return
			}
			cfg, err := config.Load(f.work)
			if err != nil {
				t.Fatal(err)
			}
			var files []string
			for p := range strings.FieldsSeq(f.git("show", "--name-only", "--format=", "HEAD")) {
				if p != "VERSION" {
					files = append(files, p)
				}
			}
			var held []string
			for _, n := range []string{"prod", "stage"} {
				if !cfg.Deploy.Targets[n].ApprovedRelease.Ignored(files) {
					held = append(held, n)
				}
			}
			t.Logf("commit files %v; deploy gate (per-target Ignored) holds it on: %v", files, held)
			if len(held) > 0 {
				t.Errorf("the commit passed ticket.required, but approved_release holds it on %v", held)
			}
		})
	}
}

func TestGateListsNewCommitFile(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\ngate: [[sh, -c, 'test ! -e new.txt || git ls-files | grep -qx new.txt']]\n",
	}, "origin")
	f.git("checkout", "-q", "-b", "main")
	f.mustGraft("commit", "-m", "chore: base")
	f.write("new.txt", "new\n")
	f.mustGraft("commit", "-m", "feat: add file")
	if !strings.Contains(f.git("show", "--name-only", "--format=", "HEAD"), "new.txt") {
		t.Fatal("new file was not included in the commit")
	}
}

func TestFailedGateRestoresIndex(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\ngate: []\n",
	}, "origin")
	f.git("checkout", "-q", "-b", "main")
	f.mustGraft("commit", "-m", "chore: base")
	f.write(".graft.yaml", "schema: 1\nversion: {mode: none}\ngate: [[sh, -c, 'exit 1']]\n")
	f.write("new.txt", "new\n")
	if out, code := f.graft("", "commit", "-m", "feat: fail gate"); code == 0 {
		t.Fatalf("commit passed the failing gate: %s", out)
	}
	if _, code := f.tryGit("diff", "--cached", "--quiet"); code != 0 {
		t.Fatalf("failed gate left staged changes: %s", f.git("diff", "--cached", "--name-only"))
	}
}

// A key after a word the pattern also matches (SHA-256) still names the item,
// and so does a key in the scope of the subject.
func TestTicketRequiredKeyAfterFalseKey(t *testing.T) {
	f := ticketRepo(t, "{pattern: '[A-Z][A-Z]+-[0-9]+', required: true}", "", ticketDeploy, "[]", nil)
	f.write("a.txt", "a\n")
	f.mustGraft("commit", "-m", "fix: SHA-256 digest for PROJ-12")
	f.write("b.txt", "b\n")
	f.mustGraft("commit", "-m", "fix(PROJ-13): b")
}

// graft amend checks the amended commit whole, against its parent: docs added
// to a commit of code that names no item do not let it through.
func TestTicketRequiredAmendChecksWholeCommit(t *testing.T) {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\nticket: {pattern: 'PROJ-[0-9]+', required: true, skip_branches: ['wip/*']}\nenvs: {local: {}}\n" + ticketDeploy + "gate: []\n",
	})
	f.git("checkout", "-q", "-b", "wip/x")
	f.write("code.txt", "c\n")
	f.mustGraft("commit", "-m", "feat: code")
	f.git("branch", "-m", "feature-x")
	f.write("docs/guide.md", "g\n")
	if out, code := f.graft("", "amend"); code == 0 || !strings.Contains(out, "ticket.required") {
		t.Fatalf("an amend of code without a key passed on docs alone: exit %d\n%s", code, out)
	}
	if _, code := f.tryGit("diff", "--cached", "--quiet"); code != 0 {
		t.Errorf("the refused amend left staged changes: %s", f.git("diff", "--cached", "--name-only"))
	}
}
