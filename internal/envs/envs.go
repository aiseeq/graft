// Package envs runs commands in a configured environment: on this machine, or
// on a host over ssh.
package envs

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/dotenv"
)

// Env is a resolved environment.
type Env struct {
	Name   string
	Config config.Env
	// Root is the work tree root; local commands run there.
	Root string
	// Lookup resolves ${KEY} in local argv.
	Lookup *dotenv.Lookup
}

// Get resolves a configured environment by name.
func Get(cfg *config.Config, root, name string) (*Env, error) {
	if err := cfg.RequireEnv("--env", name); err != nil {
		return nil, err
	}
	return &Env{Name: name, Config: *cfg.Envs[name], Root: root, Lookup: dotenv.NewLookup(root + "/" + cfg.DotEnv)}, nil
}

// IsRemote reports whether commands go over ssh.
func (e *Env) IsRemote() bool { return e.Config.SSH != nil }

// Destination is user@host or host, for messages.
func (e *Env) Destination() string {
	s := e.Config.SSH
	if s == nil {
		return "local"
	}
	if s.User != "" {
		return s.User + "@" + s.Host
	}
	return s.Host
}

// Command builds the process for c in this environment. A shell command line
// runs through sh -c locally or the remote login shell over ssh; argv runs
// directly locally (after ${KEY} expansion) or shell-quoted over ssh.
func (e *Env) Command(ctx context.Context, c config.EnvCommand) (*exec.Cmd, error) {
	return e.CommandArgs(ctx, c, nil)
}

// CommandArgs is Command with the user's arguments put in at {args},
// verbatim (see ExpandArgv and ShellArgs).
func (e *Env) CommandArgs(ctx context.Context, c config.EnvCommand, args []string) (*exec.Cmd, error) {
	if len(args) > 0 && !TakesArgs(c) {
		return nil, fmt.Errorf("%q takes no arguments (no %s in it)", c.Source, config.ArgsPlaceholder)
	}
	if len(c.Argv) > 0 && !e.IsRemote() {
		argv, err := ExpandArgv(c.Argv, args, e.Lookup)
		if err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = e.Root
		return cmd, nil
	}
	if len(c.Argv) > 0 {
		argv, err := ExpandArgv(c.Argv, args, nil)
		if err != nil {
			return nil, err
		}
		return e.Shell(ctx, QuoteArgs(argv)), nil
	}
	line, err := ShellArgs(c.Shell, args, nil)
	if err != nil {
		return nil, err
	}
	return e.Shell(ctx, line), nil
}

// TakesArgs reports whether a command has an {args} placeholder.
func TakesArgs(c config.EnvCommand) bool {
	return strings.Contains(c.Shell, config.ArgsPlaceholder) ||
		slices.ContainsFunc(c.Argv, func(w string) bool { return strings.Contains(w, config.ArgsPlaceholder) })
}

// ExpandArgv expands ${KEY} in the configured words (when l is not nil) and
// then puts the user's arguments in verbatim: what follows -- is never
// expanded. {args} as a word becomes all the arguments; inside a word it
// takes exactly one.
func ExpandArgv(argv, args []string, l *dotenv.Lookup) ([]string, error) {
	expand := func(s string) (string, error) {
		if l == nil {
			return s, nil
		}
		return dotenv.Expand(s, l)
	}
	out := make([]string, 0, len(argv)+len(args))
	for _, w := range argv {
		if w == config.ArgsPlaceholder {
			out = append(out, args...)
			continue
		}
		before, after, found := strings.Cut(w, config.ArgsPlaceholder)
		before, err := expand(before)
		if err != nil {
			return nil, err
		}
		if !found {
			out = append(out, before)
			continue
		}
		if len(args) != 1 {
			return nil, fmt.Errorf("%q takes exactly one argument, got %d", w, len(args))
		}
		if after, err = expand(after); err != nil {
			return nil, err
		}
		out = append(out, before+args[0]+after)
	}
	return out, nil
}

// ShellArgs puts the user's arguments into a shell command line at {args},
// shell-quoted: a whole word takes all of them, {args} inside a word exactly
// one. fill, when not nil, is applied to the configured text around it only,
// so nothing the user passed is ever filled in or expanded.
func ShellArgs(line string, args []string, fill func(string) (string, error)) (string, error) {
	if fill == nil {
		fill = func(s string) (string, error) { return s, nil }
	}
	parts := strings.Split(line, config.ArgsPlaceholder)
	var b strings.Builder
	for i, part := range parts {
		filled, err := fill(part)
		if err != nil {
			return "", err
		}
		b.WriteString(filled)
		if i == len(parts)-1 {
			break
		}
		next := parts[i+1]
		startsWord := (i == 0 && part == "") || (part != "" && isBlank(part[len(part)-1]))
		endsWord := (i+1 == len(parts)-1 && next == "") || (next != "" && isBlank(next[0]))
		switch wholeWord := startsWord && endsWord; {
		case wholeWord:
			b.WriteString(QuoteArgs(args))
		case len(args) == 1:
			b.WriteString(Quote(args[0]))
		default:
			return "", fmt.Errorf("%s inside a word in %q takes exactly one argument, got %d", config.ArgsPlaceholder, line, len(args))
		}
	}
	return b.String(), nil
}

func isBlank(c byte) bool { return c == ' ' || c == '\t' || c == '\n' }

// Shell builds the process running a shell command line in this environment.
func (e *Env) Shell(ctx context.Context, line string) *exec.Cmd {
	if !e.IsRemote() {
		cmd := exec.CommandContext(ctx, "sh", "-c", line)
		cmd.Dir = e.Root
		return cmd
	}
	return exec.CommandContext(ctx, "ssh", append(e.sshArgs(), line)...)
}

func (e *Env) sshArgs() []string {
	s := e.Config.SSH
	var args []string
	if s.Port != 0 {
		args = append(args, "-p", strconv.Itoa(s.Port))
	}
	if s.Key != "" {
		args = append(args, "-i", s.Key)
	}
	for _, o := range s.Options {
		args = append(args, "-o", o)
	}
	return append(args, e.Destination())
}

// Quote makes s a single POSIX shell word.
func Quote(s string) string {
	if s != "" && safeWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var safeWord = regexp.MustCompile(`^[A-Za-z0-9_./:=@%+,-]+$`)

// QuoteArgs joins argv into a shell command line.
func QuoteArgs(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = Quote(a)
	}
	return strings.Join(quoted, " ")
}

var placeholderRe = regexp.MustCompile(`\{([a-z_][a-z0-9_]*)\}`)

// Fill replaces {name} placeholders with values passed through escape. A
// placeholder without a value is an error, so a typo never reaches a server as
// literal text.
func Fill(template string, values map[string]string, escape func(string) string) (string, error) {
	var missing []string
	out := placeholderRe.ReplaceAllStringFunc(template, func(m string) string {
		name := m[1 : len(m)-1]
		v, ok := values[name]
		if !ok {
			missing = append(missing, m)
			return m
		}
		return escape(v)
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("unknown placeholder %s in %q", strings.Join(missing, ", "), template)
	}
	return out, nil
}
