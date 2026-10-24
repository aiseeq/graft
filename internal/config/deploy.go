package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
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
	// LoadWait bounds how long the heavy tasks of a deploy, and of the
	// grafts its script starts, wait for a busy machine: one budget for
	// the whole deploy.
	LoadWait LoadWait `yaml:"load_wait"`
}

// LoadWait is a deploy's budget for waiting on a busy machine.
type LoadWait struct {
	// Budget is the whole deploy's; DefaultLoadWait when unset.
	Budget time.Duration `yaml:"budget"`
	// Then is what a heavy task does when the budget is spent: start (the
	// default) or fail, failing the deploy.
	Then string `yaml:"then"`
}

// DefaultLoadWait is a deploy's budget for waiting when it sets none.
const DefaultLoadWait = 10 * time.Minute

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
	// MergeIntoTrunk records a branch other than the trunk on the trunk once
	// the target runs it, by a merge keeping the trunk's tree, when the
	// trunk has all the branch's work by content.
	MergeIntoTrunk bool `yaml:"merge_into_trunk"`
	// Window is when the target takes releases; a hotfix goes any time.
	Window *Window `yaml:"window"`
	// ReleaseVersion keeps the Jira versions of the windows.
	ReleaseVersion *ReleaseVersion `yaml:"release_version"`
	// ReleasePage publishes the release notes page in Confluence.
	ReleasePage *ReleasePage `yaml:"release_page"`
	// Integrations refuses a release that the target from Requires runs with
	// an integration this target does not configure.
	Integrations *Integrations `yaml:"integrations"`
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

// Integrations compares the integrations two targets configure.
type Integrations struct {
	// Command prints one integration a line, the ones the environment's
	// configuration turns on; it runs in the environment of this target and
	// of the target from requires. With {staged} in it, it asks the build
	// the deploy script uploaded: the script calls graft deploy
	// check-integrations <path> once the build is there.
	Command EnvCommand `yaml:"command"`
	// Running is what {staged} is on the target from requires, which runs
	// the release already: the path of the build it runs.
	Running string `yaml:"running"`
	// Exceptions is a file of "integration|reason" lines: integrations the
	// target from requires runs and this one leaves off on purpose.
	Exceptions string `yaml:"exceptions"`
}

// StagedPlaceholder in Integrations.Command is the build the command asks.
const StagedPlaceholder = "{staged}"

// Staged tells whether the command asks the uploaded build.
func (i *Integrations) Staged() bool {
	return strings.Contains(i.Command.Shell, StagedPlaceholder) ||
		slices.ContainsFunc(i.Command.Argv, func(w string) bool { return strings.Contains(w, StagedPlaceholder) })
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
	// Comment is this target's release note text instead of the top-level
	// release_notes.jira.comment; {release} names the release by its window
	// (release_version.name, release_version.hotfix for a hotfix).
	Comment string `yaml:"comment"`
	// FixVersion adds the project's next unreleased version to every item
	// that gets a note.
	FixVersion *FixVersion `yaml:"fix_version"`
	// Subtasks follow their parent: when a delivered item is in, or reaches,
	// the status a transition leads to, its subtasks in that transition's
	// from move there by it.
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
	// Attribute ties commits whose messages name no item to their items.
	Attribute []Attribution `yaml:"attribute"`
	// Attributed maps the full commit hashes of Attribute to their entries.
	Attributed map[string]Attribution `yaml:"-"`
}

