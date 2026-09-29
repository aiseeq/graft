// Command gatehelper is a portable stand-in for gate commands in graft's
// integration tests, so they need no shell.
//
//	gatehelper ok                 exit 0
//	gatehelper fail               exit 1
//	gatehelper failif <file>      exit 1 if file exists
//	gatehelper touch <file>       create file
//	gatehelper log <file> <ms>    append start and end timestamps around a sleep
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gatehelper:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("no command")
	}
	switch args[0] {
	case "ok":
		return nil
	case "fail":
		return errors.New("failing on purpose")
	case "failif":
		if _, err := os.Stat(args[1]); err == nil {
			return fmt.Errorf("%s exists", args[1])
		}
		return nil
	case "touch":
		return os.WriteFile(args[1], nil, 0o644)
	case "log":
		ms, err := strconv.Atoi(args[2])
		if err != nil {
			return err
		}
		if err := appendLine(args[1], "start"); err != nil {
			return err
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
		return appendLine(args[1], "end")
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func appendLine(path, event string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(f, "%s %d %d\n", event, os.Getpid(), time.Now().UnixNano())
	return errors.Join(err, f.Close())
}
