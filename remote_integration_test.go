package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// remoteRepo is a project whose remote_heavy host is this machine behind a
// run script that logs its argv to calls and runs it; up answers while the
// file up exists. The machine is loaded, so a heavy task left here would
// wait.
func remoteRepo(t *testing.T, threads int, tasks string) (f *fixture, state string) {
	t.Helper()
	state = t.TempDir()
	writeFile(t, filepath.Join(state, "up"), "")
	writeFile(t, filepath.Join(state, "up.sh"), `[ -e "`+filepath.Join(state, "up")+`" ] || { echo no route to pc; exit 1; }`+"\n")
	writeFile(t, filepath.Join(state, "run.sh"), `echo "$*" >> "`+filepath.Join(state, "calls")+`"`+"\nexec \"$@\"\n")
	f = newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\n" +
			"remote_heavy: {host: pc, up: [sh, " + yamlQuote(filepath.Join(state, "up.sh")) + "], run: [sh, " + yamlQuote(filepath.Join(state, "run.sh")) + "], threads: " + strconv.Itoa(threads) + "}\n" +
			"tasks:\n" + tasks,
	})
	cfgDir := t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "load: {threads: 6, poll: 20ms, settle: 10ms, swap_window: 10ms}\n")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, fakeMachine(t, filepath.Join(state, "proc"), 30), "XDG_CACHE_HOME="+filepath.Join(state, "cache"))
	return f, state
}

func calm(t *testing.T, state string) {
	t.Helper()
	setRunnable(t, filepath.Join(state, "proc"), 0)
}

const remoteTasks = `  build:
    heavy: true
    env: {MODE: fast}
    run: [[sh, -c, 'echo threads={threads} env=$GRAFT_THREADS heavy=$GRAFT_HEAVY mode=$MODE >> out']]
  here:
    heavy: true
    remote: false
    run: [[sh, -c, 'echo here={threads} >> out']]
`

// TestHeavyTaskRunsOnRemoteHost: with remote_heavy a heavy task's steps go
// to the host through its run, with graft's variables and the host's
// threads, skipping this machine's queue; the stats name the host.
func TestHeavyTaskRunsOnRemoteHost(t *testing.T) {
	f, state := remoteRepo(t, 8, remoteTasks)
	out := f.mustGraft("build")
	if !strings.Contains(out, "graft: build: heavy, on pc") {
		t.Errorf("no line naming the host:\n%s", out)
	}
	if got := f.read("out"); got != "threads=8 env=8 heavy=local mode=fast\n" {
		t.Errorf("the step on the host: %q", got)
	}
	calls := readFile(t, filepath.Join(state, "calls"))
	if want := "env GRAFT_HEAVY=local MODE=fast GRAFT_THREADS=8 GOMAXPROCS=8 sh -c echo threads=8"; !strings.HasPrefix(calls, want) {
		t.Errorf("run got %q, want it to start %q", calls, want)
	}
	hosts := map[string]string{}
	for _, l := range readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl")) {
		hosts[l.Kind] = l.Host
	}
	if hosts["task"] != "pc" || hosts["step"] != "pc" {
		t.Errorf("stats hosts: %v", hosts)
	}
}

// TestHeavyTaskStaysHere: GRAFT_HEAVY=local, remote: false and a host that
// does not answer keep the heavy task on this machine, in its queue, said in
// a line; anything else in GRAFT_HEAVY is an error.
func TestHeavyTaskStaysHere(t *testing.T) {
	f, state := remoteRepo(t, 8, remoteTasks)
	calm(t, state)
	f.env = append(f.env, "GRAFT_HEAVY=local")
	if out := f.mustGraft("build"); !strings.Contains(out, "graft: build: heavy, on this machine: GRAFT_HEAVY=local") {
		t.Errorf("GRAFT_HEAVY=local:\n%s", out)
	}
	f.env = f.env[:len(f.env)-1]
	if out := f.mustGraft("here"); !strings.Contains(out, "on this machine: the task has remote: false") {
		t.Errorf("remote: false:\n%s", out)
	}
	if err := os.Remove(filepath.Join(state, "up")); err != nil {
		t.Fatal(err)
	}
	if out := f.mustGraft("build"); !strings.Contains(out, "on this machine: pc did not answer (") || !strings.Contains(out, "no route to pc") {
		t.Errorf("host down:\n%s", out)
	}
	if got := f.read("out"); got != "threads=6 env=6 heavy=local mode=fast\nhere=6\nthreads=6 env=6 heavy= mode=fast\n" {
		t.Errorf("steps here: %q", got)
	}
	if _, err := os.Stat(filepath.Join(state, "calls")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("run was called: %v", err)
	}
	f.env = append(f.env, "GRAFT_HEAVY=pc")
	if out, code := f.graft("", "build"); code == 0 || !strings.Contains(out, `GRAFT_HEAVY="pc"`) {
		t.Errorf("GRAFT_HEAVY=pc: exit %d\n%s", code, out)
	}
}

