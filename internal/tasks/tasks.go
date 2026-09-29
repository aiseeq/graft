// Package tasks runs the project's tasks and the commit gate: steps run as
// argv (or through sh when asked), with a controlled environment, named
// read/write locks, and each task at most once per invocation.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/dotenv"
	"github.com/aiseeq/graft/internal/envs"
	"github.com/aiseeq/graft/internal/gitx"
	"github.com/aiseeq/graft/internal/lock"
)

// MarkerEnv is set for git commands graft itself runs, so the pre-commit hook
// lets them through. It is never passed on to steps: a test that runs git
// commit must not inherit the pass.
const MarkerEnv = "GRAFT_COMMIT"

// ArgsPlaceholder in a step receives the arguments given after --.
const ArgsPlaceholder = config.ArgsPlaceholder

// Runner runs tasks of one project.
type Runner struct {
	Root   string
	Config *config.Config
	Stdout io.Writer
	Stderr io.Writer
	// Stdin is given to steps; nil closes it (the gate: nothing may wait for
	// input there).
	Stdin io.Reader
	// LockDir holds the named locks.
	LockDir string
	// TestDSN returns the checked test database DSN for test_db tasks.
	TestDSN func(context.Context) (string, error)
	// Service starts, stops or restarts a service (every service when name
	// is empty) for {service: ...} steps.
	Service func(ctx context.Context, action, name string) error
	// Version returns the project version (graft version --describe) and
	// whether there is one: a repository without commits has none.
	Version func() (string, bool, error)

	lookup  *dotenv.Lookup
	base    []string
	version *string
	dsn     *string
	done    map[string]bool
	held    map[string]lock.Mode
}

// VersionEnv carries the project version into every step.
const VersionEnv = "GRAFT_VERSION"

// NewRunner prepares a runner.
func NewRunner(root string, cfg *config.Config, stdout, stderr io.Writer) *Runner {
	return &Runner{
		Root: root, Config: cfg, Stdout: stdout, Stderr: stderr,
		lookup: dotenv.NewLookup(filepath.Join(root, cfg.DotEnv)),
		done:   map[string]bool{},
		held:   map[string]lock.Mode{},
	}
}

// Gate runs the commit gate with stdin closed.
func (r *Runner) Gate(ctx context.Context) error {
	if len(r.Config.Gate) == 0 {
		fmt.Fprintln(r.Stdout, "graft: gate is empty, nothing to run")
		return nil
	}
	r.Stdin = nil
	if err := r.preflight(r.Config.Gate, &config.Task{}, "gate"); err != nil {
		return err
	}
	for i, s := range r.Config.Gate {
		fmt.Fprintf(r.Stdout, "graft: gate %d/%d: %s\n", i+1, len(r.Config.Gate), s.Source)
		if err := r.step(ctx, s, &config.Task{}, nil); err != nil {
			return fmt.Errorf("gate failed at %q: %w", s.Source, err)
		}
	}
	return nil
}

// Run runs a task with its deps. args replace {args} in its steps.
func (r *Runner) Run(ctx context.Context, name string, args []string) error {
	t, ok := r.Config.Tasks[name]
	if !ok {
		return fmt.Errorf("unknown task %q (see graft help)", name)
	}
	if len(args) > 0 && !t.TakesArgs() {
		return fmt.Errorf("task %s takes no arguments (no %s in its steps)", name, ArgsPlaceholder)
	}
	if len(args) == 0 && t.Args == config.ArgsRequired {
		return fmt.Errorf("task %s needs arguments: graft %s -- %s", name, name, t.UsageText())
	}
	if err := checkArgs(name, t, args); err != nil {
		return err
	}
	if err := r.preflight(t.Run, t, name); err != nil {
		return err
	}
	return r.run(ctx, name, args)
}

// checkArgs fits the arguments into the task's steps before anything runs,
// deps included: {args} inside a word takes exactly one.
func checkArgs(name string, t *config.Task, args []string) error {
	for _, s := range t.Run {
		var err error
		switch {
		case s.Shell != "":
			_, err = envs.ShellArgs(s.Shell, args, nil)
		case len(s.Argv) > 0:
			_, err = envs.ExpandArgv(s.Argv, args, nil)
		}
		if err != nil {
			return fmt.Errorf("task %s: %w", name, err)
		}
	}
	return nil
}

