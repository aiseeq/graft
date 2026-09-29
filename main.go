// Command graft is a development workflow tool for git repositories: one
// commit command with a gate, version bump, content checks and push to every
// remote, plus the hooks that keep plain git commit out.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"

	"github.com/aiseeq/graft/internal/app"
	"github.com/aiseeq/graft/internal/hooks"
	"github.com/aiseeq/graft/internal/message"
	"github.com/aiseeq/graft/internal/version"
)

// buildVersion is set by the Makefile through -ldflags; go install builds fall
// back to the module version recorded in the binary.
var buildVersion string

const usage = `graft - commit, version and guard a git repository as .graft.yaml describes

Usage:
  graft commit [--minor|--major] (-m <msg>... | -F <file> | -F -)
  graft amend
  graft release [--minor|--major|--version X.Y.Z]
  graft version
  graft init
  graft check
  graft gate
  graft hook (pre-commit|post-merge)
  graft flags [status] [--env E]
  graft flags show <id> [--env E]
  graft flags ack <id> --reason R [--env E]
  graft flags mute <id> --reason R [--match S] [--all-envs] [--env E]
  graft --version

Commands:
  commit   gate, bump the version, stage all, check, commit, push to every remote
  amend    gate, stage all, check, fold into the last unpushed commit
  release  tag the pushed HEAD with the version and push the tag
  version  print the project version
  init     install the git hooks (core.hooksPath)
  check    scan the staged changes for secrets, binaries, large files, version drift
  gate     run the gate commands
  hook     entry point of the installed hooks
  flags    review the project's event journal: status, show, ack, mute
`

// errUsage marks command line mistakes; they exit with status 2.
var errUsage = errors.New("usage")

func main() {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "graft:", err)
		os.Exit(1)
	}
	a := &app.App{Dir: wd, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	// Ctrl-C stops a lock wait or a gate command instead of leaving them
	// running behind the user's back.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, a, os.Args[1:])
	stop()
	os.Exit(code)
}

func run(ctx context.Context, a *app.App, args []string) int {
	err := dispatch(ctx, a, args)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		fmt.Fprintln(a.Stderr, "graft:", err)
		fmt.Fprint(a.Stderr, "\n"+usage)
		return 2
	default:
		fmt.Fprintln(a.Stderr, "graft:", err)
		return 1
	}
}

func dispatch(ctx context.Context, a *app.App, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(a.Stdout, usage)
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "commit":
		return commitCmd(ctx, a, rest)
	case "amend":
		return noArgs(a, cmd, rest, func() error { return a.Amend(ctx) })
	case "release":
		return releaseCmd(ctx, a, rest)
	case "version":
		return noArgs(a, cmd, rest, a.Version)
	case "init":
		return noArgs(a, cmd, rest, a.Init)
	case "check":
		return noArgs(a, cmd, rest, a.Check)
	case "gate":
		return noArgs(a, cmd, rest, func() error { return a.Gate(ctx) })
	case "hook":
		return hookCmd(ctx, a, rest)
	case "flags":
		return flagsCmd(ctx, a, rest)
	case "help", "-h", "--help":
		fmt.Fprint(a.Stdout, usage)
		return nil
	case "--version", "-v":
		fmt.Fprintln(a.Stdout, "graft", toolVersion())
		return nil
	default:
		return fmt.Errorf("%w: unknown command %q", errUsage, cmd)
	}
}

func newFlags(a *app.App, name string) *flag.FlagSet {
	fs := flag.NewFlagSet("graft "+name, flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	return fs
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: %s: unexpected argument %q", errUsage, fs.Name(), fs.Arg(0))
	}
	return nil
}

func noArgs(a *app.App, name string, args []string, fn func() error) error {
	if err := parse(newFlags(a, name), args); err != nil {
		return err
	}
	return fn()
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

// String joins the collected values.
func (m *multiFlag) String() string { return strings.Join(*m, "\n\n") }

// Set adds one value.
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func levelFlags(fs *flag.FlagSet) func() (version.Level, error) {
	minor := fs.Bool("minor", false, "bump the minor version")
	major := fs.Bool("major", false, "bump the major version")
	return func() (version.Level, error) {
		switch {
		case *minor && *major:
			return version.Patch, fmt.Errorf("%w: --minor and --major exclude each other", errUsage)
		case *minor:
			return version.Minor, nil
		case *major:
			return version.Major, nil
		default:
			return version.Patch, nil
		}
	}
}

func commitCmd(ctx context.Context, a *app.App, args []string) error {
	fs := newFlags(a, "commit")
	var parts multiFlag
	fs.Var(&parts, "m", "commit message paragraph (repeatable)")
	file := fs.String("F", "", "read the commit message from a file, - for stdin")
	level := levelFlags(fs)
	if err := parse(fs, args); err != nil {
		return err
	}
	lvl, err := level()
	if err != nil {
		return err
	}
	return a.Commit(ctx, app.CommitOptions{
		Message: message.Source{Parts: parts, File: *file, Stdin: a.Stdin},
		Level:   lvl,
	})
}

func releaseCmd(ctx context.Context, a *app.App, args []string) error {
	fs := newFlags(a, "release")
	exact := fs.String("version", "", "release exactly this X.Y.Z (git-tag mode)")
	level := levelFlags(fs)
	if err := parse(fs, args); err != nil {
		return err
	}
	lvl, err := level()
	if err != nil {
		return err
	}
	return a.Release(ctx, app.ReleaseOptions{Level: lvl, Version: *exact})
}

func hookCmd(ctx context.Context, a *app.App, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: hook needs a name: %s", errUsage, strings.Join(hooks.Names, " or "))
	}
	// Git passes hook-specific arguments (post-merge gets the squash flag);
	// graft decides from the repository state instead.
	switch args[0] {
	case hooks.PreCommit:
		return a.HookPreCommit(ctx)
	case hooks.PostMerge:
		return a.HookPostMerge(ctx)
	default:
		return fmt.Errorf("%w: unknown hook %q", errUsage, args[0])
	}
}

func toolVersion() string {
	if buildVersion != "" {
		return buildVersion
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "unknown"
}

// parseInterspersed parses flags wherever they appear among positional
// arguments, which the flag package alone stops at.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: %w", errUsage, err)
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func flagsCmd(ctx context.Context, a *app.App, args []string) error {
	fs := newFlags(a, "flags")
	var o app.FlagsOptions
	fs.StringVar(&o.Env, "env", "", "environment (default: flags.default_env)")
	fs.StringVar(&o.Reason, "reason", "", "why the event is closed (ack, mute)")
	fs.StringVar(&o.Match, "match", "", "mute: the part of the subject the rule matches (default: all of it, * for the whole class)")
	fs.BoolVar(&o.AllEnvs, "all-envs", false, "mute: write the rule for every environment")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	o.Command = "status"
	if len(pos) > 0 {
		o.Command = pos[0]
		pos = pos[1:]
	}
	switch o.Command {
	case "status":
		if len(pos) != 0 {
			return fmt.Errorf("%w: flags status takes no arguments", errUsage)
		}
	case "show", "ack", "mute":
		if len(pos) != 1 {
			return fmt.Errorf("%w: flags %s takes one event id", errUsage, o.Command)
		}
		o.ID = pos[0]
	default:
		return fmt.Errorf("%w: unknown flags command %q (status, show, ack, mute)", errUsage, o.Command)
	}
	if (o.Match != "" || o.AllEnvs) && o.Command != "mute" {
		return fmt.Errorf("%w: --match and --all-envs apply to mute only", errUsage)
	}
	return a.Flags(ctx, o)
}
