package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "load: {threads: 6, poll: 20ms, settle: 10ms}\n")
	writeTestPressure(t, cfgDir)
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, fakeMachine(t, filepath.Join(state, "proc"), 30), "XDG_CACHE_HOME="+filepath.Join(state, "cache"))
	return f, state
}

func remoteLoadRepo(t *testing.T, tasks string, load []string) (*fixture, string) {
	t.Helper()
	f, state := remoteRepo(t, 8, tasks)
	cfg := strings.Replace(f.read(".graft.yaml"), "host: pc", "host: hostA", 1)
	if len(load) > 0 {
		words := make([]string, len(load))
		for i, word := range load {
			words[i] = yamlQuote(word)
		}
		cfg = strings.Replace(cfg, "threads: 8}", "threads: 8, load: ["+strings.Join(words, ", ")+"]}", 1)
	}
	f.write(".graft.yaml", cfg)
	return f, state
}

// TestRemoteHostLoadStats checks remote load sampling and failure handling.
func TestRemoteHostLoadStats(t *testing.T) {
	for _, tc := range []struct {
		name      string
		load      string
		wantStart float64
		wantMax   float64
		wantWarn  bool
	}{
		{name: "configured", wantStart: 3.5, wantMax: 7.25},
		{name: "unset"},
		{name: "failed", load: "[sh, -c, 'exit 1']", wantWarn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, state := remoteRepo(t, 8, "  work:\n    heavy: true\n    run: [[sh, -c, 'sleep 5.3']]\n")
			if tc.name == "configured" {
				loadScript := filepath.Join(state, "load.sh")
				writeFile(t, loadScript, "#!/bin/sh\nif [ -e "+filepath.Join(state, "seen")+" ]; then echo '7.25 2.00 1.00 1/100 123'; else touch "+filepath.Join(state, "seen")+"; echo '3.50 2.00 1.00 1/100 123'; fi\n")
				tc.load = "[sh, " + yamlQuote(loadScript) + "]"
			}
			cfg := strings.Replace(f.read(".graft.yaml"), "host: pc", "host: hostA", 1)
			if tc.load != "" {
				cfg = strings.Replace(cfg, "threads: 8}", "threads: 8, load: "+tc.load+"}", 1)
			}
			if tc.name == "unset" {
				cfg = strings.Replace(cfg, "run: [[sh, -c, 'sleep 5.3']]", "run: [[sh, -c, 'true']]", 1)
			} else if tc.name == "failed" {
				cfg = strings.Replace(cfg, "run: [[sh, -c, 'sleep 5.3']]", "run: [[sh, -c, 'sleep 0.2'], [sh, -c, 'sleep 0.2']]", 1)
			}
			f.write(".graft.yaml", cfg)
			out, code := f.graft("", "work")
			if code != 0 {
				t.Fatalf("work failed (%d):\n%s", code, out)
			}
			lines := readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl"))
			var step, task *statsLine
			for i := range lines {
				if lines[i].Host == "hostA" && lines[i].Kind == "step" {
					step = &lines[i]
				}
				if lines[i].Host == "hostA" && lines[i].Kind == "task" {
					task = &lines[i]
				}
			}
			if step == nil || task == nil {
				t.Fatalf("host records absent: %+v", lines)
			}
			if tc.name == "configured" {
				for _, line := range []*statsLine{step, task} {
					if line.LoadMin == nil || line.LoadMax == nil || *line.LoadMin != tc.wantStart || *line.LoadMax != tc.wantMax {
						t.Errorf("host load = %v, %v; want %v, %v", line.LoadMin, line.LoadMax, tc.wantStart, tc.wantMax)
					}
				}
			} else if step.LoadMin != nil || step.LoadMax != nil || task.LoadMin != nil || task.LoadMax != nil {
				t.Errorf("host records contain load without a complete host sample: step=%+v task=%+v", step, task)
			}
			if got := strings.Count(out, "remote load command"); (got == 1) != tc.wantWarn {
				t.Errorf("remote load warning count = %d; output:\n%s", got, out)
			}
		})
	}
}

