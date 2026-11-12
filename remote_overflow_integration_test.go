package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	akitproc "github.com/aiseeq/akit/proc"

	"github.com/aiseeq/graft/v2/internal/loadgate"
	"github.com/aiseeq/graft/v2/internal/lock"
	"github.com/aiseeq/graft/v2/internal/tasks"
)

func fakeGlint(t *testing.T, f *fixture) (string, string) {
	t.Helper()
	dir := filepath.Join(f.base, "glint-bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "glint")
	writeFile(t, path, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	f.env = append(f.env, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return path, fmt.Sprintf("%x", sum)
}

func writeHostGlint(t *testing.T, state, host, path, sum string) {
	t.Helper()
	file := filepath.Join(state, "cache", "graft", "hosts.json")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]map[string]string{host: {path: sum}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUserRemoteHeavyGlintGoesWhenInstallCopiedIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, "  lint:\n    heavy: true\n    run: [[glint, check, ./...]]\n")
	path, sum := fakeGlint(t, f)
	writeHostGlint(t, state, "pc", path, sum)
	loaded(t, state)
	cmd, out := f.start(f.work, "lint")
	waitFor(t, func() bool { return strings.Contains(out.String(), "heavy, on pc") })
	setRunnable(t, filepath.Join(state, "proc"), 0)
	// The host runs the copy install recorded, by its path: the host's PATH
	// may find another glint first.
	if err := cmd.Wait(); err != nil || !strings.Contains(out.String(), "heavy, on pc") || !strings.Contains(callsOf(t, state), path+" check ./...") {
		t.Fatalf("matching glint copy was not sent by its path %s: %v\n%s\ncalls: %q", path, err, out.String(), callsOf(t, state))
	}
}

func TestUserRemoteHeavyGlintStaysWithoutTheSameCopy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	for _, c := range []struct{ name, sum, extra, vet, reason string }{
		{"missing", "", "", "", "glint here ("},
		{"different", strings.Repeat("a", 64), "", "", "glint here ("},
		{"cpuprofile", "", "--cpuprofile=cpu.out", "", "only go test and go vet"},
		// go vet beside it may go: the glint copy alone keeps the task here.
		{"beside go vet", strings.Repeat("a", 64), "", "[go, vet, ./...], ", "glint here ("},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := "glint, check, ./..."
			if c.extra != "" {
				run += ", " + c.extra
			}
			f, state := userRemoteRepo(t, "  lint:\n    heavy: true\n    run: ["+c.vet+"["+run+"]]\n")
			path, _ := fakeGlint(t, f)
			if c.sum != "" {
				writeHostGlint(t, state, "pc", path, c.sum)
			}
			loaded(t, state)
			cmd, out := f.start(f.work, "lint")
			waitFor(t, func() bool { return strings.Contains(out.String(), "heavy, on this machine:") })
			setRunnable(t, filepath.Join(state, "proc"), 0)
			if err := cmd.Wait(); err != nil || !strings.Contains(out.String(), c.reason) || callsOf(t, state) != "" {
				t.Errorf("glint should stay: %v\n%s\ncalls: %q", err, out.String(), callsOf(t, state))
			}
		})
	}
}

// TestGlintStaysWhenPackagesAreLinkedOutOfTheTree: a tree carried to the host
// keeps a worktree.link as a link to what the host does not have, so glint
// there would find no packages of the project; packages that are part of the
// tree travel with it.
func TestGlintStaysWhenPackagesAreLinkedOutOfTheTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	const linkConfig = "  deps:\n    run: [[\"true\"]]\nworktree:\n  link:\n    - {key: [a.go], paths: [web/node_modules], setup: deps}\n"
	outside := func(t *testing.T, f *fixture, link string) {
		if err := os.Symlink(filepath.Join(f.base, "main-packages"), link); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name, task string
		packages   func(t *testing.T, f *fixture, link string)
		stays      bool
	}{
		{"link out of the tree", "", outside, true},
		{"link out of the tree, remote: true", "    remote: true\n", outside, true},
		{"relative link out of the tree", "", func(t *testing.T, f *fixture, link string) {
			if err := os.Symlink(filepath.Join("..", "..", "main-packages"), link); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"absolute link into the tree", "", func(t *testing.T, f *fixture, link string) {
			if err := os.Symlink(filepath.Join(f.work, "packages"), link); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"link inside the tree", "", func(t *testing.T, f *fixture, link string) {
			if err := os.Symlink("packages", link); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"a directory of the tree", "", func(t *testing.T, f *fixture, link string) {
			if err := os.Mkdir(link, 0o755); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"no packages", "", func(*testing.T, *fixture, string) {}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, state := userRemoteRepo(t, "  lint:\n    heavy: true\n"+c.task+"    run: [[glint, check, ./...]]\n"+linkConfig)
			path, sum := fakeGlint(t, f)
			writeHostGlint(t, state, "pc", path, sum)
			if err := os.MkdirAll(filepath.Join(f.work, "web"), 0o755); err != nil {
				t.Fatal(err)
			}
			c.packages(t, f, filepath.Join(f.work, "web", "node_modules"))
			loaded(t, state)
			want := "heavy, on pc"
			if c.stays {
				want = "heavy, on this machine: web/node_modules links out of the work tree"
			}
			cmd, out := f.start(f.work, "lint")
			waitFor(t, func() bool { return strings.Contains(out.String(), want) })
			setRunnable(t, filepath.Join(state, "proc"), 0)
			if err := cmd.Wait(); err != nil || c.stays != (callsOf(t, state) == "") {
				t.Errorf("stays=%v: %v\n%s\ncalls: %q", c.stays, err, out.String(), callsOf(t, state))
			}
		})
	}
}

func TestUserRemoteHeavyGlintInABusyGateSplit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, "")
	path, sum := fakeGlint(t, f)
	writeHostGlint(t, state, "pc", path, sum)
	config := strings.Replace(splitGateConfig, "  unit:\n    heavy: true\n    remote: true\n    run: [[go, test, -count=1, ./...]]\n", "  unit:\n    heavy: true\n    remote: true\n    run: [[go, vet, ./...], [glint, check, ./...]]\n", 1)
	f.write(".graft.yaml", config)
	loaded(t, state)
	out := runUntilMachineWait(t, f, state, "gate")
	calls := callsOf(t, state)
	if !strings.Contains(calls, "go vet ./...") || !strings.Contains(calls, "glint check ./...") || !strings.Contains(out, "echo here") {
		t.Errorf("gate split did not send both checks and keep other work here:\n%s\ncalls: %q", out, calls)
	}
}

func TestInstallRecordsTheCopyOnTheHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake rsync is sh")
	}
	dest := filepath.Join(t.TempDir(), "bin")
	f := installRepo(t, dest)
	state, cache, cfgDir, fakebin := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	writeUserConfig(t, cfgDir, "tools: {bin_dir: "+yamlQuote(dest)+"}\ninstall: {hosts: [pc]}\n")
	writeFile(t, filepath.Join(fakebin, "rsync"), "#!/bin/sh\n[ ! -e \""+filepath.Join(state, "fail")+"\" ] || exit 1\nsrc=$3\ndst=${4#*:}\nmkdir -p \"$(dirname \"$dst\")\"\ncp \"$src\" \"$dst\"\n")
	if err := os.Chmod(filepath.Join(fakebin, "rsync"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, "APPDATA="+cfgDir, "XDG_CACHE_HOME="+cache, "LOCALAPPDATA="+cache, "PATH="+fakebin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(state, "fail"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, code := f.graft("", "install-script"); code == 0 {
		t.Fatal("failed rsync passed")
	}
	registry := filepath.Join(cache, "graft", "hosts.json")
	if _, err := os.Stat(registry); !os.IsNotExist(err) {
		t.Fatalf("failed rsync wrote the host registry: %v", err)
	}
	if err := os.Remove(filepath.Join(state, "fail")); err != nil {
		t.Fatal(err)
	}
	f.mustGraft("install-script")
	installed := filepath.Join(dest, "run.sh")
	data, err := os.ReadFile(installed)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	var copies map[string]map[string]string
	encoded, err := os.ReadFile(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &copies); err != nil {
		t.Fatal(err)
	}
	if copies["pc"][installed] != fmt.Sprintf("%x", sum) {
		t.Errorf("host copy registry: %#v", copies)
	}
}

// userRemoteRepo is a Go project without remote_heavy on a machine whose
// user config names one, in its default when: busy. Its run logs the argv
// to calls and runs it here, or exits 125, a path that broke, while the
// file broken exists; up answers while the file up exists. The machine is
// calm until the test loads it.
func userRemoteRepo(t *testing.T, tasks string) (f *fixture, state string) {
	t.Helper()
	state = t.TempDir()
	writeFile(t, filepath.Join(state, "run.sh"), `[ -e "`+filepath.Join(state, "broken")+`" ] && { echo connection to pc lost >&2; exit 125; }`+"\n"+
		`echo "$*" >> "`+filepath.Join(state, "calls")+`"`+"\nexec \"$@\"\n")
	return userRemoteRepoRun(t, tasks, state, "[sh, "+yamlQuote(filepath.Join(state, "run.sh"))+"]"), state
}

// userRemoteRepoRun is userRemoteRepo with run as remote_heavy's run, its
// scripts in state.
func userRemoteRepoRun(t *testing.T, tasks, state, run string) *fixture {
	t.Helper()
	writeFile(t, filepath.Join(state, "up"), "")
	writeFile(t, filepath.Join(state, "up.sh"), `[ -e "`+filepath.Join(state, "up")+`" ] || { echo no route to pc; exit 1; }`+"\n")
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\ntasks:\n" + tasks,
		"go.mod":      "module example.com/a\n\ngo 1.22\n",
		"a.go":        "package a\n",
	})
	cfgDir := t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "load: {threads: 6, poll: 20ms, settle: 10ms}\n"+
		"remote_heavy: {host: pc, up: [sh, "+yamlQuote(filepath.Join(state, "up.sh"))+"], run: "+run+", threads: 8}\n")
	writeTestPressure(t, cfgDir)
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, fakeMachine(t, filepath.Join(state, "proc"), 0), "XDG_CACHE_HOME="+filepath.Join(state, "cache"))
	return f
}

func remoteWaitRepo(t *testing.T, notice string, limit string) (*fixture, string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	state, cfgDir := t.TempDir(), t.TempDir()
	run := filepath.Join(state, "run.sh")
	writeFile(t, run, notice+"\nexec \"$@\"\n")
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\ntasks:\n  sent:\n    heavy: true\n    remote: true\n    run: [[sh, -c, 'sleep 0.01']]\n",
		"go.mod":      "module example.com/a\n\ngo 1.22\n",
		"a.go":        "package a\n",
	})
	remote := "remote_heavy: {host: pc, up: [sh, -c, 'true'], run: [sh, " + yamlQuote(run) + "], threads: 8}\n"
	limits := ""
	if limit != "" {
		limits = "limits: {task: " + limit + "}\n"
	}
	writeUserConfig(t, cfgDir, remote+limits)
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, "XDG_CACHE_HOME="+filepath.Join(state, "cache"), fakeMachine(t, filepath.Join(state, "proc"), 0))
	return f, filepath.Join(state, "cache", "graft", "stats.jsonl"), state
}

