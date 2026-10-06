package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/aiseeq/graft/internal/app"
	"github.com/aiseeq/graft/internal/config"
)

// command is one entry of graft help. applies decides whether the project's
// config makes it usable; outside a project every command is shown.
type command struct {
	name    string
	syntax  func(cfg *config.Config) []string
	summary func(cfg *config.Config) string
	applies func(cfg *config.Config) bool
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
	{name: "leaks", syntax: fixed("graft leaks <revision range>"), summary: text("scan commits for the private names and terms of the user config (leaks)")},
	{name: "gate", syntax: fixed("graft gate"), summary: text("run the gate")},
	{name: "hook", syntax: fixed("graft hook (pre-commit|post-merge)"), summary: text("entry point of the installed hooks")},
	{name: "flags", syntax: fixed(
		"graft flags [status] [--env E]",
		"graft flags show <id> [--env E]",
		"graft flags ack <id> --reason R [--env E]",
		"graft flags mute <id> --reason R [--match S] [--all-envs] [--env E]",
	), summary: text("review the project's event journal: status, show, ack, mute"),
		applies: func(c *config.Config) bool { return c.Flags != nil }},
	{name: "deploy", syntax: fixed(
		"graft deploy [--redeploy] <target> [-- script args]",
		"graft deploy status <target> [-- args]",
		"graft deploy logs <target> [--lines N] [--grep RE] [-- args]",
		"graft deploy check-head",
	), summary: text("run the project's deploy script between graft's checks; status, logs"),
		applies: func(c *config.Config) bool { return c.Deploy != nil }},
	{name: "run", syntax: fixed("graft <task> [-- args]", "graft run <task> [-- args]"), summary: text("run a task with its deps"),
		applies: func(c *config.Config) bool { return len(c.Tasks) > 0 }},
	{name: "tasks", syntax: fixed("graft tasks"), summary: text("list every task, building blocks included"),
		applies: func(c *config.Config) bool { return len(c.Tasks) > 0 }},
	{name: "start", syntax: fixed("graft start|stop|restart [service]", "graft logs <service> [--lines N] [-f]"), summary: text("start services in the background or through systemd --user; stop, restart, logs"),
		applies: func(c *config.Config) bool { return len(c.Services) > 0 }},
	{name: "status", syntax: fixed("graft status"), summary: text("show services, the test database and held locks")},
	{name: "locks", syntax: fixed("graft locks"), summary: text("show who holds graft's locks")},
	{name: "testdb", syntax: fixed("graft testdb (up|down|status|recreate)"), summary: text("manage the disposable test PostgreSQL in docker"),
		applies: func(c *config.Config) bool { return c.TestDB != nil }},
	{name: "tools", syntax: fixed("graft tools [install]"), summary: text("check the pinned tools; install installs the failing ones"),
		applies: func(c *config.Config) bool { return len(c.Tools) > 0 }},
	{name: "help", syntax: fixed("graft help", "graft --version"), summary: text("this text; the version of graft itself")},
}

func releaseSyntax(c *config.Config) []string {
	if c != nil && c.Version.Mode == config.ModeFile {
		return []string{"graft release"}
	}
	return []string{"graft release [--minor|--major|--version X.Y.Z]"}
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
	b.WriteString("graft - the development workflow of a git repository, as .graft.yaml describes\n\nUsage:\n")
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
