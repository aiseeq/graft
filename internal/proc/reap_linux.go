//go:build linux

package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	akitproc "github.com/aiseeq/akit/proc"
	"golang.org/x/sys/unix"
)

// reapArg0 is the argv[0] that turns a binary importing this package into
// the reaper of the command after it (Reap). The binary runs again through
// /proc/self/exe, so a graft replaced on disk meanwhile reaps too, and so do
// the test binaries of the packages that run commands.
const reapArg0 = "graft-reap"

func init() {
	if len(os.Args) >= 3 && os.Args[0] == reapArg0 {
		os.Exit(RunReaper(os.Args[1], os.Args[2:]))
	}
}

// Reap makes cmd run under a reaper, a process of the same binary between
// graft and the command: a process the command leaves behind becomes its
// child instead of the session manager's (systemd --user), so the kill
// "$PPID" of such an orphan ends this command, not the user's session, and
// the orphans still in the command's session end with it. Call it before
// Start. A cmd that failed to resolve, or a path that is no executable
// file, is left as it is: Start reports it as it would.
func Reap(cmd *exec.Cmd) {
	if cmd.Err != nil || cmd.Process != nil {
		return
	}
	if err := startable(cmd); err != nil {
		// Start reports it, as the command would have failed to start.
		cmd.Err = &exec.Error{Name: cmd.Path, Err: err}
		return
	}
	args := cmd.Args
	if len(args) == 0 {
		args = []string{cmd.Path}
	}
	cmd.Args = append([]string{reapArg0, cmd.Path}, args...)
	cmd.Path = "/proc/self/exe"
}

// startable is why cmd's path, a relative one from cmd's directory, names
// no file this process may execute, nil when it names one: exec.Command
// looks up only a bare name.
func startable(cmd *exec.Cmd) error {
	path := cmd.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(cmd.Dir, path)
	}
	if err := unix.Faccessat(unix.AT_FDCWD, path, unix.X_OK, unix.AT_EACCESS); err != nil {
		return fmt.Errorf("executing %s: %w", path, err)
	}
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return fmt.Errorf("executing %s: %w", path, err)
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		return fmt.Errorf("executing %s: %w", path, unix.EISDIR)
	}
	return nil
}

// The reaper's timing: how long a process left behind gets to leave the
// session (a daemon starting) before it is taken for an orphan, how long the
// orphans get after SIGTERM, how long SIGKILL takes, and the poll between.
var (
	orphanSettle = 500 * time.Millisecond
	orphanGrace  = 2 * time.Second
	orphanKill   = time.Second
	orphanPoll   = 50 * time.Millisecond
)

// ExitReaper is the reaper's own failure: it could not become a subreaper.
const ExitReaper = 125

// RunReaper runs path with argv as a subreaper (graft reap, Reap) and
// returns the command's exit status; a command a signal ended ends the
// reaper with the same signal, so the caller sees what it would have seen
// of the command. SIGTERM goes on to the command while it runs; SIGINT,
// SIGQUIT and SIGHUP do not, the terminal sends them to the command's
// process group itself; a SIGHUP or SIGINT the reaper was started ignoring
// stays ignored for the command (Go keeps no other signal ignored). Once the
// command ended, the processes left in its session get SIGTERM, then
// SIGKILL, at once when the reaper got SIGTERM or, after the command, one of
// those signals; those that left the session (daemons)
// stay and are not waited for. A command that could not start exits 127
// when it is missing, 126 otherwise.
func RunReaper(path string, argv []string) int {
	// The command's Pdeathsig follows the thread that started it.
	runtime.LockOSThread()
	if err := akitproc.BecomeChildSubreaper(); err != nil {
		fmt.Fprintf(os.Stderr, "graft: reaper: becoming a subreaper: %v\n", err)
		return ExitReaper
	}
	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs, unix.SIGCHLD)
	// A SIGHUP or SIGINT the reaper was started ignoring (nohup, a background
	// job) stays ignored for the command: a handler would reset it on exec.
	// Go's runtime keeps only these two ignored, SIGTERM and SIGQUIT it
	// handles whatever it inherited.
	for _, sig := range []os.Signal{unix.SIGTERM, unix.SIGINT, unix.SIGQUIT, unix.SIGHUP} {
		if !signal.Ignored(sig) {
			signal.Notify(sigs, sig)
		}
	}
	pid, err := syscall.ForkExec(path, argv, &syscall.ProcAttr{
		Env:   os.Environ(),
		Files: []uintptr{0, 1, 2},
		// The reaper killed outright takes the command along.
		Sys: &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "graft: start %s: %v\n", path, err)
		if errors.Is(err, syscall.ENOENT) {
			return ExitNotFound
		}
		return ExitCannotRun
	}
	r := &reaper{child: pid}
	for !r.done {
		if sig, ok := (<-sigs).(unix.Signal); ok && r.note(sig) {
			// The command is not reaped yet: its pid is still its own.
			if err := akitproc.SignalChild(pid, sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
				fmt.Fprintf(os.Stderr, "graft: reaper: passing %v to %d: %v\n", sig, pid, err)
			}
		}
		if err := r.reapAll(); err != nil {
			fmt.Fprintf(os.Stderr, "graft: reaper: %v\n", err)
			return ExitReaper
		}
	}
	// A signal sent to the group with the command (Ctrl-C) may still wait
	// behind the SIGCHLD of the command it ended; read after the command, it
	// hurries the orphans as one in the settle would.
	for drained := false; !drained; {
		select {
		case sig := <-sigs:
			if s, ok := sig.(unix.Signal); ok && s != unix.SIGCHLD {
				r.signalled, r.hurried = true, true
			}
		default:
			drained = true
		}
	}
	// The new name tells graft the command ended alone: a SIGTERM from now
	// on hurries the orphans and does not end the command (reaperEnded). A
	// command a signal reached may have ended by it, even with status 0: the
	// reaper keeps its name, and graft's stop counts as the command's end.
	if !r.signalled {
		if err := nameReaped(); err != nil {
			// graft then takes a signal it sends for the end of the command.
			fmt.Fprintf(os.Stderr, "graft: reaper: %v\n", err)
		}
	}
	// The signals stay caught to the end: one that ended the reaper now
	// would lose the command's status and leave its orphans behind.
	if err := r.endOrphans(sigs); err != nil {
		fmt.Fprintf(os.Stderr, "graft: reaper: %v\n", err)
	}
	if r.status.Signaled() {
		dieOf(r.status.Signal())
	}
	return r.status.ExitStatus()
}