func TestRemoteHeavyHostAdmissionWaitIsInStepAndTaskStats(t *testing.T) {
	f, statsFile, state := remoteWaitRepo(t,
		"sleep 1\necho 'graft: sent: heavy, admitted after abc' >&2\necho 'graft: sent: heavy, admitted after 0.500s' >&2\necho 'graft: nested: heavy, admitted after 0.900s' >&2", "")
	loaded(t, state)
	f.mustGraft("sent")
	lines := readStats(t, statsFile)
	for _, line := range lines {
		if line.Kind != "step" && line.Kind != "task" {
			continue
		}
		if line.Host != "pc" {
			t.Errorf("%s host = %q, want pc", line.Kind, line.Host)
		}
		if line.WaitS < 0.5 || !near(line.WaitWhy["host"], 0.5) {
			t.Errorf("%s wait = %v, why = %v; want at least 0.5s and host about 0.5s", line.Kind, line.WaitS, line.WaitWhy)
		}
	}
}

func TestRemoteHeavyWithoutHostAdmissionNoticeHasNoHostWait(t *testing.T) {
	f, statsFile, state := remoteWaitRepo(t, "", "")
	loaded(t, state)
	f.mustGraft("sent")
	for _, line := range readStats(t, statsFile) {
		if (line.Kind == "step" || line.Kind == "task") && (line.WaitS != 0 || line.WaitWhy["host"] != 0) {
			t.Errorf("%s unexpectedly has host wait: %+v", line.Kind, line)
		}
	}
}

func TestRemoteHeavyHostWaitIsExcludedFromTaskLimit(t *testing.T) {
	// The host makes the step wait past the limit, and says so.
	f, _, state := remoteWaitRepo(t, "sleep 1.2\necho 'graft: sent: heavy, admitted after 1.200s' >&2", "500ms")
	loaded(t, state)
	if out := f.mustGraft("sent"); strings.Contains(out, "over the limit") {
		t.Errorf("host queue time counted against the task limit:\n%s", out)
	}
}

func loaded(t *testing.T, state string) {
	t.Helper()
	setRunnable(t, filepath.Join(state, "proc"), 30)
}

const overflowTasks = `  vet:
    heavy: true
    run: [[go, vet, ./...]]
  build:
    heavy: true
    run: [[go, build, -o, bin/a, .]]
  script:
    heavy: true
    run: [[sh, -c, 'echo here >> out']]
  sent:
    heavy: true
    remote: true
    run: [[sh, -c, 'echo there >> out']]
  failing:
    heavy: true
    remote: true
    run: [[sh, -c, 'exit 3']]
`

// callsOf is what the user's run got, "" when it was not called.
func callsOf(t *testing.T, state string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(state, "calls"))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestUserRemoteHeavyOverflowsWhenBusy: the user config's remote_heavy
// takes a heavy task only when this machine's queue would not start it now;
// on a calm machine it runs here, as without remote_heavy.
func TestUserRemoteHeavyOverflowsWhenBusy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, overflowTasks)
	if out := f.mustGraft("vet"); strings.Contains(out, "on pc") || callsOf(t, state) != "" {
		t.Fatalf("a calm machine sent vet to pc:\n%s\ncalls: %q", out, callsOf(t, state))
	}
	loaded(t, state)
	out := f.mustGraft("vet")
	if !strings.Contains(out, "graft: vet: heavy, on pc") {
		t.Errorf("a busy machine kept vet here:\n%s", out)
	}
	if calls := callsOf(t, state); !strings.Contains(calls, "go vet ./...") {
		t.Errorf("run got %q, want the go vet step", calls)
	}
	out = f.mustGraft("exec", "--heavy", "--", "go", "vet", "./...")
	if !strings.Contains(out, "on pc") || strings.Count(callsOf(t, state), "go vet") != 2 {
		t.Errorf("exec --heavy of go vet on a busy machine stayed here:\n%s\ncalls: %q", out, callsOf(t, state))
	}
}

// TestUserRemoteHeavyKeepsOtherStepsHere: from the user config only go test
// and go vet steps that write no file go to the host, unless the task says
// remote: true; the others say why they stay and wait in the queue here.
func TestUserRemoteHeavyKeepsOtherStepsHere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, overflowTasks)
	for _, task := range []string{"build", "script"} {
		loaded(t, state)
		cmd, out := f.start(f.work, task)
		waitFor(t, func() bool { return strings.Contains(out.String(), "graft: "+task+": heavy, on this machine: ") })
		setRunnable(t, filepath.Join(state, "proc"), 0)
		if err := cmd.Wait(); err != nil {
			t.Fatalf("%s: %v\n%s", task, err, out.String())
		}
	}
	if calls := callsOf(t, state); calls != "" {
		t.Errorf("run got %q, want nothing", calls)
	}
	loaded(t, state)
	if out := f.mustGraft("sent"); !strings.Contains(out, "graft: sent: heavy, on pc") || !strings.Contains(callsOf(t, state), "echo there") {
		t.Errorf("remote: true kept the task here:\n%s\ncalls: %q", out, callsOf(t, state))
	}
}

func TestHeavyAdmissionNoticeIsPrintedOnlyAfterWaiting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, overflowTasks)
	if out := f.mustGraft("script"); strings.Contains(out, "admitted after") {
		t.Errorf("calm admission printed a wait notice:\n%s", out)
	}
	loaded(t, state)
	cmd, out := f.start(f.work, "script")
	waitFor(t, func() bool { return strings.Contains(out.String(), "waiting") })
	setRunnable(t, filepath.Join(state, "proc"), 0)
	if err := cmd.Wait(); err != nil || !strings.Contains(out.String(), "graft: script: heavy, admitted after ") {
		t.Errorf("waited admission notice missing: %v\n%s", err, out.String())
	}
}