// preflight checks, before any step runs, that every .env key and ${KEY}
// the steps need, through deps and task steps, is set: a missing key found
// after the build has run wastes the build and hides the next missing key.
func (r *Runner) preflight(steps []config.Step, owner *config.Task, name string) error {
	var problems []string
	seen := map[string]bool{}
	var visit func(name string, t *config.Task, steps []config.Step)
	visit = func(name string, t *config.Task, steps []config.Step) {
		for _, d := range t.Deps {
			if !seen[d] {
				seen[d] = true
				visit(d, r.Config.Tasks[d], r.Config.Tasks[d].Run)
			}
		}
		for _, s := range steps {
			if s.Task != "" && !seen[s.Task] {
				seen[s.Task] = true
				visit(s.Task, r.Config.Tasks[s.Task], r.Config.Tasks[s.Task].Run)
			}
		}
		for _, p := range r.missing(t, steps) {
			problems = append(problems, name+": "+p)
		}
	}
	visit(name, owner, steps)
	if len(problems) > 0 {
		return fmt.Errorf("nothing was run, missing:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// missing lists what a task's steps need from the environment and .env but
// do not find.
func (r *Runner) missing(t *config.Task, steps []config.Step) []string {
	provided := map[string]bool{VersionEnv: true}
	for k := range t.Env {
		provided[k] = true
	}
	if t.TestDB {
		provided[r.Config.TestDB.DSNVar] = true
	}
	var problems []string
	check := func(key string) {
		if provided[key] {
			return
		}
		provided[key] = true // report once
		if _, err := r.lookup.Value(key); err != nil {
			problems = append(problems, err.Error())
		}
	}
	for _, k := range t.Keys {
		switch {
		case k.NonEmpty:
			provided[k.Name] = true
			if v, err := r.lookup.Value(k.Name); err != nil {
				problems = append(problems, err.Error())
			} else if v == "" {
				problems = append(problems, k.Name+" is empty")
			}
		case !k.Optional:
			check(k.Name)
		}
	}
	var texts []string
	for _, k := range sortedKeys(t.Env) {
		texts = append(texts, t.Env[k])
	}
	for _, s := range steps {
		texts = append(texts, s.Argv...)
	}
	for _, text := range texts {
		refs, err := dotenv.Refs(text)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		for _, key := range refs {
			check(key)
		}
	}
	return problems
}

func (r *Runner) run(ctx context.Context, name string, args []string) error {
	if r.done[name] {
		return nil
	}
	t := r.Config.Tasks[name]
	for _, d := range t.Deps {
		if err := r.run(ctx, d, nil); err != nil {
			return err
		}
	}
	release, err := r.lock(ctx, name, t.Lock)
	if err != nil {
		return err
	}
	err = r.steps(ctx, name, t, args)
	r.done[name] = true
	return errors.Join(err, release())
}

func (r *Runner) steps(ctx context.Context, name string, t *config.Task, args []string) error {
	var failed []string
	for _, s := range t.Run {
		fmt.Fprintf(r.Stderr, "graft: %s: %s\n", name, s.Source)
		err := r.step(ctx, s, t, args)
		if err == nil {
			continue
		}
		if !t.KeepGoing {
			if s.Task != "" {
				return err // the inner task already names its failing step
			}
			return fmt.Errorf("task %s failed at %q: %w", name, s.Source, err)
		}
		fmt.Fprintf(r.Stderr, "graft: %s: %q failed: %v (keep_going)\n", name, s.Source, err)
		failed = append(failed, s.Source)
	}
	if len(failed) > 0 {
		return fmt.Errorf("task %s: %d of %d steps failed: %s", name, len(failed), len(t.Run), strings.Join(failed, "; "))
	}
	return nil
}

func (r *Runner) step(ctx context.Context, s config.Step, t *config.Task, args []string) error {
	if s.Task != "" {
		return r.run(ctx, s.Task, nil)
	}
	if s.ServiceAction != "" {
		if r.Service == nil {
			return errors.New("service steps are not available here")
		}
		return r.Service(ctx, s.ServiceAction, s.ServiceName)
	}
	vars, err := r.vars(ctx, t)
	if err != nil {
		return err
	}
	env, err := r.env(t, vars)
	if err != nil {
		return err
	}
	var cmd *exec.Cmd
	if s.Shell != "" {
		line, err := envs.ShellArgs(s.Shell, args, nil)
		if err != nil {
			return err
		}
		cmd = exec.CommandContext(ctx, "sh", "-c", line)
	} else {
		argv, err := envs.ExpandArgv(s.Argv, args, r.lookup.With(vars))
		if err != nil {
			return err
		}
		cmd = exec.CommandContext(ctx, argv[0], argv[1:]...)
	}
	cmd.Dir = r.Root
	if t.Dir != "" {
		cmd.Dir = filepath.Join(r.Root, filepath.FromSlash(t.Dir))
	}
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = r.Stdin, r.Stdout, r.Stderr
	return cmd.Run()
}

// vars are the variables graft sets for a task's steps: its env values, the
// project version and the test database DSN. ${KEY} in argv sees them too.
func (r *Runner) vars(ctx context.Context, t *config.Task) (map[string]string, error) {
	vars := map[string]string{}
	for _, k := range sortedKeys(t.Env) {
		v, err := dotenv.Expand(t.Env[k], r.lookup)
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", k, err)
		}
		vars[k] = v
	}
	if r.version == nil && r.Version != nil {
		v, ok, err := r.Version()
		if err != nil {
			return nil, fmt.Errorf("project version: %w", err)
		}
		if ok {
			r.version = &v
		}
	}
	if r.version != nil {
		vars[VersionEnv] = *r.version
	}
	if t.TestDB {
		if r.TestDSN == nil {
			return nil, errors.New("test_db is set but no test database is configured")
		}
		if r.dsn == nil {
			dsn, err := r.TestDSN(ctx)
			if err != nil {
				return nil, err
			}
			r.dsn = &dsn
		}
		vars[r.Config.TestDB.DSNVar] = *r.dsn
	}
	return vars, nil
}

// env is the base environment (without graft's marker and git's hook
// variables) plus the task's .env keys and vars.
func (r *Runner) env(t *config.Task, vars map[string]string) ([]string, error) {
	if r.base == nil {
		base, err := BaseEnv()
		if err != nil {
			return nil, err
		}
		r.base = base
	}
	env := slices.Clone(r.base)
	keys, err := KeyValues(r.lookup, t.Keys)
	if err != nil {
		return nil, err
	}
	env = append(env, keys...)
	for _, k := range sortedKeys(vars) {
		env = append(env, k+"="+vars[k])
	}
	return env, nil
}

// KeyValues resolves .env keys into KEY=VALUE entries; optional keys set
// nowhere are left out.
func KeyValues(l *dotenv.Lookup, keys []config.DotEnvKey) ([]string, error) {
	var env []string
	for _, k := range keys {
		if !k.Optional {
			v, err := l.Value(k.Name)
			if err != nil {
				return nil, err
			}
			if k.NonEmpty && v == "" {
				return nil, fmt.Errorf("%s is empty", k.Name)
			}
			env = append(env, k.Name+"="+v)
			continue
		}
		v, ok, err := l.Optional(k.Name)
		if err != nil {
			return nil, err
		}
		if ok {
			env = append(env, k.Name+"="+v)
		}
	}
	return env, nil
}

// BaseEnv is the process environment without graft's commit marker and
// without the variables git sets for hooks (GIT_DIR, GIT_INDEX_FILE, ...):
// a step started from a hook must not have its own git calls, or those of the
// tests it runs, silently redirected to this repository's index.
func BaseEnv() ([]string, error) {
	gitVars, err := gitx.LocalEnvVars()
	if err != nil {
		return nil, fmt.Errorf("listing git environment variables: %w", err)
	}
	drop := append(gitVars, MarkerEnv)
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		// Windows treats variable names case-insensitively.
		if !slices.ContainsFunc(drop, func(d string) bool { return strings.EqualFold(d, name) }) {
			env = append(env, kv)
		}
	}
	return env, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// Hold takes a named lock for who the way a task's lock is taken, so tasks
// run while it is held reuse it instead of waiting for it.
func (r *Runner) Hold(ctx context.Context, who string, l *config.TaskLock) (func() error, error) {
	return r.lock(ctx, who, l)
}

// lock takes the task's named lock. A lock this invocation already holds is
// reused: a task inside a task under the same lock would otherwise wait for
// itself. Upgrading read to write is refused for the same reason.
func (r *Runner) lock(ctx context.Context, task string, l *config.TaskLock) (func() error, error) {
	noop := func() error { return nil }
	if l == nil {
		return noop, nil
	}
	mode := lock.Shared
	if l.Mode == "write" {
		mode = lock.Exclusive
	}
	if held, ok := r.held[l.Name]; ok {
		if held == lock.Shared && mode == lock.Exclusive {
			return nil, fmt.Errorf("task %s needs the %s lock for writing, but it is held for reading by an outer task", task, l.Name)
		}
		return noop, nil
	}
	lk, err := lock.AcquireMode(ctx, filepath.Join(r.LockDir, l.Name+".lock"), mode, r.Config.Lock.Timeout, "graft "+task, r.Stderr)
	if err != nil {
		return nil, err
	}
	r.held[l.Name] = mode
	return func() error {
		delete(r.held, l.Name)
		return lk.Release()
	}, nil
}
