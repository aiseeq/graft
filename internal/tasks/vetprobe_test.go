package tasks

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	akitproc "github.com/aiseeq/akit/proc"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/iosched"
	"github.com/aiseeq/graft/v2/internal/loadgate"
	"github.com/aiseeq/graft/v2/internal/stats"
)

// vetProbeRunner is a Runner whose step records go to a stats file of the
// test and to its one frame.
func vetProbeRunner(t *testing.T) (*Runner, *frame, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var out, errOut bytes.Buffer
	f := &frame{task: stats.GateTask}
	r := &Runner{
		Root: t.TempDir(), Stats: stats.NewRecorder(io.Discard),
		frames: []*frame{f}, noRepoName: true, Stdout: &out, Stderr: &errOut,
	}
	return r, f, &out, &errOut
}

func vetProbeRecords(t *testing.T) []stats.Record {
	t.Helper()
	path, err := stats.Path()
	if err != nil {
		t.Fatal(err)
	}
	records, _, err := stats.Load(path, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return records
}

// vetScripts is a command for builtinVetStep: the trimmed run is the shell
// script trimmed, the other one again; calls gets the argv of each.
func vetScripts(calls *[][]string, trimmed, again string) func([]string) *exec.Cmd {
	return func(argv []string) *exec.Cmd {
		*calls = append(*calls, argv)
		script := again
		if slices.Contains(argv, "-trimpath") {
			script = trimmed
		}
		return exec.Command("sh", "-c", script)
	}
}

var (
	vetTrimmedArgv = []string{"go", "vet", "-trimpath", "./..."}
	vetArgv        = []string{"go", "vet", "./..."}
)

func TestBuiltinVetStepShowsOnlyTheRerunOfAReportedVet(t *testing.T) {
	r, f, out, errOut := vetProbeRunner(t)
	var calls [][]string
	err := r.builtinVetStep(context.Background(), GoVetSource, vetTrimmedArgv, vetArgv, vetScripts(&calls,
		"sleep 0.2; echo trimmed out; echo '../work/pkg/p.go:5:39: bad' >&2; exit 1",
		"sleep 0.2; echo '# example.com/projecta/pkg'; echo 'pkg/p.go:5:39: bad' >&2; exit 1"))
	if err == nil {
		t.Fatal("a vet whose rerun failed passed")
	}
	if len(calls) != 2 || !slices.Equal(calls[0], vetTrimmedArgv) || !slices.Equal(calls[1], vetArgv) {
		t.Fatalf("calls = %q, want the trimmed vet, then vet as written", calls)
	}
	shown := out.String() + errOut.String()
	if strings.Contains(shown, "../work/") || strings.Contains(shown, "trimmed out") {
		t.Errorf("the trimmed run's output was shown:\n%s", shown)
	}
	if !strings.Contains(errOut.String(), "pkg/p.go:5:39: bad") || !strings.Contains(out.String(), "# example.com/projecta/pkg") {
		t.Errorf("the rerun's output was not shown:\nstdout %q\nstderr %q", out, errOut)
	}
	if n := strings.Count(errOut.String(), vetRerunNotice); n != 1 {
		t.Errorf("rerun notice said %d times, want once:\n%s", n, errOut)
	}
	if f.steps != 1 || f.exit != 1 || !strings.Contains(f.reason, "pkg/p.go:5:39") || strings.Contains(f.reason, "../work/") {
		t.Errorf("frame steps %d exit %d reason %q, want the rerun's only", f.steps, f.exit, f.reason)
	}
	records := vetProbeRecords(t)
	if len(records) != 1 || records[0].Exit != 1 || records[0].Step != GoVetSource || strings.Contains(records[0].Reason, "../work/") {
		t.Fatalf("records = %+v, want one: the rerun's", records)
	}
	if records[0].WallS < 0.4 {
		t.Errorf("the record's wall %gs misses the trimmed run's", records[0].WallS)
	}
}

func TestBuiltinVetStepRerunThatPassesLeavesNoFailure(t *testing.T) {
	r, f, _, errOut := vetProbeRunner(t)
	var calls [][]string
	err := r.builtinVetStep(context.Background(), GoVetSource, vetTrimmedArgv, vetArgv, vetScripts(&calls,
		"echo stale >&2; exit 1", "exit 0"))
	if err != nil || len(calls) != 2 {
		t.Fatalf("err %v, calls %q: want the passing rerun", err, calls)
	}
	if f.steps != 1 || f.exit != 0 || f.reason != "" {
		t.Errorf("frame steps %d exit %d reason %q, want one passed step", f.steps, f.exit, f.reason)
	}
	if records := vetProbeRecords(t); len(records) != 1 || records[0].Exit != 0 || records[0].Reason != "" {
		t.Fatalf("records = %+v, want one passed step", records)
	}
	if strings.Contains(errOut.String(), "stale") {
		t.Errorf("the trimmed run's output was shown:\n%s", errOut)
	}
}

func TestBuiltinVetStepCleanRunIsTheStep(t *testing.T) {
	r, f, out, errOut := vetProbeRunner(t)
	var calls [][]string
	err := r.builtinVetStep(context.Background(), GoVetSource, vetTrimmedArgv, vetArgv, vetScripts(&calls, "exit 0", "exit 3"))
	if err != nil || len(calls) != 1 {
		t.Fatalf("err %v, calls %q: want the clean trimmed vet alone", err, calls)
	}
	if out.Len()+errOut.Len() != 0 || f.steps != 1 || f.exit != 0 {
		t.Errorf("frame steps %d exit %d, output %q %q", f.steps, f.exit, out, errOut)
	}
}

// A vet whose run printed and exited 0 reported too: it may name paths.
func TestBuiltinVetStepOutputOnSuccessIsRerun(t *testing.T) {
	r, _, _, errOut := vetProbeRunner(t)
	var calls [][]string
	err := r.builtinVetStep(context.Background(), GoVetSource, vetTrimmedArgv, vetArgv, vetScripts(&calls, "echo '../work/p.go: note' >&2", "echo 'p.go: note' >&2"))
	if err != nil || len(calls) != 2 || strings.Contains(errOut.String(), "../work/") || !strings.Contains(errOut.String(), "p.go: note") {
		t.Fatalf("err %v, calls %q, stderr %q", err, calls, errOut)
	}
}

// graft's own lines of the trimmed run are held with its output: shown when
// it stands for the step, dropped with it when vet runs again.
func TestBuiltinVetStepHoldsGraftLinesOfTheTrimmedRun(t *testing.T) {
	for _, c := range []struct {
		trimmed string
		shown   bool
	}{{"exit 0", true}, {"exit 1", false}} {
		r, _, _, errOut := vetProbeRunner(t)
		stderr := r.Stderr
		var calls [][]string
		command := vetScripts(&calls, c.trimmed, "exit 1")
		_ = r.builtinVetStep(context.Background(), GoVetSource, vetTrimmedArgv, vetArgv, func(argv []string) *exec.Cmd {
			if len(calls) == 0 {
				io.WriteString(r.Stderr, "graft: probe line\n")
				if errOut.Len() != 0 {
					t.Errorf("%s: a line of the trimmed run went out at once: %q", c.trimmed, errOut)
				}
			}
			return command(argv)
		})
		if r.Stderr != stderr {
			t.Fatalf("%s: the runner's stderr was not given back", c.trimmed)
		}
		if got := strings.Contains(errOut.String(), "graft: probe line"); got != c.shown {
			t.Errorf("%s: trimmed run's graft line shown %v, want %v:\n%s", c.trimmed, got, c.shown, errOut)
		}
	}
}

// A vet stopped while it ran did not finish its report: the step is that
// run, not a rerun, though vet exited non-zero by itself on the signal.
func TestBuiltinVetStepCancelledIsNotRerun(t *testing.T) {
	r, f, _, errOut := vetProbeRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var calls [][]string
	err := r.builtinVetStep(ctx, GoVetSource, vetTrimmedArgv, vetArgv, func(argv []string) *exec.Cmd {
		calls = append(calls, argv)
		cmd := exec.CommandContext(ctx, "sh", "-c", "trap 'kill $!; exit 1' TERM; echo partial >&2; sleep 5 >/dev/null 2>&1 & wait")
		cmd.Cancel = func() error {
			_, err := akitproc.StopChild(cmd.Process.Pid, 2*time.Second, func() error { return cmd.Process.Signal(syscall.SIGTERM) })
			return err
		}
		return cmd
	})
	if err == nil || len(calls) != 1 {
		t.Fatalf("err %v, calls %q: want the cancelled trimmed vet alone", err, calls)
	}
	if strings.Contains(errOut.String(), vetRerunNotice) || !strings.Contains(errOut.String(), "partial") {
		t.Errorf("stderr %q: want the trimmed run's output and no rerun", errOut)
	}
	if f.steps != 1 || f.exit != 1 {
		t.Errorf("frame steps %d exit %d, want the cancelled run", f.steps, f.exit)
	}
}

// A vet a signal ended, the kernel's for one, reported nothing either.
func TestBuiltinVetStepKilledIsNotRerun(t *testing.T) {
	r, _, _, errOut := vetProbeRunner(t)
	var calls [][]string
	err := r.builtinVetStep(context.Background(), GoVetSource, vetTrimmedArgv, vetArgv, vetScripts(&calls, "echo partial >&2; kill -KILL $$", "exit 1"))
	if err == nil || len(calls) != 1 || strings.Contains(errOut.String(), vetRerunNotice) {
		t.Fatalf("err %v, calls %q, stderr %q: want the killed trimmed vet alone", err, calls, errOut)
	}
}

// vetMemoryScope puts r's steps in a heavy scope with a ceiling whose
// memory.events is a file of the test: the shell line it returns makes the
// kernel kill a process there.
func vetMemoryScope(t *testing.T, r *Runner) string {
	t.Helper()
	proc := t.TempDir()
	events := filepath.Join(proc, "cgroup", "t", "memory.events")
	for path, data := range map[string]string{filepath.Join(proc, "self", "cgroup"): "0::/t\n", events: "oom 0\noom_kill 0\n"} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(loadgate.ProcEnv, proc)
	r.heavy, r.heavyPlace = 1, iosched.Placement{Ceiling: 1 << 30, Scope: "t.scope", Source: "test"}
	return "printf 'oom 1\\noom_kill 1\\n' > '" + events + "'"
}

// A trimmed vet during which the kernel killed a process of the heavy scope
// fails the step: vet does not run again to pass it.
func TestBuiltinVetStepMemoryKillOfTheTrimmedRunFails(t *testing.T) {
	r, f, _, _ := vetProbeRunner(t)
	kill := vetMemoryScope(t, r)
	var calls [][]string
	err := r.builtinVetStep(context.Background(), GoVetSource, vetTrimmedArgv, vetArgv, vetScripts(&calls, kill+"; echo 'signal: killed' >&2; exit 1", "exit 0"))
	if err == nil || !strings.Contains(err.Error(), "the kernel killed 1 process") || len(calls) != 1 {
		t.Fatalf("err %v, calls %q: want the trimmed run's memory failure, no rerun", err, calls)
	}
	if f.steps != 1 || f.exit == 0 || !strings.Contains(f.reason, "memory ceiling") {
		t.Errorf("frame steps %d exit %d reason %q, want the memory failure", f.steps, f.exit, f.reason)
	}
}

func TestBuiltinVetStepMemoryKillOfTheRerunFails(t *testing.T) {
	r, f, _, _ := vetProbeRunner(t)
	kill := vetMemoryScope(t, r)
	var calls [][]string
	err := r.builtinVetStep(context.Background(), GoVetSource, vetTrimmedArgv, vetArgv, vetScripts(&calls, "exit 1", kill+"; exit 0"))
	if err == nil || !strings.Contains(err.Error(), "the kernel killed 1 process") || len(calls) != 2 {
		t.Fatalf("err %v, calls %q: want the rerun's memory failure", err, calls)
	}
	if f.steps != 1 || f.exit == 0 {
		t.Errorf("frame steps %d exit %d, want the memory failure", f.steps, f.exit)
	}
}

// A rerun that does not start is the step's failure, still one record, with
// the trimmed run's cost.
func TestBuiltinVetStepRerunNotStartedIsRecorded(t *testing.T) {
	r, f, _, _ := vetProbeRunner(t)
	var calls [][]string
	err := r.builtinVetStep(context.Background(), GoVetSource, vetTrimmedArgv, vetArgv, func(argv []string) *exec.Cmd {
		calls = append(calls, argv)
		if len(calls) == 1 {
			return exec.Command("sh", "-c", "sleep 0.2; exit 1")
		}
		return exec.Command("/nonexistent/graft-test-go")
	})
	if err == nil || len(calls) != 2 {
		t.Fatalf("err %v, calls %q: want the rerun's failed start", err, calls)
	}
	if f.steps != 1 || f.exit != 1 || !strings.Contains(f.reason, "graft-test-go") {
		t.Errorf("frame steps %d exit %d reason %q, want the failed start", f.steps, f.exit, f.reason)
	}
	records := vetProbeRecords(t)
	if len(records) != 1 || records[0].Exit != 1 || records[0].WallS < 0.2 {
		t.Fatalf("records = %+v, want one failed step with the trimmed run's wall", records)
	}
}

func TestBuiltinVetStepNotStartedIsNotRerun(t *testing.T) {
	r, f, _, _ := vetProbeRunner(t)
	var calls [][]string
	err := r.builtinVetStep(context.Background(), GoVetSource, vetTrimmedArgv, vetArgv, func(argv []string) *exec.Cmd {
		calls = append(calls, argv)
		return exec.Command("/nonexistent/graft-test-go")
	})
	if err == nil || len(calls) != 1 {
		t.Fatalf("err %v, calls %q: want one failed start", err, calls)
	}
	// Recorded as a rerun that does not start is.
	if f.steps != 1 || f.exit != 1 || !strings.Contains(f.reason, "graft-test-go") {
		t.Errorf("frame steps %d exit %d reason %q, want the failed start", f.steps, f.exit, f.reason)
	}
	if records := vetProbeRecords(t); len(records) != 1 || records[0].Exit != 1 || records[0].Step != GoVetSource {
		t.Fatalf("records = %+v, want one failed step", records)
	}
}

func TestVetProbeKeepsItsLatestOutput(t *testing.T) {
	p := &vetProbe{}
	w := p.stream(true, true)
	io.WriteString(w, strings.Repeat("a", vetProbeLimit))
	io.WriteString(w, "tail\n")
	var out, errOut bytes.Buffer
	if err := p.replay(&out, &errOut); err != nil {
		t.Fatal(err)
	}
	got := errOut.String()
	if !strings.HasPrefix(got, "graft: go vet: the first 5 bytes of its output were dropped\n") || !strings.HasSuffix(got, "tail\n") || len(got) != vetProbeLimit+len("graft: go vet: the first 5 bytes of its output were dropped\n") {
		t.Fatalf("replay of %d bytes starts %q", len(got), got[:80])
	}
}

func TestBuiltinVetTrimOnlyForTheLocalBuiltinVet(t *testing.T) {
	vet := config.Step{Argv: vetArgv, Source: GoVetSource}
	local := &Runner{frames: []*frame{{}}}
	remote := &Runner{frames: []*frame{{remote: true}}}
	for _, c := range []struct {
		name    string
		r       *Runner
		s       config.Step
		builtin bool
		env     []string
		want    bool
	}{
		{"built-in here", local, vet, true, nil, true},
		{"project step", local, vet, false, nil, false},
		{"on the host", remote, vet, true, nil, false},
		{"GOFLAGS -trimpath=false", local, vet, true, []string{"GOFLAGS=-trimpath=false"}, false},
		{"go fix", local, config.Step{Argv: []string{"go", "fix", "-diff", "./..."}}, true, nil, false},
	} {
		got := c.r.builtinVetTrim(c.s, &config.Task{}, c.builtin, c.s.Argv, c.env)
		if (got != nil) != c.want || got != nil && !slices.Equal(got, vetTrimmedArgv) {
			t.Errorf("%s: builtinVetTrim = %q, want trimmed %v", c.name, got, c.want)
		}
	}
}