// brokenLine is a line of remote-broken.jsonl, which the machine's guardian
// reads to write to the session that ran graft.
type brokenLine struct {
	At        string `json:"at"`
	Project   string `json:"project"`
	Task      string `json:"task"`
	Host      string `json:"host"`
	Reason    string `json:"reason"`
	PID       int    `json:"pid"`
	PPID      int    `json:"ppid"`
	Cwd       string `json:"cwd"`
	ClaudePID string `json:"claude_pid"`
	Worker    string `json:"worker"`
}

func readBroken(t *testing.T, state string) []brokenLine {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(state, "cache", "graft", "remote-broken.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var lines []brokenLine
	for l := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var b brokenLine
		if err := json.Unmarshal([]byte(l), &b); err != nil {
			t.Fatalf("remote-broken.jsonl line %q: %v", l, err)
		}
		lines = append(lines, b)
	}
	return lines
}

// TestBrokenRemotePathRunsHereAndIsJournaled: a host whose run exits 125 or
// whose up does not answer is a broken path, not a failed task: graft says
// so, runs the task here through the queue and appends a line naming the
// session and the worker job that ran it. A task that fails on the host is
// a failure, run once.
func TestBrokenRemotePathRunsHereAndIsJournaled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, overflowTasks)
	f.env = append(f.env, "CLAUDE_PID=4242", "WORKER_JOB=1028-1003-probe")
	writeFile(t, filepath.Join(state, "broken"), "")
	before := time.Now().Add(-time.Second)
	for i, cause := range []string{"connection to pc lost", "no route to pc"} {
		if i == 1 {
			if err := os.Remove(filepath.Join(state, "broken")); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(state, "up")); err != nil {
				t.Fatal(err)
			}
		}
		loaded(t, state)
		cmd, out := f.start(f.work, "sent")
		waitFor(t, func() bool { return strings.Contains(out.String(), "graft: sent: pc path broken: ") })
		setRunnable(t, filepath.Join(state, "proc"), 0)
		if err := cmd.Wait(); err != nil {
			t.Fatalf("%s: the task did not run here: %v\n%s", cause, err, out.String())
		}
		if !strings.Contains(out.String(), cause) || !strings.Contains(out.String(), "running here") {
			t.Errorf("%s: the broken line lacks the cause or the fallback:\n%s", cause, out.String())
		}
	}
	if got := f.read("out"); got != "there\nthere\n" {
		t.Errorf("the task's runs here: %q", got)
	}
	lines := readBroken(t, state)
	if len(lines) != 2 {
		t.Fatalf("remote-broken.jsonl has %d lines, want 2: %+v", len(lines), lines)
	}
	for i, l := range lines {
		at, err := time.Parse(time.RFC3339, l.At)
		if err != nil || at.Before(before) {
			t.Errorf("line %d at %q: %v", i, l.At, err)
		}
		if l.Task != "sent" || l.Host != "pc" || l.PID <= 0 || l.PPID <= 0 || l.Cwd == "" || l.Project == "" || l.ClaudePID != "4242" || l.Worker != "1028-1003-probe" {
			t.Errorf("line %d: %+v", i, l)
		}
	}
	if !strings.Contains(lines[0].Reason, "125") || !strings.Contains(lines[1].Reason, "no route to pc") {
		t.Errorf("reasons: %q, %q", lines[0].Reason, lines[1].Reason)
	}
	writeFile(t, filepath.Join(state, "up"), "")
	loaded(t, state)
	out, code := f.graft("", "failing")
	if code == 0 || strings.Contains(out, "path broken") || strings.Count(callsOf(t, state), "exit 3") != 1 || len(readBroken(t, state)) != 2 {
		t.Errorf("a task failing on the host: exit %d, calls %q\n%s", code, callsOf(t, state), out)
	}
}

// TestProjectRemoteHeavyWinsOverTheUsers: a project's own remote_heavy
// replaces the user's whole, and keeps its default when: always.
func TestProjectRemoteHeavyWinsOverTheUsers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, overflowTasks)
	own := filepath.Join(state, "own-calls")
	writeFile(t, filepath.Join(state, "own.sh"), `echo "$*" >> "`+own+`"`+"\nexec \"$@\"\n")
	f.write(".graft.yaml", strings.Replace(f.read(".graft.yaml"), "tasks:\n",
		"remote_heavy: {host: box, up: [sh, -c, 'true'], run: [sh, "+yamlQuote(filepath.Join(state, "own.sh"))+"], threads: 4}\ntasks:\n", 1))
	out := f.mustGraft("script")
	if !strings.Contains(out, "graft: script: heavy, on box") || !strings.Contains(readFile(t, own), "echo here") || callsOf(t, state) != "" {
		t.Errorf("the project's remote_heavy did not take the task on a calm machine:\n%s\ncalls: %q", out, callsOf(t, state))
	}
}

