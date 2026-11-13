//go:build linux

package proc

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	akitproc "github.com/aiseeq/akit/proc"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	if err := akitproc.BecomeChildSubreaper(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// reaped starts sh -c script under the reaper, with $1 the test's directory.
func reaped(t *testing.T, dir, script string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sh", "-c", script, "sh", dir)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	Reap(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

// waitFile returns the trimmed content of path once it is not empty.
func waitFile(t *testing.T, path string) string {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return strings.TrimSpace(string(data))
		}
	}
	t.Fatalf("%s stays empty", path)
	return ""
}

func pidIn(t *testing.T, path string) int {
	t.Helper()
	pid, err := strconv.Atoi(waitFile(t, path))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// stopAfter kills the sleep 30 of pid when the test ends, should it still
// run: a pid that another process took meanwhile is left alone.
func stopAfter(t *testing.T, pid int) {
	t.Cleanup(func() {
		argv, err := Cmdline(pid)
		if Gone(err) || err == nil && strings.Join(argv, " ") != "sleep 30" {
			return
		}
		if err != nil {
			t.Error(err)
			return
		}
		if err := akitproc.SignalDescendant(pid, syscall.SIGKILL); err != nil && !akitproc.Gone(err) && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		var status unix.WaitStatus
		if _, err := unix.Wait4(pid, &status, unix.WNOHANG, nil); err != nil && !errors.Is(err, unix.ECHILD) {
			t.Error(err)
		}
	})
}

func signalOf(t *testing.T, err error) syscall.Signal {
	t.Helper()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("want a command a signal ended, got %v", err)
	}
	ws := exit.Sys().(syscall.WaitStatus)
	if !ws.Signaled() {
		t.Fatalf("want a command a signal ended, got exit %d", ws.ExitStatus())
	}
	return ws.Signal()
}