// TestRemoteTaskLoadAggregatesEveryHostStep checks load from multiple host steps.
func TestRemoteTaskLoadAggregatesEveryHostStep(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake remote uses shell scripts")
	}
	state := t.TempDir()
	seen := filepath.Join(state, "seen")
	loadScript := filepath.Join(state, "load.sh")
	writeFile(t, loadScript, "#!/bin/sh\nif [ -e "+seen+" ]; then echo '7.25 2 1 1/100 123'; else touch "+seen+"; echo '3.50 2 1 1/100 123'; fi\n")
	tasks := `  work:
    heavy: true
    run: [[sh, -c, 'sleep 0.2'], [sh, -c, 'sleep 0.2']]
`
	f, state := remoteLoadRepo(t, tasks, []string{"sh", loadScript})
	out, code := f.graft("", "work")
	if code != 0 {
		t.Fatalf("work failed (%d):\n%s", code, out)
	}
	var steps []statsLine
	var task *statsLine
	lines := readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl"))
	for i := range lines {
		if lines[i].Host == "hostA" && lines[i].Kind == "step" {
			steps = append(steps, lines[i])
		}
		if lines[i].Host == "hostA" && lines[i].Kind == "task" {
			task = &lines[i]
		}
	}
	if len(steps) != 2 || task == nil {
		t.Fatalf("host records: %d steps, task=%+v", len(steps), task)
	}
	if steps[0].LoadMin == nil || *steps[0].LoadMin != 3.5 || steps[1].LoadMin == nil || *steps[1].LoadMin != 7.25 {
		t.Fatalf("step samples: %+v", steps)
	}
	if task.LoadMin == nil || *task.LoadMin != 3.5 || task.LoadMax == nil || *task.LoadMax != 7.25 {
		t.Errorf("task did not aggregate both host steps: %+v", task)
	}
}

func TestRemoteLoadFailureDuringStepIsReportedOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake remote uses shell scripts")
	}
	state := t.TempDir()
	seen := filepath.Join(state, "seen")
	loadScript := filepath.Join(state, "load.sh")
	writeFile(t, loadScript, "#!/bin/sh\nif [ -e "+seen+" ]; then exit 1; fi\ntouch "+seen+"\necho '3.50 2 1 1/100 123'\n")
	tasks := `  work:
    heavy: true
    run: [[sh, -c, 'sleep 5.5']]
`
	f, state := remoteLoadRepo(t, tasks, []string{"sh", loadScript})
	out, code := f.graft("", "work")
	if code != 0 {
		t.Fatalf("work failed (%d):\n%s", code, out)
	}
	lines := readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl"))
	var step, task *statsLine
	for i := range lines {
		if lines[i].Host == "hostA" && lines[i].Kind == "step" {
			step = &lines[i]
		}
		if lines[i].Host == "hostA" && lines[i].Kind == "task" {
			task = &lines[i]
		}
	}
	if step == nil || step.LoadMin != nil || task == nil || task.LoadMax != nil {
		t.Errorf("failed sample recorded: step=%+v task=%+v", step, task)
	}
	if got := strings.Count(out, "remote load command"); got != 1 {
		t.Errorf("warnings=%d, want 1: %s", got, out)
	}
	if strings.Contains(out, "resource stats not recorded") {
		t.Errorf("generic stats warning leaked: %s", out)
	}
}

func TestRemoteLoadDoesNotDelayStepEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake remote uses shell scripts")
	}
	tasks := `  work:
    heavy: true
    run: [[sh, -c, 'sleep 1']]
`
	f, state := remoteLoadRepo(t, tasks, []string{"sh", "-c", "sleep 3; echo '3.50 2 1 1/100 123'"})
	out, code := f.graft("", "work")
	if code != 0 {
		t.Fatalf("work failed (%d):\n%s", code, out)
	}
	for _, line := range readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl")) {
		if line.Host == "hostA" && line.Kind == "step" {
			if line.WallS >= 2 {
				t.Errorf("load command delayed step: wall_s=%v", line.WallS)
			}
			if line.LoadMin != nil || line.LoadMax != nil {
				t.Errorf("unfinished load recorded: %+v", line)
			}
		}
	}
	if strings.Contains(out, "remote load command") {
		t.Errorf("intentional cancellation warned: %s", out)
	}
}