// TestInstallCopiesToTheUsersHosts: with install.hosts in the user config
// the install step, after installing here, puts the same binary at the same
// path on each host with rsync; a host it cannot reach fails the step loudly
// and leaves the binary installed here.
func TestInstallCopiesToTheUsersHosts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake rsync is sh")
	}
	dest := filepath.Join(t.TempDir(), "bin")
	f := installRepo(t, dest)
	state := t.TempDir()
	fake := filepath.Join(state, "fakebin")
	// The fake rsync takes the last two arguments as source and host:path,
	// and copies into hosts/<host><path>; the host down fails like rsync.
	writeFile(t, filepath.Join(fake, "rsync"), "#!/bin/sh\n"+
		`echo "$*" >> "`+filepath.Join(state, "rsync")+`"`+"\n"+
		`for a; do src=$dst; dst=$a; done`+"\n"+
		`host=${dst%%:*}; path=${dst#*:}`+"\n"+
		`[ -e "`+filepath.Join(state, "down")+`" ] && { echo "ssh: connect to host $host: No route to host" >&2; exit 255; }`+"\n"+
		`mkdir -p "`+filepath.Join(state, "hosts")+`/$host$(dirname "$path")" && cp "$src" "`+filepath.Join(state, "hosts")+`/$host$path"`+"\n")
	if err := os.Chmod(filepath.Join(fake, "rsync"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgDir := t.TempDir()
	writeUserConfig(t, cfgDir, "install: {hosts: [pc]}\n")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, "PATH="+fake+string(os.PathListSeparator)+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	installed := filepath.Join(dest, "tool")
	out := f.mustGraft("install")
	if !strings.Contains(out, "graft: install: tool ") || !strings.Contains(out, " -> pc:"+installed) {
		t.Errorf("install does not say it copied to pc:\n%s", out)
	}
	if got := readBytes(t, filepath.Join(state, "hosts", "pc"+installed)); string(got) != string(readBytes(t, installed)) {
		t.Errorf("pc got another binary than %s", installed)
	}
	writeFile(t, filepath.Join(state, "down"), "")
	f.write("main.go", "package main\n\nfunc main() { println(\"two\") }\n")
	f.git("commit", "-q", "-am", "two")
	out, code := f.graft("", "install")
	if code == 0 || !strings.Contains(out, "pc") || !strings.Contains(out, "No route to host") {
		t.Errorf("an unreachable host: exit %d\n%s", code, out)
	}
	if !strings.Contains(string(readBytes(t, installed)), "two") {
		t.Errorf("the binary here was not installed before the host failed")
	}
}

// TestInstallHereDoesNotStopAtAHostDown: a host down does not stop the
// install of the other files here; the step tries the host once and fails
// naming it and every file it did not get.
func TestInstallHereDoesNotStopAtAHostDown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake rsync is sh")
	}
	dest := filepath.Join(t.TempDir(), "bin")
	f := installRepo(t, dest)
	f.write("scripts/two.sh", "#!/bin/sh\necho two\n")
	f.git("add", "-A")
	f.git("commit", "-q", "-m", "two scripts")
	state := t.TempDir()
	fake := filepath.Join(state, "fakebin")
	writeFile(t, filepath.Join(fake, "rsync"), "#!/bin/sh\n"+
		`echo "$*" >> "`+filepath.Join(state, "rsync")+`"`+"\n"+
		`echo "ssh: Could not resolve hostname pc" >&2; exit 255`+"\n")
	if err := os.Chmod(filepath.Join(fake, "rsync"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgDir := t.TempDir()
	writeUserConfig(t, cfgDir, "install: {hosts: [pc]}\n")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, "PATH="+fake+string(os.PathListSeparator)+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, code := f.graft("", "install-script")
	for _, name := range []string{"run.sh", "two.sh"} {
		if _, err := os.Stat(filepath.Join(dest, name)); err != nil {
			t.Errorf("%s was not installed here: %v\n%s", name, err, out)
		}
		if !strings.Contains(out, name) {
			t.Errorf("the failure does not name %s:\n%s", name, out)
		}
	}
	if code == 0 || !strings.Contains(out, "pc") || !strings.Contains(out, "Could not resolve hostname") {
		t.Errorf("a host down: exit %d\n%s", code, out)
	}
	if n := strings.Count(readFile(t, filepath.Join(state, "rsync")), "\n"); n != 1 {
		t.Errorf("rsync to the host down ran %d times, want once", n)
	}
}

const inDirTasks = `  indir:
    heavy: true
    remote: true
    dir: sub
    run: [[sh, -c, 'echo there >> out']]
`

// TestUserRemoteHeavyRunsATaskInItsDirectoryWithRoot: a task with dir goes
// to the host only when remote_heavy's run takes the work tree root as
// {root}, and then starts from the task's directory with the root filled
// in; without {root} it stays here, as the line says.
func TestUserRemoteHeavyRunsATaskInItsDirectoryWithRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, inDirTasks)
	f.write("sub/keep", "")
	loaded(t, state)
	cmd, out := f.start(f.work, "indir")
	waitFor(t, func() bool { return strings.Contains(out.String(), "names no {root}") })
	setRunnable(t, filepath.Join(state, "proc"), 0)
	if err := cmd.Wait(); err != nil || callsOf(t, state) != "" {
		t.Errorf("a task with dir went to a run without {root} (%v):\n%s", err, out.String())
	}

	state = t.TempDir()
	writeFile(t, filepath.Join(state, "run.sh"), `root=$1; shift; echo "$PWD root=$root $*" >> "`+filepath.Join(state, "calls")+`"`+"\nexec \"$@\"\n")
	f = userRemoteRepoRun(t, inDirTasks, state, "[sh, "+yamlQuote(filepath.Join(state, "run.sh"))+", '{root}']")
	f.write("sub/keep", "")
	loaded(t, state)
	sent := f.mustGraft("indir")
	if calls := callsOf(t, state); !strings.HasPrefix(calls, filepath.Join(f.work, "sub")+" root="+f.work+" env ") {
		t.Errorf("run got %q, want it in sub with the root\n%s", calls, sent)
	}
	if got := f.read("sub/out"); got != "there\n" {
		t.Errorf("sub/out = %q, want the step run in sub", got)
	}
}

