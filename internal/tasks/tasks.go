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
	"time"

	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/dotenv"
	"github.com/aiseeq/graft/internal/envs"
	"github.com/aiseeq/graft/internal/gitx"
	"github.com/aiseeq/graft/internal/lock"
	"github.com/aiseeq/graft/internal/stats"
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
	// Extra are variables every step gets, such as the message of the commit
	// the gate guards.
	Extra map[string]string
	// Stats receives the resource use of every process a step runs and of
	// every task; nil records nothing.
	Stats *stats.Recorder
	// Top names, in the stats, the command the tasks run under (commit,
	// gate, deploy); empty names the outermost task instead.
	Top string

	lookup  *dotenv.Lookup
	base    []string
	version *string
	dsn     *string
	done    map[string]bool
	held    map[string]lock.Mode
	// frames are the tasks running now, outermost first; every process a
	// step runs counts in all of them.
	frames []*frame
}

// frame is a running task as the stats see it.
type frame struct {
	task  string
	start time.Time
	steps int
	use   stats.Usage
	// exit is the first non-zero exit status of its processes.
	exit int
}

// VersionEnv carries the project version into every step.
const VersionEnv = "GRAFT_VERSION"

// MessageFileEnv names, in the gate of graft commit and graft amend, a file
// holding the message of the commit being made: the final text, work item key
// included.
const MessageFileEnv = "GRAFT_COMMIT_MESSAGE_FILE"

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
func (r *Runner) Gate(ctx context.Context) (err error) {
	if len(r.Config.Gate) == 0 {
		fmt.Fprintln(r.Stdout, "graft: gate is empty, nothing to run")
		return nil
	}
	r.Stdin = nil
	if err := r.preflight(r.Config.Gate, &config.Task{}, "gate"); err != nil {
		return err
	}
	f := r.begin("gate")
	defer func() { r.end(f, err != nil) }()
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
	if problems := r.problems(steps, owner, name); len(problems) > 0 {
		return fmt.Errorf("nothing was run, missing:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// DepsMissing is the preflight of tasks run as deps of something that is not
// a task (a deploy target): what they and their own deps need but do not find.
func (r *Runner) DepsMissing(deps []string) []string {
	return r.problems(nil, &config.Task{Deps: deps}, "deps")
}

// RunDeps runs tasks the way a task's deps run: in order, each at most once
// per runner, stopping at the first failure.
func (r *Runner) RunDeps(ctx context.Context, deps []string) error {
	for _, d := range deps {
		if err := r.run(ctx, d, nil); err != nil {
			return err
		}
	}
	return nil
}

// problems lists, for preflight, what the steps of a task and of everything
// it runs need but do not find, each prefixed with the task's name.
func (r *Runner) problems(steps []config.Step, owner *config.Task, name string) []string {
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
	return problems
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

func (r *Runner) run(ctx context.Context, name string, args []string) (err error) {
	if r.done[name] {
		return nil
	}
	f := r.begin(name)
	defer func() { r.end(f, err != nil) }()
	t := r.Config.Tasks[name]
	if err := r.RunDeps(ctx, t.Deps); err != nil {
		return err
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
	return r.measure(cmd, s.Source)
}

// Measure runs cmd as the one step of name, a unit of work that is not a
// task (the deploy script), and records it the way task steps are recorded.
func (r *Runner) Measure(name, source string, cmd *exec.Cmd) (err error) {
	f := r.begin(name)
	defer func() { r.end(f, err != nil) }()
	return r.measure(cmd, source)
}

// measure runs cmd and records what it cost against every running task.
// A command that never started records nothing: no process ran.
func (r *Runner) measure(cmd *exec.Cmd, source string) error {
	start := time.Now()
	err := cmd.Run()
	if cmd.ProcessState == nil || len(r.frames) == 0 {
		return err
	}
	use := stats.FromProcess(cmd.ProcessState, time.Since(start))
	code := cmd.ProcessState.ExitCode()
	for _, f := range r.frames {
		f.steps++
		f.use.Add(use)
		if f.exit == 0 {
			f.exit = code
		}
	}
	inner := r.frames[len(r.frames)-1]
	r.write(stats.Record{Kind: stats.KindStep, Task: inner.task, Step: stats.StepText(source), Exit: code}, use, r.frames[0].task)
	return err
}

// begin opens the stats frame of a task.
func (r *Runner) begin(task string) *frame {
	f := &frame{task: task, start: time.Now()}
	r.frames = append(r.frames, f)
	return f
}

// end closes the frame of a task and records the task as a whole; the
// task's error itself goes back to its caller.
func (r *Runner) end(f *frame, failed bool) {
	r.frames = r.frames[:len(r.frames)-1]
	rec := stats.Record{Kind: stats.KindTask, Task: f.task, Steps: f.steps}
	if failed {
		// A failure outside the processes (a missing key, a lock timeout)
		// has no exit status of its own.
		rec.Exit = f.exit
		if rec.Exit == 0 {
			rec.Exit = -1
		}
	}
	use := f.use
	use.Wall = time.Since(f.start)
	outermost := f.task // the outermost task's own frame is already closed
	if len(r.frames) > 0 {
		outermost = r.frames[0].task
	}
	r.write(rec, use, outermost)
}

// write records rec with its usage; outermost names the top when the runner
// has no Top of its own.
func (r *Runner) write(rec stats.Record, use stats.Usage, outermost string) {
	if r.Stats == nil {
		return
	}
	rec.Time = time.Now()
	rec.Project = r.Root
	rec.Top = r.Top
	if rec.Top == "" {
		rec.Top = outermost
	}
	use.Fill(&rec)
	r.Stats.Write(rec)
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
	for k, v := range r.Extra {
		vars[k] = v
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
	// Non-nil even when empty: the runner caches it and nil means not read yet.
	env := []string{}
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
