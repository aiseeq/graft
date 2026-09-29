package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Step is one thing a task (or the gate) does: a program run as argv, a
// shell command line run by sh, or another task.
type Step struct {
	Argv   []string
	Shell  string
	Task   string
	Source string
}

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
		var m struct {
			Sh   string `yaml:"sh"`
			Task string `yaml:"task"`
		}
		dec := node
		if err := dec.Decode(&m); err != nil {
			return fmt.Errorf("line %d: %w", node.Line, err)
		}
		if len(node.Content) != 2 || (m.Sh == "") == (m.Task == "") {
			return fmt.Errorf("line %d: a step map is {sh: \"...\"} or {task: name}", node.Line)
		}
		s.Shell, s.Task = m.Sh, m.Task
		s.Source = strings.TrimSpace(m.Sh)
		if first, _, multi := strings.Cut(s.Source, "\n"); multi {
			s.Source = first + " ..."
		}
		if m.Task != "" {
			s.Source = "task " + m.Task
		}
		return nil
	default:
		return fmt.Errorf("line %d: a step is a string, a list, {sh: ...} or {task: ...}", node.Line)
	}
}

// Task is a named project command.
type Task struct {
	Desc string   `yaml:"desc"`
	Deps []string `yaml:"deps"`
	Run  []Step   `yaml:"run"`
	// Dir is where the steps run, relative to the work tree root.
	Dir string `yaml:"dir"`
	// DotEnv lists .env keys passed to the steps; the rest of .env is not.
	DotEnv []string `yaml:"dotenv"`
	// Env sets variables; ${KEY} in values is expanded.
	Env map[string]string `yaml:"env"`
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
	DotEnv  DotEnvKeys        `yaml:"dotenv"`
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
}

// DotEnvKeys is a list of keys, or all of them.
type DotEnvKeys struct {
	All  bool
	Keys []string
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

// Tool is a pinned development tool installed with go install.
type Tool struct {
	GoInstall string   `yaml:"go_install"`
	Tags      []string `yaml:"tags"`
	// Check runs the tool; its output must contain Expect (a version, or a
	// capability such as a rule name).
	Check  []string `yaml:"check"`
	Expect string   `yaml:"expect"`
}

// Builtin command names a task cannot take.
var builtinNames = []string{
	"commit", "amend", "release", "version", "init", "check", "gate", "hook", "flags", "deploy",
	"run", "help", "start", "stop", "restart", "status", "locks", "testdb", "tools", "tasks",
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
	if t.Lock != nil {
		if t.Lock.Mode != "read" && t.Lock.Mode != "write" {
			return fmt.Errorf("%s.lock: mode read or write, got %q", where, t.Lock.Mode)
		}
		if !taskNameRe.MatchString(t.Lock.Name) {
			return fmt.Errorf("%s.lock.name: invalid %q", where, t.Lock.Name)
		}
	}
	if t.TestDB && c.TestDB == nil {
		return fmt.Errorf("%s.test_db: needs a test_db section", where)
	}
	refs := slices.Clone(t.Deps)
	for _, s := range t.Run {
		if s.Task != "" {
			refs = append(refs, s.Task)
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
		if s == nil || len(s.Run.Argv) == 0 || s.PIDFile == "" || s.Log == "" {
			return fmt.Errorf("%s: run, pidfile and log are required", where)
		}
		for field, p := range map[string]string{"pidfile": s.PIDFile, "log": s.Log} {
			if err := checkRelPath(where+"."+field, p); err != nil {
				return err
			}
		}
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

func (c *Config) validateTools() error {
	for _, name := range keysOf(c.Tools) {
		t := c.Tools[name]
		where := "tools." + name
		if t == nil || !pinnedRe.MatchString(t.GoInstall) {
			return fmt.Errorf("%s.go_install: a module path pinned to a version (path@vX.Y.Z), not @latest", where)
		}
		if len(t.Check) == 0 || t.Expect == "" {
			return fmt.Errorf("%s: check and expect are required: presence on PATH says nothing about the version", where)
		}
	}
	return nil
}
