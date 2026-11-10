//go:build linux

package main

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/proc"
)

// orphanScripts leave an orphan that writes its parent's command line to
// the file named by orphan.sh's argument: the parent it has once the shell
// that started it is gone. Nothing here sends a signal.
var orphanScripts = map[string]string{
	"orphan.sh": `out=$1
sh -c '(sleep 0.1; exec sh ./parent.sh "$0") & exit 0' "$out"
i=0
while [ ! -s "$out" ] && [ $i -lt 200 ]; do sleep 0.05; i=$((i+1)); done
`,
	"parent.sh": `tr '\0' ' ' < /proc/$PPID/cmdline > "$1.tmp" && mv "$1.tmp" "$1"
`,
}

// orphanParent is the command line of the orphan's parent that orphan.sh
// wrote to out.
func orphanParent(t *testing.T, out string) string {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if data, err := os.ReadFile(out); err == nil && len(data) > 0 {
			return strings.TrimSpace(string(data))
		}
	}
	t.Fatalf("the orphan wrote no parent to %s", out)
	return ""
}

// TestOrphansOfCommandsStayWithGraft: the orphan a task step, graft exec
// or graft reap leaves has graft's reaper for its parent, not the session
// manager, so its kill "$PPID" cannot end the user's session.
func TestOrphansOfCommandsStayWithGraft(t *testing.T) {
	f := newRepo(t, orphanScripts)
	taskOut := filepath.Join(t.TempDir(), "parent")
	f.write(".graft.yaml", `schema: 1
version: {mode: none}
tasks:
  orphan:
    run: [[sh, orphan.sh, '`+taskOut+`']]
`)
	for _, c := range []struct {
		name   string
		args   []string
		parent string
	}{
		{"task", []string{"orphan"}, "graft-reap "},
		{"exec", []string{"exec", "--", "sh", "orphan.sh"}, "graft-reap "},
		{"reap", []string{"reap", "--", "sh", "orphan.sh"}, graftBin + " reap -- sh orphan.sh"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, args := taskOut, c.args
			if c.name != "task" {
				out = filepath.Join(t.TempDir(), "parent")
				args = append(slices.Clone(c.args), out)
			}
			if got, code := f.graft("", args...); code != 0 {
				t.Fatalf("exit %d\n%s", code, got)
			}
			if parent := orphanParent(t, out); !strings.HasPrefix(parent, c.parent) {
				t.Errorf("the orphan's parent is %q, want %q…", parent, c.parent)
			}
		})
	}
}

// A step graft is told to stop keeps its reaper until the step ends, past
// the wait for its output pipes: what the step left behind ends with it and
// does not pass to the session manager. The step ignores SIGTERM for longer
// than that wait and leaves an orphan that ignores it too.
func TestStoppedStepTakesItsOrphans(t *testing.T) {
	dir := t.TempDir()
	f := newRepo(t, map[string]string{"step.sh": `trap '' TERM
sh -c 'trap "" TERM; exec sleep 30' &
echo $! > "$1/orphan.tmp" && mv "$1/orphan.tmp" "$1/orphan"
i=0
while [ $i -lt 60 ]; do sleep 0.05; i=$((i+1)); done
`})
	f.write(".graft.yaml", `schema: 1
version: {mode: none}
tasks:
  step:
    run: [[sh, step.sh, '`+dir+`']]
`)
	cmd, out := f.start(f.work, "step")
	orphan, err := strconv.Atoi(orphanParent(t, filepath.Join(dir, "orphan")))
	if err != nil {
		t.Fatal(err)
	}
	sleeping := func() bool {
		argv, err := proc.Cmdline(orphan)
		return err == nil && strings.Join(argv, " ") == "sleep 30"
	}
	t.Cleanup(func() {
		if sleeping() {
			_ = syscall.Kill(orphan, syscall.SIGKILL)
		}
	})
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("graft did not stop\n%s", out.String())
	}
	if sleeping() {
		t.Fatalf("the step's orphan %d outlived graft\n%s", orphan, out.String())
	}
}