// TestUserRemoteHeavyStopsACancelledStepWithSIGTERM: an interrupted graft
// asks remote_heavy's run to stop (SIGTERM), so it can take its work off the
// host, rather than killing it.
func TestUserRemoteHeavyStopsACancelledStepWithSIGTERM(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses Linux child signaling and a shell run script")
	}
	state := t.TempDir()
	writeFile(t, filepath.Join(state, "run.sh"), `trap 'echo term > "`+filepath.Join(state, "term")+`"; kill $pid; exit 143' TERM`+"\n"+
		`echo started > "`+filepath.Join(state, "calls")+`"`+"\n\"$@\" &\npid=$!\nwait $pid\n")
	f := userRemoteRepoRun(t, "  long:\n    heavy: true\n    remote: true\n    run: [[sleep, '10']]\n", state, "[sh, "+yamlQuote(filepath.Join(state, "run.sh"))+"]")
	loaded(t, state)
	cmd, out := f.start(f.work, "long")
	for deadline := time.Now().Add(10 * time.Second); callsOf(t, state) == ""; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("run did not start:\n%s", out.String())
		}
	}
	if err := akitproc.SignalChild(cmd.Process.Pid, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if data, err := os.ReadFile(filepath.Join(state, "term")); err != nil || string(data) != "term\n" {
		t.Errorf("run did not get SIGTERM (%v):\n%s", err, out.String())
	}
}

// TestUserRemoteHeavyOverflowsBehindALiveReserve: a live reservation makes
// the next task wait here, so it goes to the host though the machine is calm.
func TestUserRemoteHeavyOverflowsBehindALiveReserve(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, overflowTasks)
	reserveData, err := json.Marshal(struct {
		Cores    int    `json:"cores"`
		Admitted string `json:"admitted"`
		Task     string `json:"task"`
	}{22, time.Now().Format(time.RFC3339Nano), "projectA"})
	if err != nil {
		t.Fatal(err)
	}
	releaseReserve, err := lock.Reserve(filepath.Join(state, "cache", "graft"), "load.lock.*.reserve", reserveData)
	if err != nil {
		t.Fatal(err)
	}
	out := f.mustGraft("vet")
	if !strings.Contains(out, "graft: vet: heavy, on pc") || !strings.Contains(callsOf(t, state), "go vet ./...") {
		t.Errorf("behind a held queue vet stayed here:\n%s\ncalls: %q", out, callsOf(t, state))
	}
	if err := releaseReserve(); err != nil {
		t.Fatal(err)
	}
	// A graft a step of an admitted task starts does not queue: it runs here.
	admitted := *f
	admitted.env = append(slices.Clone(f.env), "GRAFT_ADMITTED=1")
	if out := admitted.mustGraft("exec", "--heavy", "--", "go", "vet", "./..."); strings.Contains(out, "on pc") || strings.Count(callsOf(t, state), "go vet") != 1 {
		t.Errorf("an admitted exec went to pc behind the queue it does not wait for:\n%s\ncalls: %q", out, callsOf(t, state))
	}
	// A task with a priority waits for the machine, not for the queue: on a
	// calm machine it runs here, on a busy one it goes.
	budget, err := loadgate.NewBudget(t.TempDir(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	prio := *f
	prio.env = append(slices.Clone(f.env), loadgate.EnvPriority+"=deploy", loadgate.EnvBudget+"="+budget, loadgate.EnvThen+"="+loadgate.ThenStart)
	if out := prio.mustGraft("exec", "--heavy", "--", "go", "vet", "./..."); strings.Contains(out, "on pc") || strings.Count(callsOf(t, state), "go vet") != 1 {
		t.Errorf("a task with a priority on a calm machine went to pc:\n%s\ncalls: %q", out, callsOf(t, state))
	}
	loaded(t, state)
	if out := prio.mustGraft("exec", "--heavy", "--", "go", "vet", "./..."); !strings.Contains(out, "on pc") {
		t.Errorf("a task with a priority on a busy machine stayed here:\n%s\ncalls: %q", out, callsOf(t, state))
	}
}

// TestUserRemoteHeavyTestFindsTheStandardLibrary: the host does not get this
// machine's environment, so a go test sent there runs without the GOROOT
// graft sets here for a -trimpath test; the test still finds its GOROOT.
func TestUserRemoteHeavyTestFindsTheStandardLibrary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	state := t.TempDir()
	writeFile(t, filepath.Join(state, "run.sh"), `echo "$*" >> "`+filepath.Join(state, "calls")+`"`+"\nexec env -u GOROOT \"$@\"\n")
	f := userRemoteRepoRun(t, "  unit:\n    heavy: true\n    run: [[go, test, -count=1, ./...]]\n", state, "[sh, "+yamlQuote(filepath.Join(state, "run.sh"))+"]")
	f.write("a_test.go", "package a\n\nimport (\n\t\"runtime\"\n\t\"testing\"\n)\n\nfunc TestRoot(t *testing.T) {\n\tif runtime.GOROOT() == \"\" {\n\t\tt.Fatal(\"no GOROOT: go/importer finds no standard library\")\n\t}\n}\n")
	loaded(t, state)
	out, code := f.graft("", "unit")
	if code != 0 || !strings.Contains(out, "graft: unit: heavy, on pc") {
		t.Errorf("go test on pc: exit %d\n%s\ncalls: %q", code, out, callsOf(t, state))
	}
	out, code = f.graft("", "exec", "--heavy", "--", "go", "test", "-count=1", "./...")
	if code != 0 || !strings.Contains(out, "graft: exec: heavy, on pc") {
		t.Errorf("exec go test on pc: exit %d\n%s\ncalls: %q", code, out, callsOf(t, state))
	}
}

