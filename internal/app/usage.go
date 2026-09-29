package app

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/gitx"
)

// ProjectConfig loads the config of the project in the current directory.
// found is false outside a project (no repository or no .graft.yaml), where
// graft still answers help.
func (a *App) ProjectConfig() (cfg *config.Config, found bool, err error) {
	_, cfg, err = a.open()
	if errors.Is(err, gitx.ErrNotRepo) || errors.Is(err, config.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return cfg, true, nil
}

// bumps reports whether graft commit bumps the version; without a project
// every option is shown.
func bumps(cfg *config.Config) bool {
	return cfg == nil || cfg.Version.Mode == config.ModeFile
}

// CommitSyntax is the graft commit command line the project allows.
func CommitSyntax(cfg *config.Config) string {
	if bumps(cfg) {
		return "graft commit [--minor|--major] (-m <msg>... | -F <file> | -F -)"
	}
	return "graft commit (-m <msg>... | -F <file> | -F -)"
}

// CommitSummary says what graft commit does in the project.
func CommitSummary(cfg *config.Config) string {
	steps := []string{"gate"}
	if bumps(cfg) {
		steps = append(steps, "bump the version")
	}
	steps = append(steps, "stage all", "check")
	switch {
	case cfg == nil:
		steps = append(steps, "commit with the work item key from the branch name")
	case cfg.Ticket != nil:
		steps = append(steps, "commit with the work item key from the branch name")
	default:
		steps = append(steps, "commit")
	}
	if cfg != nil && len(cfg.Push.Remotes) > 0 {
		steps = append(steps, "push to "+strings.Join(cfg.Push.Remotes, ", "))
	} else {
		steps = append(steps, "push to every remote")
	}
	return strings.Join(steps, ", ")
}

// commitHowTo is the pre-commit hook's answer to a plain git commit.
func commitHowTo(cfg *config.Config) string {
	var b strings.Builder
	b.WriteString("plain git commit is not used in this repository; commit with graft:\n")
	line := func(cmd, what string) { fmt.Fprintf(&b, "  %-38s %s\n", cmd, what) }
	if bumps(cfg) {
		line(`graft commit -m "fix: ..."`, "patch version")
		line(`graft commit --minor -m "feat: ..."`, "minor version")
		line(`graft commit --major -m "feat!: ..."`, "major version")
	} else {
		line(`graft commit -m "fix: ..."`, "commit")
	}
	line("graft amend", "fold the work tree into the last, unpushed commit")
	b.WriteString("graft commit: " + CommitSummary(cfg))
	return b.String()
}

// ProjectHelp lists the project's tasks that have a desc, and its services.
func (a *App) ProjectHelp(cfg *config.Config) error {
	var b strings.Builder
	if len(cfg.Tasks) > 0 {
		var rows [][2]string
		for _, name := range sortedNames(cfg.Tasks) {
			// A task without desc is a building block of others.
			if t := cfg.Tasks[name]; t.Desc != "" {
				rows = append(rows, [2]string{taskSyntax(name, t), t.Desc})
			}
		}
		b.WriteString("\nTasks (graft <task> [-- args]; graft tasks lists all):\n")
		writeRows(&b, "  ", rows)
	}
	if len(cfg.Services) > 0 {
		var rows [][2]string
		for _, name := range sortedNames(cfg.Services) {
			s := cfg.Services[name]
			desc := s.Desc
			if s.SystemdUnit != "" {
				desc = strings.TrimSpace(desc + " [systemd --user " + s.SystemdUnit + "]")
			}
			rows = append(rows, [2]string{name, desc})
		}
		b.WriteString("\nServices (graft start|stop|restart [service]; graft status; graft logs <service> [-f]):\n")
		writeRows(&b, "  ", rows)
	}
	_, err := io.WriteString(a.Stdout, b.String())
	return err
}

// nameColumn caps the width of the name column: one long task syntax must not
// push every description to the right edge.
const nameColumn = 24

// writeRows prints name and description columns. A name wider than the
// column gets its description on the next line.
func writeRows(b *strings.Builder, indent string, rows [][2]string) {
	width := 0
	for _, r := range rows {
		if n := len(r[0]); n <= nameColumn {
			width = max(width, n)
		}
	}
	for _, r := range rows {
		switch {
		case r[1] == "":
			fmt.Fprintf(b, "%s%s\n", indent, r[0])
		case len(r[0]) <= width:
			fmt.Fprintf(b, "%s%-*s  %s\n", indent, width, r[0], r[1])
		default:
			fmt.Fprintf(b, "%s%s\n%s%*s  %s\n", indent, r[0], indent, width, "", r[1])
		}
	}
}

// taskSyntax is the task's command line: name, and what it takes after --.
// A usage that already brackets its optional parts is not bracketed again.
func taskSyntax(name string, t *config.Task) string {
	switch {
	case !t.TakesArgs():
		return name
	case t.Args == config.ArgsRequired, strings.HasPrefix(t.Usage, "["):
		return name + " -- " + t.UsageText()
	default:
		return name + " [-- " + t.UsageText() + "]"
	}
}

// Tasks lists every task with its deps, building blocks included.
func (a *App) Tasks() error {
	_, cfg, err := a.open()
	if err != nil {
		return err
	}
	if len(cfg.Tasks) == 0 {
		a.printf("no tasks in .graft.yaml")
		return nil
	}
	var rows [][2]string
	for _, name := range sortedNames(cfg.Tasks) {
		t := cfg.Tasks[name]
		line := t.Desc
		if len(t.Deps) > 0 {
			line = strings.TrimSpace(line + " (deps: " + strings.Join(t.Deps, ", ") + ")")
		}
		rows = append(rows, [2]string{taskSyntax(name, t), line})
	}
	var b strings.Builder
	writeRows(&b, "", rows)
	_, err = io.WriteString(a.Stdout, b.String())
	return err
}
