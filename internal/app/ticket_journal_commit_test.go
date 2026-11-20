package app

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/message"
	"github.com/aiseeq/graft/v2/internal/version"
)

const journalTicketJSON = `[{"path":"docs/tasks/proj-123-fix.md","slug":"proj-123-fix","status":"в работе"}]`

func trackedJournalFixture(t *testing.T) *syncPushFixture {
	return trackedJournalFixtureWithTicket(t, "proj-123-fix.md")
}

func trackedJournalFixtureWithTicket(t *testing.T, filename string) *syncPushFixture {
	t.Helper()
	f := newSyncPushFixture(t, false)
	if err := os.MkdirAll(filepath.Join(f.work, "docs", "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.work, "docs", "tasks", filename), []byte("## Журнал\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git(f.work, "add", "docs/tasks/"+filename)
	f.git(f.work, "commit", "-q", "-m", "add ticket")
	f.git(f.work, "push", "-q", "origin", "main")
	return f
}

func trackedJournalTK(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "tk")
	script := "#!/bin/sh\nif [ \"$1\" = ls ]; then printf '%s' \"$TICKETS_JSON\"; exit 0; fi\nprintf '%s\\n' \"$@\" >> \"$TK_ARGV\"\nprintf '%s\\n' \"$3\" >> \"$TK_TICKET_FILE\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TICKETS_JSON", journalTicketJSON)
	log := filepath.Join(dir, "argv")
	t.Setenv("TK_ARGV", log)
	return log
}