// TestExecHeavyRunsOnRemoteHost: graft exec --heavy goes to the host from
// the work tree root, --local and a subdirectory keep it here, and --local
// alone is misuse.
func TestExecHeavyRunsOnRemoteHost(t *testing.T) {
	f, state := remoteRepo(t, 8, remoteTasks)
	cmd := []string{"sh", "-c", "echo exec=$GRAFT_THREADS heavy=$GRAFT_HEAVY >> out"}
	if out := f.mustGraft(append([]string{"exec", "--heavy", "--"}, cmd...)...); !strings.Contains(out, "graft: exec: heavy, on pc") {
		t.Errorf("exec --heavy:\n%s", out)
	}
	calm(t, state)
	if out := f.mustGraft(append([]string{"exec", "--heavy", "--local", "--"}, cmd...)...); !strings.Contains(out, "on this machine: GRAFT_HEAVY=local") {
		t.Errorf("exec --heavy --local:\n%s", out)
	}
	if got := f.read("out"); got != "exec=8 heavy=local\nexec=6 heavy=local\n" {
		t.Errorf("exec output: %q", got)
	}
	sub := f.path("sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, code := f.exec(sub, "", graftBin, "exec", "--heavy", "--", "true"); code != 0 || !strings.Contains(out, "on this machine: it runs in a subdirectory") {
		t.Errorf("exec --heavy in a subdirectory: exit %d\n%s", code, out)
	}
	if out, code := f.graft("", "exec", "--local", "--", "true"); code != 125 {
		t.Errorf("exec --local without --heavy: exit %d\n%s", code, out)
	}
}

// TestRemoteThreadsLeftToHost: with threads 0 a step on the host gets no
// GRAFT_THREADS, and one that uses {threads} is refused.
func TestRemoteThreadsLeftToHost(t *testing.T) {
	f, _ := remoteRepo(t, 0, `  bare:
    heavy: true
    run: [[sh, -c, 'echo threads=$GRAFT_THREADS >> out']]
  counted:
    heavy: true
    run: [[make, '-j{threads}']]
`)
	f.mustGraft("bare")
	if got := f.read("out"); got != "threads=\n" {
		t.Errorf("bare: %q", got)
	}
	if out, code := f.graft("", "counted"); code == 0 || !strings.Contains(out, "set remote_heavy.threads") {
		t.Errorf("counted: exit %d\n%s", code, out)
	}
}

// TestGateTaskReadingMessageStaysHere: a heavy gate task that reads the
// commit message file stays here, where the file is.
func TestGateTaskReadingMessageStaysHere(t *testing.T) {
	state := t.TempDir()
	out := filepath.Join(state, "out")
	f, host := remoteRepo(t, 8, `  msg:
    heavy: true
    run: [[sh, -c, 'cat "$GRAFT_COMMIT_MESSAGE_FILE" >> `+out+`']]
`)
	f.write(".graft.yaml", f.read(".graft.yaml")+"gate: [{task: msg}]\n")
	calm(t, host)
	f.write("a.txt", "a\n")
	if got := f.mustGraft("commit", "-m", "feat: a"); !strings.Contains(got, "graft: msg: heavy, on this machine: the commit message file is on this machine") {
		t.Errorf("commit:\n%s", got)
	}
	if got := readFile(t, out); !strings.HasPrefix(got, "feat: a") {
		t.Errorf("the gate read %q", got)
	}
}
