package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/aiseeq/graft/v2/internal/app"
	"github.com/aiseeq/graft/v2/internal/config"
)

// command is one entry of graft help. applies decides whether the project's
// config makes it usable; outside a project every command is shown.
type command struct {
	name    string
	syntax  func(cfg *config.Config) []string
	summary func(cfg *config.Config) string
	applies func(cfg *config.Config) bool
	// details is what graft <command> -h adds to the summary.
	details string
}

func fixed(lines ...string) func(*config.Config) []string {
	return func(*config.Config) []string { return lines }
}

func text(s string) func(*config.Config) string {
	return func(*config.Config) string { return s }
}

var commands = []command{
	{name: "commit", syntax: func(c *config.Config) []string { return []string{app.CommitSyntax(c)} }, summary: app.CommitSummary},
	{name: "amend", syntax: fixed("graft amend"), summary: text("gate, stage all, check, fold into the last unpushed commit")},
	{name: "release", syntax: releaseSyntax, summary: text("tag the pushed HEAD with the version and push the tag"),
		applies: func(c *config.Config) bool { return c.Version.Mode != config.ModeNone }},
	{name: "version", syntax: fixed("graft version [--describe]", "graft version bump [--minor|--major]"), summary: text("print the project version; --describe: the build identity, for stamping binaries; bump: raise it in the files without committing (version mode file)")},
	{name: "init", syntax: fixed("graft init"), summary: text("install the git hooks (core.hooksPath) and the pinned tools")},
	{name: "check", syntax: fixed("graft check"), summary: text("scan the staged changes for secrets, binaries, large files, version drift")},
	{name: "leaks", syntax: fixed("graft leaks <revision>…"), summary: text("scan commits for the private names and terms of the user config (leaks)")},
	{name: "gate", syntax: fixed("graft gate"), summary: text("run the gate (go vet ./... in every Go module first; go_vet.enabled: false or go_vet.exclude, with a reason, skips it)")},
	{name: "hook", syntax: fixed("graft hook (pre-commit|post-merge)"), summary: text("entry point of the installed hooks")},
	{name: "flags", syntax: fixed(
		"graft flags [status] [--env E]",
		"graft flags show <id> [--env E]",
		"graft flags ack <id> --reason R [--env E]",
		"graft flags mute <id> --reason R [--match S] [--all-envs] [--env E]",
	), summary: text("review the project's event journal: status, show, ack, mute"),
		applies: func(c *config.Config) bool { return c.Flags != nil }},
	{name: "deploy", syntax: fixed(
		"graft deploy [--redeploy] [--unapproved] <target> [-- script args]",
		"graft deploy status <target> [-- args]",
		"graft deploy logs <target> [--lines N] [--grep RE] [-- args]",
		"graft deploy check <target>",
		"graft deploy versions",
		"graft deploy cutoff <target>",
		"graft deploy sweep <target> [--apply]",
		"graft deploy handover <target>",
		"graft deploy check-head",
		"graft deploy check-integrations [build]",
	), summary: text("run the project's deploy script between graft's checks; status, logs, what a deploy would refuse, the versions the targets run, the release cutoff, the items git and the target contradict, the items held for their comment"),
		applies: func(c *config.Config) bool { return c.Deploy != nil }},
	{name: "run", syntax: fixed("graft <task> [-- args]", "graft run <task> [-- args]"), summary: text("run a task with its deps"),
		applies: func(c *config.Config) bool { return len(c.Tasks) > 0 }},
	{name: "tasks", syntax: fixed("graft tasks"), summary: text("list every task, building blocks included"),
		applies: func(c *config.Config) bool { return len(c.Tasks) > 0 }},
	{name: "start", syntax: fixed("graft start|stop|restart [service]", "graft logs <service> [--lines N] [-f]"), summary: text("start services in the background or through systemd --user; stop, restart, logs"),
		applies: func(c *config.Config) bool { return len(c.Services) > 0 }},
	{name: "exec", syntax: fixed("graft exec [--heavy [--local] [--env <name>]... [--settle <duration>]] -- <command> [args]"),
		summary: text("run a command that is not a task, recorded in the stats as exec; --heavy: queue for the machine as heavy tasks do, {threads} and GRAFT_THREADS from load.threads"),
		details: "--local: with --heavy, run on this machine though the project has remote_heavy (GRAFT_HEAVY=local, for the grafts the command starts too).\n" +
			"--env: with --heavy, a variable of the caller's that remote_heavy's host gets too (GRAFT_PASS_ENV, for the grafts the command starts too): the host gets the task's env, the threads and GOOS, GOARCH, GOFLAGS, CGO_ENABLED, GOEXPERIMENT, not the caller's other variables, so FOO=1 graft exec --heavy -- go test does not see FOO there; pass it with --env FOO, or keep the command here with --local.\n" +
			"--settle: with --heavy, keep the queue for this long after the command starts, or until it ends if sooner, in place of load.settle: for a command whose load comes later than its start.\n\n" +
			"Exit status: the command's, 128 and the signal for one a signal ended; on remote_heavy's host, the one its run gives.\n" +
			"  125  graft itself: a usage error, -C, the config, the queue, remote_heavy's run that does not start, an interrupt that ended the command\n" +
			"  126  the command was found but not started\n" +
			"  127  the command was not found\n" +
			"126 and 127 come with the line graft: exec: start <command>: …, which a command exiting 126 or 127 itself has not."},
	{name: "stats", syntax: fixed("graft stats [--tests | --value wall-p50|wall-p95|wait-p95 [--min-runs N]] [--since 7d] [--project <root>|--all] [--task <name>] [--max-load N]"),
		summary: text("resource use recorded for tasks: runs, wall and CPU time, peak memory, failures; --tests: the slowest tests, the skipped and the flaky ones")},
	{name: "status", syntax: fixed("graft status"), summary: text("show services, the test database and held locks")},
	{name: "locks", syntax: fixed("graft locks"), summary: text("show who holds graft's locks")},
	{name: "testdb", syntax: fixed("graft testdb (up|down|status|recreate)"), summary: text("manage the disposable test PostgreSQL in docker"),
		applies: func(c *config.Config) bool { return c.TestDB != nil }},
	{name: "tools", syntax: fixed("graft tools [install]"), summary: text("check the pinned tools; install installs the failing ones"),
		applies: func(c *config.Config) bool { return len(c.Tools) > 0 }},
	{name: "worktree", syntax: fixed("graft worktree add [--trunk <ref>] <branch> [<base>]", "graft worktree clean [--apply] [--trunk <ref>] [--min-age 1h]", "graft worktree sync [--trunk <ref>]"),
		summary: text("a worktree under the user's cache, with go.work's neighbours linked; remove the worktrees and branches whose work is in the trunk by content; move this worktree's work onto the fresh trunk, uncommitted changes kept uncommitted")},
	{name: "gocache", syntax: fixed("graft gocache trim [--older 25h] [--apply]"),
		summary: text("remove the entries of the Go build cache unused for longer than --older, where go keeps them five days; without --apply only report")},
	{name: "help", syntax: fixed("graft help [<command>]", "graft <command> -h", "graft --version"), summary: text("this text; one command's part of it; the version of graft itself")},
}

