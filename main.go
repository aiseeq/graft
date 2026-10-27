// Command graft is a development workflow tool for git repositories: one
// commit command with a gate, version bump, content checks and push to every
// remote, the hooks that keep plain git commit out, and the project's tasks,
// services, test database, tools, event journal and deploy wrapper.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"
	// Release windows name their time zone; Windows has no zoneinfo of its own.
	_ "time/tzdata"

	"github.com/aiseeq/graft/v2/internal/app"
	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/envs"
	"github.com/aiseeq/graft/v2/internal/hooks"
	"github.com/aiseeq/graft/v2/internal/message"
	"github.com/aiseeq/graft/v2/internal/stats"
	"github.com/aiseeq/graft/v2/internal/tasks"
	"github.com/aiseeq/graft/v2/internal/version"
)

// buildVersion is set by the build task through -ldflags; go install builds fall
// back to the module version recorded in the binary.
var buildVersion string

// errUsage marks command line mistakes; they exit with status 2.
var errUsage = errors.New("usage")

func main() {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "graft:", err)
		os.Exit(1)
	}
	// GRAFT_VERSION is graft's to set for the project it runs in; a value
	// inherited from an outer graft (a task that runs graft) describes
	// another project, or the same one at another moment.
	if err := os.Unsetenv(tasks.VersionEnv); err != nil {
		fmt.Fprintln(os.Stderr, "graft:", err)
		os.Exit(1)
	}
	if err := stats.StartRun(); err != nil {
		fmt.Fprintln(os.Stderr, "graft:", err)
		os.Exit(1)
	}
	a := &app.App{Dir: wd, ToolVersion: toolVersion(), Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	// Ctrl-C stops a lock wait or a gate command instead of leaving them
	// running behind the user's back.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, a, os.Args[1:])
	stop()
	os.Exit(code)
}

func run(ctx context.Context, a *app.App, args []string) int {
	for len(args) > 0 && args[0] == "-C" {
		if len(args) < 2 || args[1] == "" {
			return runError(a, fmt.Errorf("%w: -C requires a directory", errUsage))
		}
		dir := args[1]
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(a.Dir, dir)
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return runError(a, fmt.Errorf("-C %q: %w", args[1], err))
		}
		info, err := os.Stat(abs)
		if err != nil {
			return runError(a, fmt.Errorf("-C %q: %w", abs, err))
		}
		if !info.IsDir() {
			return runError(a, fmt.Errorf("-C %q: not a directory", abs))
		}
		if err := os.Chdir(abs); err != nil {
			return runError(a, fmt.Errorf("-C %q: %w", abs, err))
		}
		wd, err := os.Getwd()
		if err != nil {
			return runError(a, fmt.Errorf("getting the working directory after -C %q: %w", abs, err))
		}
		a.Dir = wd
		args = args[2:]
	}
	if err := misplacedDir(a, args); err != nil {
		return runError(a, err)
	}
	err := dispatch(ctx, a, args)
	return runError(a, err)
}

// misplacedDir refuses -C <directory> after the command, before any --, with
// the form that works: as with git, -C goes first, and a task would otherwise
// take it among its arguments.
func misplacedDir(a *app.App, args []string) error {
	for i := 1; i+1 < len(args) && args[i] != "--"; i++ {
		if args[i] != "-C" {
			continue
		}
		dir := args[i+1]
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(a.Dir, dir)
		}
		// No such directory: -C is some flag's value, not graft's.
		info, err := os.Stat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("-C %q: %w", dir, err)
		}
		if !info.IsDir() {
			continue
		}
		rest := append(slices.Clone(args[:i]), args[i+2:]...)
		return fmt.Errorf("%w: -C goes before the command: graft -C %s %s", errUsage, envs.Quote(args[i+1]), envs.QuoteArgs(rest))
	}
	return nil
}

func runError(a *app.App, err error) int {
	var exitCode *app.ExitCodeError
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.As(err, &exitCode):
		fmt.Fprintln(a.Stderr, "graft:", err)
		return exitCode.Code
	case errors.Is(err, errUsage):
		fmt.Fprintln(a.Stderr, "graft:", strings.TrimPrefix(err.Error(), errUsage.Error()+": ")+" (see graft help)")
		return 2
	default:
		fmt.Fprintln(a.Stderr, "graft:", err)
		return 1
	}
}

