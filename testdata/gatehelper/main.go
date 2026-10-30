// Command gatehelper is a portable stand-in for gate commands in graft's
// integration tests, so they need no shell.
//
//	gatehelper ok                 exit 0
//	gatehelper fail               exit 1
//	gatehelper failif <file>      exit 1 if file exists
//	gatehelper touch <file>       create file
//	gatehelper log <file> <ms>    append start and end timestamps around a sleep
//	gatehelper await <file> <release> append start, wait for release to exist, append end
//	gatehelper append <file> <w>... append the words as one line
//	gatehelper env <file> <key>...  append KEY=value, or KEY unset, per key
//	gatehelper serve <addr>         listen on addr until terminated
//	gatehelper stubborn <addr>      listen on addr, ignoring SIGTERM
//	gatehelper sleep                run until terminated
//	gatehelper print <text>         print text
//	gatehelper pwd <file>           write the working directory to file
//	gatehelper alloc <mb>           touch mb megabytes of memory, then exit
//	gatehelper write <file> <mb> <ms> write mb megabytes to file and sync it, then sleep ms
//	gatehelper parent <cmd> <arg>... run cmd as a child with this environment, passing on SIGTERM and SIGINT, and exit with its status
package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) > 2 && os.Args[1] == "parent" {
		os.Exit(parent(os.Args[2:]))
	}
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
			return fmt.Errorf("log: delay %q: %w", args[2], err)
		}
		if err := appendLine(args[1], stamp("start")); err != nil {
			return err
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
		return appendLine(args[1], stamp("end"))
	case "await":
		if err := appendLine(args[1], stamp("start")); err != nil {
			return err
		}
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(args[2]); err == nil {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("await: %s did not appear", args[2])
			}
		}
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
			return fmt.Errorf("%s: %w", args[0], err)
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
	case "copyenv":
		// copyenv VAR dst: copy the file VAR names to dst.
		src, ok := os.LookupEnv(args[1])
		if !ok {
			return fmt.Errorf("%s is not set", args[1])
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("copyenv: %w", err)
		}
		return os.WriteFile(args[2], data, 0o644)
	case "print":
		fmt.Println(args[1])
		return nil
	case "pwd":
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("getting the working directory: %w", err)
		}
		return os.WriteFile(args[1], []byte(wd+"\n"), 0o644)
	case "cat":
		// cat FILE [EXIT]: print the file, then exit with EXIT.
		data, err := os.ReadFile(args[1])
		if err != nil {
			return fmt.Errorf("cat: %w", err)
		}
		os.Stdout.Write(data)
		if len(args) > 2 {
			code, err := strconv.Atoi(args[2])
			if err != nil {
				return fmt.Errorf("cat: exit %q: %w", args[2], err)
			}
			os.Exit(code)
		}
		return nil
	case "hold":
		// hold MB MS: keep MB of touched memory for MS milliseconds.
		mb, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("hold: size %q: %w", args[1], err)
		}
		ms, err := strconv.Atoi(args[2])
		if err != nil {
			return fmt.Errorf("hold: delay %q: %w", args[2], err)
		}
		buf := make([]byte, mb<<20)
		for i := 0; i < len(buf); i += 4096 {
			buf[i] = 1
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
		fmt.Println("held", mb, "MB, last byte", buf[len(buf)-1])
		return nil
	case "alloc":
		mb, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("alloc: size %q: %w", args[1], err)
		}
		buf := make([]byte, mb<<20)
		for i := 0; i < len(buf); i += 4096 {
			buf[i] = 1
		}
		fmt.Println("allocated", mb, "MB, last byte", buf[len(buf)-1])
		return nil
	case "write":
		mb, err := strconv.Atoi(args[2])
		if err != nil {
			return fmt.Errorf("write: size %q: %w", args[2], err)
		}
		ms, err := strconv.Atoi(args[3])
		if err != nil {
			return fmt.Errorf("write: delay %q: %w", args[3], err)
		}
		file, err := os.Create(args[1])
		if err != nil {
			return fmt.Errorf("write: %w", err)
		}
		chunk := make([]byte, 1<<20)
		for i := range chunk {
			chunk[i] = byte(i)
		}
		for range mb {
			if _, err := file.Write(chunk); err != nil {
				return errors.Join(err, file.Close())
			}
		}
		if err := errors.Join(file.Sync(), file.Close()); err != nil {
			return err
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func stamp(event string) string {
	return fmt.Sprintf("%s %d %d", event, os.Getpid(), time.Now().UnixNano())
}

// parent runs argv as its child, as a shell would, and returns its exit
// status: the child's parent has the child's environment.
func parent(argv []string) int {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, os.Interrupt)
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "gatehelper:", err)
		return 1
	}
	go func() {
		for s := range signals {
			if err := cmd.Process.Signal(s); err != nil {
				fmt.Fprintln(os.Stderr, "gatehelper:", err)
			}
		}
	}()
	err := cmd.Wait()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr) && exitErr.ExitCode() >= 0:
		return exitErr.ExitCode()
	default:
		fmt.Fprintln(os.Stderr, "gatehelper:", err)
		return 1
	}
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("appending to %s: %w", path, err)
	}
	_, err = fmt.Fprintln(f, line)
	return errors.Join(err, f.Close())
}
