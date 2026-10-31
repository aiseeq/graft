package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/loadgate"
	"github.com/aiseeq/graft/v2/internal/lock"
)

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
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh, and Windows has no SIGTERM")
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
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
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
// the caller's but the go command's own (GOFLAGS here) and the ones graft
// exec --env names: FOO=1 graft exec --heavy -- go test does not see FOO
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
		{nil, "FOO=unset GOFLAGS=-p=2"},
		{[]string{"--env", "FOO"}, "FOO=1 GOFLAGS=-p=2"},
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
			_ = cmd.Process.Kill()
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
// had a host and ran here says why, also when graft said nothing: a calm
// machine, or inside an admitted heavy task; one sent to the host says
// nothing.
func TestHeavyTaskLineSaysWhyItStayedHere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script is sh")
	}
	f, state := userRemoteRepo(t, overflowTasks+`  outer:
    heavy: true
    remote: false
    run: [[graft, vet]]
`)
	f.mustGraft("vet")
	f.mustGraft("build")
	f.mustGraft("outer")
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