// A go test step on the host answers for its -run like one here, with the
// GOFLAGS it is sent with; this machine's go env says nothing about it.
func TestGoTestRunCheckOnRemoteHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake go is a shell script")
	}
	f, _ := remoteRepo(t, 8, `  test:
    heavy: true
    tests: go
    run: ['go test -json -skip ^TestLocal {args} ./...']
  benched:
    heavy: true
    tests: go
    env: {GOFLAGS: -bench=.}
    run: ['go test -json {args} ./...']
`)
	fakeBin := t.TempDir()
	script := "#!/bin/sh\n[ \"$1\" = env ] && { echo -list=x; exit 0; }\n" +
		`echo '{"Action":"run","Package":"projectA/pkg","Test":"TestReal"}'` + "\n" +
		`echo '{"Action":"pass","Package":"projectA/pkg","Test":"TestReal"}'` + "\n" +
		`echo '{"Action":"pass","Package":"projectA/pkg"}'` + "\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.env = append(f.env, "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, code := f.graft("", "test", "--", "-run", "TestGone"); code == 0 || !strings.Contains(out, "heavy, on pc") ||
		!strings.Contains(out, "-run names TestGone, which no package ran (the step has -skip ^TestLocal)") {
		t.Errorf("a missing name passed on the host (exit %d):\n%s", code, out)
	}
	if out, code := f.graft("", "test", "--", "-run", "TestReal"); code != 0 {
		t.Errorf("a test that ran failed the check on the host (exit %d):\n%s", code, out)
	}
	if out, code := f.graft("", "benched", "--", "-run", "TestGone"); code != 0 {
		t.Errorf("the GOFLAGS the step is sent with left the check on (exit %d):\n%s", code, out)
	}
	f.env = append(f.env, "GOFLAGS=-bench=.")
	if out, code := f.graft("", "test", "--", "-run", "TestGone"); code != 0 {
		t.Errorf("the caller's GOFLAGS sent to the host left the check on (exit %d):\n%s", code, out)
	}
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
// threads, and the caller's GOFLAGS with the host's threads as -p, skipping this machine's queue; the
// stats name the host.
func TestHeavyTaskRunsOnRemoteHost(t *testing.T) {
	f, state := remoteRepo(t, 8, remoteTasks)
	f.env = append(f.env, "GOFLAGS=-p=3")
	out := f.mustGraft("build")
	if !strings.Contains(out, "graft: build: heavy, on pc\n") {
		// A task's steps are the project's, not the caller's command: the
		// line about the caller's variables is exec's alone.
		t.Errorf("no line naming the host, or the exec line on a task:\n%s", out)
	}
	if got := f.read("out"); got != "threads=8 env=8 heavy=local mode=fast\n" {
		t.Errorf("the step on the host: %q", got)
	}
	calls := readFile(t, filepath.Join(state, "calls"))
	if want := "env GRAFT_HEAVY=local GRAFT_HEAVY_BY="; !strings.HasPrefix(calls, want) || !strings.Contains(calls, " MODE=fast GOFLAGS=-p=3 -p=8 GRAFT_THREADS=8 GOMAXPROCS=8 sh -c echo threads=8") {
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

// TestHeavyTaskLocalChildIsNotMarkedHeavy: a light nested task runs here
// inside a heavy task whose process steps run on the host.
func TestHeavyTaskLocalChildIsNotMarkedHeavy(t *testing.T) {
	f, state := remoteRepo(t, 8, `  outer:
    heavy: true
    remote: true
    run: [{task: inner}]
  inner:
    run: [[sh, -c, 'echo inner >> out']]
`)
	f.mustGraft("outer")
	var outerHost string
	var localStep *statsLine
	for _, line := range readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl")) {
		if line.Kind == "task" && line.Task == "outer" {
			outerHost = line.Host
		}
		if line.Kind == "step" && line.Task == "inner" {
			copy := line
			localStep = &copy
		}
	}
	if outerHost != "pc" || localStep == nil {
		t.Fatalf("outer host=%q, inner step=%+v", outerHost, localStep)
	}
	if localStep.Host != "" || localStep.Heavy || localStep.Here != "" {
		t.Errorf("local step of host heavy task has misplaced metadata: %+v", *localStep)
	}
}

// TestHeavyTaskOnHostThreads: on the host a task's ${GOFLAGS} brings the
// caller's -p, which the host's threads follow; without the host's threads
// the caller's -p, the default of a command run without admission, stays
// here, and the task's own -p goes as it is.
func TestHeavyTaskOnHostThreads(t *testing.T) {
	for _, c := range []struct {
		threads int
		env     string
		want    string
	}{
		{8, "    env: {GOFLAGS: '${GOFLAGS} -race'}\n", "GOFLAGS=-p=1 -race -p=8 GRAFT_THREADS=8 GOMAXPROCS=8 sh"},
		{0, "", "GOFLAGS=-tags=x sh"},
		{0, "    env: {GOFLAGS: -p=3}\n", "GOFLAGS=-p=3 sh"},
	} {
		f, state := remoteRepo(t, c.threads, "  build:\n    heavy: true\n"+c.env+"    run: [[sh, -c, 'echo ok']]\n")
		flags := "-p=1"
		if c.threads == 0 {
			flags = "-p=1 -tags=x"
		}
		f.env = append(f.env, "GOMAXPROCS=1", "GOFLAGS="+flags)
		f.mustGraft("build")
		run := ""
		for _, line := range readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl")) {
			if line.Kind == "task" && line.Task == "build" {
				run = line.Run
			}
		}
		// The marker names the graft run that sent the step, so a graft
		// there reports (from graft <run>).
		if calls := readFile(t, filepath.Join(state, "calls")); run == "" || !strings.Contains(calls, "GRAFT_HEAVY_BY="+run+" ") || !strings.Contains(calls, c.want) {
			t.Errorf("threads %d, %q: run got %q, want GRAFT_HEAVY_BY=%s and %q", c.threads, c.env, calls, run, c.want)
		}
	}
}

