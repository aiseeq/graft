// Package tasks runs the project's tasks and the commit gate: steps run as
// argv (or through sh when asked), with a controlled environment, named
// read/write locks, and each task at most once per invocation.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/dotenv"
	"github.com/aiseeq/graft/v2/internal/envs"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/lintcache"
	"github.com/aiseeq/graft/v2/internal/loadavg"
	"github.com/aiseeq/graft/v2/internal/loadgate"
	"github.com/aiseeq/graft/v2/internal/lock"
	"github.com/aiseeq/graft/v2/internal/stats"
	"github.com/aiseeq/graft/v2/internal/testreport"
	"github.com/aiseeq/graft/v2/internal/userconfig"
)

// MarkerEnv is set for git commands graft itself runs, so the pre-commit hook
// lets them through. It is never passed on to steps: a test that runs git
// commit must not inherit the pass.
const MarkerEnv = "GRAFT_COMMIT"

// ArgsPlaceholder in a step receives the arguments given after --.
const ArgsPlaceholder = config.ArgsPlaceholder

// Runner runs tasks of one project.
type Runner struct {
	Root string
	// Repo is the repository at Root; nil opens it when a step needs it.
	Repo   *gitx.Repo
	Config *config.Config
	Stdout io.Writer
	Stderr io.Writer
	// Stdin is given to steps; nil closes it (the gate: nothing may wait for
	// input there).
	Stdin io.Reader
	// LockDir holds the named locks.
	LockDir string
	// TestDB returns the checked test database for test_db tasks.
	TestDB func(context.Context) (TestDB, error)
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
	// GraftVersion is this graft's version, part of every cache key.
	GraftVersion string
	// NoCache runs the tasks with cache as if they had none.
	NoCache bool

	lookup *dotenv.Lookup
	base   []string
	// lintCache is the work tree's golangci-lint cache, once looked for;
	// lintCacheErr says why there is none.
	lintCache    string
	lintCacheErr error
	version      *string
	testDB       *TestDB
	done         map[string]bool
	held         map[string]lock.Mode
	// Load reads this machine's load settings for heavy tasks.
	Load func() (userconfig.MachineLoad, error)
	// Limits are the user's limits, which the outermost task's run is
	// held to when it ends.
	Limits func() (userconfig.Limits, error)
	// heavy counts the heavy tasks running, admitted by the outermost.
	heavy int
	// remoteDown is why remote_heavy's host did not answer, once asked:
	// remoteAsked; "" when it did.
	remoteAsked bool
	remoteDown  string
	// frames are the tasks running now, outermost first; every process a
	// step runs counts in all of them.
	frames []*frame
}

// frame is a running task as the stats see it.
type frame struct {
	task string
	// in is the task or unit it runs in; "" for the outermost.
	in    string
	start time.Time
	steps int
	use   stats.Usage
	// exit is the first non-zero exit status of its processes.
	exit int
	// db follows the test database's container in a test_db task.
	db   *stats.CgroupWatch
	load *stats.LoadWatch
	// remote runs the steps of a heavy task on remote_heavy's host.
	remote bool
}

// TestDB is the test database of test_db tasks.
type TestDB struct {
	DSN string
	// Cgroup is the container's cgroup when graft runs the container and
	// can see it; CgroupErr tells why it could not be found, empty both
	// when there is nothing to follow (a DSN from the environment, docker
	// in a virtual machine).
	Cgroup    string
	CgroupErr error
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
		Load:   userconfig.LoadSettings,
		Limits: userconfig.LimitSettings,
	}
}

// GoVetSource is how the implicit go vet step of a Go project's gate reads in
// the output; a module below the root adds its directory in parentheses.
const GoVetSource = "go vet ./..."

// GoFixSource is how a go_fix step reads, as GoVetSource does.
const GoFixSource = "go fix -diff ./..."

// goFixHint follows a failing go_fix step.
const goFixHint = "go fix has rewrites to make: run go fix ./... in the module, again until go fix -diff ./... is empty (one pass leaves rewrites that overlap), and commit them. " +
	"A new Go release brings new rewrites: after a Go upgrade this step fails in every project that turns it on, whatever the change"

// gateStep is a step of the gate with the task that sets its directory,
// and what to do when it fails.
type gateStep struct {
	step config.Step
	task *config.Task
	hint string
}

