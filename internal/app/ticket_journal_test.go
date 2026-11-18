package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/gittest"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/message"
)

func ticketJournalRepo(t *testing.T) *gitx.Repo {
	t.Helper()
	root := t.TempDir()
	gittest.Run(t, root, "init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(root, "docs", "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo, err := gitx.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func ticketJournalStub(t *testing.T, json string, failNote bool) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "argv")
	stub := filepath.Join(dir, "tk")
	script := "#!/bin/sh\nif [ \"$1\" = ls ]; then printf '%s' \"$TICKETS_JSON\"; exit 0; fi\nprintf '%s\\n' \"$@\" >> \"$TK_ARGV\"\n"
	if failNote {
		script += "exit 1\n"
	}
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TK_ARGV", log)
	t.Setenv("TICKETS_JSON", json)
	return log
}

func TestTicketJournalNamedSlugAndActiveSelection(t *testing.T) {
	t.Run("slug", func(t *testing.T) {
		repo := ticketJournalRepo(t)
		log := ticketJournalStub(t, `[{"path":"docs/tasks/ticket.md","slug":"proj-123-fix","status":"в работе"}]`, false)
		var stderr bytes.Buffer
		(&App{Stderr: &stderr}).ticketJournal(context.Background(), repo, "fix: proj-123-fix thing", "commit abcdef123456 on work: fix: thing")
		got, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), "proj-123-fix\n") || !strings.Contains(string(got), "commit abcdef123456 on work: fix: thing") {
			t.Fatalf("unexpected tk argv %q", got)
		}
	})
	t.Run("one active and ambiguity", func(t *testing.T) {
		repo := ticketJournalRepo(t)
		log := ticketJournalStub(t, `[{"path":"docs/tasks/one.md","slug":"one","status":"в работе"}]`, false)
		(&App{}).ticketJournal(context.Background(), repo, "unrelated", "commit abc")
		if got, err := os.ReadFile(log); err != nil || !strings.Contains(string(got), "one\n") {
			t.Fatalf("single active ticket: %q, %v", got, err)
		}
		log = ticketJournalStub(t, `[{"path":"docs/tasks/one.md","slug":"one","status":"в работе"},{"path":"docs/tasks/two.md","slug":"two","status":"в работе"}]`, false)
		(&App{}).ticketJournal(context.Background(), repo, "unrelated", "commit abc")
		if _, err := os.Stat(log); !os.IsNotExist(err) {
			t.Fatalf("ambiguous tickets called tk note: %v", err)
		}
	})
}

