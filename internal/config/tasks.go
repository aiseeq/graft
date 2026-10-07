package config

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Step is one thing a task (or the gate) does: a program run as argv, a
// shell command line run by sh, another task, or a service action.
type Step struct {
	Argv  []string
	Shell string
	Task  string
	// ServiceAction (start, stop, restart) acts on ServiceName, or on every
	// service when it is empty.
	ServiceAction string
	ServiceName   string
	Source        string
}

// Service actions a step can take.
var serviceActions = []string{"start", "stop", "restart"}

// UnmarshalYAML accepts a string (argv by shell quoting rules, no shell), a
// list (argv as is), {sh: "..."} or {task: name}.
func (s *Step) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode, yaml.SequenceNode:
		var c Command
		if err := c.UnmarshalYAML(node); err != nil {
			return err
		}
		s.Argv, s.Source = c.Argv, c.Source
		return nil
	case yaml.MappingNode:
		return s.unmarshalMap(node)
	default:
		return fmt.Errorf("line %d: a step is a string, a list, {sh: ...}, {task: ...} or {service: ...}", node.Line)
	}
}

func (s *Step) unmarshalMap(node *yaml.Node) error {
	if len(node.Content) != 2 || node.Content[1].Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: a step map is {sh: \"...\"}, {task: name} or {service: action [name]}", node.Line)
	}
	key, value := node.Content[0].Value, node.Content[1].Value
	switch key {
	case "sh":
		s.Shell, s.Source = value, shellLabel(value)
	case "task":
		s.Task, s.Source = value, "task "+value
	case "service":
		fields := strings.Fields(value)
		if len(fields) == 0 || len(fields) > 2 || !slices.Contains(serviceActions, fields[0]) {
			return fmt.Errorf("line %d: service: %q is not start|stop|restart [name]", node.Line, value)
		}
		s.ServiceAction = fields[0]
		if len(fields) == 2 {
			s.ServiceName = fields[1]
		}
		s.Source = "service " + value
	default:
		// "echo a: b" unquoted is a map to YAML.
		return fmt.Errorf("line %d: %q was read as a map (\": \" in a plain string); quote the whole step, or use {sh: ...}, {task: ...} or {service: ...}",
			node.Line, key+": "+value)
	}
	if value == "" {
		return fmt.Errorf("line %d: %s: empty", node.Line, key)
	}
	return nil
}

// shellLabel names a shell step in graft's output: the script itself when it
// is one line, else its first line that does something (not blank, a
// comment or a set option) followed by "...".
func shellLabel(script string) string {
	script = strings.TrimSpace(script)
	if !strings.Contains(script, "\n") {
		return script
	}
	for line := range strings.SplitSeq(script, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "set ") {
			continue
		}
		return line + " ..."
	}
	return script
}

// Task is a named project command.
type Task struct {
	Desc string   `yaml:"desc"`
	Deps []string `yaml:"deps"`
	Run  []Step   `yaml:"run"`
	// Dir is where the steps run, relative to the work tree root.
	Dir string `yaml:"dir"`
	// DotEnv lists .env keys passed to the steps; the rest of .env is not.
	DotEnv []DotEnvKey `yaml:"dotenv"`
	// DotEnvSets names sets from the top-level dotenv_sets to pass as well.
	DotEnvSets []string `yaml:"dotenv_sets"`
	// Keys is DotEnv with the sets merged in.
	Keys []DotEnvKey `yaml:"-"`
	// Env sets variables; ${KEY} in values is expanded.
	Env map[string]string `yaml:"env"`
	// Args is required when the task refuses to run without arguments.
	Args string `yaml:"args"`
	// Usage describes the arguments, for help and errors.
	Usage string `yaml:"usage"`
	// KeepGoing runs every step and fails at the end, listing what failed.
	KeepGoing bool      `yaml:"keep_going"`
	Lock      *TaskLock `yaml:"lock"`
	// TestDB passes the test database DSN, after checking it points at the
	// test database.
	TestDB bool `yaml:"test_db"`
}

// TaskLock is a named read/write lock held while a task runs.
type TaskLock struct {
	Name string `yaml:"name"`
	Mode string `yaml:"mode"`
}