func dispatch(ctx context.Context, a *app.App, args []string) error {
	if len(args) == 0 {
		return help(a)
	}
	cmd, rest := args[0], args[1:]
	if v, ok := os.LookupEnv(tasks.NoCacheEnv); ok {
		noCache, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%w: %s=%q: want true or false", errUsage, tasks.NoCacheEnv, v)
		}
		a.NoCacheEnv = noCache
	}
	// The stats record tasks as run under the command; runTask clears it,
	// so a task run directly names itself.
	a.Command = cmd
	switch cmd {
	case "commit":
		return commitCmd(ctx, a, rest)
	case "amend":
		return noArgs(a, cmd, rest, func() error { return a.Amend(ctx) })
	case "release":
		return releaseCmd(ctx, a, rest)
	case "version":
		return versionCmd(ctx, a, rest)
	case "init":
		return noArgs(a, cmd, rest, func() error { return a.Init(ctx) })
	case "check":
		return noArgs(a, cmd, rest, a.Check)
	case "leaks":
		if len(rest) != 1 || strings.HasPrefix(rest[0], "-") {
			return fmt.Errorf("%w: graft leaks <revision range>", errUsage)
		}
		return a.Leaks(rest[0])
	case "gate":
		return noArgs(a, cmd, rest, func() error { return a.Gate(ctx) })
	case "hook":
		return hookCmd(ctx, a, rest)
	case "flags":
		return flagsCmd(ctx, a, rest)
	case "deploy":
		return deployCmd(ctx, a, rest)
	case "stats":
		return statsCmd(a, rest)
	case "worktree":
		return worktreeCmd(ctx, a, rest)
	case "exec":
		return execCmd(ctx, a, rest)
	default:
		return projectCmd(ctx, a, cmd, rest)
	}
}

// projectCmd runs the commands that act on the project's tasks, services,
// test database and tools, and graft <task>.
func projectCmd(ctx context.Context, a *app.App, cmd string, rest []string) error {
	switch cmd {
	case "run":
		if len(rest) == 0 {
			return fmt.Errorf("%w: graft run <task> [-- args]", errUsage)
		}
		return runTask(ctx, a, rest[0], rest[1:])
	case "start", "stop", "restart":
		return serviceCmd(ctx, a, app.ServiceAction(cmd), rest)
	case "status":
		return noArgs(a, cmd, rest, func() error { return a.Status(ctx) })
	case "locks":
		return noArgs(a, cmd, rest, a.Locks)
	case "logs":
		return logsCmd(ctx, a, rest)
	case "testdb":
		return testDBCmd(ctx, a, rest)
	case "tools":
		return toolsCmd(ctx, a, rest)
	case "help", "-h", "--help":
		return noArgs(a, cmd, rest, func() error { return help(a) })
	case "tasks":
		return noArgs(a, cmd, rest, a.Tasks)
	case "--version", "-v":
		fmt.Fprintln(a.Stdout, "graft", toolVersion())
		return nil
	default:
		return taskShortcut(ctx, a, cmd, rest)
	}
}

// taskShortcut runs graft <task> as graft run <task>.
func taskShortcut(ctx context.Context, a *app.App, name string, args []string) error {
	ok, err := a.IsTask(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: unknown command or task %q", errUsage, name)
	}
	return runTask(ctx, a, name, args)
}

// runTask takes the task arguments after --, so a task argument is never
// mistaken for one of graft's.
func runTask(ctx context.Context, a *app.App, name string, args []string) error {
	if len(args) > 0 && args[0] == "--no-cache" {
		a.NoCache, args = true, args[1:]
	}
	if len(args) > 0 {
		if args[0] != "--" {
			return fmt.Errorf("%w: task arguments go after --: graft %s -- %s", errUsage, name, envs.QuoteArgs(args))
		}
		args = args[1:]
	}
	a.Command = ""
	return a.Run(ctx, name, args)
}

