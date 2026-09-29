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
const ArgsPlaceholder = "{args}"

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

	lookup *dotenv.Lookup
	base   []string
	done   map[string]bool
	held   map[string]lock.Mode
}

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
	if len(args) > 0 && !takesArgs(t) {
		return fmt.Errorf("task %s takes no arguments (no %s in its steps)", name, ArgsPlaceholder)
	}
	return r.run(ctx, name, args)
}

func takesArgs(t *config.Task) bool {
	for _, s := range t.Run {
		if slices.Contains(s.Argv, ArgsPlaceholder) || strings.Contains(s.Shell, ArgsPlaceholder) {
			return true
		}
	}
	return false
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
	env, err := r.env(ctx, t)
	if err != nil {
		return err
	}
	var cmd *exec.Cmd
	if s.Shell != "" {
		line := strings.ReplaceAll(s.Shell, ArgsPlaceholder, envs.QuoteArgs(args))
		cmd = exec.CommandContext(ctx, "sh", "-c", line)
	} else {
		argv, err := dotenv.ExpandAll(expandArgs(s.Argv, args), r.lookup)
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

func expandArgs(argv, args []string) []string {
	out := make([]string, 0, len(argv)+len(args))
	for _, a := range argv {
		if a == ArgsPlaceholder {
			out = append(out, args...)
			continue
		}
		out = append(out, a)
	}
	return out
}

// env is the base environment (without graft's marker and git's hook
// variables) plus the task's .env keys, variables and test database DSN.
func (r *Runner) env(ctx context.Context, t *config.Task) ([]string, error) {
	if r.base == nil {
		base, err := BaseEnv()
		if err != nil {
			return nil, err
		}
		r.base = base
	}
	env := slices.Clone(r.base)
	keys, err := r.lookup.Values(t.DotEnv)
	if err != nil {
		return nil, err
	}
	env = append(env, keys...)
	for _, k := range sortedKeys(t.Env) {
		v, err := dotenv.Expand(t.Env[k], r.lookup)
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", k, err)
		}
		env = append(env, k+"="+v)
	}
	if t.TestDB {
		if r.TestDSN == nil {
			return nil, errors.New("test_db is set but no test database is configured")
		}
		dsn, err := r.TestDSN(ctx)
		if err != nil {
			return nil, err
		}
		env = append(env, r.Config.TestDB.DSNVar+"="+dsn)
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