// DefaultLockName is the lock a bare "lock: read|write" refers to.
const DefaultLockName = "work-tree"

// UnmarshalYAML accepts "read", "write" or {name, mode}.
func (l *TaskLock) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		l.Name, l.Mode = DefaultLockName, node.Value
		return nil
	}
	type plain TaskLock
	var p plain
	if err := node.Decode(&p); err != nil {
		return err
	}
	*l = TaskLock(p)
	return nil
}

// Service is a long-running local process graft starts in the background,
// tracked by a pid file: the native-binary counterpart of a dev container.
type Service struct {
	Desc string `yaml:"desc"`
	// Build is a task run before start.
	Build string  `yaml:"build"`
	Run   Command `yaml:"run"`
	// DotEnv lists the .env keys the process gets; "all" passes the whole
	// file, as an application reading its config from the environment needs.
	DotEnv     DotEnvKeys `yaml:"dotenv"`
	DotEnvSets []string   `yaml:"dotenv_sets"`
	// Keys is the DotEnv list with the sets merged in (unused with all).
	Keys    []DotEnvKey       `yaml:"-"`
	Env     map[string]string `yaml:"env"`
	Log     string            `yaml:"log"`
	PIDFile string            `yaml:"pidfile"`
	// Addr is host:port the service listens on; ${KEY} is expanded and an
	// empty host means localhost. start waits for it, stop waits for it to
	// be released.
	Addr         string        `yaml:"addr"`
	StartTimeout time.Duration `yaml:"start_timeout"`
	StopTimeout  time.Duration `yaml:"stop_timeout"`
	// Lock is taken for writing while the service starts or stops.
	Lock string `yaml:"lock"`
	// SystemdUnit hands the service to systemd --user: start, stop, status
	// and logs go through systemctl and journalctl, and run, pidfile, log,
	// dotenv and env belong to the unit instead.
	SystemdUnit string `yaml:"systemd_unit"`
}

// DotEnvKey is a .env key passed to a task or service. "KEY?" marks it
// optional: when it is set nowhere it is left out instead of failing, for
// programs that have their own default.
type DotEnvKey struct {
	Name     string
	Optional bool
	// NonEmpty ("KEY!") refuses a key that is set but empty.
	NonEmpty bool
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// UnmarshalYAML accepts KEY, KEY? (optional) or KEY! (must not be empty).
func (k *DotEnvKey) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: a .env key is a string", node.Line)
	}
	name := node.Value
	name, k.Optional = strings.CutSuffix(name, "?")
	if !k.Optional {
		name, k.NonEmpty = strings.CutSuffix(name, "!")
	}
	if !envKeyRe.MatchString(name) {
		return fmt.Errorf("line %d: %q is not a .env key (KEY, KEY? or KEY!)", node.Line, node.Value)
	}
	k.Name = name
	return nil
}

// mergeKeys joins key lists; a key required anywhere is required, and one
// that must not be empty anywhere must not be empty.
func mergeKeys(lists ...[]DotEnvKey) []DotEnvKey {
	var out []DotEnvKey
	index := map[string]int{}
	for _, list := range lists {
		for _, k := range list {
			if i, ok := index[k.Name]; ok {
				out[i].Optional = out[i].Optional && k.Optional
				out[i].NonEmpty = out[i].NonEmpty || k.NonEmpty
				continue
			}
			index[k.Name] = len(out)
			out = append(out, k)
		}
	}
	return out
}

// resolveKeys merges a task's or service's own keys with the named sets.
func (c *Config) resolveKeys(where string, own []DotEnvKey, sets []string) ([]DotEnvKey, error) {
	lists := [][]DotEnvKey{}
	for _, name := range sets {
		set, ok := c.DotEnvSets[name]
		if !ok {
			return nil, fmt.Errorf("%s.dotenv_sets: unknown set %q", where, name)
		}
		lists = append(lists, set)
	}
	return mergeKeys(append(lists, own)...), nil
}

// DotEnvKeys is a list of keys, or all of them.
type DotEnvKeys struct {
	All  bool
	Keys []DotEnvKey
}

