package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
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
	Hotfix    *Hotfix                  `yaml:"hotfix"`
}

// DeployTarget is one place the project deploys to.
type DeployTarget struct {
	Env string `yaml:"env"`
	// Deps are tasks run after the requires check and before the sudo
	// prompt and the deploy script, the way a task's deps run.
	Deps []string `yaml:"deps"`
	// Run is the project's deploy script, run on this machine in the
	// foreground.
	Run Command `yaml:"run"`
	// Version prints the version currently deployed, run in Env.
	Version EnvCommand `yaml:"version"`
	// Requires names a target that must already run the version being
	// deployed (prod after test).
	Requires string `yaml:"requires"`
	// Confirm is "sudo": a sudo prompt in the foreground after the checks
	// and deps, before the deploy script runs.
	Confirm      string       `yaml:"confirm"`
	DeployedSHA  *DeployedSHA `yaml:"deployed_sha"`
	ReleaseNotes *TargetNotes `yaml:"release_notes"`
	// ApprovedRelease lets out only commits that name an item in one of the
	// statuses, of a version every approver approved, or both.
	ApprovedRelease *ApprovedRelease `yaml:"approved_release"`
	// Window is when the target takes releases; a hotfix goes any time.
	Window *Window `yaml:"window"`
	// ReleaseVersion keeps the Jira versions of the windows.
	ReleaseVersion *ReleaseVersion `yaml:"release_version"`
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
	// Transitions move the delivered items: an item in a status no
	// transition moves from, paused say, keeps its status.
	Transitions []NoteTransition `yaml:"transitions"`
	// OldTransition and OldFrom are the single transition Transitions
	// replaced; they are read only to say how to rewrite them.
	OldTransition string   `yaml:"transition"`
	OldFrom       []string `yaml:"from"`
	// FixVersion adds the project's next unreleased version to every item
	// that gets a note.
	FixVersion *FixVersion `yaml:"fix_version"`
	// Subtasks follow their parent: when a delivered item is in, or reaches,
	// the status a transition leads to, its open subtasks move there by that
	// transition, whatever its from says, and so does a delivered subtask
	// whose parent is in such a status.
	Subtasks bool `yaml:"subtasks"`
}

// NoteTransition moves an item in one of From to To.
type NoteTransition struct {
	From []string `yaml:"from"`
	To   string   `yaml:"to"`
	// Assign is the account id the item is handed over to on the move: the
	// reviewer of Testing, say.
	Assign string `yaml:"assign"`
}

// Leading returns the transition that leads to the status, nil for none.
func (n *TargetNotes) Leading(status string) *NoteTransition {
	for i := range n.Transitions {
		if n.Transitions[i].To == status {
			return &n.Transitions[i]
		}
	}
	return nil
}

// Moving returns the transition that moves an item in the status, nil for
// none.
func (n *TargetNotes) Moving(status string) *NoteTransition {
	for i := range n.Transitions {
		if slices.Contains(n.Transitions[i].From, status) {
			return &n.Transitions[i]
		}
	}
	return nil
}

// FromStatuses lists every status a transition moves from.
func (n *TargetNotes) FromStatuses() []string {
	var all []string
	for _, t := range n.Transitions {
		all = append(all, t.From...)
	}
	return all
}

// FixVersion picks the version by the start of its name or by a name
// template with the window's day.
type FixVersion struct {
	// Prefix is how the names of the versions to pick from begin, such as
	// "Product v".
	Prefix string `yaml:"prefix"`
	// Name is a template such as "Product {dd}.{mm}.{yy}": the versions
	// follow it, and the day in the name orders them.
	Name string `yaml:"name"`
	// Template is Name compiled.
	Template *VersionName `yaml:"-"`
}

// Matches tells whether a version is one fix_version picks from.
func (f *FixVersion) Matches(name string) bool {
	if f.Template != nil {
		_, ok := f.Template.Date(name)
		return ok
	}
	return strings.HasPrefix(name, f.Prefix)
}

// ApprovedRelease lets out only the commits of checked items: items in one of
// Statuses, items of an approved version of Prefix, or both when both are set.
type ApprovedRelease struct {
	// Statuses are the item statuses that go out, such as Ready for release;
	// a subtask also goes out when its parent is in one of them.
	Statuses []string `yaml:"statuses"`
	// Prefix is how the names of the versions begin, such as "Product v".
	Prefix string `yaml:"prefix"`
	// IgnorePaths are globs of files that ship nothing: a commit that
	// changes only these passes whatever it names.
	IgnorePaths []string `yaml:"ignore_paths"`
	// Ignore is IgnorePaths compiled.
	Ignore []*regexp.Regexp `yaml:"-"`
}

// Ignored tells whether every one of the files matches IgnorePaths; no files
// is not ignored.
func (r *ApprovedRelease) Ignored(files []string) bool {
	if len(files) == 0 || len(r.Ignore) == 0 {
		return false
	}
	for _, f := range files {
		if !slices.ContainsFunc(r.Ignore, func(re *regexp.Regexp) bool { return re.MatchString(f) }) {
			return false
		}
	}
	return true
}

// ShipsNothing tells whether every one of the files matches the
// approved_release ignore_paths of some target; no files is not.
func (c *Config) ShipsNothing(files []string) bool {
	if c.Deploy == nil {
		return false
	}
	all := &ApprovedRelease{}
	for _, n := range keysOf(c.Deploy.Targets) {
		if r := c.Deploy.Targets[n].ApprovedRelease; r != nil {
			all.Ignore = append(all.Ignore, r.Ignore...)
		}
	}
	return all.Ignored(files)
}

// ReleaseNotes configures the Jira release notes.
type ReleaseNotes struct {
	Jira *JiraNotes `yaml:"jira"`
}

