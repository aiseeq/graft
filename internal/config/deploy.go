package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Deploy configures graft deploy: the common steps around a project's own
// deploy script.
type Deploy struct {
	// Remote whose branch HEAD must match before a deploy.
	Remote string `yaml:"remote"`
	// Branch, when set, is the only branch deployed; otherwise the current
	// branch is compared with its counterpart on Remote.
	Branch string `yaml:"branch"`
	// Untracked limits the untracked-files check to these paths; by default
	// any untracked file (not ignored) blocks a deploy.
	Untracked []string                 `yaml:"untracked"`
	Targets   map[string]*DeployTarget `yaml:"targets"`
}

// DeployTarget is one place the project deploys to.
type DeployTarget struct {
	Env string `yaml:"env"`
	// Run is the project's deploy script, run on this machine in the
	// foreground.
	Run Command `yaml:"run"`
	// Version prints the version currently deployed, run in Env.
	Version EnvCommand `yaml:"version"`
	// Requires names a target that must already run the version being
	// deployed (prod after test).
	Requires string `yaml:"requires"`
	// Confirm is "sudo": a sudo prompt in the foreground before anything
	// happens.
	Confirm      string       `yaml:"confirm"`
	DeployedSHA  *DeployedSHA `yaml:"deployed_sha"`
	ReleaseNotes *TargetNotes `yaml:"release_notes"`
	// Status runs in Env; {args} receives what follows --.
	Status EnvCommand `yaml:"status"`
	// Logs runs in Env; {lines} is filled in, {args} receives what
	// follows --.
	Logs string `yaml:"logs"`
	// DotEnv and DotEnvSets are the .env keys the deploy script gets, as
	// for tasks.
	DotEnv     []DotEnvKey `yaml:"dotenv"`
	DotEnvSets []string    `yaml:"dotenv_sets"`
	// Keys is DotEnv with the sets merged in.
	Keys []DotEnvKey `yaml:"-"`
}

// DeployedSHA is a file in the target environment recording the deployed
// commit; release notes cover the commits since the previous one.
type DeployedSHA struct {
	Path string `yaml:"path"`
	Sudo bool   `yaml:"sudo"`
}

// TargetNotes turns release notes on for a target.
type TargetNotes struct {
	// Transition moves every delivered item to this status.
	Transition string `yaml:"transition"`
}

// ReleaseNotes configures the Jira release notes.
type ReleaseNotes struct {
	Jira *JiraNotes `yaml:"jira"`
}

// JiraNotes says which keys are this project's and what the comment says.
type JiraNotes struct {
	ProjectKeys  []string `yaml:"project_keys"`
	Comment      string   `yaml:"comment"`
	SkipStatuses []string `yaml:"skip_statuses"`
}

// DefaultNoteComment is the release note text; {target}, {version}, {short}
// and {sha} are filled in.
const DefaultNoteComment = "Deployed to {target}, version {version}, commit {short}"

var projectKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9]+$`)

func (c *Config) validateDeploy() error {
	d := c.Deploy
	if d == nil {
		return nil
	}
	if d.Remote == "" {
		return errors.New("deploy.remote: required")
	}
	for i, p := range d.Untracked {
		if err := checkRelPath(fmt.Sprintf("deploy.untracked[%d]", i), p); err != nil {
			return err
		}
	}
	if len(d.Targets) == 0 {
		return errors.New("deploy.targets: at least one target is required")
	}
	for _, name := range keysOf(d.Targets) {
		if err := c.validateTarget(name, d.Targets[name]); err != nil {
			return err
		}
	}
	return c.validateReleaseNotes()
}

func (c *Config) validateTarget(name string, t *DeployTarget) error {
	where := "deploy.targets." + name
	if !envNameRe.MatchString(name) {
		return fmt.Errorf("%s: invalid target name", where)
	}
	if name == "status" || name == "logs" || name == "check-head" {
		return fmt.Errorf("%s: %q is a graft deploy subcommand, pick another name", where, name)
	}
	if t == nil || len(t.Run.Argv) == 0 {
		return fmt.Errorf("%s.run: required", where)
	}
	if err := c.RequireEnv(where+".env", t.Env); err != nil {
		return err
	}
	if err := c.validateRequires(where, name, t); err != nil {
		return err
	}
	if t.Confirm != "" && t.Confirm != "sudo" {
		return fmt.Errorf("%s.confirm: only sudo is supported, got %q", where, t.Confirm)
	}
	if t.DeployedSHA != nil && (t.DeployedSHA.Path == "" || strings.ContainsAny(t.DeployedSHA.Path, "\n\r")) {
		return fmt.Errorf("%s.deployed_sha.path: required, one line", where)
	}
	if err := checkArgsPlaceholder(Step{Argv: t.Status.Argv}); err != nil {
		return fmt.Errorf("%s.status: %w", where, err)
	}
	keys, err := c.resolveKeys(where, t.DotEnv, t.DotEnvSets)
	if err != nil {
		return err
	}
	t.Keys = keys
	return c.validateTargetNotes(where, t)
}

func (c *Config) validateRequires(where, name string, t *DeployTarget) error {
	if t.Requires == "" {
		return nil
	}
	req, ok := c.Deploy.Targets[t.Requires]
	switch {
	case !ok:
		return fmt.Errorf("%s.requires: unknown target %q", where, t.Requires)
	case t.Requires == name:
		return fmt.Errorf("%s.requires: a target cannot require itself", where)
	case !req.Version.IsSet():
		return fmt.Errorf("%s.requires: target %q has no version command to check", where, t.Requires)
	case c.Version.Mode == ModeNone:
		return fmt.Errorf("%s.requires: needs a project version (version.mode is none)", where)
	}
	return nil
}

func (c *Config) validateTargetNotes(where string, t *DeployTarget) error {
	if t.ReleaseNotes == nil {
		return nil
	}
	if t.DeployedSHA == nil {
		return fmt.Errorf("%s.release_notes: needs deployed_sha to know which commits were delivered", where)
	}
	if c.ReleaseNotes == nil || c.ReleaseNotes.Jira == nil {
		return fmt.Errorf("%s.release_notes: needs a top-level release_notes.jira section", where)
	}
	return nil
}

func (c *Config) validateReleaseNotes() error {
	n := c.ReleaseNotes
	if n == nil || n.Jira == nil {
		return nil
	}
	if len(n.Jira.ProjectKeys) == 0 {
		return errors.New("release_notes.jira.project_keys: required (without them any AB-12-shaped text, such as SHA-256, counts as a key)")
	}
	for _, k := range n.Jira.ProjectKeys {
		if !projectKeyRe.MatchString(k) {
			return fmt.Errorf("release_notes.jira.project_keys: %q is not a project key like PROJ", k)
		}
	}
	if n.Jira.Comment == "" {
		n.Jira.Comment = DefaultNoteComment
	}
	return nil
}