// TestUserRemoteHeavyPassesTheVariablesNamed: the host gets no variable of
// the caller's but the go command's own (GOFLAGS here, the host's threads
// as -p after it) and the ones graft exec --env names: FOO=1 graft exec --heavy -- go test does not see FOO
// there, with --env FOO it does.
func TestUserRemoteHeavyPassesTheVariablesNamed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	state := t.TempDir()
	// The host's environment lacks the caller's variables.
	writeFile(t, filepath.Join(state, "run.sh"), `echo "$*" >> "`+filepath.Join(state, "calls")+`"`+"\nexec env -u FOO -u GOFLAGS \"$@\"\n")
	f := userRemoteRepoRun(t, "", state, "[sh, "+yamlQuote(filepath.Join(state, "run.sh"))+"]")
	f.write("a_test.go", "package a\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\n"+
		"func TestEnv(t *testing.T) {\n\tfoo, set := os.LookupEnv(\"FOO\")\n\tif !set {\n\t\tfoo = \"unset\"\n\t}\n"+
		"\tif err := os.WriteFile(\"env.out\", []byte(\"FOO=\"+foo+\" GOFLAGS=\"+os.Getenv(\"GOFLAGS\")), 0o644); err != nil {\n\t\tt.Fatal(err)\n\t}\n}\n")
	f.env = append(f.env, "FOO=1", "GOFLAGS=-p=2")
	for _, c := range []struct {
		flags []string
		want  string
	}{
		{nil, "FOO=unset GOFLAGS=-p=2 -p=8"},
		{[]string{"--env", "FOO"}, "FOO=1 GOFLAGS=-p=2 -p=8"},
	} {
		loaded(t, state)
		args := append(append([]string{"exec", "--heavy"}, c.flags...), "--", "go", "test", "-count=1", "./...")
		out, code := f.graft("", args...)
		if code != 0 || !strings.Contains(out, "graft: exec: heavy, on pc") {
			t.Fatalf("%v: exit %d\n%s\ncalls: %q", c.flags, code, out, callsOf(t, state))
		}
		if got := readFile(t, f.path("env.out")); got != c.want {
			t.Errorf("%v: the test on pc saw %q, want %q", c.flags, got, c.want)
		}
	}
}

// TestUserRemoteHeavyKeepsATaskReadingFilesOfThisMachine: a task whose cache
// names files outside the tree reads them, and the host has none of them.
func TestUserRemoteHeavyKeepsATaskReadingFilesOfThisMachine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	outside := filepath.Join(t.TempDir(), "models")
	writeFile(t, outside, "model-a\n")
	f, state := userRemoteRepo(t, "  unit:\n    heavy: true\n    cache: {files: ["+yamlQuote(outside)+"]}\n    run: [[go, vet, ./...]]\n")
	loaded(t, state)
	cmd, out := f.start(f.work, "unit")
	want := "graft: unit: heavy, on this machine: the task reads " + outside + " (cache.files), which the host does not have"
	waitFor(t, func() bool { return strings.Contains(out.String(), "graft: unit: heavy, on ") })
	setRunnable(t, filepath.Join(state, "proc"), 0)
	if err := cmd.Wait(); err != nil || !strings.Contains(out.String(), want) || callsOf(t, state) != "" {
		t.Errorf("a task reading a file of this machine went to pc: %v\n%s\ncalls: %q", err, out.String(), callsOf(t, state))
	}
}

// TestUserRemoteHeavyJudgesTheArgumentsGiven: a go test step with {args}
// goes to the host when the arguments given write no file, and stays here
// when one of them does.
func TestUserRemoteHeavyJudgesTheArgumentsGiven(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, "  test:\n    heavy: true\n    run: [[go, test, -v, '{args}', ./...]]\n")
	for _, c := range []struct {
		args  []string
		where string
	}{
		{[]string{"test"}, "on pc"},
		{[]string{"test", "--", "-run", "TestX"}, "on pc"},
		{[]string{"test", "--", "-coverprofile=c.out"}, "on this machine: "},
	} {
		loaded(t, state)
		cmd, out := f.start(f.work, c.args...)
		waitFor(t, func() bool { return strings.Contains(out.String(), "graft: test: heavy, on ") })
		setRunnable(t, filepath.Join(state, "proc"), 0)
		if err := cmd.Wait(); err != nil || !strings.Contains(out.String(), "graft: test: heavy, "+c.where) {
			t.Errorf("graft %q on a busy machine, want %q: %v\n%s", c.args, c.where, err, out.String())
		}
	}
	if calls := callsOf(t, state); strings.Contains(calls, "coverprofile") || strings.Count(calls, "go test") != 2 {
		t.Errorf("run got %q, want the two runs without a file", calls)
	}
}

// TestExecBrokenRemotePathRunsHere: graft exec --heavy sent to the
// user's host whose run exits broken_exit runs the command here through the
// heavy queue and journals the break, as a heavy task does.
func TestExecBrokenRemotePathRunsHere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, overflowTasks)
	writeFile(t, filepath.Join(state, "broken"), "")
	loaded(t, state)
	cmd, out := f.start(f.work, "exec", "--heavy", "--", "go", "vet", "./...")
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(out.String(), "graft: exec: pc path broken: "); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			stopTestChild(cmd)
			t.Fatalf("no broken path line:\n%s", out.String())
		}
	}
	setRunnable(t, filepath.Join(state, "proc"), 0)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("exec did not run here: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "connection to pc lost") || !strings.Contains(out.String(), "running here") {
		t.Errorf("the broken line lacks the cause or the fallback:\n%s", out.String())
	}
	if strings.Count(out.String(), "graft: exec: heavy, on") != 1 {
		t.Errorf("want one placement line, sent to pc once:\n%s", out.String())
	}
	lines := readBroken(t, state)
	if len(lines) != 1 || lines[0].Task != "exec" || lines[0].Host != "pc" {
		t.Errorf("remote-broken.jsonl: %+v, want one exec line for pc", lines)
	}
}

