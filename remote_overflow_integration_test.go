package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// userRemoteRepo is a Go project without remote_heavy on a machine whose
// user config names one, in its default when: busy. Its run logs the argv
// to calls and runs it here, or exits 125, a path that broke, while the
// file broken exists; up answers while the file up exists. The machine is
// calm until the test loads it.
func userRemoteRepo(t *testing.T, tasks string) (f *fixture, state string) {
	t.Helper()
	state = t.TempDir()
	writeFile(t, filepath.Join(state, "up"), "")
	writeFile(t, filepath.Join(state, "up.sh"), `[ -e "`+filepath.Join(state, "up")+`" ] || { echo no route to pc; exit 1; }`+"\n")
	writeFile(t, filepath.Join(state, "run.sh"), `[ -e "`+filepath.Join(state, "broken")+`" ] && { echo connection to pc lost >&2; exit 125; }`+"\n"+
		`echo "$*" >> "`+filepath.Join(state, "calls")+`"`+"\nexec \"$@\"\n")
	f = newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\ntasks:\n" + tasks,
		"go.mod":      "module example.com/a\n\ngo 1.22\n",
		"a.go":        "package a\n",
	})
	cfgDir := t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "load: {threads: 6, poll: 20ms, settle: 10ms, swap_window: 10ms}\n"+
		"remote_heavy: {host: pc, up: [sh, "+yamlQuote(filepath.Join(state, "up.sh"))+"], run: [sh, "+yamlQuote(filepath.Join(state, "run.sh"))+"], threads: 8}\n")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, fakeMachine(t, filepath.Join(state, "proc"), 0), "XDG_CACHE_HOME="+filepath.Join(state, "cache"))
	return f, state
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
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "install: {hosts: [pc]}\n")
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
