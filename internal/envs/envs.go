// Package envs runs commands in a configured environment: on this machine, or
// on a host over ssh.
package envs

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
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
	if len(c.Argv) > 0 && !e.IsRemote() {
		argv, err := dotenv.ExpandAll(c.Argv, e.Lookup)
		if err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = e.Root
		return cmd, nil
	}
	line := c.Shell
	if len(c.Argv) > 0 {
		line = QuoteArgs(c.Argv)
	}
	return e.Shell(ctx, line), nil
}

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
