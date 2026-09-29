// Command gatehelper is a portable stand-in for gate commands in graft's
// integration tests, so they need no shell.
//
//	gatehelper ok                 exit 0
//	gatehelper fail               exit 1
//	gatehelper failif <file>      exit 1 if file exists
//	gatehelper touch <file>       create file
//	gatehelper log <file> <ms>    append start and end timestamps around a sleep
//	gatehelper append <file> <w>... append the words as one line
//	gatehelper env <file> <key>...  append KEY=value, or KEY unset, per key
//	gatehelper serve <addr>         listen on addr until terminated
//	gatehelper stubborn <addr>      listen on addr, ignoring SIGTERM
//	gatehelper sleep                run until terminated
//	gatehelper print <text>         print text
package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
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
		if err := appendLine(args[1], stamp("start")); err != nil {
			return err
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
		return appendLine(args[1], stamp("end"))
	case "append":
		return appendLine(args[1], strings.Join(args[2:], " "))
	case "env":
		for _, k := range args[2:] {
			line := k + " unset"
			if v, ok := os.LookupEnv(k); ok {
				line = k + "=" + v
			}
			if err := appendLine(args[1], line); err != nil {
				return err
			}
		}
		return nil
	case "serve", "stubborn":
		if args[0] == "stubborn" {
			signal.Ignore(syscall.SIGTERM)
		}
		l, err := net.Listen("tcp", args[1])
		if err != nil {
			return err
		}
		fmt.Println("listening on", l.Addr())
		for {
			c, err := l.Accept()
			if err != nil {
				return err
			}
			c.Close()
		}
	case "sleep":
		for {
			time.Sleep(time.Hour)
		}
	case "print":
		fmt.Println(args[1])
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func stamp(event string) string {
	return fmt.Sprintf("%s %d %d", event, os.Getpid(), time.Now().UnixNano())
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(f, line)
	return errors.Join(err, f.Close())
}