// gateSteps is the gate: go vet ./... in every Go module of the tree first,
// unless go_vet is turned off, then go fix -diff ./... in each with go_fix,
// then the steps of the config. A module is the
// root when it has go.mod, and every directory below it with a go.mod that
// git lists (tracked, or not ignored); testdata, vendor and node_modules are
// skipped, and so are the directories of go_vet.exclude. The steps take
// go_vet.env, the project's build flags: vet with other flags is another
// build, which the build cache does not share.
func (r *Runner) gateSteps() ([]gateStep, error) {
	var steps []gateStep
	if !r.Config.GoVet.IsEnabled() && !r.Config.GoFix.Enabled {
		return r.configSteps(steps), nil
	}
	mods, err := r.goModules()
	if err != nil {
		return nil, err
	}
	inModule := func(src, dir string) string {
		if dir != "." {
			src += " (" + dir + ")"
		}
		return src
	}
	if r.Config.GoVet.IsEnabled() {
		for _, dir := range mods {
			steps = append(steps, gateStep{
				step: config.Step{Argv: []string{"go", "vet", "./..."}, Source: inModule(GoVetSource, dir)},
				task: &config.Task{Dir: dir, Env: r.Config.GoVet.Env},
			})
		}
	}
	if r.Config.GoFix.Enabled {
		// The build flags of go_vet.env: go fix analyzes the same build.
		for _, dir := range mods {
			steps = append(steps, gateStep{
				step: config.Step{Argv: []string{"go", "fix", "-diff", "./..."}, Source: inModule(GoFixSource, dir)},
				task: &config.Task{Dir: dir, Env: r.Config.GoVet.Env},
				hint: goFixHint,
			})
		}
	}
	return r.configSteps(steps), nil
}

// configSteps are steps with the gate steps of the config after them.
func (r *Runner) configSteps(steps []gateStep) []gateStep {
	for _, s := range r.Config.Gate {
		steps = append(steps, gateStep{step: s, task: &config.Task{}})
	}
	return steps
}

// goModules lists the directories to vet, relative to the root, "." first.
func (r *Runner) goModules() ([]string, error) {
	var dirs []string
	// Any other error than a missing file leaves vet in: it fails visibly.
	if _, err := os.Stat(filepath.Join(r.Root, "go.mod")); !errors.Is(err, os.ErrNotExist) && !vetSkipped(".", r.Config.GoVet.Exclude) {
		dirs = append(dirs, ".")
	}
	repo, err := r.repo()
	if err != nil {
		return nil, fmt.Errorf("listing Go modules: %w", err)
	}
	out, err := repo.Git("ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "*/go.mod")
	if err != nil {
		return nil, fmt.Errorf("listing Go modules: %w", err)
	}
	for f := range strings.SplitSeq(out, "\x00") {
		dir, ok := strings.CutSuffix(strings.TrimSpace(f), "/go.mod")
		if !ok || vetSkipped(dir, r.Config.GoVet.Exclude) {
			continue
		}
		_, err := os.Stat(filepath.Join(r.Root, filepath.FromSlash(dir), "go.mod"))
		if errors.Is(err, os.ErrNotExist) {
			continue // tracked, but deleted in the work tree
		}
		if err != nil {
			return nil, fmt.Errorf("module %s: %w", dir, err)
		}
		dirs = append(dirs, dir)
	}
	slices.Sort(dirs[min(len(dirs), 1):])
	return r.inWorkspace(dirs)
}

// inWorkspace keeps, when the root has go.work, the modules its use lists:
// go.work defines the workspace, and a go.mod outside it (a fenced-off copy
// of foreign code) does not build with it. Without go.work, all dirs stay.
func (r *Runner) inWorkspace(dirs []string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(r.Root, "go.work")); errors.Is(err, os.ErrNotExist) {
		return dirs, nil
	} else if err != nil {
		return nil, err
	}
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}}")
	cmd.Dir = r.Root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("listing the modules of go.work: %w", err)
	}
	used := map[string]bool{}
	// One directory per line: a path may hold spaces.
	for d := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		rel, err := filepath.Rel(r.Root, d)
		if err != nil {
			return nil, fmt.Errorf("go.work module %s is not under %s: %w", d, r.Root, err)
		}
		used[filepath.ToSlash(rel)] = true
	}
	return slices.DeleteFunc(dirs, func(d string) bool { return !used[d] }), nil
}