func statsCmd(a *app.App, args []string) error {
	fs := newFlags(a, "stats")
	since := fs.String("since", "7d", "how far back: days (7d), or a duration such as 12h or 30m")
	project := fs.String("project", "", "`root` of the project to report (default: the current one)")
	all := fs.Bool("all", false, "report every project")
	task := fs.String("task", "", "break this `task` down into its steps; with --tests, only its tests")
	tests := fs.Bool("tests", false, "report tests instead of tasks: the slowest, and the flaky ones")
	maxLoad := fs.Float64("max-load", 0, "count only the runs whose 1-minute load average stayed below this `load`")
	if err := parse(fs, args); err != nil {
		return err
	}
	age, err := parseAge(*since)
	if err != nil {
		return fmt.Errorf("%w: --since: %w", errUsage, err)
	}
	if *all && *project != "" {
		return fmt.Errorf("%w: --all and --project exclude each other", errUsage)
	}
	maxLoadSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "max-load" {
			maxLoadSet = true
		}
	})
	if maxLoadSet && (*maxLoad <= 0 || math.IsNaN(*maxLoad) || math.IsInf(*maxLoad, 0)) {
		return fmt.Errorf("%w: --max-load: want a positive load, got %v", errUsage, *maxLoad)
	}
	return a.Stats(app.StatsOptions{Since: age, Project: *project, All: *all, Task: *task, Tests: *tests, MaxLoad: *maxLoad})
}

// parseAge reads a positive age: whole days (7d) or a Go duration (12h).
func parseAge(s string) (time.Duration, error) {
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("%q is not a number of days", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, fmt.Errorf("%q is neither days (7d) nor a duration (12h)", s)
		}
	}
	if d <= 0 {
		return 0, fmt.Errorf("%q is not positive", s)
	}
	return d, nil
}

func serviceCmd(ctx context.Context, a *app.App, action app.ServiceAction, args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("%w: graft %s [service]", errUsage, action)
	}
	name := ""
	if len(args) == 1 {
		name = args[0]
	}
	return a.Service(ctx, action, name)
}

func logsCmd(ctx context.Context, a *app.App, args []string) error {
	fs := newFlags(a, "logs")
	lines := fs.Int("lines", 100, "how many of the last lines to show")
	follow := fs.Bool("f", false, "go on printing new lines until Ctrl-C")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *lines <= 0 {
		return fmt.Errorf("%w: graft logs <service> [--lines N] [-f]", errUsage)
	}
	return a.ServiceLogs(ctx, pos[0], *lines, *follow)
}

func testDBCmd(ctx context.Context, a *app.App, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("%w: %s", errUsage, app.TestDBUsage())
	}
	err := a.TestDB(ctx, args[0])
	if errors.Is(err, app.ErrUnknownTestDBAction) {
		return fmt.Errorf("%w: %s", errUsage, app.TestDBUsage())
	}
	return err
}

func worktreeCmd(ctx context.Context, a *app.App, args []string) error {
	usage := fmt.Errorf("%w: graft worktree add [--trunk <ref>] <branch> [<base>] | clean [--apply] [--trunk <ref>] [--min-age 1h] | sync [--trunk <ref>]", errUsage)
	switch {
	case len(args) > 0 && args[0] == "sync":
		fs := newFlags(a, "worktree sync")
		o := app.WorktreeSyncOptions{}
		fs.StringVar(&o.Trunk, "trunk", "", "the ref to sync onto (default: the HEAD branch of the deploy remote or of the only remote)")
		pos, err := parseInterspersed(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) != 0 {
			return usage
		}
		return a.WorktreeSync(ctx, o)
	case len(args) > 0 && args[0] == "add":
		fs := newFlags(a, "worktree add")
		o := app.WorktreeAddOptions{}
		fs.StringVar(&o.Trunk, "trunk", "", "the ref to create the branch from")
		pos, err := parseInterspersed(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) < 1 || len(pos) > 2 || (o.Trunk != "" && len(pos) == 2) {
			return usage
		}
		o.Branch = pos[0]
		if len(pos) == 2 {
			o.Base = pos[1]
		}
		return a.WorktreeAdd(ctx, o)
	case len(args) == 0 || args[0] != "clean":
		return usage
	}
	fs := newFlags(a, "worktree clean")
	o := app.WorktreeCleanOptions{}
	fs.BoolVar(&o.Apply, "apply", false, "remove; without it only report")
	fs.StringVar(&o.Trunk, "trunk", "", "the ref work must be in (default: the HEAD branch of the deploy remote or of the only remote)")
	fs.DurationVar(&o.MinAge, "min-age", time.Hour, "leave alone a worktree or branch used more recently")
	pos, err := parseInterspersed(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usage
	}
	return a.WorktreeClean(ctx, o)
}