// The orphan's $PPID is the reaper, so its kill "$PPID" would end the
// command. The stub signals the pid the test started and wrote down, the
// reaper's, and only when that is its $PPID: without a reaper its $PPID is
// another process, and it sends nothing.
func TestReapOrphanParentIsTheReaper(t *testing.T) {
	dir := t.TempDir()
	stub := `d=$1
echo "$PPID" > "$d/ppid.tmp" && mv "$d/ppid.tmp" "$d/ppid"
i=0
while [ ! -s "$d/expect" ] && [ $i -lt 200 ]; do sleep 0.05; i=$((i+1)); done
reaper=$(cat "$d/expect")
if [ "$PPID" = "$reaper" ]; then kill -TERM "$reaper"; fi
`
	if err := os.WriteFile(filepath.Join(dir, "stub.sh"), []byte(stub), 0o644); err != nil {
		t.Fatal(err)
	}
	// The inner sh exits at once: the stub, started after that, is an
	// orphan from its first line.
	cmd := reaped(t, dir, `sh -c '(sleep 0.2; exec sh "$1/stub.sh" "$1") & exit 0' sh "$1"
i=0
while [ $i -lt 200 ]; do sleep 0.05; i=$((i+1)); done`)
	if err := os.WriteFile(filepath.Join(dir, "expect"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	if got := waitFile(t, filepath.Join(dir, "ppid")); got != strconv.Itoa(cmd.Process.Pid) {
		t.Fatalf("the orphan's parent is %s, not the reaper %d", got, cmd.Process.Pid)
	}
	if sig := signalOf(t, err); sig != syscall.SIGTERM {
		t.Fatalf("the command ended with %v, want the SIGTERM the orphan sent", sig)
	}
}

// What the command leaves in its session ends with it; a daemon that left
// the session, even a moment after the command ended, stays and is not
// waited for.
func TestReapEndsOrphansKeepsDaemons(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	cmd := reaped(t, dir, `sleep 30 & echo $! > "$1/orphan"
setsid sleep 30 & echo $! > "$1/daemon"
(sleep 0.05; exec setsid sleep 30) & echo $! > "$1/late"
exit 0`)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	orphan, daemon, late := pidIn(t, filepath.Join(dir, "orphan")), pidIn(t, filepath.Join(dir, "daemon")), pidIn(t, filepath.Join(dir, "late"))
	stopAfter(t, daemon)
	stopAfter(t, late)
	if running(orphan) {
		t.Errorf("the orphan %d outlived the command", orphan)
	}
	if !running(daemon) {
		t.Errorf("the daemon %d was ended", daemon)
	}
	if !running(late) {
		t.Errorf("the daemon %d that left the session late was ended", late)
	}
	if took > orphanSettle+orphanGrace {
		t.Errorf("the reaper took %v: it waited for the daemons", took)
	}
}

func TestReapKeepsDaemonThatLeavesDuringGrace(t *testing.T) {
	dir := t.TempDir()
	cmd := reaped(t, dir, `sh -c 'trap '\''setsid sh -c "echo \$\$ > \"$1/daemon\"; exec sleep 30" & exit 0'\'' TERM
while :; do sleep 0.02; done' sh "$1" &
echo $! > "$1/worker"
exit 0`)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	daemon := pidIn(t, filepath.Join(dir, "daemon"))
	stopAfter(t, daemon)
	if !running(daemon) {
		t.Fatalf("daemon %d that left the session during grace was stopped", daemon)
	}
}

func TestReapRequestsOrphanStopAfterCommandExit(t *testing.T) {
	dir := t.TempDir()
	cmd := reaped(t, dir, `sh -c 'trap '\''if [ -s "$1/command-done" ]; then echo term; else echo early; fi > "$1/term"; exit 0'\'' TERM
echo ready > "$1/ready"
while :; do sleep 0.02; done' sh "$1" &
while [ ! -s "$1/ready" ]; do sleep 0.02; done
echo done > "$1/command-done"`)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if got := waitFile(t, filepath.Join(dir, "term")); got != "term" {
		t.Fatalf("orphan signal marker = %q", got)
	}
	if got := waitFile(t, filepath.Join(dir, "command-done")); got != "done" {
		t.Fatalf("command marker = %q", got)
	}
}

func TestReapExitStatus(t *testing.T) {
	if err := reaped(t, t.TempDir(), "exit 3").Wait(); err == nil || err.(*exec.ExitError).ExitCode() != 3 {
		t.Fatalf("want exit 3, got %v", err)
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		err := reaped(t, t.TempDir(), "kill -s "+strings.TrimPrefix(unix.SignalName(sig), "SIG")+" $$").Wait()
		if got := signalOf(t, err); got != sig {
			t.Errorf("the command ended with %v, the reaper with %v", sig, got)
		}
	}
	// A core-dumping signal: a shell would leave a crash record of its own,
	// so the command is this binary, made non-dumpable first.
	abort := exec.Command(os.Args[0], "-test.run=^TestReapAbortCommand$")
	abort.Env = append(os.Environ(), abortEnv+"=1")
	abort.Stdout, abort.Stderr = os.Stdout, os.Stderr
	Reap(abort)
	if err := abort.Start(); err != nil {
		t.Fatal(err)
	}
	if got := signalOf(t, abort.Wait()); got != syscall.SIGABRT {
		t.Errorf("the command ended with %v, the reaper with %v", syscall.SIGABRT, got)
	}
	missing := exec.Command("/proc/self/exe")
	missing.Args = []string{reapArg0, "/nonexistent/command", "command"}
	if err := missing.Run(); err == nil || err.(*exec.ExitError).ExitCode() != 127 {
		t.Fatalf("want exit 127 for a missing command, got %v", err)
	}
}

// abortEnv makes TestReapAbortCommand, run as a command, end by SIGABRT.
const abortEnv = "GRAFT_TEST_ABORT_COMMAND"

// TestReapAbortCommand is a command for TestReapExitStatus: non-dumpable, so
// the kernel hands no core to systemd-coredump, it ends by SIGABRT.
func TestReapAbortCommand(t *testing.T) {
	if os.Getenv(abortEnv) == "" {
		t.Skip("a command of TestReapExitStatus")
	}
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	dieOf(syscall.SIGABRT)
}

// SIGTERM reaches the command; SIGINT does not, a terminal sends it to the
// command itself.
func TestReapPassesSIGTERMNotSIGINT(t *testing.T) {
	dir := t.TempDir()
	// A shell would not do: bash drops a SIGINT that a SIGTERM follows.
	cmd := exec.Command(os.Args[0], "-test.run=^TestReapSignalRecorder$")
	cmd.Env = append(os.Environ(), recorderEnv+"="+dir)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	Reap(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFile(t, filepath.Join(dir, "ready"))
	// A SIGINT passed on would reach the command before the SIGTERM after it.
	if err := akitproc.SignalCmd(cmd, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if err := akitproc.SignalCmd(cmd, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil || err.(*exec.ExitError).ExitCode() != recorderExit {
		t.Fatalf("want the exit %d of the command's SIGTERM, got %v", recorderExit, err)
	}
	if got := waitFile(t, filepath.Join(dir, "got")); got != "terminated" {
		t.Fatalf("the command got %q, want only terminated", got)
	}
}

// recorderEnv names the directory where TestReapSignalRecorder, run as a
// command, writes the signals it gets.
const recorderEnv = "GRAFT_TEST_SIGNAL_RECORDER"

// recorderExit is how the recorder exits on SIGTERM.
const recorderExit = 7

// TestReapSignalRecorder is a command for TestReapPassesSIGTERMNotSIGINT:
// it writes down every SIGINT and SIGTERM in their order and exits on SIGTERM.
func TestReapSignalRecorder(t *testing.T) {
	dir := os.Getenv(recorderEnv)
	if dir == "" {
		t.Skip("a command of TestReapPassesSIGTERMNotSIGINT")
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	if err := os.WriteFile(filepath.Join(dir, "ready"), []byte("ready"), 0o644); err != nil {
		t.Fatal(err)
	}
	var got []string
	for s := range sigs {
		got = append(got, s.String())
		if s != syscall.SIGTERM {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, "got"), []byte(strings.Join(got, "\n")), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Exit(recorderExit)
	}
}

// The reaper killed outright takes the command along.
func TestReapKilledTakesCommandAlong(t *testing.T) {
	dir := t.TempDir()
	cmd := reaped(t, dir, `echo $$ > "$1/pid"; exec sleep 30`)
	pid := pidIn(t, filepath.Join(dir, "pid"))
	stopAfter(t, pid)
	if err := akitproc.SignalCmd(cmd, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); signalOf(t, err) != syscall.SIGKILL {
		t.Fatalf("want the reaper killed, got %v", err)
	}
	for deadline := time.Now().Add(5 * time.Second); running(pid); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the command %d outlived its reaper", pid)
		}
	}
}

// An orphan that starts a child when it gets SIGTERM, one that ignores it,
// loses both to SIGKILL.
func TestReapKillsChildrenStartedOnSIGTERM(t *testing.T) {
	dir := t.TempDir()
	for name, script := range map[string]string{
		"a.sh": `trap 'sh "$1/b.sh" & echo $! > "$1/b"; sleep 30' TERM
echo $$ > "$1/a"
i=0
while [ $i -lt 600 ]; do sleep 0.05; i=$((i+1)); done`,
		"b.sh": `trap '' TERM
sleep 30`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := reaped(t, dir, `sh "$1/a.sh" "$1" &
while [ ! -s "$1/a" ]; do sleep 0.02; done
exit 0`)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	a, b := pidIn(t, filepath.Join(dir, "a")), pidIn(t, filepath.Join(dir, "b"))
	stopAfter(t, a)
	stopAfter(t, b)
	for _, pid := range []int{a, b} {
		if running(pid) {
			t.Errorf("%d outlived the reaper", pid)
		}
	}
}

// A SIGTERM that reaches the reaper while it ends the command's orphans,
// from one of them here, neither ends it nor loses the command's status:
// it hurries the orphans to SIGKILL.
func TestReapSignalWhileEndingOrphans(t *testing.T) {
	dir := t.TempDir()
	// The orphan ignores SIGTERM and signals the pid the test started and
	// wrote down, the reaper's, only once that is its parent: once the
	// command that started it is gone.
	stub := `d=$1
trap '' TERM
echo $$ > "$d/orphan"
i=0
while [ ! -s "$d/expect" ] && [ $i -lt 200 ]; do sleep 0.05; i=$((i+1)); done
reaper=$(cat "$d/expect")
if [ "$PPID" = "$reaper" ]; then kill -TERM "$reaper"; fi
exec sleep 30
`
	if err := os.WriteFile(filepath.Join(dir, "stub.sh"), []byte(stub), 0o644); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	cmd := reaped(t, dir, `(sleep 0.1; exec sh "$1/stub.sh" "$1") &
exit 0`)
	if err := os.WriteFile(filepath.Join(dir, "expect"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	took := time.Since(start)
	orphan := pidIn(t, filepath.Join(dir, "orphan"))
	stopAfter(t, orphan)
	if err != nil {
		t.Fatalf("want the command's exit 0, got %v", err)
	}
	if running(orphan) {
		t.Errorf("the orphan %d outlived the reaper", orphan)
	}
	if took >= orphanSettle+orphanGrace {
		t.Errorf("the reaper took %v: the signal did not hurry it to SIGKILL", took)
	}
}

// A signal the reaper was started ignoring, as nohup and a background job
// start it, stays ignored for the command.
func TestReapKeepsIgnoredSignals(t *testing.T) {
	ignored := []syscall.Signal{syscall.SIGHUP, syscall.SIGINT}
	for _, sig := range ignored {
		signal.Ignore(sig)
	}
	t.Cleanup(func() {
		for _, sig := range ignored {
			signal.Reset(sig)
		}
	})
	dir := t.TempDir()
	if err := reaped(t, dir, `grep '^SigIgn:' /proc/self/status > "$1/ign"`).Wait(); err != nil {
		t.Fatal(err)
	}
	field := strings.Fields(waitFile(t, filepath.Join(dir, "ign")))
	mask, err := strconv.ParseUint(field[len(field)-1], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	for _, sig := range ignored {
		if mask&(1<<(sig-1)) == 0 {
			t.Errorf("the command does not ignore %v: SigIgn %x", sig, mask)
		}
	}
}

// SIGTERM to the reaper while its command runs ends the command, and the
// orphans after it at once: a caller that stops it waits no grace for them.
func TestReapSIGTERMHurriesOrphans(t *testing.T) {
	dir := t.TempDir()
	cmd := reaped(t, dir, `sh -c 'trap "echo term > $1/term" TERM; while :; do sleep 30; done' sh "$1" & echo $! > "$1/orphan"
while :; do sleep 0.05; done`)
	orphan := pidIn(t, filepath.Join(dir, "orphan"))
	stopAfter(t, orphan)
	start := time.Now()
	if err := akitproc.SignalCmd(cmd, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	took := time.Since(start)
	if sig := signalOf(t, err); sig != syscall.SIGTERM {
		t.Fatalf("the command ended with %v, want SIGTERM", sig)
	}
	if _, err := os.Stat(filepath.Join(dir, "term")); err != nil {
		t.Fatalf("orphan did not receive SIGTERM before SIGKILL: %v", err)
	}
	if running(orphan) {
		t.Errorf("the orphan %d outlived the reaper", orphan)
	}
	if took >= orphanGrace {
		t.Errorf("the reaper took %v after SIGTERM: it gave the orphans their grace", took)
	}
}