// reapedName is the reaper's name once its command ended and only its
// orphans keep it: its thread runs init and stays locked, so the name is
// the process's in /proc.
const reapedName = "graft-reaped"

// nameReaped renames the reaper reapedName.
func nameReaped() error {
	name, err := unix.BytePtrFromString(reapedName)
	if err == nil {
		err = unix.Prctl(unix.PR_SET_NAME, uintptr(unsafe.Pointer(name)), 0, 0, 0)
	}
	if err != nil {
		return fmt.Errorf("naming itself %s: %w", reapedName, err)
	}
	return nil
}

// reaperEnded tells whether cmd is a reaper whose command already ended, so
// a signal to it ends only the orphans, not the command.
func reaperEnded(cmd *exec.Cmd) bool {
	if cmd.Process == nil || len(cmd.Args) == 0 || cmd.Args[0] != reapArg0 {
		return false
	}
	comm, err := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/comm")
	return err == nil && strings.TrimSpace(string(comm)) == reapedName
}

type reaper struct {
	child  int
	done   bool
	status unix.WaitStatus
	// hurried: a signal asked the reaper to stop.
	hurried bool
	// signalled: a signal other than SIGCHLD came while the command ran.
	signalled bool
	// childless: wait4 found no child, so no descendant is left at all.
	childless bool
}

// note records sig, which came while the command ran, and tells whether
// the reaper passes it on: SIGTERM, which asks the reaper to stop, so it
// ends the orphans at once after the command. SIGINT, SIGQUIT and SIGHUP
// the terminal sends to the command's group itself.
func (r *reaper) note(sig unix.Signal) bool {
	if sig == unix.SIGCHLD {
		return false
	}
	r.signalled = true
	if sig == unix.SIGTERM {
		r.hurried = true
		return true
	}
	return false
}

// reapAll waits for every child that has exited, the command's status kept.
func (r *reaper) reapAll() error {
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.ECHILD):
			r.childless = true
			return nil
		case err == nil && pid == 0:
			return nil
		case err != nil:
			return fmt.Errorf("waiting for children: %w", err)
		}
		if pid == r.child {
			r.done, r.status = true, ws
		}
	}
}

// endOrphans ends the processes the command left in its session: SIGTERM
// after a settle in which a starting daemon leaves the session, SIGKILL to
// those still there and those they started meanwhile. A signal that asks
// the reaper to stop meanwhile skips to SIGKILL.
func (r *reaper) endOrphans(sigs <-chan os.Signal) error {
	left, err := r.settle(orphanSettle, sigs)
	if err != nil || len(left) == 0 {
		return err
	}
	signalAll(left, unix.SIGTERM)
	if left, err = r.settle(orphanGrace, sigs); err != nil || len(left) == 0 {
		return err
	}
	deadline := time.Now().Add(orphanKill)
	for len(left) > 0 && time.Now().Before(deadline) {
		signalAll(left, unix.SIGKILL)
		time.Sleep(orphanPoll)
		if err := r.reapAll(); err != nil {
			return err
		}
		if left, err = orphans(); err != nil {
			return err
		}
	}
	if len(left) > 0 {
		return fmt.Errorf("processes %v outlived SIGKILL", left)
	}
	return nil
}