func trackedJournalCommit(t *testing.T, f *syncPushFixture, subject string) (string, error) {
	t.Helper()
	trackedJournalTK(t)
	t.Setenv("TK_TICKET_FILE", filepath.Join(f.work, "docs", "tasks", "proj-123-fix.md"))
	if err := os.WriteFile(filepath.Join(f.work, "change"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := (&App{Dir: f.work, Stdout: &stdout, Stderr: &stderr}).Commit(context.Background(), CommitOptions{Message: message.Source{Parts: []string{subject}}})
	return stdout.String() + stderr.String(), err
}

func TestCommitJournalTrackedTasksGoesIntoTheCommit(t *testing.T) {
	f := trackedJournalFixture(t)
	if err := os.WriteFile(filepath.Join(f.work, "docs", "tasks", "proj-123-fix.md"), []byte("## Журнал\nuser edit before commit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	parent := f.git(f.work, "rev-parse", "HEAD")
	if _, err := trackedJournalCommit(t, f, "fix: proj-123-fix thing"); err != nil {
		t.Fatal(err)
	}
	if got := f.git(f.work, "status", "--porcelain"); got != "" {
		t.Fatalf("work tree after commit = %q, want clean", got)
	}
	content := f.git(f.work, "show", "HEAD:docs/tasks/proj-123-fix.md")
	want := "graft commit after " + parent[:12] + " on main: fix: proj-123-fix thing"
	if !strings.Contains(content, want) {
		t.Fatalf("committed ticket journal lacks %q:\n%s", want, content)
	}
	if !strings.Contains(content, "user edit before commit") {
		t.Fatalf("user ticket edit was not included with graft note:\n%s", content)
	}
}

func TestCommitJournalTrackedTasksSelectsTicketByID(t *testing.T) {
	filename := "20261119-0150-MED-proj-123-fix.md"
	f := trackedJournalFixtureWithTicket(t, filename)
	log := trackedJournalTK(t)
	t.Setenv("TICKETS_JSON", `[{"path":"docs/tasks/`+filename+`","slug":"proj-123-fix","status":"в работе"}]`)
	t.Setenv("TK_TICKET_FILE", filepath.Join(f.work, "docs", "tasks", filename))
	if err := os.WriteFile(filepath.Join(f.work, "change"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&App{Dir: f.work, Stdout: io.Discard, Stderr: io.Discard}).Commit(context.Background(), CommitOptions{Message: message.Source{Parts: []string{"fix: " + filename}}}); err != nil {
		t.Fatalf("commit by ticket ID: %v", err)
	}
	content := f.git(f.work, "show", "HEAD:docs/tasks/"+filename)
	if !strings.Contains(content, "graft commit after ") {
		t.Fatalf("ticket selected by ID has no note:\n%s", content)
	}
	argv, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(argv), filename+"\n") {
		t.Fatalf("tk note did not receive filename ID %q: %q", filename, argv)
	}
}

func TestCommitThenInstallTrackedTasks(t *testing.T) {
	f := trackedJournalFixture(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	target := filepath.Join(f.root, "installed")
	yaml := "schema: 1\nversion: {mode: none}\ngate: []\ngo_vet: {enabled: false, reason: synthetic projectA install fixture}\ngo_fmt: {enabled: false, reason: synthetic projectA install fixture}\ntasks:\n  projecta:\n    run:\n      - {install: bin/projecta, to: " + target + "}\n"
	for path, contents := range map[string]string{
		".graft.yaml": yaml,
		".gitignore":  "bin/\n",
		"go.mod":      "module projectA\n\ngo 1.23\n",
		"main.go":     "package main\nfunc main() {}\n",
	} {
		full := filepath.Join(f.work, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	trackedJournalTK(t)
	t.Setenv("TK_TICKET_FILE", filepath.Join(f.work, "docs", "tasks", "proj-123-fix.md"))
	var commitOut, commitErr bytes.Buffer
	if err := (&App{Dir: f.work, Stdout: &commitOut, Stderr: &commitErr}).Commit(context.Background(), CommitOptions{Message: message.Source{Parts: []string{"fix: proj-123-fix install"}}}); err != nil {
		t.Fatalf("commit: %v\nstdout: %s\nstderr: %s", err, commitOut.String(), commitErr.String())
	}
	content := f.git(f.work, "show", "HEAD:docs/tasks/proj-123-fix.md")
	if !strings.Contains(content, "graft commit after ") {
		t.Fatalf("ticket note is missing from HEAD:\n%s", content)
	}
	if err := os.MkdirAll(filepath.Join(f.work, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", "bin/projecta", ".")
	build.Dir = f.work
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build projectA binary: %v\n%s", err, output)
	}
	if err := (&App{Dir: f.work, Stdout: io.Discard, Stderr: io.Discard}).Run(context.Background(), nil, "projecta", nil); err != nil {
		t.Fatalf("install task after commit: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("installed binary missing: %v", err)
	}
}

func TestReleaseDoesNotWriteTrackedTicketNote(t *testing.T) {
	f := trackedJournalFixture(t)
	config := "schema: 1\nversion: {mode: file, file: VERSION}\n"
	if err := os.WriteFile(filepath.Join(f.work, ".graft.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.work, "VERSION"), []byte("1.2.3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git(f.work, "add", ".graft.yaml", "VERSION")
	f.git(f.work, "commit", "-q", "-m", "configure release")
	f.git(f.work, "push", "-q", "origin", "main")
	f.git(f.work, "checkout", "-q", "-b", "topic/proj-123-fix")
	f.git(f.work, "push", "-q", "-u", "origin", "topic/proj-123-fix")
	log := ticketJournalStub(t, journalTicketJSON, false)
	t.Setenv("TK_TICKET_FILE", filepath.Join(f.work, "docs", "tasks", "proj-123-fix.md"))
	before := f.git(f.work, "rev-parse", "HEAD")
	if err := (&App{Dir: f.work, Stdout: io.Discard, Stderr: io.Discard}).Release(context.Background(), ReleaseOptions{Level: version.Patch, Patch: true}); err != nil {
		t.Fatalf("release: %v", err)
	}
	if after := f.git(f.work, "rev-parse", "HEAD"); after == before {
		t.Fatal("file-mode release did not create its version commit")
	}
	if got := f.git(f.work, "show", "HEAD:VERSION"); got != "1.2.4" {
		t.Fatalf("release commit VERSION = %q, want 1.2.4", got)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatalf("release called tk note: %v", err)
	}
	content := f.git(f.work, "show", "HEAD:docs/tasks/proj-123-fix.md")
	if strings.Contains(content, "graft commit") {
		t.Fatalf("release note entered ticket:\n%s", content)
	}
}

func TestReleaseDoesNotWriteIgnoredTicketNote(t *testing.T) {
	f := newSyncPushFixture(t, false)
	externalTasks := filepath.Join(f.root, "external-tasks")
	if err := os.MkdirAll(externalTasks, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.work, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalTasks, filepath.Join(f.work, "docs", "tasks")); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		".gitignore":  "/docs/tasks\n",
		".graft.yaml": "schema: 1\nversion: {mode: file, file: VERSION}\n",
		"VERSION":     "1.2.3\n",
	} {
		if err := os.WriteFile(filepath.Join(f.work, path), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.git(f.work, "add", ".")
	f.git(f.work, "commit", "-q", "-m", "configure release")
	f.git(f.work, "push", "-q", "origin", "main")
	json := `[{"path":"docs/tasks/release.md","slug":"release","status":"в работе"}]`
	log := ticketJournalStub(t, json, false)
	before := f.git(f.work, "rev-parse", "HEAD")
	if err := (&App{Dir: f.work, Stdout: io.Discard, Stderr: io.Discard}).Release(context.Background(), ReleaseOptions{Level: version.Patch, Patch: true}); err != nil {
		t.Fatalf("release: %v", err)
	}
	if after := f.git(f.work, "rev-parse", "HEAD"); after == before {
		t.Fatal("file-mode release did not create its version commit")
	}
	if got := f.git(f.work, "show", "HEAD:VERSION"); got != "1.2.4" {
		t.Fatalf("release commit VERSION = %q, want 1.2.4", got)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatalf("release called tk note for ignored ticket: %v", err)
	}
}

func TestCommitJournalRollbackOnRunChecksFailure(t *testing.T) {
	f := trackedJournalFixture(t)
	before, err := os.ReadFile(filepath.Join(f.work, "docs", "tasks", "proj-123-fix.md"))
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Join([]string{"AKIA", "ABCDEFGHIJKLMNOP"}, "")
	if err := os.WriteFile(filepath.Join(f.work, "secret.txt"), []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := trackedJournalCommit(t, f, "fix: proj-123-fix thing")
	if err == nil || !strings.Contains(output+err.Error(), "secret") {
		t.Fatalf("commit error = %v, output = %s; want secret scan refusal", err, output)
	}
	after, readErr := os.ReadFile(filepath.Join(f.work, "docs", "tasks", "proj-123-fix.md"))
	if readErr != nil || !bytes.Equal(after, before) {
		t.Fatalf("ticket bytes changed after runChecks refusal: %q, %v", after, readErr)
	}
	if got := f.git(f.work, "diff", "--cached", "--", "docs/tasks/proj-123-fix.md"); got != "" {
		t.Fatalf("ticket note remains staged after refusal:\n%s", got)
	}
}

func TestCommitJournalRollbackOnGitCommitFailure(t *testing.T) {
	f := trackedJournalFixture(t)
	hooks := filepath.Join(f.root, "reject-hooks")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "commit-msg"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.git(f.work, "config", "core.hooksPath", hooks)
	before, err := os.ReadFile(filepath.Join(f.work, "docs", "tasks", "proj-123-fix.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = trackedJournalCommit(t, f, "fix: proj-123-fix thing")
	if err == nil {
		t.Fatal("commit-msg hook failure was ignored")
	}
	after, readErr := os.ReadFile(filepath.Join(f.work, "docs", "tasks", "proj-123-fix.md"))
	if readErr != nil || !bytes.Equal(after, before) {
		t.Fatalf("ticket bytes changed after git commit refusal: %q, %v", after, readErr)
	}
	if got := f.git(f.work, "diff", "--cached", "--", "docs/tasks/proj-123-fix.md"); got != "" {
		t.Fatalf("ticket note remains staged after refusal:\n%s", got)
	}
	if got := f.git(f.work, "rev-parse", "HEAD"); got != f.git(f.remote, "rev-parse", "refs/heads/main") {
		t.Fatalf("HEAD moved after rejected commit: %s", got)
	}
}

func TestCommitJournalTrackedTasksDoesNotUseSingleActiveFallback(t *testing.T) {
	f := trackedJournalFixture(t)
	trackedJournalTK(t)
	logPath := filepath.Join(f.root, "tk-notes")
	t.Setenv("TK_TICKET_FILE", logPath)
	if err := os.WriteFile(filepath.Join(f.work, "change"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	err := (&App{Dir: f.work, Stdout: &stdout, Stderr: io.Discard}).Commit(context.Background(), CommitOptions{Message: message.Source{Parts: []string{"fix: unrelated change"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("unnamed active ticket received note: %v", err)
	}
	content := f.git(f.work, "show", "HEAD:docs/tasks/proj-123-fix.md")
	if strings.Contains(content, "graft commit") {
		t.Fatalf("unnamed ticket got a commit note:\n%s", content)
	}
}

func TestCommitJournalTrackedTasksFromWorktreeIsNotNoted(t *testing.T) {
	f := trackedJournalFixture(t)
	worktree := filepath.Join(f.root, "linked")
	f.git(f.work, "worktree", "add", "-q", "-b", "topic/proj-123-fix", worktree, "HEAD")
	if err := os.WriteFile(filepath.Join(worktree, "change"), []byte("worktree change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	trackedJournalTK(t)
	t.Setenv("TK_TICKET_FILE", filepath.Join(f.work, "docs", "tasks", "proj-123-fix.md"))
	var stdout, stderr bytes.Buffer
	err := (&App{Dir: worktree, Stdout: &stdout, Stderr: &stderr}).Commit(context.Background(), CommitOptions{Message: message.Source{Parts: []string{"feat: worktree change"}}})
	if err != nil {
		t.Fatalf("worktree commit: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "graft: ticket proj-123-fix: not noted (docs/tasks is tracked):") {
		t.Fatalf("missing not-noted message: %q", stderr.String())
	}
	if got := f.git(f.work, "status", "--porcelain"); got != "" {
		t.Fatalf("main checkout changed after worktree commit: %q", got)
	}
	content := f.git(worktree, "show", "HEAD:docs/tasks/proj-123-fix.md")
	if strings.Contains(content, "graft commit") {
		t.Fatalf("worktree commit contains a ticket note:\n%s", content)
	}
}