// TestHeavyTaskStaysHere: GRAFT_HEAVY=local, remote: false and a host that
// does not answer keep the heavy task on this machine, in its queue, said in
// a line; anything else in GRAFT_HEAVY is an error.
func TestHeavyTaskStaysHere(t *testing.T) {
	f, state := remoteRepo(t, 8, remoteTasks)
	calm(t, state)
	f.env = append(f.env, "GRAFT_HEAVY=local")
	if out := f.mustGraft("build"); !strings.Contains(out, "graft: build: heavy, on this machine: GRAFT_HEAVY=local (from the caller)") {
		t.Errorf("GRAFT_HEAVY=local:\n%s", out)
	}
	lines := readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl"))
	for _, line := range lines {
		if line.Kind == "step" && (!line.Heavy || line.Here != "GRAFT_HEAVY=local (from the caller)") {
			t.Errorf("heavy local step stats: %+v", line)
		}
	}
	f.env = f.env[:len(f.env)-1]
	if out := f.mustGraft("here"); !strings.Contains(out, "on this machine: the task has remote: false") {
		t.Errorf("remote: false:\n%s", out)
	}
	f.write(".graft.yaml", f.read(".graft.yaml")+"  light:\n    run: [[sh, -c, 'true']]\n")
	f.mustGraft("light")
	for _, line := range readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl")) {
		if line.Kind == "step" && line.Task == "light" && (line.Heavy || line.Here != "") {
			t.Errorf("light step carries heavy or here metadata: %+v", line)
		}
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
	if out := f.mustGraft(append([]string{"exec", "--heavy", "--"}, cmd...)...); !strings.Contains(out, "graft: exec: heavy, on pc (the caller's variables, TMPDIR too, stay here: --env NAME passes one, --local runs here)") {
		t.Errorf("exec --heavy: want the line that says the caller's variables stay here:\n%s", out)
	}
	calm(t, state)
	if out := f.mustGraft(append([]string{"exec", "--heavy", "--local", "--"}, cmd...)...); !strings.Contains(out, "on this machine: GRAFT_HEAVY=local (--local)") {
		t.Errorf("exec --heavy --local:\n%s", out)
	}
	lines := readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl"))
	found := false
	for _, line := range lines {
		if line.Kind == "step" && line.Step != "" && line.Heavy && line.Here == "GRAFT_HEAVY=local (--local)" {
			found = true
		}
	}
	if !found {
		t.Errorf("local heavy exec step missing heavy/here: %+v", lines)
	}
	if got := f.read("out"); got != "exec=8 heavy=local\nexec=6 heavy=local\n" {
		t.Errorf("exec output: %q", got)
	}
	if out := f.mustGraft("exec", "--heavy", "--local", "--", graftBin, "exec", "--heavy", "--", "true"); !strings.Contains(out, "GRAFT_HEAVY=local (from graft ") {
		t.Errorf("outer local exec: %s", out)
	}
	lines = readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl"))
	foundParent := false
	for _, line := range lines {
		if line.Kind == "task" && line.Task == "exec" && line.Parent != "" && line.Here == "GRAFT_HEAVY=local (from graft "+line.Parent+")" {
			foundParent = true
		}
	}
	if !foundParent {
		t.Errorf("nested graft did not record its parent as local source: %+v", lines)
	}
	// The shell's own ${w%|*} is no .env key: the command runs, on the host.
	if out := f.mustGraft("exec", "--heavy", "--", "sh", "-c", `w="a|b"; echo ${w%|*} >> out`); !strings.Contains(out, "graft: exec: heavy, on pc") {
		t.Errorf("exec --heavy with a shell expansion:\n%s", out)
	}
	if got := f.read("out"); !strings.HasSuffix(got, "\na\n") {
		t.Errorf("exec output after the shell expansion: %q", got)
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
	// A run that does not start ran no command: graft's failure, not 127.
	writeFile(t, f.path(".graft.yaml"), strings.Replace(f.read(".graft.yaml"), "run: [sh, ", "run: [no-such-remote-run, ", 1))
	if out, code := f.graft("", "exec", "--heavy", "--", "true"); code != 125 || !strings.Contains(out, "remote_heavy run") {
		t.Errorf("a run that does not start: exit %d\n%s", code, out)
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