// vetSkipped tells whether the module directory is never vetted: a testdata,
// vendor or node_modules segment in the path, or a go_vet.exclude directory
// that is the module or contains it.
func vetSkipped(dir string, exclude []string) bool {
	for seg := range strings.SplitSeq(dir, "/") {
		if seg == "testdata" || seg == "vendor" || seg == "node_modules" {
			return true
		}
	}
	return slices.ContainsFunc(exclude, func(e string) bool {
		e = strings.Trim(path.Clean(e), "/")
		return dir == e || strings.HasPrefix(dir, e+"/")
	})
}

// Gate runs the commit gate with stdin closed.
func (r *Runner) Gate(ctx context.Context) (err error) {
	steps, err := r.gateSteps()
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		fmt.Fprintln(r.Stdout, "graft: gate is empty, nothing to run")
		return nil
	}
	r.Stdin = nil
	all := make([]config.Step, len(steps))
	for i, gs := range steps {
		all[i] = gs.step
	}
	if err := r.preflight(all, &config.Task{}, "gate"); err != nil {
		return err
	}
	f := r.begin(stats.GateTask)
	defer func() { r.end(f, err != nil) }()
	for i, gs := range steps {
		s, _, err := r.threads(gs.step, gs.task)
		if err != nil {
			return fmt.Errorf("gate: %w", err)
		}
		fmt.Fprintf(r.Stdout, "graft: gate %d/%d: %s\n", i+1, len(steps), s.Source)
		if err := r.step(ctx, s, gs.task, nil, ""); err != nil {
			if gs.hint != "" {
				return fmt.Errorf("gate failed at %q: %w\n%s", s.Source, err, gs.hint)
			}
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
	if reportFile(t) {
		provided[config.TestReportEnv] = true
	}
	c := NewKeyCheck(r.lookup, provided)
	c.Keys(t.Keys)
	var texts []string
	for _, k := range sortedKeys(t.Env) {
		texts = append(texts, t.Env[k])
	}
	for _, s := range steps {
		texts = append(texts, s.Argv...)
	}
	c.Refs(texts)
	return c.Problems()
}

func (r *Runner) run(ctx context.Context, name string, args []string) (err error) {
	if r.done[name] {
		return nil
	}
	t := r.Config.Tasks[name]
	var key string
	if t.Cache != nil && !r.NoCache {
		k, hit, cerr := r.cached(ctx, name, args)
		switch {
		case cerr != nil:
			fmt.Fprintf(r.Stderr, "graft: %s: not looked up in the cache, running: %v\n", name, cerr)
		case hit:
			r.done[name] = true
			return nil
		}
		key = k
	}
	f := r.begin(name)
	defer func() {
		use := r.end(f, err != nil)
		if err == nil && key != "" {
			r.keep(ctx, key, name, args, use)
		}
	}()
	if err := r.RunDeps(ctx, t.Deps); err != nil {
		return err
	}
	if t.Heavy {
		remote, err := r.placeHeavy(ctx, name, t, r.Root)
		if err != nil {
			return err
		}
		f.remote = remote
	}
	if t.Heavy && !f.remote {
		// A heavy task inside a heavy task was admitted with it.
		if r.heavy == 0 {
			waits, err := r.admit(ctx, name)
			r.waitedFor(waits)
			if err != nil {
				return err
			}
		}
		r.heavy++
		defer func() { r.heavy-- }()
	}
	release, waited, err := r.lock(ctx, name, t.Lock)
	r.Waited(WhyLock, waited)
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
		s, threads, err := r.threads(s, t)
		if err != nil {
			return fmt.Errorf("task %s: %w", name, err)
		}
		fmt.Fprintf(r.Stderr, "graft: %s: %s\n", name, s.Source)
		err = r.step(ctx, s, t, args, threads)
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

// step runs s, its {threads} already filled; threads is the thread count a
// heavy task passes as GRAFT_THREADS.
func (r *Runner) step(ctx context.Context, s config.Step, t *config.Task, args []string, threads string) error {
	if s.Task != "" {
		return r.run(ctx, s.Task, nil)
	}
	if s.ServiceAction != "" {
		if r.Service == nil {
			return errors.New("service steps are not available here")
		}
		return r.Service(ctx, s.ServiceAction, s.ServiceName)
	}
	var report string
	if reportFile(t) {
		dir, err := os.MkdirTemp("", "graft-tests-")
		if err != nil {
			return fmt.Errorf("a directory for the test report: %w", err)
		}
		defer os.RemoveAll(dir)
		report = filepath.Join(dir, "report.json")
	}
	vars, err := r.vars(ctx, t, report)
	if err != nil {
		return err
	}
	env, err := r.env(t, vars)
	if err != nil {
		return err
	}
	if t.Heavy {
		env = append(env, ThreadsEnv+"="+threads)
	}
	var cmd *exec.Cmd
	var words []string
	if s.Shell != "" {
		line, err := envs.ShellArgs(s.Shell, args, nil)
		if err != nil {
			return err
		}
		cmd = exec.CommandContext(ctx, "sh", "-c", line)
		words = strings.Fields(line)
	} else {
		argv, err := r.argv(s, t, args, vars)
		if err != nil {
			return err
		}
		argv = trimPaths(argv, env)
		cmd = exec.CommandContext(ctx, argv[0], argv[1:]...)
		words = argv
	}
	cmd.Dir = r.Root
	if t.Dir != "" {
		cmd.Dir = filepath.Join(r.Root, filepath.FromSlash(t.Dir))
	}
	cmd.Env = env
	if r.frames[len(r.frames)-1].remote {
		cmd = r.remoteCmd(ctx, cmd, vars, threads)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = r.Stdin, r.Stdout, r.Stderr
	switch t.Tests {
	case config.TestsGo:
		tree := r.treeID()
		report := testreport.NewGoTest(r.Stdout, verboseGoTest(words))
		cmd.Stdout = report
		use, err := r.measure(cmd, s.Source)
		runs, cerr := report.Close()
		if cerr != nil {
			fmt.Fprintf(r.Stderr, "graft: printing the test output: %v\n", cerr)
		}
		if report.Events() == 0 && strings.Contains(s.Source, "go test") {
			fmt.Fprintf(r.Stderr, "graft: %s: the task has tests: go, but go test printed no -json events; pass -json to it\n", r.frames[len(r.frames)-1].task)
		}
		r.recordTests(runs, tree, t.Tests, use)
		return err
	case config.TestsJest, config.TestsPlaywright:
		return r.reportFileStep(cmd, s.Source, t.Tests, report)
	}
	_, err = r.measure(cmd, s.Source)
	return err
}

// reportFile tells whether the task's steps write a test report to the file
// in GRAFT_TEST_REPORT.
func reportFile(t *config.Task) bool {
	return t.Tests == config.TestsJest || t.Tests == config.TestsPlaywright
}

// reportFileStep runs a step whose test runner writes its JSON report to
// path, the file in GRAFT_TEST_REPORT, then records the tests in it.
func (r *Runner) reportFileStep(cmd *exec.Cmd, source, runner, path string) error {
	tree := r.treeID()
	use, err := r.measure(cmd, source)
	data, rerr := os.ReadFile(path)
	switch {
	case errors.Is(rerr, os.ErrNotExist):
		if strings.Contains(source, runner) {
			fmt.Fprintf(r.Stderr, "graft: %s: the task has tests: %s, but the step wrote no report to %s; see README, Tasks\n", r.frames[len(r.frames)-1].task, runner, config.TestReportEnv)
		}
		return err
	case rerr != nil:
		fmt.Fprintf(r.Stderr, "graft: reading the %s report: %v\n", runner, rerr)
		return err
	}
	var runs []stats.PackageRun
	if runner == config.TestsJest {
		runs, rerr = testreport.ParseJest(data, cmd.Dir)
	} else {
		runs, rerr = testreport.ParsePlaywright(data)
	}
	if rerr != nil {
		fmt.Fprintf(r.Stderr, "graft: the %s report is not recorded: %v\n", runner, rerr)
		return err
	}
	r.recordTests(runs, tree, runner, use)
	return err
}

// verboseGoTest tells whether the command asks go test for -v: the report
// then prints every line, as go test -v would.
func verboseGoTest(words []string) bool {
	return slices.ContainsFunc(words, func(w string) bool {
		flag, ok := strings.CutPrefix(w, "-")
		flag = strings.TrimPrefix(flag, "-")
		return ok && (flag == "v" || flag == "v=true" || flag == "test.v" || flag == "test.v=true")
	})
}

// treeID identifies the work tree a tests step runs on; "" when the stats
// are off or the tree cannot be read, which the flaky report then skips.
func (r *Runner) treeID() string {
	if r.Stats == nil {
		return ""
	}
	repo, err := r.repo()
	if err == nil {
		var id string
		if id, err = repo.WorkTreeID(); err == nil {
			return id
		}
	}
	r.Stats.Warn(fmt.Errorf("identifying the work tree of the tests: %w", err))
	return ""
}

// recordTests writes the package runs of a tests step to the tests file.
func (r *Runner) recordTests(runs []stats.PackageRun, tree, runner string, use stats.Usage) {
	if r.Stats == nil || len(r.frames) == 0 {
		return
	}
	for i := range runs {
		run := &runs[i]
		run.Project, run.Task, run.Top = r.Root, r.frames[len(r.frames)-1].task, r.top(r.frames[0].task)
		run.Tree, run.Runner = tree, runner
		run.LoadStart, run.LoadMax = use.Loads()
		r.Stats.WriteTests(*run)
	}
}

// top is what the tasks run under: the runner's Top, or the outermost task.
func (r *Runner) top(outermost string) string {
	if r.Top != "" {
		return r.Top
	}
	return outermost
}

// ExecTask names in the stats a command run by graft exec.
const ExecTask = "exec"

// Exec runs argv in dir, a command that is not a task, recorded as the task
// exec. A heavy one queues for the machine as a heavy task does and gets
// {threads} filled and GRAFT_THREADS.
func (r *Runner) Exec(ctx context.Context, dir string, argv []string, heavy bool) (err error) {
	t := &config.Task{Heavy: heavy}
	f := r.begin(ExecTask)
	defer func() { r.end(f, err != nil) }()
	if heavy {
		if f.remote, err = r.placeHeavy(ctx, ExecTask, t, dir); err != nil {
			return err
		}
	}
	if heavy && !f.remote {
		waits, err := r.admit(ctx, ExecTask)
		r.waitedFor(waits)
		if err != nil {
			return err
		}
	}
	s, threads, err := r.threads(config.Step{Argv: argv, Source: strings.Join(argv, " ")}, t)
	if err != nil {
		return err
	}
	env, err := r.env(t, nil)
	if err != nil {
		return err
	}
	if heavy {
		env = append(env, ThreadsEnv+"="+threads)
	}
	argv = trimPaths(s.Argv, env)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = dir, env
	if f.remote {
		cmd = r.remoteCmd(ctx, cmd, nil, threads)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = r.Stdin, r.Stdout, r.Stderr
	_, err = r.measure(cmd, s.Source)
	return err
}

// Unit runs fn as name, a unit of work that is not a task (a deploy), and
// records it the way a task is recorded: the tasks fn runs are recorded as
// run in it, and Measure records its steps.
func (r *Runner) Unit(name string, fn func() error) (err error) {
	f := r.begin(name)
	defer func() { r.end(f, err != nil) }()
	return fn()
}

// Measure runs cmd as a step of the unit or task running, and records it
// the way task steps are recorded.
func (r *Runner) Measure(source string, cmd *exec.Cmd) error {
	_, err := r.measure(cmd, source)
	return err
}

// measure runs cmd and records what it cost against every running task.
// A command that never started records nothing: no process ran.
func (r *Runner) measure(cmd *exec.Cmd, source string) (stats.Usage, error) {
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return stats.Usage{}, fmt.Errorf("start %s: %w", cmd.Path, err)
	}
	watch := stats.WatchProcess(cmd.Process.Pid)
	err := cmd.Wait()
	seen := watch.Stop()
	if len(r.frames) == 0 {
		return stats.Usage{}, err
	}
	use := stats.FromProcess(cmd.ProcessState, time.Since(start))
	if werr := use.Watched(seen); werr != nil && r.Stats != nil {
		r.Stats.Warn(werr)
	}
	code := cmd.ProcessState.ExitCode()
	for _, f := range r.frames {
		f.steps++
		f.use.Add(use)
		if f.exit == 0 {
			f.exit = code
		}
	}
	inner := r.frames[len(r.frames)-1]
	r.write(stats.Record{Kind: stats.KindStep, Task: inner.task, Step: stats.StepText(source), Exit: code, Host: r.hostOf(inner)}, use, r.frames[0].task)
	return use, err
}

// begin opens the stats frame of a task.
func (r *Runner) begin(task string) *frame {
	f := &frame{task: task, start: time.Now()}
	if len(r.frames) > 0 {
		f.in = r.frames[len(r.frames)-1].task
	}
	r.frames = append(r.frames, f)
	if r.Stats == nil {
		return f
	}
	f.load = stats.WatchLoad()
	load, err := loadavg.Read()
	switch {
	case err == nil:
		f.use.LoadStart, f.use.LoadMax, f.use.LoadKnown = load, load, true
	case !stats.Unsupported(err):
		r.Stats.Warn(fmt.Errorf("reading the load average: %w", err))
	}
	return f
}

// end closes the frame of a task and records the task as a whole, returning
// what it cost; the task's error itself goes back to its caller.
func (r *Runner) end(f *frame, failed bool) stats.Usage {
	r.frames = r.frames[:len(r.frames)-1]
	rec := stats.Record{Kind: stats.KindTask, Task: f.task, In: f.in, Steps: f.steps, Host: r.hostOf(f)}
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
	if f.load != nil {
		if err := use.Watched(f.load.Stop()); err != nil {
			r.Stats.Warn(err)
		}
	}
	if f.db != nil {
		if use.DB = f.db.Stop(); use.DB.Err == nil {
			use.DBKnown = true
		} else if r.Stats != nil {
			r.Stats.Warn(fmt.Errorf("reading the test database's container: %w", use.DB.Err))
		}
	}
	outermost := f.task // the outermost task's own frame is already closed
	if len(r.frames) > 0 {
		outermost = r.frames[0].task
	}
	r.write(rec, use, outermost)
	if len(r.frames) == 0 {
		r.overLimit(f.task, use)
	}
	return use
}

// overLimit says when the outermost task took longer than its user config
// limit without its waits, the line graft stats marks a slow task with.
func (r *Runner) overLimit(task string, use stats.Usage) {
	if r.Limits == nil {
		return
	}
	limits, err := r.Limits()
	if err != nil {
		fmt.Fprintf(r.Stderr, "graft: %v\n", err)
		return
	}
	limit, key := stats.LimitOf(task, limits.Gate, limits.Task)
	if own := use.Wall - use.Wait; own > limit {
		fmt.Fprintf(r.Stderr, "graft: %s\n", stats.FormatRunOverLimit(task, own, limit, key))
	}
}

// write records rec with its usage; outermost names the top when the runner
// has no Top of its own.
func (r *Runner) write(rec stats.Record, use stats.Usage, outermost string) {
	if r.Stats == nil {
		return
	}
	rec.Time = time.Now()
	rec.Project = r.Root
	rec.Top = r.top(outermost)
	use.Fill(&rec)
	r.Stats.Write(rec)
}

// vars are the variables graft sets for a task's steps: its env values, the
// project version and the test database DSN. ${KEY} in argv sees them too.
func (r *Runner) vars(ctx context.Context, t *config.Task, report string) (map[string]string, error) {
	vars := map[string]string{}
	lookup := r.lookup
	if report != "" {
		lookup = lookup.With(map[string]string{config.TestReportEnv: report})
	}
	for _, k := range sortedKeys(t.Env) {
		v, err := dotenv.Expand(t.Env[k], lookup)
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
	maps.Copy(vars, r.Extra)
	if report != "" {
		vars[config.TestReportEnv] = report
	}
	if t.TestDB {
		if r.TestDB == nil {
			return nil, errors.New("test_db is set but no test database is configured")
		}
		if r.testDB == nil {
			db, err := r.TestDB(ctx)
			if err != nil {
				return nil, err
			}
			r.testDB = &db
		}
		vars[r.Config.TestDB.DSNVar] = r.testDB.DSN
		r.watchDB()
	}
	return vars, nil
}

// watchDB starts following the test database's container for the running
// task, once, from its first step on.
func (r *Runner) watchDB() {
	if r.Stats == nil || len(r.frames) == 0 {
		return
	}
	f := r.frames[len(r.frames)-1]
	switch {
	case f.db != nil:
	case r.testDB.CgroupErr != nil:
		r.Stats.Warn(fmt.Errorf("the test database's container: %w", r.testDB.CgroupErr))
		r.testDB.CgroupErr = nil // one warning
	case r.testDB.Cgroup != "":
		f.db = stats.WatchCgroup(r.testDB.Cgroup)
	}
}

// env is the base environment (without graft's marker and git's hook
// variables) plus the task's .env keys and vars, and the work tree's own
// golangci-lint cache unless one is set (lintcache).
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
	if v, set := lookupEnv(env, lintcache.Env); set {
		owned, err := lintcache.Owned(v)
		if err != nil {
			return nil, fmt.Errorf("%s=%s: %w", lintcache.Env, v, err)
		}
		if !owned {
			return env, nil
		}
	}
	env = gitx.Without(env, []string{lintcache.Env})
	if r.lintCache == "" && r.lintCacheErr == nil {
		r.lintCache, r.lintCacheErr = lintcache.Dir(r.Root)
		if r.lintCacheErr != nil {
			// Once: a failover, which tasks without golangci-lint need not stop on.
			fmt.Fprintf(r.Stderr, "graft: no golangci-lint cache of this work tree, steps share golangci-lint's own: %v\n", r.lintCacheErr)
		}
	}
	if r.lintCacheErr != nil {
		return env, nil
	}
	return append(env, lintcache.Env+"="+r.lintCache), nil
}

// lookupEnv is the last value of the variable in env, which is the one a
// process sees.
func lookupEnv(env []string, name string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		k, v, _ := strings.Cut(env[i], "=")
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}

// KeyCheck collects what .env lacks for a command, each key reported once.
type KeyCheck struct {
	lookup   *dotenv.Lookup
	seen     map[string]bool
	problems []string
}

// NewKeyCheck starts a check; provided holds the keys that need no .env value.
func NewKeyCheck(l *dotenv.Lookup, provided map[string]bool) *KeyCheck {
	seen := map[string]bool{}
	maps.Copy(seen, provided)
	return &KeyCheck{lookup: l, seen: seen}
}

func (c *KeyCheck) require(key string) {
	if c.seen[key] {
		return
	}
	c.seen[key] = true
	if _, err := c.lookup.Value(key); err != nil {
		c.problems = append(c.problems, err.Error())
	}
}

// Keys checks the declared keys: a required one is set, a "KEY!" one set and
// not empty; an optional one may be missing.
func (c *KeyCheck) Keys(keys []config.DotEnvKey) {
	for _, k := range keys {
		switch {
		case k.NonEmpty:
			c.seen[k.Name] = true
			if v, err := c.lookup.Value(k.Name); err != nil {
				c.problems = append(c.problems, err.Error())
			} else if v == "" {
				c.problems = append(c.problems, k.Name+" is empty")
			}
		case !k.Optional:
			c.require(k.Name)
		}
	}
}

// Refs checks that every ${KEY} in texts is set.
func (c *KeyCheck) Refs(texts []string) {
	for _, text := range texts {
		refs, err := dotenv.Refs(text)
		if err != nil {
			c.problems = append(c.problems, err.Error())
			continue
		}
		for _, key := range refs {
			c.require(key)
		}
	}
}

// Problems lists what is missing, in the order found.
func (c *KeyCheck) Problems() []string { return c.problems }

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
	// Non-nil even when empty: the runner caches it and nil means not read yet.
	return append([]string{}, gitx.Without(os.Environ(), append(gitVars, MarkerEnv))...), nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// admit waits until the machine takes a heavy task and returns how long it
// waited, by why.
func (r *Runner) admit(ctx context.Context, name string) (loadgate.Waits, error) {
	l, err := r.Load()
	if err != nil {
		return nil, err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("locating the cache for the load queue: %w", err)
	}
	var priority *loadgate.Priority
	p, ok, err := loadgate.PriorityFromEnv()
	if err != nil {
		return nil, err
	}
	if ok {
		priority = &p
	}
	return loadgate.Wait(ctx, name, priority, l, filepath.Join(cache, "graft", "load.lock"), r.Stderr)
}

// Why a task waited besides the machine, whose waits are loadgate's Why
// constants.
const (
	// WhyLock is the wait for a task lock another graft holds.
	WhyLock = "lock"
	// WhySudo is the wait for a deploy's sudo confirmation.
	WhySudo = "sudo"
)

// Waited adds a wait before steps (the load, a lock, a confirmation) to
// every running task: the innermost's own and those it runs under, whose
// wall time holds it too. why says what it waited for.
func (r *Runner) Waited(why string, d time.Duration) {
	if d <= 0 {
		return
	}
	for _, f := range r.frames {
		f.use.Wait += d
		if f.use.WaitWhy == nil {
			f.use.WaitWhy = map[string]time.Duration{}
		}
		f.use.WaitWhy[why] += d
	}
}

// waitedFor adds the waits of a heavy task's admission.
func (r *Runner) waitedFor(w loadgate.Waits) {
	for why, d := range w {
		r.Waited(why, d)
	}
}

// ThreadsEnv carries the user config's load.threads into a heavy task.
const ThreadsEnv = "GRAFT_THREADS"

// threads fills {threads} in the step, its echoed source included, and
// returns the thread count for a heavy task's GRAFT_THREADS: "" when the step
// does not use it and the task is not heavy.
func (r *Runner) threads(s config.Step, t *config.Task) (config.Step, string, error) {
	uses := strings.Contains(s.Shell, config.ThreadsPlaceholder) ||
		slices.ContainsFunc(s.Argv, func(w string) bool { return strings.Contains(w, config.ThreadsPlaceholder) })
	if !uses && !t.Heavy {
		return s, "", nil
	}
	var n string
	if len(r.frames) > 0 && r.frames[len(r.frames)-1].remote {
		rh := r.Config.RemoteHeavy
		if rh.Threads == 0 {
			if uses {
				return s, "", fmt.Errorf("%s in a step run on %s: set remote_heavy.threads", config.ThreadsPlaceholder, rh.Host)
			}
			return s, "", nil
		}
		n = strconv.Itoa(rh.Threads)
	} else {
		l, err := r.Load()
		if err != nil {
			return s, "", err
		}
		n = strconv.Itoa(l.Threads)
	}
	s.Shell = strings.ReplaceAll(s.Shell, config.ThreadsPlaceholder, n)
	s.Source = strings.ReplaceAll(s.Source, config.ThreadsPlaceholder, n)
	s.Argv = slices.Clone(s.Argv)
	for i, w := range s.Argv {
		s.Argv[i] = strings.ReplaceAll(w, config.ThreadsPlaceholder, n)
	}
	return s, n, nil
}

// Hold takes a named lock for who the way a task's lock is taken, so tasks
// run while it is held reuse it instead of waiting for it.
func (r *Runner) Hold(ctx context.Context, who string, l *config.TaskLock) (func() error, error) {
	release, _, err := r.lock(ctx, who, l)
	return release, err
}

// lock takes the task's named lock. A lock this invocation already holds is
// reused: a task inside a task under the same lock would otherwise wait for
// itself. Upgrading read to write is refused for the same reason.
func (r *Runner) lock(ctx context.Context, task string, l *config.TaskLock) (release func() error, waited time.Duration, err error) {
	noop := func() error { return nil }
	if l == nil {
		return noop, 0, nil
	}
	mode := lock.Shared
	if l.Mode == "write" {
		mode = lock.Exclusive
	}
	if held, ok := r.held[l.Name]; ok {
		if held == lock.Shared && mode == lock.Exclusive {
			return nil, 0, fmt.Errorf("task %s needs the %s lock for writing, but it is held for reading by an outer task", task, l.Name)
		}
		return noop, 0, nil
	}
	lk, err := lock.AcquireMode(ctx, filepath.Join(r.LockDir, l.Name+".lock"), mode, r.Config.Lock.Timeout, "graft "+task, r.Stderr)
	if err != nil {
		return nil, 0, err
	}
	r.held[l.Name] = mode
	return func() error {
		delete(r.held, l.Name)
		return lk.Release()
	}, lk.Waited, nil
}

// argv is the command of a step that is not a shell line, the arguments in
// it; in a tests: go step packages among them replace the step's own.
func (r *Runner) argv(s config.Step, t *config.Task, args []string, vars map[string]string) ([]string, error) {
	own := s.Argv
	if t.Tests == config.TestsGo {
		var dropped []string
		if own, dropped = ownPackages(s.Argv, args); len(dropped) > 0 {
			fmt.Fprintf(r.Stderr, "graft: the packages after -- replace %s of the step\n", strings.Join(dropped, " "))
		}
	}
	return envs.ExpandArgv(own, args, r.lookup.With(vars))
}

// repo is the repository at Root, opened once.
func (r *Runner) repo() (*gitx.Repo, error) {
	if r.Repo == nil {
		repo, err := gitx.Open(r.Root)
		if err != nil {
			return nil, err
		}
		r.Repo = repo
	}
	return r.Repo, nil
}
