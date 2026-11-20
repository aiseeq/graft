package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/lock"
	"github.com/aiseeq/graft/v2/internal/tasks"
)

func prepareMergeAfter(t *testing.T, after, script string, heavy ...bool) *syncPushFixture {
	t.Helper()
	f := newSyncPushFixture(t, false)
	if err := os.WriteFile(filepath.Join(f.work, ".gitignore"), []byte("generated\nfirst-runs\ntask-two-runs\nexecutions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := "schema: 1\nversion: {mode: none}\nmerge:\n"
	if after != "" {
		data += after
	}
	data += "tasks:\n  projecta:\n"
	if len(heavy) > 0 && heavy[0] {
		data += "    heavy: true\n"
	}
	data += "    run:\n      - {sh: " + strconv.Quote(script) + "}\n"
	data += "  projectb:\n    run: [{sh: 'echo second >> task-two-runs; cp model generated'}]\n"
	if len(heavy) > 0 && heavy[0] {
		remoteScript := "touch " + filepath.Join(f.root, "remote-ran") + "; exec \"$@\""
		data += "remote_heavy:\n  host: hostA\n  up: [\"true\"]\n  run: [sh, -c, " + strconv.Quote(remoteScript) + ", sh]\n"
	}
	if err := os.WriteFile(filepath.Join(f.work, ".graft.yaml"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git(f.work, "add", ".gitignore", ".graft.yaml")
	f.git(f.work, "commit", "-q", "-m", "configure merge after")
	f.git(f.work, "push", "-q", "origin", "main")
	f.git(f.work, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(f.work, "model"), []byte("fresh model\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git(f.work, "add", "model")
	f.git(f.work, "commit", "-q", "-m", "feature model")
	f.git(f.work, "checkout", "-q", "main")
	return f
}

func TestMergeAfterWritesIgnoredOutputFromMergedTree(t *testing.T) {
	f := prepareMergeAfter(t, "  after: [projecta]\n", "cp model generated")
	var stderr bytes.Buffer
	err := (&App{Dir: f.work, Stdout: io.Discard, Stderr: &stderr}).Merge(context.Background(), MergeOptions{Branch: "feature"})
	if err != nil {
		t.Fatalf("Merge: %v\nstderr: %s", err, stderr.String())
	}
	got, err := os.ReadFile(filepath.Join(f.work, "generated"))
	if err != nil || string(got) != "fresh model\n" {
		t.Fatalf("generated = %q, %v; want fresh model", got, err)
	}
}

func TestMergeAfterFailureDoesNotFailMergeAndPrintsRetry(t *testing.T) {
	f := prepareMergeAfter(t, "  after: [projecta]\n", "exit 7")
	var stderr bytes.Buffer
	err := (&App{Dir: f.work, Stdout: io.Discard, Stderr: &stderr}).Merge(context.Background(), MergeOptions{Branch: "feature"})
	if err != nil {
		t.Fatalf("Merge failed: %v", err)
	}
	if !strings.Contains(stderr.String(), "graft: merge.after: projecta failed:") || !strings.Contains(stderr.String(), "rerun: graft -C '") || !strings.Contains(stderr.String(), "' 'projecta'") {
		t.Fatalf("stderr %q; want one retry command", stderr.String())
	}
	if strings.Count(stderr.String(), "graft: merge.after: projecta failed:") != 1 {
		t.Fatalf("stderr should have one failure report followed by captured task output: %q", stderr.String())
	}
}

func TestMergeAfterFailureFlattensErrorAndShowsLastTenStderrLines(t *testing.T) {
	var stderr bytes.Buffer
	a := &App{Stderr: &stderr}
	var taskStderr strings.Builder
	for i := 1; i <= 12; i++ {
		fmt.Fprintf(&taskStderr, "task-error-%02d\n", i)
	}
	a.mergeAfterFailure("/tmp/project", "projecta", errors.New("first line\nsecond line"), taskStderr.String())
	lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
	if len(lines) != 11 || !strings.Contains(lines[0], "first line; second line") {
		t.Fatalf("failure output has %d lines: %q", len(lines), stderr.String())
	}
	if strings.Contains(stderr.String(), "task-error-01") || !strings.Contains(stderr.String(), "task-error-03") || !strings.Contains(stderr.String(), "task-error-12") {
		t.Fatalf("stderr tail is wrong: %q", stderr.String())
	}
}

func TestMergeAfterChecksHeadBetweenTasksAndLaterRunFinishesLatestTree(t *testing.T) {
	f := prepareMergeAfter(t, "  after: [projecta, projectb]\n", "true")
	f.git(f.work, "merge", "--ff-only", "feature")
	repo, err := gitx.Open(f.work)
	if err != nil {
		t.Fatal(err)
	}
	merged := f.git(f.work, "rev-parse", "HEAD")
	var stderr bytes.Buffer
	a := &App{Dir: f.work, Stdout: io.Discard, Stderr: &stderr}
	var moved bool
	a.runMergeAfterWithHooks(context.Background(), repo, merged, mergeAfterHooks{afterTask: func() {
		if !moved {
			if err := os.WriteFile(filepath.Join(f.work, "model"), []byte("latest model\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			f.git(f.work, "add", "model")
			f.git(f.work, "commit", "-q", "-m", "latest model")
			latest := f.git(f.work, "rev-parse", "HEAD")
			// A concurrent merge calls after while this pass owns the lock.
			a.runMergeAfter(context.Background(), repo, latest)
			moved = true
		}
	}})
	got, err := os.ReadFile(filepath.Join(f.work, "generated"))
	if err != nil || string(got) != "latest model\n" {
		t.Fatalf("generated output = %q, %v; want latest HEAD", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(f.work, "task-two-runs")); err != nil || string(got) != "second\n" {
		t.Fatalf("second task run count = %q, %v; want only latest pass", got, err)
	}
	if !strings.Contains(stderr.String(), "another merge is running the tasks; it picks up ") {
		t.Fatalf("stderr %q; want nonblocking handoff line", stderr.String())
	}
}

func TestMergeAfterRechecksHeadAfterUnlock(t *testing.T) {
	f := prepareMergeAfter(t, "  after: [projecta]\n", "cp model generated")
	f.git(f.work, "merge", "--ff-only", "feature")
	repo, err := gitx.Open(f.work)
	if err != nil {
		t.Fatal(err)
	}
	merged := f.git(f.work, "rev-parse", "HEAD")
	a := &App{Dir: f.work, Stdout: io.Discard, Stderr: io.Discard}
	var moved bool
	a.runMergeAfterWithHooks(context.Background(), repo, merged, mergeAfterHooks{afterRelease: func() {
		if moved {
			return
		}
		if err := os.WriteFile(filepath.Join(f.work, "model"), []byte("latest model\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.git(f.work, "add", "model")
		f.git(f.work, "commit", "-q", "-m", "latest model")
		moved = true
	}})
	got, err := os.ReadFile(filepath.Join(f.work, "generated"))
	if err != nil || string(got) != "latest model\n" {
		t.Fatalf("generated output = %q, %v; want post-unlock HEAD", got, err)
	}
}

func TestMergeAfterCanAcquireRepositoryLock(t *testing.T) {
	f := prepareMergeAfter(t, "  after: [projecta]\n", "flock -w 0.2 .git/graft.lock true && cp model generated")
	var stderr bytes.Buffer
	err := (&App{Dir: f.work, Stdout: io.Discard, Stderr: &stderr}).Merge(context.Background(), MergeOptions{Branch: "feature"})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(f.work, "generated"))
	if err != nil || string(got) != "fresh model\n" {
		t.Fatalf("task could not run while taking repo lock: generated=%q err=%v stderr=%s", got, err, stderr.String())
	}
}

func TestMergeAfterHeavyTaskStaysLocalWithAvailableRemote(t *testing.T) {
	t.Setenv(tasks.HeavyEnv, "")
	t.Setenv(tasks.HeavyOriginEnv, "")
	t.Setenv(tasks.HeavyByEnv, "")
	t.Setenv("GRAFT_PROC", t.TempDir())
	// The test is already admitted by its harness; avoid a second machine
	// admission while exercising the remote placement decision.
	t.Setenv(tasks.AdmittedEnv, "1")
	f := prepareMergeAfter(t, "  after: [projecta]\n", "cp model generated", true)
	var stderr bytes.Buffer
	if err := (&App{Dir: f.work, Stdout: io.Discard, Stderr: &stderr}).Merge(context.Background(), MergeOptions{Branch: "feature"}); err != nil {
		t.Fatalf("Merge: %v\nstderr: %s", err, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(f.root, "remote-ran")); !os.IsNotExist(err) {
		t.Fatalf("remote ran despite merge.after local-only setting: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(f.work, "generated")); err != nil || string(got) != "fresh model\n" {
		t.Fatalf("generated output = %q, %v; stderr: %s", got, err, stderr.String())
	}
}

func TestMergeAfterRerunsIgnoredOutputDespiteUnchangedCacheInputs(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	f := newSyncPushFixture(t, false)
	if err := os.WriteFile(filepath.Join(f.work, "model"), []byte("stable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.work, ".gitignore"), []byte("generated\nexecutions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := "schema: 1\nversion: {mode: none}\nmerge:\n  after: [projecta]\ntasks:\n  projecta:\n    cache: {paths: [model]}\n    run: [{sh: 'echo run >> executions; cp model generated'}]\n"
	if err := os.WriteFile(filepath.Join(f.work, ".graft.yaml"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git(f.work, "add", "model", ".gitignore", ".graft.yaml")
	f.git(f.work, "commit", "-q", "-m", "configure cached merge after")
	f.git(f.work, "push", "-q", "origin", "main")
	f.git(f.work, "checkout", "-q", "-b", "feature-one")
	f.commit(f.work, "change-one")
	f.git(f.work, "checkout", "-q", "main")
	app := &App{Dir: f.work, Stdout: io.Discard, Stderr: io.Discard}
	if err := app.Merge(context.Background(), MergeOptions{Branch: "feature-one"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.work, "generated")); err != nil {
		t.Fatal(err)
	}
	f.git(f.work, "checkout", "-q", "-b", "feature-two")
	f.commit(f.work, "change-two")
	f.git(f.work, "checkout", "-q", "main")
	if err := app.Merge(context.Background(), MergeOptions{Branch: "feature-two"}); err != nil {
		t.Fatal(err)
	}
	count, err := os.ReadFile(filepath.Join(f.work, "executions"))
	if err != nil || string(count) != "run\nrun\n" {
		t.Fatalf("task execution record = %q, %v; want two runs", count, err)
	}
	if got, err := os.ReadFile(filepath.Join(f.work, "generated")); err != nil || string(got) != "stable\n" {
		t.Fatalf("generated output = %q, %v; want restored output", got, err)
	}
}

func TestMergeAfterSkipsWhenHeadMovedWhileWaitingForLock(t *testing.T) {
	f := prepareMergeAfter(t, "  after: [projecta]\n", "cp model generated")
	repo, err := gitx.Open(f.work)
	if err != nil {
		t.Fatal(err)
	}
	lk, err := lock.Acquire(context.Background(), filepath.Join(repo.CommonDir, "graft-locks", "merge-after.lock"), time.Second, "test", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	merged := f.git(f.work, "rev-parse", "HEAD")
	app := &App{Dir: f.work, Stdout: io.Discard, Stderr: io.Discard}
	var stderr bytes.Buffer
	app.Stderr = &stderr
	done := make(chan struct{})
	go func() { app.runMergeAfter(context.Background(), repo, merged); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("merge.after blocked on the held lock")
	}
	f.commit(f.work, "later")
	if err := lk.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.work, "generated")); !os.IsNotExist(err) {
		t.Fatalf("task ran after HEAD moved: stat err %v", err)
	}
	if !strings.Contains(stderr.String(), "another merge is running the tasks; it picks up ") {
		t.Fatalf("stderr %q; want busy line", stderr.String())
	}
}

func TestMergeWithoutAfterKeepsExistingBehavior(t *testing.T) {
	f := prepareMergeAfter(t, "", "cp model generated")
	if err := (&App{Dir: f.work, Stdout: io.Discard, Stderr: io.Discard}).Merge(context.Background(), MergeOptions{Branch: "feature"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.work, "generated")); !os.IsNotExist(err) {
		t.Fatalf("task unexpectedly ran: %v", err)
	}
	repo, err := gitx.Open(f.work)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo.CommonDir, "graft-locks", "merge-after.lock")); !os.IsNotExist(err) {
		t.Fatalf("after lock created without merge.after: %v", err)
	}
}

func TestMergeAfterPassLimitNamesTasksOfLatestHead(t *testing.T) {
	f := prepareMergeAfter(t, "  after: [projecta, projectb]\n", "true")
	f.git(f.work, "merge", "--ff-only", "feature")
	repo, err := gitx.Open(f.work)
	if err != nil {
		t.Fatal(err)
	}
	merged := f.git(f.work, "rev-parse", "HEAD")
	var stderr bytes.Buffer
	a := &App{Dir: f.work, Stdout: io.Discard, Stderr: &stderr}
	moves := 0
	a.runMergeAfterWithHooks(context.Background(), repo, merged, mergeAfterHooks{afterTask: func() {
		moves++
		conf, err := os.ReadFile(filepath.Join(f.work, ".graft.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		next := strings.Replace(string(conf), "after: [projecta, projectb]", "after: [projectb]", 1)
		if err := os.WriteFile(filepath.Join(f.work, ".graft.yaml"), []byte(next), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.work, "model"), []byte(strings.Repeat("x", moves)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.git(f.work, "add", ".graft.yaml", "model")
		f.git(f.work, "commit", "-q", "-m", "moved again")
	}})
	out := stderr.String()
	if !strings.Contains(out, "stopped after 5 passes; rerun: graft -C ") || !strings.Contains(out, " 'projectb'\n") {
		t.Fatalf("stderr %q; want a rerun of the tasks of the latest HEAD", out)
	}
	if strings.Contains(out, "projecta") {
		t.Fatalf("stderr %q names projecta, which the latest HEAD no longer runs", out)
	}
}
