package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeMarker leaves the marker of a job running models in the worktree at
// dir from base, as the tool running it would.
func writeMarker(t *testing.T, f *fixture, id, dir, base string, models ...string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"id": id, "models": models, "client": "cli", "dir": dir, "base": base, "started": "2026-01-02T03:04:05Z"})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.markers(), id), string(data))
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
	f.mustGraft("commit", "-m", "chore: base")
	return f
}

// TestWorkerTrailersInTheJobsWorktree: a commit in the worktree a job runs
// in names each of its models once, the ones the message names already
// too; a commit with no job names none.
func TestWorkerTrailersInTheJobsWorktree(t *testing.T) {
	f := workerRepo(t, "file")
	f.write("a.txt", "a1\n")
	f.mustGraft("commit", "-m", "feat: no job")
	if got := f.workers("HEAD"); got != "" {
		t.Errorf("no job, trailers %q", got)
	}
	writeMarker(t, f, "job1", f.work, f.git("rev-parse", "HEAD"), "vendor/model-a", "vendor/model-b", "vendor/model-a")
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
	if out, code := f.graft("", "commit", "-m", "feat: broken marker"); code == 0 || !strings.Contains(out, "the worker marker ") {
		t.Errorf("a broken marker: exit %d\n%s", code, out)
	}
}

// TestWorkerTrailersForWorkCarriedOver: a commit in another worktree names
// the job's models when it stages a file the job changed since its base,
// committed or not; a commit of other files, the version file bumped by
// both aside, names none; a job whose worktree is gone is passed over.
func TestWorkerTrailersForWorkCarriedOver(t *testing.T) {
	f := workerRepo(t, "file")
	job := filepath.Join(f.base, "job")
	f.git("worktree", "add", "-q", "-b", "w/job", job, "main")
	writeMarker(t, f, "job1", job, f.git("rev-parse", "HEAD"), "vendor/model-a")
	writeFile(t, filepath.Join(job, "a.txt"), "by the job\n")
	if out, code := f.exec(job, "", graftBin, "commit", "-m", "feat: the job's work"); code != 0 {
		t.Fatalf("commit in the job's worktree: exit %d\n%s", code, out)
	}
	writeFile(t, filepath.Join(job, "c.txt"), "not committed yet\n")

	f.write("b.txt", "b1\n")
	f.mustGraft("commit", "-m", "feat: other work")
	if got := f.workers("HEAD"); got != "" {
		t.Errorf("other files, trailers %q", got)
	}
	f.write("a.txt", "by the job\n")
	f.mustGraft("commit", "-m", "feat: carried over")
	if got := f.workers("HEAD"); got != "vendor/model-a" {
		t.Errorf("a committed file of the job, trailers %q", got)
	}
	f.write("c.txt", "not committed yet\n")
	f.mustGraft("commit", "-m", "feat: carried over uncommitted")
	if got := f.workers("HEAD"); got != "vendor/model-a" {
		t.Errorf("an uncommitted file of the job, trailers %q", got)
	}
	writeMarker(t, f, "job2", filepath.Join(f.base, "gone"), f.git("rev-parse", "HEAD"), "vendor/model-z")
	f.write("b.txt", "b2\n")
	if out := f.mustGraft("commit", "-m", "feat: a stale marker"); !strings.Contains(out, "which is gone: passed over") {
		t.Errorf("a stale marker:\n%s", out)
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
