package config

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/aiseeq/graft/internal/dotenv"
)

// DefaultDotEnv is the project .env file graft reads single keys from.
const DefaultDotEnv = ".env"

// Env is a named place commands run in: this machine, or a host over ssh.
type Env struct {
	SSH *SSH `yaml:"ssh"`
}

// SSH says how to reach a host. Unset fields are left to the ssh config.
type SSH struct {
	Host    string   `yaml:"host"`
	User    string   `yaml:"user"`
	Port    int      `yaml:"port"`
	Key     string   `yaml:"key"`
	Options []string `yaml:"options"`
}

// EnvCommand is a command bound to an environment. A string is a shell
// command line run by the environment's shell (sh -c here, the login shell
// over ssh); a list is argv, with ${KEY} taken from the environment or .env
// when run on this machine.
type EnvCommand struct {
	Shell  string
	Argv   []string
	Source string
}

// IsSet reports whether the command was given.
func (c EnvCommand) IsSet() bool { return c.Shell != "" || len(c.Argv) > 0 }

// UnmarshalYAML accepts a string or a list of strings.
func (c *EnvCommand) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if strings.TrimSpace(node.Value) == "" {
			return fmt.Errorf("line %d: empty command", node.Line)
		}
		c.Shell, c.Source = node.Value, node.Value
		return nil
	case yaml.SequenceNode:
		argv, err := decodeArgv(node)
		if err != nil {
			return err
		}
		c.Argv = argv
		if len(c.Argv) == 0 || c.Argv[0] == "" {
			return fmt.Errorf("line %d: empty command", node.Line)
		}
		c.Source = strings.Join(c.Argv, " ")
		return nil
	default:
		return fmt.Errorf("line %d: a command is a string or a list of strings", node.Line)
	}
}

var envNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

func (c *Config) validateEnvs() error {
	if c.DotEnv == "" {
		c.DotEnv = DefaultDotEnv
	}
	if err := checkRelPath("dotenv", c.DotEnv); err != nil {
		return err
	}
	if c.DotEnvTemplate != "" {
		if err := checkRelPath("dotenv_template", c.DotEnvTemplate); err != nil {
			return err
		}
		if path.Clean(c.DotEnvTemplate) == path.Clean(c.DotEnv) {
			return fmt.Errorf("dotenv_template: %q is the dotenv file itself", c.DotEnvTemplate)
		}
	}
	for name, env := range c.Envs {
		if !envNameRe.MatchString(name) {
			return fmt.Errorf("envs: invalid name %q (lowercase letters, digits, - and _)", name)
		}
		if env == nil {
			c.Envs[name] = &Env{}
			continue
		}
		if env.SSH != nil && env.SSH.Host == "" {
			return fmt.Errorf("envs.%s.ssh.host: required", name)
		}
		if env.SSH != nil && (env.SSH.Port < 0 || env.SSH.Port > 65535) {
			return fmt.Errorf("envs.%s.ssh.port: out of range", name)
		}
	}
	return nil
}

// RequireEnv checks that name is a configured environment.
func (c *Config) RequireEnv(where, name string) error {
	if _, ok := c.Envs[name]; ok {
		return nil
	}
	return fmt.Errorf("%s: unknown env %q (configured: %s)", where, name, strings.Join(c.EnvNames(), ", "))
}

// EnvNames lists the configured environments in a stable order.
func (c *Config) EnvNames() []string {
	names := make([]string, 0, len(c.Envs))
	for n := range c.Envs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// checkRemoteArgv refuses ${KEY} in an argv command bound to an ssh
// environment. Over ssh argv is sent shell-quoted, so ${KEY} would reach the
// server as literal text; expanding it here would put local values, secrets
// included, into the ssh command line and the process lists on both sides,
// while the keys of that environment live on the server.
func (c *Config) checkRemoteArgv(where, envName string, cmd EnvCommand) error {
	env, ok := c.Envs[envName]
	if !ok || env.SSH == nil {
		return nil
	}
	for _, w := range cmd.Argv {
		refs, err := dotenv.Refs(w)
		if err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if len(refs) > 0 {
			return fmt.Errorf("%s: ${%s} in an argv command is not supported in the ssh environment %s: write the command as a string, the shell on the server expands it from its own environment",
				where, refs[0], envName)
		}
	}
	return nil
}