// graft -C dir reap runs the command in dir, as other commands take -C.
func TestReapTakesDir(t *testing.T) {
	f := newRepo(t, map[string]string{"sub/a": "a"})
	out, code := f.graft("", "-C", "sub", "reap", "--", "sh", "-c", "pwd -P")
	want, err := filepath.EvalSymlinks(filepath.Join(f.work, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || strings.TrimSpace(out) != want {
		t.Fatalf("exit %d, ran in %q, want %q", code, strings.TrimSpace(out), want)
	}
}

// reaperOf is the reaper of the orphan whose pid the file holds, its parent,
// once the orphan has written it.
func reaperOf(t *testing.T, pidFile string) int {
	t.Helper()
	orphan := waitForProcessID(t, pidFile)
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(orphan) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("parent of %d in %q: %v", orphan, stat, err)
	}
	return ppid
}

func processName(pid int) string {
	comm, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	return strings.TrimSpace(string(comm))
}

// leaveOrphan is a shell command leaving an orphan that writes its pid to $1
// and lives 3 s with no pipe of the command's.
const leaveOrphan = `(sh -c 'echo $$ > "$0"; exec sleep 3' "$1" </dev/null >/dev/null 2>&1 &)`

// A command that ended alone keeps its status when its caller leaves while
// graft still ends the orphans it left: graft did not end the command.
func TestCallerGoneAfterCommandEndedKeepsItsStatus(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "orphan.pid")
	cmd, out, errOut := callerCommand(t, "exec", "--", "sh", "-c", leaveOrphan+"; printf 'alive\\n'", "sh", pidFile)
	_ = errOut.Close()
	data, _ := bufio.NewReader(out).ReadString('\n')
	reaper := reaperOf(t, pidFile)
	// The caller leaves once the command has ended: its reaper says so.
	waitFor(t, func() bool { return processName(reaper) == "graft-reaped" })
	_ = out.Close()
	if err := waitProcess(t, cmd); err != nil || data != "alive\n" {
		t.Fatalf("exit %v, stdout %q", err, data)
	}
}

// A command a signal reached (Ctrl-C to its group) and that ended with
// status 0 is still a command graft stopped: the caller leaving after it is
// no success.
func TestCallerGoneAfterSignalledCommandIsStopped(t *testing.T) {
	dir := t.TempDir()
	pidFile, shFile, termFile := filepath.Join(dir, "orphan.pid"), filepath.Join(dir, "sh.pid"), filepath.Join(dir, "orphan.term")
	// The orphan outlives the reaper's SIGTERM and marks it: the reaper then
	// waits out its grace, and it names itself, if at all, before that SIGTERM.
	orphan := `(sh -c 'trap "echo \$\$ > \"\$1\"" TERM; echo $$ > "$0"; while :; do sleep 0.05; done' "$1" "$3" </dev/null >/dev/null 2>&1 &)`
	cmd, out, errOut := callerCommand(t, "exec", "--", "sh", "-c", `trap 'exit 0' INT; `+orphan+`; echo $$ > "$2"; printf 'alive\n'; while :; do sleep 0.05; done`, "sh", pidFile, shFile, termFile)
	_ = errOut.Close()
	data, _ := bufio.NewReader(out).ReadString('\n')
	reaper := reaperOf(t, pidFile)
	command := waitForProcessID(t, shFile)
	// The terminal's SIGINT to the group, graft left out: the reaper and the
	// command, both started by this test's graft.
	for _, pid := range []int{reaper, command} {
		if err := syscall.Kill(pid, syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
	}
	waitForProcessGone(t, shFile)
	waitForProcessID(t, termFile)
	if name := processName(reaper); name == "graft-reaped" {
		t.Fatalf("the reaper of a signalled command calls it ended alone (%s)", name)
	}
	_ = out.Close()
	err := waitProcess(t, cmd)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 125 || data != "alive\n" {
		t.Fatalf("exit %v, stdout %q; want 125", err, data)
	}
}