// execCmd runs graft exec [--heavy] -- <command> [args].
func execCmd(ctx context.Context, a *app.App, args []string) error {
	usage := fmt.Errorf("%w: graft exec [--heavy [--local]] -- <command> [args]", errUsage)
	var heavy, local bool
	for len(args) > 0 && args[0] != "--" {
		switch args[0] {
		case "--heavy":
			heavy = true
		case "--local":
			local = true
		default:
			return usage
		}
		args = args[1:]
	}
	if len(args) < 2 || (local && !heavy) {
		return usage
	}
	if local {
		// The graft the command starts stays here too.
		if err := os.Setenv(tasks.HeavyEnv, "local"); err != nil {
			return fmt.Errorf("exec --local: %w", err)
		}
	}
	return a.Exec(ctx, args[1:], heavy)
}

func toolsCmd(ctx context.Context, a *app.App, args []string) error {
	switch {
	case len(args) == 0:
		return a.Tools(ctx, false)
	case len(args) == 1 && args[0] == "install":
		return a.Tools(ctx, true)
	default:
		return fmt.Errorf("%w: graft tools [install]", errUsage)
	}
}

// heldOutput keeps what the flag package prints until parsing is over: its
// usage is wanted for -h, not after every mistake, which gets one line.
type heldOutput struct {
	bytes.Buffer
	w io.Writer
}

func newFlags(a *app.App, name string) *flag.FlagSet {
	fs := flag.NewFlagSet("graft "+name, flag.ContinueOnError)
	fs.SetOutput(&heldOutput{w: a.Stderr})
	return fs
}