// UnmarshalYAML accepts "all" or a list of keys.
func (d *DotEnvKeys) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		if node.Value != "all" {
			return fmt.Errorf("line %d: dotenv is a list of keys or all", node.Line)
		}
		d.All = true
		return nil
	}
	return node.Decode(&d.Keys)
}

// TestDB describes the disposable PostgreSQL the tests run against.
type TestDB struct {
	Image     string   `yaml:"image"`
	Container string   `yaml:"container"`
	Port      int      `yaml:"port"`
	Database  string   `yaml:"database"`
	User      string   `yaml:"user"`
	Password  string   `yaml:"password"`
	Settings  []string `yaml:"settings"`
	// Tmpfs is the size of the tmpfs the data volumes of the image are
	// mounted in (512m, 2g), lowercased; empty keeps the data on disk.
	Tmpfs string `yaml:"tmpfs"`
	// Migrate runs after the database is up, with the DSN in DSNVar.
	Migrate      Command       `yaml:"migrate"`
	DSNVar       string        `yaml:"dsn_var"`
	ReadyTimeout time.Duration `yaml:"ready_timeout"`
}

// DefaultTestDBSettings make a throwaway database fast: nothing it holds
// needs to survive a crash.
var DefaultTestDBSettings = []string{"fsync=off", "synchronous_commit=off", "full_page_writes=off"}

// DSN is the connection string of the test database.
func (t *TestDB) DSN() string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(t.User, t.Password),
		Host:     fmt.Sprintf("127.0.0.1:%d", t.Port),
		Path:     "/" + t.Database,
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

// Tool is a pinned development tool: installed with go install, or by hand
// when go install cannot provide it (a system package).
type Tool struct {
	GoInstall string   `yaml:"go_install"`
	Tags      []string `yaml:"tags"`
	// Manual says how to install the tool by hand; graft prints it and never
	// runs it (it usually needs sudo or a package manager).
	Manual string `yaml:"manual"`
	// Check runs the tool; its output must contain Expect (a version, or a
	// capability such as a rule name). A go_install tool may leave both out:
	// its Go build info is checked against the pin instead.
	Check  []string `yaml:"check"`
	Expect string   `yaml:"expect"`
	// Binary names the program on PATH whose build info is checked, when it
	// is not the name go install gives it.
	Binary string `yaml:"binary"`
}

// UsesBuildInfo reports whether the tool is checked by its Go build info
// rather than by running a check command.
func (t *Tool) UsesBuildInfo() bool { return t.GoInstall != "" && len(t.Check) == 0 }

// Pin splits go_install into the package path and the pinned version.
func (t *Tool) Pin() (pkg, version string) {
	pkg, version, _ = strings.Cut(t.GoInstall, "@")
	return pkg, version
}

// Program is the program the check looks up on PATH: the check command, the
// binary override, or the name go install gives the package.
func (t *Tool) Program() string {
	switch {
	case len(t.Check) > 0:
		return t.Check[0]
	case t.Binary != "":
		return t.Binary
	}
	pkg, _ := t.Pin()
	elem := path.Base(pkg)
	// go install drops a major version suffix: example.com/tool/v2 is "tool".
	if elem != pkg && isMajorVersion(elem) {
		elem = path.Base(path.Dir(pkg))
	}
	return elem
}

// isMajorVersion is go's own test for a /vN path element (v2 and up).
func isMajorVersion(s string) bool {
	if len(s) < 2 || s[0] != 'v' || s[1] == '0' || s[1] == '1' && len(s) == 2 {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || '9' < s[i] {
			return false
		}
	}
	return true
}

// Builtin command names a task cannot take.
var builtinNames = []string{
	"commit", "amend", "release", "version", "init", "check", "gate", "hook", "flags", "deploy",
	"run", "help", "start", "stop", "restart", "status", "locks", "testdb", "tools", "tasks", "logs",
	"stats",
}

var (
	taskNameRe  = regexp.MustCompile(`^[a-z][a-z0-9_:.-]*$`)
	pinnedRe    = regexp.MustCompile(`^[a-zA-Z0-9._/-]+@v[0-9][0-9A-Za-z.+-]*$`)
	dbIdentRe   = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	containerRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)
)