func TestTicketJournalUnavailableAndFailure(t *testing.T) {
	repo := ticketJournalRepo(t)
	var stderr bytes.Buffer
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir()
	if err := os.Symlink(gitPath, filepath.Join(path, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", path)
	(&App{Stderr: &stderr}).ticketJournal(context.Background(), repo, "proj-123-fix", "commit abc")
	if stderr.Len() != 0 {
		t.Fatalf("missing tk warned: %q", stderr.String())
	}
	if err := os.RemoveAll(filepath.Join(repo.Root, "docs", "tasks")); err != nil {
		t.Fatal(err)
	}
	(&App{Stderr: &stderr}).ticketJournal(context.Background(), repo, "proj-123-fix", "commit abc")
	if stderr.Len() != 0 {
		t.Fatalf("missing tasks warned: %q", stderr.String())
	}
	if err := os.MkdirAll(filepath.Join(repo.Root, "docs", "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", path)
	log := ticketJournalStub(t, `[{"path":"docs/tasks/ticket.md","slug":"proj-123-fix","status":"в работе"}]`, true)
	(&App{Stderr: &stderr}).ticketJournal(context.Background(), repo, "proj-123-fix", "commit abc")
	if !strings.Contains(stderr.String(), "ticket journal was not written") {
		t.Fatalf("tk failure did not warn: %q", stderr.String())
	}
	if _, err := os.Stat(log); err != nil {
		t.Fatalf("expected fake tk call: %v", err)
	}
}

func TestTicketJournalDeployNote(t *testing.T) {
	repo := ticketJournalRepo(t)
	log := ticketJournalStub(t, `[{"path":"docs/tasks/ticket.md","slug":"proj-123-fix","status":"в работе"}]`, false)
	(&App{}).ticketJournal(context.Background(), repo, "", "deploy staging v1.2.3: ok")
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "deploy staging v1.2.3: ok") {
		t.Fatalf("missing deploy outcome: %q", got)
	}
}

func TestDeployJournalIsWrittenAfterDeploy(t *testing.T) {
	f := newSyncPushFixture(t, false)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	config := "schema: 1\nversion: {mode: none}\nenvs:\n  local: {}\ndeploy:\n  remote: origin\n  targets:\n    stage:\n      env: local\n      run: [sh, deploy.sh]\n"
	for path, contents := range map[string]string{
		".graft.yaml":          config,
		"deploy.sh":            "#!/bin/sh\nexit 0\n",
		"docs/tasks/ticket.md": "synthetic ticket\n",
	} {
		full := filepath.Join(f.work, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.git(f.work, "add", ".")
	f.git(f.work, "commit", "-q", "-m", "deploy fixture")
	f.git(f.work, "push", "-q", "origin", "HEAD")
	log := ticketJournalStub(t, `[{"path":"docs/tasks/ticket.md","slug":"proj-123-fix","status":"в работе"}]`, false)
	err := (&App{Dir: f.work, Stdout: io.Discard, Stderr: io.Discard}).Deploy(context.Background(), DeployOptions{Target: "stage"})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("deploy did not note its ticket: %v", err)
	}
	if !strings.Contains(string(got), "deploy stage ") || !strings.Contains(string(got), ": ok") {
		t.Fatalf("deploy note argv = %q", got)
	}
}

func TestTicketJournalWholeWordSingleSlug(t *testing.T) {
	tickets := []journalTicket{
		{Slug: "single", Status: "открыт"},
		{Slug: "other-ticket", Status: "в работе"},
	}
	tickets[0].Path, tickets[1].Path = "docs/tasks/single.md", "docs/tasks/other.md"
	if got := chooseJournalTicket(tickets, "fix: single", "main", "repo"); got != "single" {
		t.Fatalf("single-word slug selected %q, want single", got)
	}
	if got := chooseJournalTicket(tickets, "fix: single-part", "main", "repo"); got != "other-ticket" {
		t.Fatalf("substring match selected %q, want sole active other-ticket", got)
	}
}

func TestTicketInRootTasksAcceptsRelativeAndAbsolutePaths(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "tmp", "project")
	for _, path := range []string{
		"docs/tasks/root.md",
		filepath.Join(root, "docs", "tasks", "absolute.md"),
	} {
		if !ticketInRootTasks(path, root) {
			t.Errorf("root task path %q was rejected", path)
		}
	}
	if ticketInRootTasks("project/docs/tasks/nested.md", root) {
		t.Fatal("nested project task path was accepted")
	}
}

func TestTicketJournalRealTKSlugFormat(t *testing.T) {
	tk, err := exec.LookPath("tk")
	if err != nil {
		t.Skip("tk is not in PATH")
	}
	sandbox := t.TempDir()
	root := filepath.Join(sandbox, "project")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "docs", "tasks"), 0o700); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(root, "docs", "tasks"), 0o700); err != nil {
		t.Fatal(err)
	}
	repo, err := gitx.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	env, err := isolatedRealTKEnv(t, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PWD", root)
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(tk, args...)
		cmd.Dir, cmd.Env = dir, envWithValue(env, "PWD", dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("tk %v: %v\n%s", args, err, out)
		}
	}
	run(root, "new", "root-item", "--to", root, "--title", "Root ticket", "--by", "graft")
	textPath := filepath.Join(root, "ticket-text.md")
	if err := os.WriteFile(textPath, []byte("## Что сделать\nsynthetic\n\n## Приёмка\nsynthetic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(root, "set", "root-item", "--text", textPath, "--by", "graft")
	run(root, "start", "root-item", "--by", "graft")
	child := filepath.Join(root, "project")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, child, "init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(child, "docs", "tasks"), 0o700); err != nil {
		t.Fatal(err)
	}
	run(child, "new", "child-item", "--to", child, "--title", "Child ticket", "--by", "graft")
	run(child, "set", "child-item", "--text", textPath, "--by", "graft")
	run(child, "start", "child-item", "--by", "graft")
	var stderr bytes.Buffer
	(&App{Stderr: &stderr}).ticketJournal(context.Background(), repo, "unrelated work", "commit abcdef123456 on main: unrelated work")
	entries, err := os.ReadDir(filepath.Join(root, "docs", "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("tk created %d task files", len(entries))
	}
	rootTask := filepath.Join(root, "docs", "tasks", entries[0].Name())
	if err := assertWithinTempRoot(sandbox, rootTask); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(entries[0].Name(), "root-item") {
		t.Fatalf("unexpected root ticket file %q", entries[0].Name())
	}
	content, err := os.ReadFile(rootTask)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "graft commit abcdef123456 on main") {
		t.Fatalf("root ticket did not receive note; ticket contents:\n%s", content)
	}
	childEntries, err := os.ReadDir(filepath.Join(child, "docs", "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	if len(childEntries) != 1 || !strings.Contains(childEntries[0].Name(), "child-item") {
		t.Fatalf("unexpected child ticket files: %v", childEntries)
	}
	if err := assertWithinTempRoot(sandbox, filepath.Join(child, "docs", "tasks", childEntries[0].Name())); err != nil {
		t.Fatal(err)
	}
	childContent, err := os.ReadFile(filepath.Join(child, "docs", "tasks", childEntries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(childContent), "graft commit abcdef123456") {
		t.Fatalf("nested ticket received root journal note:\n%s", childContent)
	}
}

func TestTicketJournalCommitFromWorktreeUsesMainCheckout(t *testing.T) {
	tk, err := exec.LookPath("tk")
	if err != nil {
		t.Skip("tk is not in PATH")
	}
	f := newSyncPushFixture(t, false)
	env, err := isolatedRealTKEnv(t, f.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PWD", f.work)
	if err := os.MkdirAll(filepath.Join(f.work, "docs", "tasks"), 0o700); err != nil {
		t.Fatal(err)
	}
	textPath := filepath.Join(f.root, "ticket-text.md")
	if err := os.WriteFile(textPath, []byte("## Что сделать\nsynthetic\n\n## Приёмка\nsynthetic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTK := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(tk, args...)
		cmd.Dir, cmd.Env = dir, envWithValue(env, "PWD", dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("tk %v: %v\n%s", args, err, out)
		}
	}
	runTK(f.work, "new", "root-item", "--to", f.work, "--title", "Root ticket", "--by", "graft")
	runTK(f.work, "set", "root-item", "--text", textPath, "--by", "graft")
	runTK(f.work, "start", "root-item", "--by", "graft")
	f.git(f.work, "add", "docs/tasks")
	f.git(f.work, "commit", "-q", "-m", "add active ticket")
	f.git(f.work, "push", "-q", "origin", "main")
	entries, err := os.ReadDir(filepath.Join(f.work, "docs", "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	var taskName string
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "root-item") {
			taskName = entry.Name()
			break
		}
	}
	if taskName == "" {
		t.Fatalf("root-item file not found among %v", entries)
	}
	baseContent, err := os.ReadFile(filepath.Join(f.work, "docs", "tasks", taskName))
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(f.root, "linked")
	f.git(f.work, "worktree", "add", "-q", "-b", "topic/root-item", worktree, "HEAD")
	if err := os.WriteFile(filepath.Join(worktree, "change"), []byte("worktree change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := &App{Dir: worktree, Stdout: &stdout, Stderr: &stderr}
	if err := app.Commit(context.Background(), CommitOptions{Message: message.Source{Parts: []string{"feat: worktree change"}}}); err != nil {
		t.Fatalf("commit from linked worktree: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	mainContent, err := os.ReadFile(filepath.Join(f.work, "docs", "tasks", taskName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mainContent), "graft commit ") {
		t.Fatalf("main checkout ticket did not receive note:\n%s", mainContent)
	}
	worktreeContent, err := os.ReadFile(filepath.Join(worktree, "docs", "tasks", taskName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(worktreeContent, baseContent) {
		t.Fatalf("main journal note changed worktree ticket:\nbefore:\n%s\nafter:\n%s", baseContent, worktreeContent)
	}
	committedContent := f.git(worktree, "show", "HEAD:docs/tasks/"+taskName)
	if strings.Contains(committedContent, "graft commit ") {
		t.Fatalf("post-commit note entered worktree commit:\n%s", committedContent)
	}
}

func TestTicketJournalSeparateGitDirSkipsJournal(t *testing.T) {
	tk, err := exec.LookPath("tk")
	if err != nil {
		t.Skip("tk is not in PATH")
	}
	sandbox := t.TempDir()
	root := filepath.Join(sandbox, "separate")
	other := filepath.Join(sandbox, "other")
	if err := os.MkdirAll(filepath.Join(other, "docs", "tasks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "docs", "tasks"), 0o700); err != nil {
		t.Fatal(err)
	}
	env, err := isolatedRealTKEnv(t, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	runTK := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(tk, args...)
		cmd.Dir, cmd.Env = dir, envWithValue(env, "PWD", dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("tk %v: %v\n%s", args, err, out)
		}
	}
	textPath := filepath.Join(sandbox, "ticket-text.md")
	if err := os.WriteFile(textPath, []byte("## Что сделать\nsynthetic\n\n## Приёмка\nsynthetic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	makeActiveTicket := func(dir, slug, title string) string {
		t.Helper()
		runTK(dir, "new", slug, "--to", dir, "--title", title, "--by", "graft")
		runTK(dir, "set", slug, "--text", textPath, "--by", "graft")
		runTK(dir, "start", slug, "--by", "graft")
		entries, err := os.ReadDir(filepath.Join(dir, "docs", "tasks"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.Contains(entry.Name(), slug) {
				return filepath.Join(dir, "docs", "tasks", entry.Name())
			}
		}
		t.Fatalf("ticket %s not found under %s", slug, dir)
		return ""
	}
	otherTask := makeActiveTicket(other, "other-ticket", "Separate git metadata")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, syncPushGitEnv(sandbox)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	ownTask := makeActiveTicket(root, "own-ticket", "Own checkout")
	for _, path := range []string{ownTask, otherTask} {
		if err := assertWithinTempRoot(sandbox, path); err != nil {
			t.Fatal(err)
		}
	}
	git(sandbox, "init", "-q", "--separate-git-dir", filepath.Join(other, ".git"), root)
	if err := os.MkdirAll(filepath.Join(other, ".git", "info"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, ".git", "info", "exclude"), []byte("/docs/tasks/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(root, "config", "user.name", "Test")
	git(root, "config", "user.email", "test@example.invalid")
	for name, content := range map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\n",
		"base":        "base\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(root, "add", ".")
	git(root, "commit", "-q", "-m", "base")
	ownBefore, err := os.ReadFile(ownTask)
	if err != nil {
		t.Fatal(err)
	}
	otherBefore, err := os.ReadFile(otherTask)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "change"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PWD", root)
	var stderr bytes.Buffer
	app := &App{Dir: root, Stdout: io.Discard, Stderr: &stderr}
	if err := app.Commit(context.Background(), CommitOptions{Message: message.Source{Parts: []string{"feat: unrelated change"}}}); err != nil {
		t.Fatalf("commit from separate git dir: %v\nstderr: %s", err, stderr.String())
	}
	ownAfter, err := os.ReadFile(ownTask)
	if err != nil {
		t.Fatal(err)
	}
	otherAfter, err := os.ReadFile(otherTask)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ownAfter, ownBefore) {
		t.Fatalf("own-ticket changed:\nbefore:\n%s\nafter:\n%s", ownBefore, ownAfter)
	}
	if !bytes.Equal(otherAfter, otherBefore) {
		t.Fatalf("other-ticket was modified:\nbefore:\n%s\nafter:\n%s", otherBefore, otherAfter)
	}
	if strings.Contains(string(ownBefore), "graft commit ") {
		t.Fatal("own-ticket already contained graft note before commit")
	}
	if got := strings.Count(stderr.String(), "graft: ticket journal skipped: separate git dir"); got != 1 {
		t.Fatalf("skip message count = %d, stderr: %s", got, stderr.String())
	}
}

func TestTicketJournalWorktreeOfSeparateGitDirSkipsJournal(t *testing.T) {
	tk, err := exec.LookPath("tk")
	if err != nil {
		t.Skip("tk is not in PATH")
	}
	sandbox := t.TempDir()
	root := filepath.Join(sandbox, "separate")
	other := filepath.Join(sandbox, "other")
	if err := os.MkdirAll(filepath.Join(other, "docs", "tasks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "docs", "tasks"), 0o700); err != nil {
		t.Fatal(err)
	}
	env, err := isolatedRealTKEnv(t, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	runTK := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(tk, args...)
		cmd.Dir, cmd.Env = dir, envWithValue(env, "PWD", dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("tk %v: %v\n%s", args, err, out)
		}
	}
	textPath := filepath.Join(sandbox, "ticket-text.md")
	if err := os.WriteFile(textPath, []byte("## Что сделать\nsynthetic\n\n## Приёмка\nsynthetic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	makeActiveTicket := func(dir, slug string) string {
		t.Helper()
		runTK(dir, "new", slug, "--to", dir, "--title", slug, "--by", "graft")
		runTK(dir, "set", slug, "--text", textPath, "--by", "graft")
		runTK(dir, "start", slug, "--by", "graft")
		entries, err := os.ReadDir(filepath.Join(dir, "docs", "tasks"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.Contains(entry.Name(), slug) {
				path := filepath.Join(dir, "docs", "tasks", entry.Name())
				if err := assertWithinTempRoot(sandbox, path); err != nil {
					t.Fatal(err)
				}
				return path
			}
		}
		t.Fatalf("ticket %s not found under %s", slug, dir)
		return ""
	}
	ownTask := makeActiveTicket(root, "own-ticket")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, syncPushGitEnv(sandbox)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(sandbox, "init", "-q", "--separate-git-dir", filepath.Join(other, ".git"), root)
	if err := os.MkdirAll(filepath.Join(other, ".git", "info"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, ".git", "info", "exclude"), []byte("/docs/tasks/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(root, "config", "user.name", "Test")
	git(root, "config", "user.email", "test@example.invalid")
	for name, content := range map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none}\n", "base": "base\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(root, "add", ".")
	git(root, "commit", "-q", "-m", "base")
	otherTask := makeActiveTicket(other, "other-ticket")
	for _, path := range []string{ownTask, otherTask} {
		if err := assertWithinTempRoot(sandbox, path); err != nil {
			t.Fatal(err)
		}
	}
	ownBefore, err := os.ReadFile(ownTask)
	if err != nil {
		t.Fatal(err)
	}
	otherBefore, err := os.ReadFile(otherTask)
	if err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(sandbox, "linked")
	git(root, "worktree", "add", "-q", "-b", "topic/own-ticket", linked, "HEAD")
	if err := os.WriteFile(filepath.Join(linked, "change"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PWD", linked)
	var stderr bytes.Buffer
	app := &App{Dir: linked, Stdout: io.Discard, Stderr: &stderr}
	if err := app.Commit(context.Background(), CommitOptions{Message: message.Source{Parts: []string{"feat: worktree change"}}}); err != nil {
		t.Fatalf("commit from linked worktree: %v\nstderr: %s", err, stderr.String())
	}
	ownAfter, err := os.ReadFile(ownTask)
	if err != nil {
		t.Fatal(err)
	}
	otherAfter, err := os.ReadFile(otherTask)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ownAfter, ownBefore) || !bytes.Equal(otherAfter, otherBefore) {
		repo, openErr := gitx.Open(linked)
		if openErr != nil {
			t.Fatal(openErr)
		}
		journalRoot, skipped, rootErr := ticketJournalRoot(repo)
		t.Fatalf("ticket changed; gitdir=%s common=%s journalRoot=%s skipped=%v rootErr=%v; own before/after:\n%s\n%s\nother before/after:\n%s\n%s", repo.GitDir, repo.CommonDir, journalRoot, skipped, rootErr, ownBefore, ownAfter, otherBefore, otherAfter)
	}
	if got := strings.Count(stderr.String(), "graft: ticket journal skipped: separate git dir"); got != 1 {
		t.Fatalf("skip message count = %d, stderr: %s", got, stderr.String())
	}
}

func TestTicketJournalDeepSeparateGitDirWorktreeSkipsJournal(t *testing.T) {
	tk, err := exec.LookPath("tk")
	if err != nil {
		t.Skip("tk is not in PATH")
	}
	sandbox := t.TempDir()
	root := filepath.Join(sandbox, "deep", "parent", "checkout")
	meta := filepath.Join(sandbox, "meta")
	if err := os.MkdirAll(filepath.Join(root, "docs", "tasks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(meta, "docs", "tasks"), 0o700); err != nil {
		t.Fatal(err)
	}
	env, err := isolatedRealTKEnv(t, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	runTK := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(tk, args...)
		cmd.Dir, cmd.Env = dir, envWithValue(env, "PWD", dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("tk %v: %v\n%s", args, err, out)
		}
	}
	textPath := filepath.Join(sandbox, "ticket-text.md")
	if err := os.WriteFile(textPath, []byte("## Что сделать\nsynthetic\n\n## Приёмка\nsynthetic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	makeActiveTicket := func(dir, slug string) string {
		t.Helper()
		runTK(dir, "new", slug, "--to", dir, "--title", slug, "--by", "graft")
		runTK(dir, "set", slug, "--text", textPath, "--by", "graft")
		runTK(dir, "start", slug, "--by", "graft")
		entries, err := os.ReadDir(filepath.Join(dir, "docs", "tasks"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.Contains(entry.Name(), slug) {
				path := filepath.Join(dir, "docs", "tasks", entry.Name())
				if err := assertWithinTempRoot(sandbox, path); err != nil {
					t.Fatal(err)
				}
				return path
			}
		}
		t.Fatalf("ticket %s not found under %s", slug, dir)
		return ""
	}
	ownTask := makeActiveTicket(root, "own-ticket")
	otherTask := makeActiveTicket(meta, "other-ticket")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, syncPushGitEnv(sandbox)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(sandbox, "init", "-q", "--separate-git-dir", filepath.Join(meta, ".git"), root)
	if err := os.MkdirAll(filepath.Join(meta, ".git", "info"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(meta, ".git", "info", "exclude"), []byte("/docs/tasks/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(root, "config", "user.name", "Test")
	git(root, "config", "user.email", "test@example.invalid")
	for name, content := range map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\n",
		"base":        "base\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(root, "add", ".")
	git(root, "commit", "-q", "-m", "base")
	ownBefore, err := os.ReadFile(ownTask)
	if err != nil {
		t.Fatal(err)
	}
	otherBefore, err := os.ReadFile(otherTask)
	if err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(sandbox, "linked")
	git(root, "worktree", "add", "-q", "-b", "topic/own-ticket", linked, "HEAD")
	if err := os.WriteFile(filepath.Join(linked, "change"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PWD", linked)
	var stderr bytes.Buffer
	app := &App{Dir: linked, Stdout: io.Discard, Stderr: &stderr}
	if err := app.Commit(context.Background(), CommitOptions{Message: message.Source{Parts: []string{"feat: worktree change"}}}); err != nil {
		t.Fatalf("commit from linked worktree: %v\nstderr: %s", err, stderr.String())
	}
	ownAfter, err := os.ReadFile(ownTask)
	if err != nil {
		t.Fatal(err)
	}
	otherAfter, err := os.ReadFile(otherTask)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ownAfter, ownBefore) || !bytes.Equal(otherAfter, otherBefore) {
		t.Fatalf("ticket changed; own before/after:\n%s\n%s\nother before/after:\n%s\n%s", ownBefore, ownAfter, otherBefore, otherAfter)
	}
	if got := strings.Count(stderr.String(), "graft: ticket journal skipped: separate git dir"); got != 1 {
		t.Fatalf("skip message count = %d, stderr: %s", got, stderr.String())
	}
}

func isolatedRealTKEnv(t *testing.T, root string) ([]string, error) {
	t.Helper()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("creating temporary HOME: %w", err)
	}
	t.Setenv("HOME", home)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		for _, prefix := range []string{"XDG_", "CLAUDE_", "TK_", "GRAFT_", "CODEX_", "ANTHROPIC_", "SESSION_"} {
			if strings.HasPrefix(key, prefix) {
				t.Setenv(key, "")
				break
			}
		}
		if key == "SESSION" {
			t.Setenv(key, "")
		}
	}
	for name, dir := range map[string]string{
		"XDG_CACHE_HOME":  filepath.Join(home, ".cache"),
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_DATA_HOME":   filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME":  filepath.Join(home, ".local", "state"),
		"XDG_RUNTIME_DIR": filepath.Join(home, ".runtime"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("creating temporary %s: %w", name, err)
		}
		t.Setenv(name, dir)
	}
	t.Setenv("TK_NO_PARSE_CACHE", "1")
	t.Setenv("PWD", root)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return journalEnv()
}

func assertWithinTempRoot(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return fmt.Errorf("checking path relative to temporary root: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("tk wrote outside temporary root: %s", path)
	}
	return nil
}

func envWithValue(env []string, name, value string) []string {
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if key != name {
			result = append(result, entry)
		}
	}
	return append(result, name+"="+value)
}

func TestCommitJournalWithoutRemoteAndFailedPush(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failPush bool
	}{
		{name: "local commit without remote"},
		{name: "failed push", failPush: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSyncPushFixture(t, false)
			if tc.failPush {
				f.git(f.work, "remote", "set-url", "origin", filepath.Join(f.root, "missing.git"))
			} else {
				f.git(f.work, "remote", "remove", "origin")
			}
			if err := os.MkdirAll(filepath.Join(f.work, "docs", "tasks"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.work, "docs", "tasks", "ticket.md"), []byte("synthetic\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.work, "change"), []byte("change\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			log := ticketJournalStub(t, `[{"path":"docs/tasks/ticket.md","slug":"proj-123-fix","status":"в работе"}]`, false)
			err := (&App{Dir: f.work, Stdout: io.Discard, Stderr: io.Discard}).Commit(context.Background(), CommitOptions{Message: message.Source{Parts: []string{"fix: proj-123-fix"}}})
			if tc.failPush {
				if err == nil {
					t.Fatal("commit with failed push returned nil")
				}
				if _, statErr := os.Stat(log); !os.IsNotExist(statErr) {
					t.Fatalf("tk note ran after failed push: %v", statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("local commit: %v", err)
			}
			got, readErr := os.ReadFile(log)
			if readErr != nil {
				t.Fatalf("tk note missing after local commit: %v", readErr)
			}
			if !strings.Contains(string(got), "proj-123-fix\n") {
				t.Fatalf("tk note argv = %q", got)
			}
		})
	}
}

func TestTicketJournalSharesOneTimeoutAndStopsChildren(t *testing.T) {
	repo := ticketJournalRepo(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "note-started")
	stub := filepath.Join(dir, "tk")
	script := "#!/bin/sh\nif [ \"$1\" = ls ]; then sleep 3; printf '%s' '[{\"path\":\"docs/tasks/ticket.md\",\"slug\":\"proj-123-fix\",\"status\":\"в работе\"}]'; exit 0; fi\nprintf started > \"$NOTE_STARTED\"\nsleep 30\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("NOTE_STARTED", log)
	var stderr bytes.Buffer
	started := time.Now()
	(&App{Stderr: &stderr}).ticketJournal(context.Background(), repo, "proj-123-fix", "commit abc")
	elapsed := time.Since(started)
	if elapsed > 7500*time.Millisecond {
		t.Fatalf("tk ls + note exceeded one shared timeout: %s", elapsed)
	}
	if _, err := os.Stat(log); err != nil {
		t.Fatalf("tk note child was not started: %v", err)
	}
	if !strings.Contains(stderr.String(), "ticket journal was not written") {
		t.Fatalf("timed out tk note did not warn: %q", stderr.String())
	}
}
