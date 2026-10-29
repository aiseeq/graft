package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeMarker leaves the marker of a job running models in the worktree at
// dir while the process pid lives, as the tool running it would.
func writeMarker(t *testing.T, f *fixture, id, dir string, pid int, models ...string) {
	t.Helper()
	writeMarkerJSON(t, f, map[string]any{"id": id, "models": models, "client": "cli", "dir": dir, "pid": pid, "started": "2026-01-02T03:04:05Z"})
}

// writeMarkerJSON leaves the marker m.
func writeMarkerJSON(t *testing.T, f *fixture, m map[string]any) {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.markers(), m["id"].(string)), string(data))
}

// liveJob is the pid of a running process whose command line names the job
// id, as the job's process of a tool does.
func liveJob(t *testing.T, id string) int {
	t.Helper()
	cmd := exec.Command(helperBin, "sleep", id)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd.Process.Pid
}

// deadPID is the pid of a process that has exited: the job of a marker
// left behind.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("git", "version")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// markers is the directory of the repository's worker markers.
func (f *fixture) markers() string {
	f.t.Helper()
	return filepath.Join(f.git("rev-parse", "--path-format=absolute", "--git-common-dir"), "worker")
}

// trailers are the Worker trailers of the commit.
func (f *fixture) workers(rev string) string {
	f.t.Helper()
	return f.git("log", "-1", "--format=%(trailers:key=Worker,valueonly,separator=%x2c)", rev)
}

func workerRepo(t *testing.T, versionMode string) *fixture {
	t.Helper()
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: " + versionMode + "}\n",
		"VERSION":     "1.0.0\n",
		"a.txt":       "a\n",
		"b.txt":       "b\n",
	})
	f.git("checkout", "-q", "-b", "main")
	f.mustGraft("init")
	if versionMode == "file" {
		f.write("VERSION", "1.0.1\n")
	}
	f.mustPlainCommit("chore: base")
	return f
}