func (c *Config) validateTasks() error {
	for _, name := range keysOf(c.Tasks) {
		if err := c.validateTask(name, c.Tasks[name]); err != nil {
			return err
		}
	}
	for _, s := range c.Gate {
		if s.Task != "" {
			if _, ok := c.Tasks[s.Task]; !ok {
				return fmt.Errorf("gate: unknown task %q", s.Task)
			}
		}
		if s.ServiceAction != "" {
			return errors.New("gate: service steps belong in tasks; the gate only checks")
		}
	}
	return c.checkTaskCycles()
}

func (c *Config) validateTask(name string, t *Task) error {
	where := "tasks." + name
	if slices.Contains(builtinNames, name) {
		return fmt.Errorf("%s: %s is a graft command, name the task otherwise", where, name)
	}
	if !taskNameRe.MatchString(name) {
		return fmt.Errorf("%s: a task name is lowercase letters, digits and _:.-", where)
	}
	if t == nil || (len(t.Run) == 0 && len(t.Deps) == 0) {
		return fmt.Errorf("%s: needs run or deps", where)
	}
	if t.Dir != "" {
		if err := checkRelPath(where+".dir", t.Dir); err != nil {
			return err
		}
	}
	if err := t.Lock.validate(where + ".lock"); err != nil {
		return err
	}
	if t.TestDB && c.TestDB == nil {
		return fmt.Errorf("%s.test_db: needs a test_db section", where)
	}
	keys, err := c.resolveKeys(where, t.DotEnv, t.DotEnvSets)
	if err != nil {
		return err
	}
	t.Keys = keys
	if err := t.validateArgs(where); err != nil {
		return err
	}
	return c.validateTaskRefs(where, t)
}

// validateTaskRefs checks the tasks and services a task names.
func (c *Config) validateTaskRefs(where string, t *Task) error {
	refs := slices.Clone(t.Deps)
	for _, s := range t.Run {
		if s.Task != "" {
			refs = append(refs, s.Task)
		}
		if s.ServiceAction != "" && len(c.Services) == 0 {
			return fmt.Errorf("%s.run: a service step needs a services section", where)
		}
		if _, ok := c.Services[s.ServiceName]; s.ServiceName != "" && !ok {
			return fmt.Errorf("%s.run: unknown service %q", where, s.ServiceName)
		}
	}
	for _, ref := range refs {
		if _, ok := c.Tasks[ref]; !ok {
			return fmt.Errorf("%s: unknown task %q", where, ref)
		}
	}
	return nil
}