func releaseSyntax(c *config.Config) []string {
	if c != nil && c.Version.Mode == config.ModeFile {
		return []string{"graft release"}
	}
	return []string{"graft release [--minor|--major|--version X.Y.Z]"}
}

// helpCmd prints graft help, or with a command's name its section.
func helpCmd(a *app.App, args []string) error {
	switch {
	case len(args) == 0:
		return help(a)
	case len(args) == 1 && !strings.HasPrefix(args[0], "-"):
		var b strings.Builder
		if err := writeSection(&b, a, args[0]); err != nil {
			return err
		}
		fmt.Fprint(a.Stdout, b.String())
		return nil
	default:
		return noArgs(a, "help", args, func() error { return help(a) })
	}
}

// sectionOf finds the entry of graft help for a command, or for the
// subcommand name a flag set carries ("worktree add").
func sectionOf(name string) (command, bool) {
	root, _, _ := strings.Cut(name, " ")
	switch root {
	case "stop", "restart", "logs":
		root = "start"
	case "-h", "--help":
		root = "help"
	}
	for _, c := range commands {
		if c.name == root {
			return c, true
		}
	}
	return command{}, false
}

// writeSection writes a command's section of graft help: its syntax, the
// summary and the details.
func writeSection(w io.Writer, a *app.App, name string) error {
	c, ok := sectionOf(name)
	if !ok {
		return fmt.Errorf("%w: no command %q", errUsage, name)
	}
	cfg, _, err := a.ProjectConfig()
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("Usage:\n")
	for _, line := range c.syntax(cfg) {
		b.WriteString("  " + line + "\n")
	}
	b.WriteString("\n" + c.summary(cfg) + "\n")
	if c.details != "" {
		b.WriteString("\n" + c.details + "\n")
	}
	_, err = io.WriteString(w, b.String())
	return err
}

// help prints the commands that apply to the project in the current
// directory, then its tasks and services.
func help(a *app.App) error {
	cfg, found, err := a.ProjectConfig()
	if err != nil {
		return err
	}
	var shown []command
	for _, c := range commands {
		if !found || c.applies == nil || c.applies(cfg) {
			shown = append(shown, c)
		}
	}
	var b strings.Builder
	b.WriteString("graft - the development workflow of a git repository, as .graft.yaml describes\n\nUsage:\n  graft [-C <directory> ...] <command>\n")
	b.WriteString("  -C changes the working directory; repeated relative paths start from the previous directory.\n\n")
	for _, c := range shown {
		for _, line := range c.syntax(cfg) {
			b.WriteString("  " + line + "\n")
		}
	}
	b.WriteString("\nCommands:\n")
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, c := range shown {
		fmt.Fprintf(w, "  %s\t%s\n", c.name, c.summary(cfg))
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("formatting the command list: %w", err)
	}
	if !found {
		b.WriteString("\nOutside a project every command is listed; in one, only those its .graft.yaml uses.\n")
	}
	fmt.Fprint(a.Stdout, b.String())
	if !found {
		return nil
	}
	return a.ProjectHelp(cfg)
}