// TestHeavyTaskLineSaysWhyItStayedHere: the task line of a heavy task that
// had a host and ran here says why: a calm machine, which graft also says,
// or inside an admitted heavy task, which it does not; one sent to the host
// says nothing.
func TestHeavyTaskLineSaysWhyItStayedHere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, overflowTasks+`  outer:
    heavy: true
    remote: false
    run: [[graft, vet]]
`)
	if out := f.mustGraft("vet"); !strings.Contains(out, "graft: vet: heavy, on this machine: "+tasks.WhyNotBusy+"\n") {
		t.Errorf("a calm machine does not say the task stays here:\n%s", out)
	}
	f.mustGraft("build")
	if out := f.mustGraft("outer"); strings.Contains(out, tasks.WhyInsideHeavy) {
		t.Errorf("a task inside an admitted one says it stays here:\n%s", out)
	}
	loaded(t, state)
	f.mustGraft("vet")
	var got []string
	for _, l := range readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl")) {
		if l.Kind == "task" {
			got = append(got, l.Task+": "+l.Host+"|"+l.Here)
		}
	}
	want := []string{
		"vet: |this machine would not make it wait",
		"build: |only go test and go vet steps that write no file can use user remote_heavy",
		"vet: |inside an admitted heavy task, which waits for the machine",
		"outer: |the task has remote: false",
		"vet: pc|",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("task lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestProjectRemoteHeavyBrokenPathRunsHere: a project's own remote_heavy
// whose run exits its broken_exit runs the task here, says so and journals
// it, as the user's does; another exit is the task's failure, not retried.
func TestProjectRemoteHeavyBrokenPathRunsHere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, overflowTasks)
	own := filepath.Join(state, "own-calls")
	broken := filepath.Join(state, "own-broken")
	writeFile(t, filepath.Join(state, "own.sh"), `[ -e "`+broken+`" ] && { echo queue of box is full >&2; exit 75; }`+"\n"+
		`echo "$*" >> "`+own+`"`+"\nexec \"$@\"\n")
	f.write(".graft.yaml", strings.Replace(f.read(".graft.yaml"), "tasks:\n",
		"remote_heavy: {host: box, up: [sh, -c, 'true'], run: [sh, "+yamlQuote(filepath.Join(state, "own.sh"))+"], threads: 4, broken_exit: 75}\ntasks:\n", 1))
	writeFile(t, broken, "")
	out := f.mustGraft("sent")
	if !strings.Contains(out, "graft: sent: box path broken: run exited 75: queue of box is full, running here") || f.read("out") != "there\n" {
		t.Errorf("the task did not run here after the broken path:\n%s", out)
	}
	if lines := readBroken(t, state); len(lines) != 1 || lines[0].Host != "box" || !strings.Contains(lines[0].Reason, "queue of box is full") {
		t.Errorf("remote-broken.jsonl: %+v", lines)
	}
	var sentHere []string
	for _, l := range readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl")) {
		if l.Kind == "task" && l.Task == "sent" {
			sentHere = append(sentHere, l.Host+"|"+l.Here)
		}
	}
	if len(sentHere) != 1 || sentHere[0] != "|box path broken: run exited 75: queue of box is full" {
		t.Errorf("the task line of the task run here after the broken path: %q", sentHere)
	}
	if err := os.Remove(broken); err != nil {
		t.Fatal(err)
	}
	out, code := f.graft("", "failing")
	if code == 0 || strings.Contains(out, "path broken") || strings.Count(readFile(t, own), "exit 3") != 1 {
		t.Errorf("a task failing on the host: exit %d, calls %q\n%s", code, readFile(t, own), out)
	}
}

// TestProjectRemoteHeavyWithoutBrokenExitRunsNothingAgain: a project's
// remote_heavy that names no broken_exit has none: a run exiting 125, the
// command's own status as much as a broken path, is the task's failure, not
// run here again.
func TestProjectRemoteHeavyWithoutBrokenExitRunsNothingAgain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, overflowTasks)
	own := filepath.Join(state, "own-calls")
	writeFile(t, filepath.Join(state, "own.sh"), `echo "$*" >> "`+own+`"`+"\nexit 125\n")
	f.write(".graft.yaml", strings.Replace(f.read(".graft.yaml"), "tasks:\n",
		"remote_heavy: {host: box, up: [sh, -c, 'true'], run: [sh, "+yamlQuote(filepath.Join(state, "own.sh"))+"], threads: 4}\ntasks:\n", 1))
	out, code := f.graft("", "sent")
	if code == 0 || strings.Contains(out, "path broken") || strings.Count(readFile(t, own), "echo there") != 1 {
		t.Errorf("exit %d, calls %q\n%s", code, readFile(t, own), out)
	}
	if _, err := os.Stat(filepath.Join(f.work, "out")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the task ran here: %v", err)
	}
	if lines := readBroken(t, state); len(lines) != 0 {
		t.Errorf("remote-broken.jsonl: %+v", lines)
	}
}

// TestRemoteHeavyHostWaitIsNoLongerThanTheStep: a line claiming more wait
// than the step's wall time counts the wall time, so own time stays >= 0.
func TestRemoteHeavyHostWaitIsNoLongerThanTheStep(t *testing.T) {
	f, statsFile, state := remoteWaitRepo(t, "echo 'graft: sent: heavy, admitted after 60.000s' >&2", "")
	loaded(t, state)
	f.mustGraft("sent")
	for _, line := range readStats(t, statsFile) {
		if (line.Kind == "step" || line.Kind == "task") && (line.WaitWhy["host"] <= 0 || line.WaitS > line.WallS) {
			t.Errorf("%s wait %v, host %v, wall %v: want a host wait no longer than the wall", line.Kind, line.WaitS, line.WaitWhy["host"], line.WallS)
		}
	}
}
