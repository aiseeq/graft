package proc

import (
	"bytes"
	"fmt"
	"os/exec"
	"runtime"
	"sync"

	"golang.org/x/sys/unix"
)

// ioprioIdle is the idle I/O class, IOPRIO_PRIO_VALUE(IOPRIO_CLASS_IDLE, 0):
// the process gets the disk only when no one else wants it.
const ioprioIdle = 3 << 13

// scopeArgs is how StartLow wraps a command in a scope of the user's
// systemd: the scheduler weighs cgroups before the nice of the processes
// in them, so a nice 19 build in the terminal's cgroup still takes its
// share against a video player in its own. systemd-run expands $ in the
// command it starts unless told not to.
var scopeArgs = []string{"--user", "--scope", "--quiet", "--collect", "--expand-environment=no", "-p", "CPUWeight=idle", "--"}

var scope struct {
	once sync.Once
	path string // systemd-run, when it can start scopes here
}

// StartLow starts cmd at the lowest priority, nice 19 and the idle I/O
// class, inherited by everything it starts, and in an idle CPU scope of
// the user's systemd when it has one; warn hears once per graft why the
// scope is not there.
func StartLow(cmd *exec.Cmd, warn func(string)) error {
	if cmd.Err != nil {
		return cmd.Start()
	}
	scope.once.Do(func() {
		path, err := exec.LookPath("systemd-run")
		if err == nil {
			var out []byte
			if out, err = exec.Command(path, append(scopeArgs, "true")...).CombinedOutput(); err != nil {
				err = fmt.Errorf("systemd-run --user --scope: %w: %s", err, bytes.TrimSpace(out))
			}
		}
		if err != nil {
			warn(fmt.Sprintf("heavy tasks run at nice 19 without an idle CPU scope, so they still compete with programs in other cgroups: %v", err))
			return
		}
		scope.path = path
	})
	if scope.path != "" {
		args := append([]string{scope.path}, scopeArgs...)
		cmd.Args = append(append(args, cmd.Path), cmd.Args[1:]...)
		cmd.Path = scope.path
	}
	// Nice and the I/O class belong to a thread on Linux, and a child takes
	// them from the thread that starts it. An unprivileged process cannot
	// take nice 19 back, so the thread is not unlocked: it ends with the
	// goroutine.
	started := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		tid := unix.Gettid()
		if err := unix.Setpriority(unix.PRIO_PROCESS, tid, lowestNice); err != nil {
			started <- fmt.Errorf("nice 19: %w", err)
			return
		}
		if _, _, errno := unix.Syscall(unix.SYS_IOPRIO_SET, 1 /* IOPRIO_WHO_PROCESS */, uintptr(tid), ioprioIdle); errno != 0 {
			started <- fmt.Errorf("idle I/O class: %w", errno)
			return
		}
		started <- cmd.Start()
	}()
	return <-started
}