// Attribution ties a commit that names no item to the item it belongs to:
// the commit is checked, noted and listed as a commit naming the item.
type Attribution struct {
	// Commit is the full hash of the commit.
	Commit string `yaml:"commit"`
	// Item is the key of the item, such as PROJ-123.
	Item string `yaml:"item"`
	// Reason says why the commit belongs to the item; required.
	Reason string `yaml:"reason"`
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

// ShipsNothing tells whether every target ignores every file; no files is not.
func (c *Config) ShipsNothing(files []string) bool {
	if c.Deploy == nil {
		return false
	}
	if len(files) == 0 || len(c.Deploy.Targets) == 0 {
		return false
	}
	for _, n := range keysOf(c.Deploy.Targets) {
		r := c.Deploy.Targets[n].ApprovedRelease
		if r == nil || !r.Ignored(files) {
			return false
		}
	}
	return true
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
	// SkipKeys leaves these items alone, and their subtasks: a standing item
	// that collects small work and that commits name, say.
	SkipKeys []string `yaml:"skip_keys"`
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
	if d.LoadWait.Budget < 0 {
		return errors.New("deploy.load_wait.budget: must not be negative")
	}
	if d.LoadWait.Budget == 0 {
		d.LoadWait.Budget = DefaultLoadWait
	}
	switch d.LoadWait.Then {
	case "":
		d.LoadWait.Then = "start"
	case "start", "fail":
	default:
		return fmt.Errorf("deploy.load_wait.then: want start or fail, got %q", d.LoadWait.Then)
	}
	// The project keys first: approved_release.attribute names their items.
	if err := c.validateReleaseNotes(); err != nil {
		return err
	}
	for _, name := range keysOf(d.Targets) {
		if err := c.validateTarget(name, d.Targets[name]); err != nil {
			return err
		}
	}
	return c.validateHotfix()
}

// DeploySubcommands are the words after graft deploy that are not targets.
var DeploySubcommands = []string{"status", "logs", "check", "versions", "check-head", "check-integrations", "cutoff"}

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
	if t.MergeIntoTrunk && t.DeployedSHA == nil {
		return fmt.Errorf("%s.merge_into_trunk: needs deployed_sha: the next deploy reads what the target runs from it", where)
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
	if err := c.validateIntegrations(where, t); err != nil {
		return err
	}
	if err := c.validateReleaseVersion(where, t); err != nil {
		return err
	}
	if err := c.validateReleasePage(where, t); err != nil {
		return err
	}
	if err := c.validateApprovedRelease(where, t); err != nil {
		return err
	}
	return c.validateTargetNotes(where, t)
}

func (c *Config) validateIntegrations(where string, t *DeployTarget) error {
	in := t.Integrations
	if in == nil {
		return nil
	}
	where += ".integrations"
	switch {
	case !in.Command.IsSet():
		return fmt.Errorf("%s.command: required, prints one integration a line", where)
	case t.Requires == "":
		return fmt.Errorf("%s: needs requires, the target whose integrations this one must have", where)
	case in.Staged() && strings.TrimSpace(in.Running) == "":
		return fmt.Errorf("%s.running: required with %s in the command: the build the target from requires runs", where, StagedPlaceholder)
	case !in.Staged() && in.Running != "":
		return fmt.Errorf("%s.running: only with %s in the command", where, StagedPlaceholder)
	}
	for _, env := range []string{t.Env, c.Deploy.Targets[t.Requires].Env} {
		if err := c.checkRemoteArgv(where+".command", env, in.Command); err != nil {
			return err
		}
	}
	return nil
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
	return c.validateAttribute(where+".approved_release.attribute", t.ApprovedRelease)
}

var fullHashRe = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// validateAttribute checks the attributions as written; whether the commits
// exist and name no item is for the repository to tell (graft check, deploy).
func (c *Config) validateAttribute(where string, r *ApprovedRelease) error {
	r.Attributed = map[string]Attribution{}
	seen := map[string]int{}
	for i, at := range r.Attribute {
		w := fmt.Sprintf("%s[%d]", where, i)
		switch {
		case !fullHashRe.MatchString(at.Commit):
			return fmt.Errorf("%s.commit: %q is not a full commit hash (40 lowercase hex digits, 64 in a SHA-256 repository)", w, at.Commit)
		case !c.isProjectKey(at.Item):
			return fmt.Errorf("%s.item: %q is not an item of release_notes.jira.project_keys, such as PROJ-123", w, at.Item)
		case strings.TrimSpace(at.Reason) == "":
			return fmt.Errorf("%s.reason: required, why the commit belongs to %s", w, at.Item)
		}
		if j, ok := seen[at.Commit]; ok {
			return fmt.Errorf("%s.commit: %s is attributed twice, in %s[%d] too", w, at.Commit, where, j)
		}
		seen[at.Commit] = i
		r.Attributed[at.Commit] = at
	}
	return nil
}

var itemNumberRe = regexp.MustCompile(`^[1-9][0-9]*$`)

// isProjectKey tells whether key is an item key of one of the project keys.
func (c *Config) isProjectKey(key string) bool {
	project, number, ok := strings.Cut(key, "-")
	return ok && slices.Contains(c.ReleaseNotes.Jira.ProjectKeys, project) && itemNumberRe.MatchString(number)
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

// NoteComment is the release note text of the target.
func (c *Config) NoteComment(t *DeployTarget) string {
	if t.ReleaseNotes != nil && t.ReleaseNotes.Comment != "" {
		return t.ReleaseNotes.Comment
	}
	return c.ReleaseNotes.Jira.Comment
}

// validateReleaseName lets {release} into a target's note only where every
// deploy has a release name: the window's or the hotfix's version name.
func (c *Config) validateReleaseName(where string, t *DeployTarget) error {
	if !strings.Contains(c.NoteComment(t), "{release}") {
		return nil
	}
	switch rv := t.ReleaseVersion; {
	case rv == nil:
		return fmt.Errorf("%s.release_notes.comment: {release} needs release_version, whose name names the release (enabled: false keeps the Jira versions alone)", where)
	case c.Deploy.Hotfix != nil && rv.Hotfix == "":
		return fmt.Errorf("%s.release_notes.comment: {release} needs release_version.hotfix to name a hotfix", where)
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
	if err := c.validateReleaseName(where, t); err != nil {
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
	for _, k := range n.Jira.SkipKeys {
		if !c.isProjectKey(k) {
			return fmt.Errorf("release_notes.jira.skip_keys: %q is not an item of release_notes.jira.project_keys, such as PROJ-123", k)
		}
	}
	if n.Jira.Comment == "" {
		n.Jira.Comment = DefaultNoteComment
	}
	return nil
}
