package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// ioLine is the disk and profile part of a stats.jsonl line.
type ioLine struct {
	Kind      string   `json:"kind"`
	Task      string   `json:"task"`
	Profile   string   `json:"profile"`
	ReadMB    *float64 `json:"read_mb"`
	WriteMB   *float64 `json:"write_mb"`
	PeakWrite *float64 `json:"peak_write_mbs"`
}

func readIOStats(t *testing.T, path string) []ioLine {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []ioLine
	for text := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var l ioLine
		if err := json.Unmarshal([]byte(text), &l); err != nil {
			t.Fatalf("line %q: %v", text, err)
		}
		lines = append(lines, l)
	}
	return lines
}

// diskDir is a directory whose writes reach a disk: the tests' temporary
// one, unless it is tmpfs, whose pages are never written out and so never
// counted; then one in the user's cache.
func diskDir(t *testing.T) string {
	t.Helper()
	const tmpfsMagic = 0x01021994
	dir := t.TempDir()
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type != tmpfsMagic {
		return dir
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err = os.MkdirTemp(cache, "graft-it-io-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := syscall.Statfs(dir, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type == tmpfsMagic {
		t.Skip("no disk to write to: the temporary and the cache directories are tmpfs")
	}
	return dir
}

// TestStepDiskIO: a step's lines carry what it wrote and its peak write
// rate, the task line their sum and its largest step's peak; every line
// names its profile, the same from a worktree, and a graft exec's adds the
// first word of its command.
func TestStepDiskIO(t *testing.T) {
	big := filepath.Join(diskDir(t), "big")
	f, statsFile := statsRepo(t, "tasks:\n  io:\n    run:\n      - "+helperStep("write", big, "32", "1500")+"\n      - "+helperStep("ok")+"\n")
	f.mustGraft("io")
	lines := readIOStats(t, statsFile)
	if len(lines) != 3 {
		t.Fatalf("want 2 steps and the task: %+v", lines)
	}
	repo := filepath.Base(realRoot(t, f))
	for i, l := range lines {
		if l.Profile != repo+":io" {
			t.Errorf("line %d: profile %q, want %q", i, l.Profile, repo+":io")
		}
		if l.ReadMB == nil || l.WriteMB == nil {
			t.Fatalf("line %d: no read_mb or write_mb: %+v", i, l)
		}
	}
	step, ok, task := lines[0], lines[1], lines[2]
	if step.PeakWrite != nil {
		t.Logf("write_mb %v, peak_write_mbs %v; small step write_mb %v", *step.WriteMB, *step.PeakWrite, *ok.WriteMB)
	}
	if *step.WriteMB < 32 || *step.WriteMB > 64 {
		t.Errorf("a step writing 32 MB: write_mb %v", *step.WriteMB)
	}
	if step.PeakWrite == nil || *step.PeakWrite < 10 || *step.PeakWrite > 64 {
		t.Errorf("32 MB in a step of 1.5 s: peak_write_mbs %v", step.PeakWrite)
	}
	if ok.PeakWrite != nil {
		t.Errorf("a step shorter than a second has no peak: %v", *ok.PeakWrite)
	}
	if *task.WriteMB != *step.WriteMB+*ok.WriteMB || *task.ReadMB != *step.ReadMB+*ok.ReadMB {
		t.Errorf("the task does not sum its steps: %+v, steps %+v %+v", task, step, ok)
	}
	if task.PeakWrite == nil || *task.PeakWrite != *step.PeakWrite {
		t.Errorf("the task's peak is its largest step's: %v, step %v", task.PeakWrite, step.PeakWrite)
	}

	// A worktree is profiled by its main work tree's name; a graft exec by
	// the program it runs.
	f.git("add", "-A")
	f.git("commit", "-qm", "chore: tasks", "--no-verify")
	wt := filepath.Join(f.base, "elsewhere")
	f.git("worktree", "add", "-q", wt)
	if out, code := f.exec(wt, "", graftBin, "exec", "--", helperBin, "ok"); code != 0 {
		t.Fatalf("graft exec: exit %d\n%s", code, out)
	}
	exec := readIOStats(t, statsFile)[3:]
	if len(exec) != 2 {
		t.Fatalf("want the step and the task of the exec: %+v", exec)
	}
	for _, l := range exec {
		if l.Profile != repo+":exec:gatehelper" {
			t.Errorf("%s line of the exec: profile %q, want %q", l.Kind, l.Profile, repo+":exec:gatehelper")
		}
	}
}
