// Package crashlog keeps the text of graft's fatal runtime errors in crash.log
// next to the stats: a caller that keeps only the tail of graft's output (a
// pipe through tail, an agent's tool) loses the panic otherwise.
package crashlog

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/stats"
)

// fileName is the crash log's name in the stats directory.
const fileName = "crash.log"

// Path is crash.log in the stats directory.
func Path() (string, error) {
	path, err := stats.Path()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), fileName), nil
}

// Keep sends a fatal runtime error's text to crash.log as well as to
// stderr, and returns the file for Note. A crash.log that cannot be
// opened is said and graft runs on: it is a diagnostic aid, not a step.
func Keep() *os.File {
	path, err := Path()
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0o755)
	}
	var f *os.File
	if err == nil {
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	}
	if err == nil {
		if err = debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
			err = fmt.Errorf("sending crashes to %s: %w", path, err)
			_ = f.Close()
			f = nil
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "graft: crash log: %v; a panic goes to stderr only\n", err)
		return nil
	}
	return f
}

// Note, deferred in main, names the run before a panic of graft's main
// goroutine reaches crash.log, and lets the panic go on: the runtime then
// writes its text, the panicking frames included, to stderr and crash.log.
func Note(f *os.File, version string, args []string) {
	r := recover()
	if r == nil {
		return
	}
	if f != nil {
		fmt.Fprintf(f, "\n%s graft %s, pid %d: graft %s\n", time.Now().Format(time.RFC3339), version, os.Getpid(), strings.Join(args, " "))
	}
	panic(r)
}