// settle reaps for up to d, until no orphan is left or a signal other than
// SIGCHLD hurries the reaper, and returns those left.
func (r *reaper) settle(d time.Duration, sigs <-chan os.Signal) ([]int, error) {
	deadline := time.Now().Add(d)
	for {
		if err := r.reapAll(); err != nil {
			return nil, err
		}
		// Without a child no scan of /proc is needed: a command that ended
		// alone keeps its caller waiting no longer than it did.
		if r.childless {
			return nil, nil
		}
		left, err := orphans()
		if err != nil || len(left) == 0 || r.hurried || !time.Now().Before(deadline) {
			return left, err
		}
		select {
		case sig := <-sigs:
			r.hurried = r.hurried || sig != unix.SIGCHLD
		case <-time.After(min(orphanPoll, time.Until(deadline))):
		}
	}
}

func signalAll(pids []int, sig unix.Signal) {
	for _, pid := range pids {
		stat, err := akitproc.ReadStat("/proc", pid)
		if err != nil {
			if !akitproc.Gone(err) {
				fmt.Fprintf(os.Stderr, "graft: reaper: reading %d: %v\n", pid, err)
			}
			continue
		}
		if stat.Ppid == os.Getpid() {
			if err := akitproc.SignalChild(pid, sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
				fmt.Fprintf(os.Stderr, "graft: reaper: sending %v to %d: %v\n", sig, pid, err)
			}
			continue
		}
		// akit cannot signal a verified descendant that is not a direct child.
		if err := unix.Kill(pid, sig); err != nil && !errors.Is(err, unix.ESRCH) {
			fmt.Fprintf(os.Stderr, "graft: reaper: sending %v to %d: %v\n", sig, pid, err)
		}
	}
}

// orphans are this process's descendants still running in its session.
func orphans() ([]int, error) {
	sid, err := unix.Getsid(0)
	if err != nil {
		return nil, fmt.Errorf("own session: %w", err)
	}
	all, err := descendants(os.Getpid())
	if err != nil {
		return nil, err
	}
	var left []int
	for _, pid := range all {
		s, err := unix.Getsid(pid)
		if errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("session of %d: %w", pid, err)
		}
		zombie, err := isZombie(pid)
		if err != nil {
			return nil, err
		}
		if s == sid && !zombie {
			left = append(left, pid)
		}
	}
	return left, nil
}

// isZombie tells whether pid has exited and waits to be reaped; a pid gone
// meanwhile counts as one.
func isZombie(pid int) (bool, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if Gone(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("state of process %d: %w", pid, err)
	}
	// The state follows the command name, which may hold ") ".
	i := strings.LastIndexByte(string(data), ')')
	if i < 0 || i+2 >= len(data) {
		return false, fmt.Errorf("/proc/%d/stat: no state in %q", pid, data)
	}
	return data[i+2] == 'Z', nil
}

// dieOf ends this process with sig, as the command ended: with the default
// action restored past Go's handlers, the parent sees the same signal. The
// crash, if it is one, is the command's: the reaper turns itself
// non-dumpable first, so a core-dumping signal leaves no second crash record
// (a core_pattern pipe such as systemd-coredump ignores RLIMIT_CORE=0).
// Should it survive, it exits 128 and the signal, as a shell reports one.
func dieOf(sig syscall.Signal) {
	if sig == syscall.SIGKILL {
		if err := unix.Kill(os.Getpid(), sig); err != nil {
			fmt.Fprintf(os.Stderr, "graft: reaper: raising %v: %v\n", sig, err)
		}
	}
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		fmt.Fprintf(os.Stderr, "graft: reaper: not dumpable: %v\n", err)
	}
	// A zeroed kernel sigaction is SIG_DFL with no flags and an empty mask,
	// whatever the architecture's field order; the buffer is larger than any.
	var dfl [64]byte
	if _, _, errno := unix.RawSyscall6(unix.SYS_RT_SIGACTION, uintptr(sig), uintptr(unsafe.Pointer(&dfl)), 0, 8, 0, 0); errno != 0 {
		fmt.Fprintf(os.Stderr, "graft: reaper: default action of %v: %v\n", sig, errno)
	}
	var set unix.Sigset_t
	sigaddset(&set, sig)
	if err := unix.PthreadSigmask(unix.SIG_UNBLOCK, &set, nil); err != nil {
		fmt.Fprintf(os.Stderr, "graft: reaper: unblocking %v: %v\n", sig, err)
	}
	if err := unix.Kill(os.Getpid(), sig); err != nil {
		fmt.Fprintf(os.Stderr, "graft: reaper: raising %v: %v\n", sig, err)
	}
	time.Sleep(100 * time.Millisecond)
	os.Exit(128 + int(sig))
}

func sigaddset(set *unix.Sigset_t, sig syscall.Signal) {
	n := uint(sig - 1)
	bits := uint(unsafe.Sizeof(set.Val[0])) * 8
	set.Val[n/bits] |= 1 << (n % bits)
}