// parseError turns a flag parse error into graft's: -h shows the held
// usage, anything else is a usage error.
func parseError(fs *flag.FlagSet, err error) error {
	if errors.Is(err, flag.ErrHelp) {
		if out, ok := fs.Output().(*heldOutput); ok {
			if _, werr := out.WriteTo(out.w); werr != nil {
				return fmt.Errorf("print the usage: %w", werr)
			}
		}
		return err
	}
	return fmt.Errorf("%w: %w", errUsage, err)
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return parseError(fs, err)
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
	fs.BoolVar(&a.NoCache, "no-cache", false, "run the gate's tasks with cache even if they passed on this tree")
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

func versionCmd(ctx context.Context, a *app.App, args []string) error {
	if len(args) > 0 && args[0] == "bump" {
		fs := newFlags(a, "version bump")
		level := levelFlags(fs)
		if err := parse(fs, args[1:]); err != nil {
			return err
		}
		lvl, err := level()
		if err != nil {
			return err
		}
		return a.BumpVersion(ctx, lvl)
	}
	fs := newFlags(a, "version")
	describe := fs.Bool("describe", false, "print the build identity: works without tags, marks uncommitted changes")
	if err := parse(fs, args); err != nil {
		return err
	}
	return a.Version(*describe)
}

func hookCmd(ctx context.Context, a *app.App, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: hook needs a name: %s", errUsage, strings.Join(hooks.Names, " or "))
	}
	// Git passes hook-specific arguments (post-merge gets the squash flag);
	// graft decides from the repository state instead, and takes only the
	// message file of prepare-commit-msg.
	switch args[0] {
	case hooks.PreCommit:
		return a.HookPreCommit(ctx)
	case hooks.PrepareCommitMsg:
		if len(args) < 2 {
			return fmt.Errorf("%w: hook %s needs the message file git passes", errUsage, hooks.PrepareCommitMsg)
		}
		return a.HookPrepareCommitMsg(args[1])
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
			return nil, parseError(fs, err)
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
	fs.StringVar(&o.Match, "match", "", "mute: the part of the subject (or of --in) the rule matches (default: all of it, * for the whole class)")
	fs.StringVar(&o.In, "in", "subject", "mute: the field --match is part of: subject, body or key")
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
	if (o.Match != "" || o.AllEnvs || o.In != "subject") && o.Command != "mute" {
		return fmt.Errorf("%w: --match, --in and --all-envs apply to mute only", errUsage)
	}
	return a.Flags(ctx, o)
}

// deployFlagsMisplaced tells whether flags reach a subcommand that does not
// take them: script args check-head, check-integrations and cutoff, log flags anything but logs, deploy
// flags any subcommand.
func deployFlagsMisplaced(sub string, scriptArgs, logFlags, deployFlags bool) bool {
	return scriptArgs && (sub == "check-head" || sub == "check-integrations" || sub == "cutoff" || sub == "check" || sub == "versions") || logFlags && sub != "logs" || deployFlags && isDeploySubcommand(sub)
}

func deployCmd(ctx context.Context, a *app.App, args []string) error {
	// Everything after -- belongs to the deploy script.
	var scriptArgs []string
	if i := slices.Index(args, "--"); i >= 0 {
		args, scriptArgs = args[:i], args[i+1:]
	}
	fs := newFlags(a, "deploy")
	lines := fs.Int("lines", 200, "logs: how many lines")
	grep := fs.String("grep", "", "logs: keep lines matching this regexp")
	redeploy := fs.Bool("redeploy", false, "deploy the commit the target already runs")
	unapproved := fs.Bool("unapproved", false, "skip the approved_release check")
	apply := fs.Bool("apply", false, "sweep: move the items, not only list them")
	fs.BoolVar(&a.NoCache, "no-cache", false, "run the deps with cache even if they passed on this tree")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	usage := fmt.Errorf("%w: graft deploy [--redeploy] [--unapproved] <target> [-- args] | status <target> [-- args] | logs <target> [--lines N] [--grep RE] [-- args] | check <target> | versions | cutoff <target> | sweep <target> [--apply] | handover <target> | check-head | check-integrations [build]", errUsage)
	if len(pos) == 0 {
		return usage
	}
	logFlags := *lines != 200 || *grep != ""
	if deployFlagsMisplaced(pos[0], scriptArgs != nil, logFlags, *redeploy || *unapproved) || *apply && pos[0] != "sweep" {
		return usage
	}
	if run := trackerDeployCmd(ctx, a, pos, scriptArgs != nil, *apply); run != nil {
		return run()
	}
	if run := readOnlyDeployCmd(ctx, a, pos, scriptArgs); run != nil {
		return run()
	}
	switch {
	case pos[0] == "logs" && len(pos) == 2:
		if *lines <= 0 {
			return fmt.Errorf("%w: --lines must be positive", errUsage)
		}
		return a.DeployLogs(ctx, pos[1], *lines, *grep, scriptArgs)
	case !isDeploySubcommand(pos[0]) && len(pos) == 1:
		return a.Deploy(ctx, app.DeployOptions{Target: pos[0], Args: scriptArgs, Redeploy: *redeploy, Unapproved: *unapproved})
	default:
		return usage
	}
}

// trackerDeployCmd is the graft deploy subcommand of pos that moves tracker
// items, sweep or handover, nil for any other; neither takes script args.
func trackerDeployCmd(ctx context.Context, a *app.App, pos []string, scriptArgs, apply bool) func() error {
	if len(pos) != 2 || scriptArgs {
		return nil
	}
	switch pos[0] {
	case "sweep":
		return func() error { return a.DeploySweep(ctx, pos[1], apply) }
	case "handover":
		return func() error { return a.DeployHandover(ctx, pos[1]) }
	}
	return nil
}

// readOnlyDeployCmd is the read-only graft deploy subcommand of pos, nil
// for any other.
func readOnlyDeployCmd(ctx context.Context, a *app.App, pos, scriptArgs []string) func() error {
	switch {
	case pos[0] == "check-head" && len(pos) == 1:
		return a.DeployCheckHead
	case pos[0] == "check-integrations" && len(pos) <= 2:
		return func() error { return a.DeployCheckIntegrations(ctx, strings.Join(pos[1:], "")) }
	case pos[0] == "status" && len(pos) == 2:
		return func() error { return a.DeployStatus(ctx, pos[1], scriptArgs) }
	case pos[0] == "cutoff" && len(pos) == 2:
		return func() error { return a.DeployCutoff(ctx, pos[1]) }
	case pos[0] == "check" && len(pos) == 2:
		return func() error { return a.DeployCheck(ctx, pos[1]) }
	case pos[0] == "versions" && len(pos) == 1:
		return func() error { return a.DeployVersions(ctx) }
	}
	return nil
}

func isDeploySubcommand(s string) bool {
	return slices.Contains(config.DeploySubcommands, s)
}