// checkTaskCycles rejects tasks that reach themselves through deps or steps.
func (c *Config) checkTaskCycles() error {
	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	var visit func(name string, path []string) error
	visit = func(name string, path []string) error {
		switch state[name] {
		case visiting:
			return fmt.Errorf("tasks: cycle %s", strings.Join(append(path, name), " -> "))
		case done:
			return nil
		}
		state[name] = visiting
		t := c.Tasks[name]
		next := slices.Clone(t.Deps)
		for _, s := range t.Run {
			if s.Task != "" {
				next = append(next, s.Task)
			}
		}
		for _, n := range next {
			if err := visit(n, append(path, name)); err != nil {
				return err
			}
		}
		state[name] = done
		return nil
	}
	for _, name := range keysOf(c.Tasks) {
		if err := visit(name, nil); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) validateServices() error {
	for _, name := range keysOf(c.Services) {
		s := c.Services[name]
		where := "services." + name
		if !taskNameRe.MatchString(name) {
			return fmt.Errorf("%s: invalid name", where)
		}
		if err := s.validateRunner(where); err != nil {
			return err
		}
		if s.DotEnv.All && len(s.DotEnvSets) > 0 {
			return fmt.Errorf("%s: dotenv: all already passes every key, dotenv_sets adds nothing", where)
		}
		keys, err := c.resolveKeys(where, s.DotEnv.Keys, s.DotEnvSets)
		if err != nil {
			return err
		}
		s.Keys = keys
		if s.Build != "" {
			if _, ok := c.Tasks[s.Build]; !ok {
				return fmt.Errorf("%s.build: unknown task %q", where, s.Build)
			}
		}
		if s.StartTimeout == 0 {
			s.StartTimeout = 30 * time.Second
		}
		if s.StopTimeout == 0 {
			s.StopTimeout = 10 * time.Second
		}
		if s.Lock != "" && !taskNameRe.MatchString(s.Lock) {
			return fmt.Errorf("%s.lock: invalid name %q", where, s.Lock)
		}
	}
	return nil
}

func (c *Config) validateDotEnvSets() error {
	for _, name := range keysOf(c.DotEnvSets) {
		if !taskNameRe.MatchString(name) {
			return fmt.Errorf("dotenv_sets.%s: invalid name", name)
		}
		if len(c.DotEnvSets[name]) == 0 {
			return fmt.Errorf("dotenv_sets.%s: empty", name)
		}
	}
	return nil
}

// ArgsPlaceholder in a step receives the arguments given after --.
const ArgsPlaceholder = "{args}"

// checkArgsPlaceholder allows {args} as a whole argv word (any number of
// arguments) or once inside a word (exactly one argument, checked when the
// task runs).
func checkArgsPlaceholder(s Step) error {
	for _, w := range s.Argv {
		if strings.Count(w, ArgsPlaceholder) > 1 {
			return fmt.Errorf("%q: %s at most once per word", w, ArgsPlaceholder)
		}
	}
	return nil
}

func (l *TaskLock) validate(where string) error {
	if l == nil {
		return nil
	}
	if l.Mode != "read" && l.Mode != "write" {
		return fmt.Errorf("%s: mode read or write, got %q", where, l.Mode)
	}
	if !taskNameRe.MatchString(l.Name) {
		return fmt.Errorf("%s.name: invalid %q", where, l.Name)
	}
	return nil
}

func (t *Task) validateArgs(where string) error {
	for i, s := range t.Run {
		if err := checkArgsPlaceholder(s); err != nil {
			return fmt.Errorf("%s.run[%d]: %w", where, i, err)
		}
	}
	if t.Args != "" && t.Args != ArgsRequired {
		return fmt.Errorf("%s.args: only %q, got %q", where, ArgsRequired, t.Args)
	}
	if (t.Args != "" || t.Usage != "") && !t.TakesArgs() {
		return fmt.Errorf("%s: args and usage need %s in a step", where, ArgsPlaceholder)
	}
	return nil
}

// ArgsRequired is the args value of a task that needs arguments.
const ArgsRequired = "required"

// UsageText is what the task takes after --.
func (t *Task) UsageText() string {
	if t.Usage != "" {
		return t.Usage
	}
	return "args..."
}

// TakesArgs reports whether any step of the task receives {args}.
func (t *Task) TakesArgs() bool {
	for _, s := range t.Run {
		if strings.Contains(s.Shell, ArgsPlaceholder) || slices.ContainsFunc(s.Argv, func(w string) bool { return strings.Contains(w, ArgsPlaceholder) }) {
			return true
		}
	}
	return false
}

var unitRe = regexp.MustCompile(`^[A-Za-z0-9@._-]+\.service$`)

// validateRunner checks the fields of the way the service runs: a systemd
// unit, or a process graft starts with a pid file and a log.
func (s *Service) validateRunner(where string) error {
	if s == nil {
		return fmt.Errorf("%s: empty", where)
	}
	if s.SystemdUnit != "" {
		if !unitRe.MatchString(s.SystemdUnit) {
			return fmt.Errorf("%s.systemd_unit: %q is not a name.service unit", where, s.SystemdUnit)
		}
		if len(s.Run.Argv) > 0 || s.PIDFile != "" || s.Log != "" || s.DotEnv.All || len(s.DotEnv.Keys) > 0 || len(s.DotEnvSets) > 0 || len(s.Env) > 0 {
			return fmt.Errorf("%s: with systemd_unit, run, pidfile, log, dotenv and env belong to the unit", where)
		}
		return nil
	}
	if len(s.Run.Argv) == 0 || s.PIDFile == "" || s.Log == "" {
		return fmt.Errorf("%s: run, pidfile and log are required (or systemd_unit)", where)
	}
	for field, p := range map[string]string{"pidfile": s.PIDFile, "log": s.Log} {
		if err := checkRelPath(where+"."+field, p); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) validateTestDB() error {
	t := c.TestDB
	if t == nil {
		return nil
	}
	if t.Image == "" || !containerRe.MatchString(t.Container) || t.Port <= 0 || t.Port > 65535 {
		return errors.New("test_db: image, container and port are required")
	}
	for field, v := range map[string]string{"database": t.Database, "user": t.User} {
		if !dbIdentRe.MatchString(v) {
			return fmt.Errorf("test_db.%s: %q is not a plain lowercase identifier", field, v)
		}
	}
	if t.Password == "" {
		return errors.New("test_db.password: required (it is a throwaway local database, any value will do)")
	}
	if err := t.validateTmpfs(); err != nil {
		return err
	}
	if t.Settings == nil {
		t.Settings = slices.Clone(DefaultTestDBSettings)
	}
	if t.DSNVar == "" {
		t.DSNVar = "TEST_DB_DSN"
	}
	if t.ReadyTimeout == 0 {
		t.ReadyTimeout = 60 * time.Second
	}
	return nil
}

// minTmpfs is the smallest tmpfs a fresh cluster initializes in, with room
// left for a database.
const minTmpfs = 64 << 20

var tmpfsRe = regexp.MustCompile(`^([0-9]+)([kmg])$`)

// validateTmpfs accepts a size docker and the kernel read the same way: a
// number with a k, m or g suffix.
func (t *TestDB) validateTmpfs() error {
	if t.Tmpfs == "" {
		return nil
	}
	size := strings.ToLower(t.Tmpfs)
	m := tmpfsRe.FindStringSubmatch(size)
	if m == nil {
		return fmt.Errorf("test_db.tmpfs: %q is not a size such as 512m or 2g", t.Tmpfs)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	shift := map[string]uint{"k": 10, "m": 20, "g": 30}[m[2]]
	if err != nil || n > math.MaxInt64>>shift || n<<shift < minTmpfs {
		return fmt.Errorf("test_db.tmpfs: %q is out of range: at least 64m", t.Tmpfs)
	}
	t.Tmpfs = size
	return nil
}

func (c *Config) validateTools() error {
	for _, name := range keysOf(c.Tools) {
		t := c.Tools[name]
		where := "tools." + name
		switch {
		case t == nil || (t.GoInstall == "" && t.Manual == ""):
			return fmt.Errorf("%s: go_install or manual is required: how the tool gets installed", where)
		case t.GoInstall != "" && t.Manual != "":
			return fmt.Errorf("%s: go_install and manual are exclusive", where)
		case t.GoInstall != "" && !pinnedRe.MatchString(t.GoInstall):
			return fmt.Errorf("%s.go_install: a module path pinned to a version (path@vX.Y.Z), not @latest", where)
		case t.Manual != "" && len(t.Tags) > 0:
			return fmt.Errorf("%s.tags: build tags apply to go_install only", where)
		}
		if err := t.validateCheck(where); err != nil {
			return err
		}
	}
	return nil
}

// validateCheck: a manual tool needs check and expect, a go_install tool
// both or neither (then its build info is checked, on Binary if given).
func (t *Tool) validateCheck(where string) error {
	switch {
	case t.Manual != "" && (len(t.Check) == 0 || t.Expect == ""):
		return fmt.Errorf("%s: check and expect are required: presence on PATH says nothing about the version", where)
	case t.Manual != "" && t.Binary != "":
		return fmt.Errorf("%s.binary: names the program whose Go build info is checked, go_install tools only", where)
	case (len(t.Check) == 0) != (t.Expect == ""):
		return fmt.Errorf("%s: check and expect go together; leave both out to check the Go build info against the pin", where)
	case t.Binary != "" && len(t.Check) > 0:
		return fmt.Errorf("%s.binary: with check, check names the program", where)
	case strings.ContainsAny(t.Binary, `/\`):
		return fmt.Errorf("%s.binary: %q is a program name looked up on PATH, not a path", where, t.Binary)
	}
	return nil
}