// TestWorkerTrailersInTheJobsWorktree: a commit in the worktree a job runs
// in names each of its models once, the ones the message names already
// too; a commit with no job names none; the marker of a finished round
// names its models after its process exits: the master commits its work
// then. A marker that does not parse or names no process, the marker of a
// round not finished whose process is gone, the job killed but the marker
// left, and one whose pid another program took are reported and passed
// over.
func TestWorkerTrailersInTheJobsWorktree(t *testing.T) {
	f := workerRepo(t, "file")
	f.write("a.txt", "a1\n")
	f.mustGraft("commit", "-m", "feat: no job")
	if got := f.workers("HEAD"); got != "" {
		t.Errorf("no job, trailers %q", got)
	}
	writeMarker(t, f, "job1", f.work, liveJob(t, "job1"), "vendor/model-a", "vendor/model-b", "vendor/model-a")
	writeFile(t, filepath.Join(f.markers(), ".job2.x7k.tmp"), "{")
	f.write("a.txt", "a2\n")
	f.mustGraft("commit", "-m", "feat: by the job\n\nWorker: vendor/model-a\nCo-Authored-By: Someone <s@example.com>")
	if got := f.workers("HEAD"); got != "vendor/model-a,vendor/model-b" {
		t.Errorf("trailers %q\n%s", got, f.git("log", "-1", "--format=%B"))
	}
	f.write("a.txt", "a3\n")
	f.mustGraft("amend")
	if got := f.workers("HEAD"); got != "vendor/model-a,vendor/model-b" {
		t.Errorf("amended, trailers %q", got)
	}
	writeFile(t, filepath.Join(f.markers(), "job3"), "{")
	f.write("a.txt", "a4\n")
	if out := f.mustGraft("commit", "-m", "feat: broken marker"); !strings.Contains(out, "job3: not JSON: unexpected EOF: passed over") {
		t.Errorf("a broken marker:\n%s", out)
	}
	if got := f.workers("HEAD"); got != "vendor/model-a,vendor/model-b" {
		t.Errorf("next to a broken marker, trailers %q", got)
	}

	for _, m := range []string{"job1", "job3"} {
		if err := os.Remove(filepath.Join(f.markers(), m)); err != nil {
			t.Fatal(err)
		}
	}
	writeMarker(t, f, "job4", f.work, deadPID(t), "vendor/model-k")
	writeMarker(t, f, "job5", f.work, 0, "vendor/model-p")
	f.write("a.txt", "a5\n")
	out := f.mustGraft("commit", "-m", "feat: after the job was killed")
	if !strings.Contains(out, "is not finished: the job was killed, passed over") || !strings.Contains(out, "job5: id, models, dir and a pid or finished are required: passed over") {
		t.Errorf("a killed job and a marker with no pid:\n%s", out)
	}
	if got := f.workers("HEAD"); got != "" {
		t.Errorf("a killed job, trailers %q", got)
	}
	if runtime.GOOS == "linux" {
		writeMarker(t, f, "job6", f.work, liveJob(t, "another-job"), "vendor/model-r")
		f.write("a.txt", "a6\n")
		f.mustGraft("commit", "-m", "feat: a pid reused")
		if got := f.workers("HEAD"); got != "" {
			t.Errorf("a pid another program took, trailers %q", got)
		}
	}

	for _, m := range []string{"job4", "job5", "job6"} {
		if err := os.Remove(filepath.Join(f.markers(), m)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	writeMarkerJSON(t, f, map[string]any{"id": "job7", "models": []string{"vendor/model-f"}, "client": "cli", "dir": f.work, "pid": deadPID(t), "finished": true})
	f.write("a.txt", "a7\n")
	f.mustGraft("commit", "-m", "feat: the work of a finished round")
	if got := f.workers("HEAD"); got != "vendor/model-f" {
		t.Errorf("a finished round, trailers %q", got)
	}
}

// TestWorkerTrailersNotFromOtherWorktrees: a commit in one worktree names
// no model of a job running in another, even when both change the same
// file, committed or not, to the same text; a job whose worktree is gone
// is reported and passed over.
func TestWorkerTrailersNotFromOtherWorktrees(t *testing.T) {
	f := workerRepo(t, "file")
	job := filepath.Join(f.base, "job")
	f.git("worktree", "add", "-q", "-b", "w/job", job, "main")
	writeMarker(t, f, "job1", job, liveJob(t, "job1"), "vendor/model-a")
	writeFile(t, filepath.Join(job, "a.txt"), "shared\n")
	if out, code := f.exec(job, "", graftBin, "commit", "-m", "feat: the job's work"); code != 0 {
		t.Fatalf("commit in the job's worktree: exit %d\n%s", code, out)
	}
	writeFile(t, filepath.Join(job, "b.txt"), "not committed yet\n")

	f.write("a.txt", "shared\n")
	f.write("b.txt", "not committed yet\n")
	f.mustGraft("commit", "-m", "feat: the same files")
	if got := f.workers("HEAD"); got != "" {
		t.Errorf("files the job changed too, trailers %q", got)
	}
	if got := f.workers("w/job"); got != "vendor/model-a" {
		t.Errorf("the job's own commit, trailers %q", got)
	}
	writeMarker(t, f, "job2", filepath.Join(f.base, "gone"), os.Getpid(), "vendor/model-z")
	f.write("b.txt", "b2\n")
	if out := f.mustGraft("commit", "-m", "feat: a stale marker"); !strings.Contains(out, "which is gone: passed over") {
		t.Errorf("a stale marker:\n%s", out)
	}
}

// TestWorkerTrailersOfCarriedOverWork: a commit in another worktree that
// stages files as a finished job left them, its changes carried over by
// hand while the trunk moved on, names the job's models, also when the
// master changed some of them on top and when git makes the commit itself;
// one that changes the job's files otherwise, or puts back a file the job
// left as it found it, names none.
func TestWorkerTrailersOfCarriedOverWork(t *testing.T) {
	f := workerRepo(t, "none")
	job := filepath.Join(f.base, "job")
	f.git("worktree", "add", "-q", "-b", "w/job", job, "main")
	f.write("b.txt", "b2\n")
	f.mustPlainCommit("feat: the trunk moves on")
	writeMarkerJSON(t, f, map[string]any{"id": "job1", "models": []string{"vendor/model-a"}, "client": "cli", "dir": job, "finished": true})
	writeFile(t, filepath.Join(job, "a.txt"), "by the job\n")
	writeFile(t, filepath.Join(job, "c.txt"), "new by the job\n")

	f.write("a.txt", "by the master\n")
	f.write("b.txt", "b\n")
	f.mustGraft("commit", "-m", "feat: the master's own, b.txt put back")
	if got := f.workers("HEAD"); got != "" {
		t.Errorf("the job's files changed otherwise and one put back, trailers %q", got)
	}

	f.write("a.txt", "by the job\n")
	f.write("c.txt", "new by the job, and by the master\n")
	out := f.mustGraft("commit", "-m", "feat: the job's work carried over")
	if got := f.workers("HEAD"); got != "vendor/model-a" || !strings.Contains(out, "as the finished job job1 left them") {
		t.Errorf("the job's work carried over, trailers %q\n%s", got, out)
	}

	writeFile(t, filepath.Join(job, "d.txt"), "d by the job\n")
	f.write("d.txt", "d by the job\n")
	f.git("add", "d.txt")
	if out, code := f.exec(f.work, "", "git", "commit", "-q", "--no-verify", "-m", "feat: more of the job's work"); code != 0 {
		t.Fatalf("git commit: exit %d\n%s", code, out)
	}
	if got := f.workers("HEAD"); got != "vendor/model-a" {
		t.Errorf("git commit of the job's work, trailers %q\n%s", got, f.git("log", "-1", "--format=%B"))
	}
}

// TestWorkerTrailersOfMergedCommits: a merge, by git itself, and a squash
// name the models of the commits they bring in, with no marker left.
func TestWorkerTrailersOfMergedCommits(t *testing.T) {
	f := workerRepo(t, "none")
	f.git("switch", "-q", "-c", "side")
	f.write("a.txt", "side\n")
	f.mustGraft("commit", "-m", "feat: side\n\nWorker: vendor/model-a")
	f.write("side.txt", "s\n")
	f.mustGraft("commit", "-m", "feat: more side\n\nWorker: vendor/model-b")
	f.git("switch", "-q", "main")
	f.write("b.txt", "main\n")
	f.mustGraft("commit", "-m", "feat: main")

	f.git("switch", "-q", "-c", "squash")
	f.git("merge", "-q", "--squash", "side")
	f.mustGraft("commit", "-m", "feat: side squashed")
	if got := f.workers("HEAD"); got != "vendor/model-b,vendor/model-a" {
		t.Errorf("squash, trailers %q", got)
	}
	f.git("switch", "-q", "main")
	f.git("merge", "-q", "--no-ff", "--no-edit", "side")
	if got := f.workers("HEAD"); got != "vendor/model-b,vendor/model-a" {
		t.Errorf("merge, trailers %q\n%s", got, f.git("log", "-1", "--format=%B"))
	}
	if _, err := os.Stat(f.markers()); err == nil {
		t.Error("a marker was left")
	}
}

// TestWorkerTrailersInGitsOwnCommits: a commit git makes itself, git
// commit --no-verify through the prepare-commit-msg hook, names the models
// of the job running in its worktree, and none of a job in another one. A
// marker that cannot be read is reported and passed over.
func TestWorkerTrailersInGitsOwnCommits(t *testing.T) {
	f := workerRepo(t, "none")
	job := filepath.Join(f.base, "job")
	f.git("worktree", "add", "-q", "-b", "w/job", job, "main")
	writeMarker(t, f, "job1", job, liveJob(t, "job1"), "vendor/model-a")
	writeFile(t, filepath.Join(f.markers(), "job2"), "{")

	writeFile(t, filepath.Join(job, "d.txt"), "by the job\n")
	if out, code := f.exec(job, "", "git", "add", "d.txt"); code != 0 {
		t.Fatalf("git add: exit %d\n%s", code, out)
	}
	out, code := f.exec(job, "", "git", "commit", "-q", "--no-verify", "-m", "feat: by the job")
	if code != 0 || !strings.Contains(out, "the worker marker ") {
		t.Errorf("git commit in the job's worktree: exit %d\n%s", code, out)
	}
	if got := f.workers("w/job"); got != "vendor/model-a" {
		t.Errorf("git commit in the job's worktree, trailers %q\n%s", got, f.git("log", "-1", "--format=%B", "w/job"))
	}

	f.write("d.txt", "by the job\n")
	f.git("add", "d.txt")
	if out, code := f.exec(f.work, "", "git", "commit", "-q", "--no-verify", "-m", "feat: by hand"); code != 0 {
		t.Errorf("git commit: exit %d\n%s", code, out)
	}
	if got := f.workers("HEAD"); got != "" {
		t.Errorf("git commit in another worktree, trailers %q\n%s", got, f.git("log", "-1", "--format=%B"))
	}
}

// TestWorkerTrailersNotFromFilesGraftWrites: the version file graft
// commit bumps and the go.mod and go.sum it raises come out the same in every
// tree that bumps from the same base; a finished job whose tree holds them so
// is not carried over by a commit that stages only those and its own work.
func TestWorkerTrailersNotFromFilesGraftWrites(t *testing.T) {
	f := workerRepo(t, "file")
	job := filepath.Join(f.base, "job")
	f.git("worktree", "add", "-q", "-b", "w/job", job, "main")
	writeFile(t, filepath.Join(job, "VERSION"), "1.0.2\n")
	writeFile(t, filepath.Join(job, "sub", "go.mod"), "module example.com/a\n\ngo 1.22\n\nrequire example.com/lib v1.2.3\n")
	writeFile(t, filepath.Join(job, "sub", "go.sum"), "example.com/lib v1.2.3 h1:x=\n")
	f.gitIn(job, "add", "-A")
	f.gitIn(job, "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "the job's tree, bumped")
	writeFile(t, filepath.Join(job, "work.txt"), "the job's work\n")
	writeMarkerJSON(t, f, map[string]any{"id": "job1", "models": []string{"vendor/model-a"}, "client": "cli", "dir": job, "finished": true})

	f.write("sub/go.mod", "module example.com/a\n\ngo 1.22\n\nrequire example.com/lib v1.2.3\n")
	f.write("sub/go.sum", "example.com/lib v1.2.3 h1:x=\n")
	f.write("sub/a.go", "package a\n")
	f.write("a.txt", "by the master\n")
	out := f.mustGraft("commit", "-m", "feat: the master's own work")
	if got := f.read("VERSION"); got != "1.0.2\n" {
		t.Fatalf("the commit bumped VERSION to %q, want 1.0.2 as in the job's tree", got)
	}
	if got := f.workers("HEAD"); got != "" || strings.Contains(out, "as the finished job job1 left them") {
		t.Errorf("files graft writes named the job, trailers %q\n%s", got, out)
	}

	f.write("work.txt", "the job's work\n")
	out = f.mustGraft("commit", "-m", "feat: the job's work carried over")
	if got := f.workers("HEAD"); got != "vendor/model-a" {
		t.Errorf("the job's own work carried over, trailers %q\n%s", got, out)
	}
}