// JiraNotes says which keys are this project's and what the comment says.
type JiraNotes struct {
	ProjectKeys []string `yaml:"project_keys"`
	Comment     string   `yaml:"comment"`
	// SkipStatuses leaves items in these statuses alone: no note, no move.
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
	if err := c.validateHotfix(); err != nil {
		return err
	}
	return c.validateReleaseNotes()
}

// DeploySubcommands are the words after graft deploy that are not targets.
var DeploySubcommands = []string{"status", "logs", "check-head", "cutoff"}

func (c *Config) validateTarget(name string, t *DeployTarget) error {
	where := "deploy.targets." + name
	if !envNameRe.MatchString(name) {
		return fmt.Errorf("%s: invalid target name", where)
	}
	if slices.Contains(DeploySubcommands, name) {
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
	for _, d := range t.Deps {
		if _, ok := c.Tasks[d]; !ok {
			return fmt.Errorf("%s.deps: unknown task %q", where, d)
		}
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
	for field, cmd := range map[string]EnvCommand{"version": t.Version, "status": t.Status} {
		if err := c.checkRemoteArgv(where+"."+field, t.Env, cmd); err != nil {
			return err
		}
	}
	keys, err := c.resolveKeys(where, t.DotEnv, t.DotEnvSets)
	if err != nil {
		return err
	}
	t.Keys = keys
	return c.validateTargetRelease(where, t)
}

// validateTargetRelease checks the release window and what a target asks of
// Jira: the approved release before a deploy and the notes after it.
func (c *Config) validateTargetRelease(where string, t *DeployTarget) error {
	if err := c.validateWindow(where, t); err != nil {
		return err
	}
	if err := c.validateReleaseVersion(where, t); err != nil {
		return err
	}
	if err := c.validateApprovedRelease(where, t); err != nil {
		return err
	}
	return c.validateTargetNotes(where, t)
}

func (c *Config) validateApprovedRelease(where string, t *DeployTarget) error {
	switch {
	case t.ApprovedRelease == nil:
		return nil
	case t.DeployedSHA == nil:
		return fmt.Errorf("%s.approved_release: needs deployed_sha to know which commits go out", where)
	case c.ReleaseNotes == nil || c.ReleaseNotes.Jira == nil:
		return fmt.Errorf("%s.approved_release: needs a top-level release_notes.jira section for the project keys", where)
	case len(t.ApprovedRelease.Statuses) == 0 && strings.TrimSpace(t.ApprovedRelease.Prefix) == "":
		return fmt.Errorf("%s.approved_release: needs statuses (the item statuses that go out, such as [Ready for release]), prefix (how the names of the approved versions begin, such as \"Product v\"), or both", where)
	case t.ApprovedRelease.Prefix != "" && strings.TrimSpace(t.ApprovedRelease.Prefix) == "":
		return fmt.Errorf("%s.approved_release.prefix: blank", where)
	}
	for i, st := range t.ApprovedRelease.Statuses {
		if strings.TrimSpace(st) == "" {
			return fmt.Errorf("%s.approved_release.statuses[%d]: blank", where, i)
		}
	}
	t.ApprovedRelease.Ignore = nil
	for i, g := range t.ApprovedRelease.IgnorePaths {
		re, err := CompileGlob(g)
		if err != nil {
			return fmt.Errorf("%s.approved_release.ignore_paths[%d]: %w", where, i, err)
		}
		t.ApprovedRelease.Ignore = append(t.ApprovedRelease.Ignore, re)
	}
	return nil
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
	n := t.ReleaseNotes
	if n.OldTransition != "" || len(n.OldFrom) > 0 {
		return fmt.Errorf("%s.release_notes.transition and from are replaced by transitions: [{from: [%s], to: %s}]", where, strings.Join(n.OldFrom, ", "), n.OldTransition)
	}
	if err := validateTransitions(where+".release_notes.transitions", n.Transitions); err != nil {
		return err
	}
	switch {
	case n.Subtasks && len(n.Transitions) == 0:
		return fmt.Errorf("%s.release_notes.subtasks: needs transitions to move them by", where)
	case n.FixVersion == nil:
		return nil
	case (strings.TrimSpace(n.FixVersion.Prefix) == "") == (n.FixVersion.Name == ""):
		return fmt.Errorf("%s.release_notes.fix_version: one of prefix (how the version names begin, such as \"Product v\") or name (a template such as \"Product {dd}.{mm}.{yy}\")", where)
	case n.FixVersion.Name != "":
		var err error
		if n.FixVersion.Template, err = ParseVersionName(n.FixVersion.Name); err != nil {
			return fmt.Errorf("%s.release_notes.fix_version.name: %w", where, err)
		}
	}
	return nil
}

func validateTransitions(where string, ts []NoteTransition) error {
	from := map[string]int{}
	for i, t := range ts {
		at := fmt.Sprintf("%s[%d]", where, i)
		switch {
		case strings.TrimSpace(t.To) == "":
			return fmt.Errorf("%s.to: required, the status the item moves to", at)
		case len(t.From) == 0:
			return fmt.Errorf("%s.from: required, the statuses an item moves from, such as [In Progress] (an item elsewhere, paused or not started, keeps its status)", at)
		case slices.Contains(t.From, t.To):
			return fmt.Errorf("%s.from: lists %q, the transition's own status", at, t.To)
		case t.Assign != "" && strings.TrimSpace(t.Assign) == "":
			return fmt.Errorf("%s.assign: blank", at)
		}
		for _, f := range t.From {
			if j, ok := from[f]; ok {
				return fmt.Errorf("%s.from: %q is in %s[%d].from too: an item moves one way", at, f, where, j)
			}
			from[f] = i
		}
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
