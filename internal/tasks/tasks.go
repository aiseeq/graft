// Package tasks runs the project's tasks and the commit gate: steps run as
// argv (or through sh when asked), with a controlled environment, named
// read/write locks, and each task at most once per invocation.
package tasks

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	"github.com/aiseeq/graft/v2/internal/tools"
	"github.com/aiseeq/graft/v2/internal/userconfig"
	"golang.org/x/term"
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
	// goEnvFlags is GOFLAGS of the go env file, once read: goEnvRead.
	goEnvFlags string
	goEnvRead  bool
	version    *string
	testDB     *TestDB
	done       map[string]bool
	held       map[string]lock.Mode
	// Load reads this machine's load settings for heavy tasks.
	Load func() (userconfig.MachineLoad, error)
	// Limits are the user's limits, which the outermost task's run is
	// held to when it ends.
	Limits func() (userconfig.Limits, error)
	// heavy counts the heavy tasks running, admitted by the outermost.
	heavy int
	// remoteDown is why remote_heavy's host did not answer, once asked:
	// remoteAsked; "" when it did.
	remoteAsked        bool
	remoteDown         string
	remoteHeavy        *config.RemoteHeavy
	userRemote         bool
	remoteBrokenReason string
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
	// reason is the first non-empty reason from a failed process step.
	reason string
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

// GoFmtSource is how the implicit gofmt step reads in the gate.
const GoFmtSource = "gofmt -l"

// goFixHint follows a failing go_fix step.
const goFixHint = "go fix has rewrites to make: run go fix ./... in the module, again until go fix -diff ./... is empty (one pass leaves rewrites that overlap), and commit them. " +
	"A new Go release brings new rewrites: after a Go upgrade this step fails in every project that turns it on, whatever the change"

const goFmtHint = "run gofmt -w <files> and commit them"

// gateStep is a step of the gate with the task that sets its directory,
// and what to do when it fails.
type gateStep struct {
	step config.Step
	task *config.Task
	hint string
	run  func(context.Context) error
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
	if !r.Config.GoVet.IsEnabled() && !r.Config.GoFix.Enabled && !r.Config.GoFmt.IsEnabled() {
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
	if r.Config.GoFmt.IsEnabled() && len(mods) > 0 {
		steps = append(steps, gateStep{
			step: config.Step{Source: GoFmtSource}, task: &config.Task{}, hint: goFmtHint,
			run: func(ctx context.Context) error { return r.checkGoFmt(ctx, mods) },
		})
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

// checkGoFmt is the gofmt -l step: it prints the Go files of modules that
// git lists and go/format would change (or cannot parse), and fails on any.
// A file belongs to its nearest module, so a module left out of modules
// (go.work, go_vet.exclude) is not checked through the root.
func (r *Runner) checkGoFmt(ctx context.Context, modules []string) error {
	repo, err := r.repo()
	if err != nil {
		return fmt.Errorf("listing Go files: %w", err)
	}
	out, err := repo.Git("ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "*.go", "go.mod", "*/go.mod")
	if err != nil {
		return fmt.Errorf("listing Go files and modules: %w", err)
	}
	selected := make(map[string]bool, len(modules))
	for _, mod := range modules {
		selected[mod] = true
	}
	moduleDirs, err := goFmtModuleDirs(r.Root, out, modules)
	if err != nil {
		return err
	}
	var bad []string
	for file := range strings.SplitSeq(out, "\x00") {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("checking Go formatting: %w", err)
		}
		if !strings.HasSuffix(file, ".go") {
			continue
		}
		dir := path.Dir(file)
		if vetSkipped(dir, r.Config.GoVet.Exclude) {
			continue
		}
		if !goFmtInModule(file, moduleDirs, selected) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(r.Root, filepath.FromSlash(file)))
		if errors.Is(err, os.ErrNotExist) {
			continue // in the index, deleted in the work tree
		}
		if err != nil {
			return fmt.Errorf("reading Go file %s: %w", file, err)
		}
		data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
		// format.Source takes a file without a package clause for a
		// fragment, which gofmt rejects.
		if _, err := parser.ParseFile(token.NewFileSet(), "", data, parser.PackageClauseOnly); err != nil {
			bad = append(bad, file+": "+err.Error())
			continue
		}
		formatted, err := format.Source(data)
		if err != nil {
			bad = append(bad, file+": "+err.Error())
		} else if !bytes.Equal(data, formatted) {
			bad = append(bad, file)
		}
	}
	if len(bad) > 0 {
		slices.Sort(bad)
		// A file in a merge conflict is listed once per stage.
		bad = slices.Compact(bad)
		fmt.Fprintln(r.Stdout, strings.Join(bad, "\n"))
		return errors.New("Go files need formatting")
	}
	return nil
}

// goFmtModuleDirs are the module directories of the git listing whose
// go.mod is in the work tree, and the selected ones.
func goFmtModuleDirs(root, listing string, selected []string) (map[string]bool, error) {
	dirs := make(map[string]bool, len(selected))
	for _, dir := range selected {
		dirs[dir] = true
	}
	for item := range strings.SplitSeq(listing, "\x00") {
		if dir, ok := strings.CutSuffix(item, "/go.mod"); ok {
			dirs[dir] = true
		} else if item == "go.mod" {
			dirs["."] = true
		}
	}
	for dir := range dirs {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir), "go.mod"))
		if errors.Is(err, os.ErrNotExist) {
			delete(dirs, dir)
		} else if err != nil {
			return nil, fmt.Errorf("checking module %s: %w", dir, err)
		}
	}
	return dirs, nil
}

// goFmtInModule reports whether the nearest module of file is selected.
func goFmtInModule(file string, dirs, selected map[string]bool) bool {
	for dir := path.Dir(file); ; dir = path.Dir(dir) {
		if dirs[dir] {
			return selected[dir]
		}
		if dir == "." {
			return false
		}
	}
}

// measureFunc runs a gate step done in graft's process and records it as
// measure records a command's step.
func (r *Runner) measureFunc(source string, run func() error) error {
	start := time.Now()
	err := run()
	r.recordStep(source, stats.Usage{Wall: time.Since(start)}, stepExit(err), "")
	return err
}

// stepExit is the exit code a step done in graft records: 1 on an error.
func stepExit(err error) int {
	if err == nil {
		return 0
	}
	return 1
}

// recordStep adds a step's usage and exit to the open frames and writes its
// stats record.
func (r *Runner) recordStep(source string, use stats.Usage, code int, reason string) {
	if len(r.frames) == 0 {
		return
	}
	if code == 0 {
		reason = ""
	}
	for _, f := range r.frames {
		f.steps++
		f.use.Add(use)
		if f.exit == 0 {
			f.exit = code
		}
		if code != 0 && f.reason == "" {
			f.reason = reason
		}
	}
	inner := r.frames[len(r.frames)-1]
	r.write(stats.Record{Kind: stats.KindStep, Task: inner.task, Step: stats.StepText(source), Exit: code, Reason: reason, Host: r.hostOf(inner)}, use, r.frames[0].task)
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
	for i := range steps {
		all[i] = steps[i].step
	}
	if err := r.preflight(all, &config.Task{}, "gate"); err != nil {
		return err
	}
	f := r.begin(stats.GateTask)
	defer func() { r.end(f, err != nil) }()
	// The gate builds and tests: it is heavy as a whole, its heavy tasks
	// admitted with it.
	leave, err := r.enterHeavy(ctx, stats.GateTask)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, leave()) }()
	for i := range steps {
		gs := &steps[i]
		s, threads, err := r.threads(gs.step, gs.task)
		if err != nil {
			return fmt.Errorf("gate: %w", err)
		}
		fmt.Fprintf(r.Stdout, "graft: gate %d/%d: %s\n", i+1, len(steps), s.Source)
		var stepErr error
		if gs.run != nil {
			stepErr = r.measureFunc(s.Source, func() error { return gs.run(ctx) })
		} else {
			stepErr = r.step(ctx, s, gs.task, nil, threads)
		}
		if stepErr != nil {
			if gs.hint != "" {
				return fmt.Errorf("gate failed at %q: %w\n%s", s.Source, stepErr, gs.hint)
			}
			return fmt.Errorf("gate failed at %q: %w", s.Source, stepErr)
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
		remote, err := r.placeHeavy(ctx, name, t, r.Root, args)
		if err != nil {
			return err
		}
		f.remote = remote
	}
	if t.Heavy && !f.remote {
		leave, herr := r.enterHeavy(ctx, name)
		if herr != nil {
			return herr
		}
		defer func() { err = errors.Join(err, leave()) }()
	}
	release, waited, err := r.lock(ctx, name, t.Lock)
	r.Waited(WhyLock, waited)
	if err != nil {
		return err
	}
	err = r.steps(ctx, name, t, args)
	if leave, fallback, herr := r.fallbackBrokenRemote(ctx, name); fallback {
		if herr != nil {
			return errors.Join(err, herr)
		}
		err = r.steps(ctx, name, t, args)
		err = errors.Join(err, leave())
	}
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
	if s.Install != nil {
		return r.install(ctx, s, t)
	}
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
	env = r.heavyEnv(env, t, threads)
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
		if argv, env, err = trimRun(ctx, r.taskDir(t), argv, env, r.frames[len(r.frames)-1].remote); err != nil {
			return err
		}
		cmd = exec.CommandContext(ctx, argv[0], argv[1:]...)
		words = argv
	}
	cmd.Dir = r.taskDir(t)
	cmd.Env = env
	if r.frames[len(r.frames)-1].remote {
		cmd = r.remoteCmd(ctx, cmd, vars, threads)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = r.Stdin, r.Stdout, r.Stderr
	var remoteStderr bytes.Buffer
	if r.frames[len(r.frames)-1].remote {
		cmd.Stderr = io.MultiWriter(r.Stderr, &remoteStderr)
	}
	switch t.Tests {
	case config.TestsGo:
		tree := r.treeID()
		report := testreport.NewGoTest(r.Stdout, verboseGoTest(words))
		cmd.Stdout = report
		use, err := r.measure(cmd, s.Source)
		r.noteBrokenRemote(cmd, err, remoteStderr.String())
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
		err := r.reportFileStep(cmd, s.Source, t.Tests, report)
		r.noteBrokenRemote(cmd, err, remoteStderr.String())
		return err
	}
	_, err = r.measure(cmd, s.Source)
	r.noteBrokenRemote(cmd, err, remoteStderr.String())
	return err
}

func (r *Runner) noteBrokenRemote(cmd *exec.Cmd, err error, stderr string) {
	if err == nil || cmd.ProcessState == nil || r.remoteHeavy == nil || !r.userRemote || len(r.frames) == 0 || !r.frames[len(r.frames)-1].remote {
		return
	}
	code := cmd.ProcessState.ExitCode()
	if code != r.remoteHeavy.BrokenExit {
		return
	}
	reason := fmt.Sprintf("run exited %d", code)
	if line := lastLine(stderr); line != "" {
		reason += ": " + line
	}
	r.remoteBrokenReason = reason
}

// fallbackBrokenRemote, after the running task or exec was sent to the
// user's host and run told the path broke, says so, journals it and takes
// the heavy queue here; fallback is false when nothing broke.
func (r *Runner) fallbackBrokenRemote(ctx context.Context, name string) (func() error, bool, error) {
	if len(r.frames) == 0 || !r.frames[len(r.frames)-1].remote || r.remoteBrokenReason == "" {
		return nil, false, nil
	}
	reason := r.remoteBrokenReason
	r.remoteBrokenReason = ""
	fmt.Fprintf(r.Stderr, "graft: %s: %s path broken: %s, running here\n", name, r.remoteHeavy.Host, reason)
	if err := r.journalRemoteBroken(name, reason); err != nil {
		return nil, true, err
	}
	r.frames[len(r.frames)-1].remote = false
	leave, err := r.enterHeavy(ctx, name)
	return leave, true, err
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[len(lines)-1])
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
	if heavy {
		t.Run = []config.Step{{Argv: slices.Clone(argv)}}
	}
	f := r.begin(ExecTask)
	defer func() { r.end(f, err != nil) }()
	if heavy {
		if f.remote, err = r.placeHeavy(ctx, ExecTask, t, dir, nil); err != nil {
			return err
		}
	}
	if heavy && !f.remote {
		leave, herr := r.enterHeavy(ctx, ExecTask)
		if herr != nil {
			return herr
		}
		defer func() { err = errors.Join(err, leave()) }()
	}
	source := strings.Join(argv, " ")
	var remoteStderr bytes.Buffer
	run := func() (*exec.Cmd, error) {
		s, threads, err := r.threads(config.Step{Argv: argv, Source: source}, t)
		if err != nil {
			return nil, err
		}
		env, err := r.env(t, nil)
		if err != nil {
			return nil, err
		}
		env = r.heavyEnv(env, t, threads)
		commandArgv, env, err := trimRun(ctx, dir, s.Argv, env, f.remote)
		if err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(ctx, commandArgv[0], commandArgv[1:]...)
		cmd.Dir, cmd.Env = dir, env
		if f.remote {
			cmd = r.remoteCmd(ctx, cmd, nil, threads)
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = r.Stdin, r.Stdout, r.Stderr
		if f.remote {
			cmd.Stderr = io.MultiWriter(r.Stderr, &remoteStderr)
		}
		return cmd, nil
	}
	cmd, err := run()
	if err != nil {
		return err
	}
	_, err = r.measure(cmd, source)
	r.noteBrokenRemote(cmd, err, remoteStderr.String())
	if leave, fallback, herr := r.fallbackBrokenRemote(ctx, ExecTask); fallback {
		if herr != nil {
			return errors.Join(err, herr)
		}
		localCmd, runErr := run()
		if runErr != nil {
			return errors.Join(err, runErr, leave())
		}
		_, err = r.measure(localCmd, source)
		return errors.Join(commandError(localCmd, err), leave())
	}
	switch {
	case err == nil:
		return nil
	case f.remote && cmd.Process == nil:
		// remote_heavy's run did not start: no command ran.
		return fmt.Errorf("remote_heavy run: %w", err)
	}
	return commandError(cmd, err)
}

func commandError(cmd *exec.Cmd, err error) error {
	if err == nil {
		return nil
	}
	return &CommandError{Started: cmd.Process != nil, Err: err}
}

// CommandError is how the command Exec runs failed, told apart from the
// failures of graft around it.
type CommandError struct {
	// Started is false when the command could not be started.
	Started bool
	Err     error
}

func (e *CommandError) Error() string { return e.Err.Error() }

func (e *CommandError) Unwrap() error { return e.Err }

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
	_, err := r.measureWithCapture(cmd, source, false)
	return err
}

// measure runs cmd and records what it cost against every running task.
// A command that never started records nothing: no process ran.
func (r *Runner) measure(cmd *exec.Cmd, source string) (stats.Usage, error) {
	return r.measureWithCapture(cmd, source, true)
}

// backgroundPipeWait bounds how long an inherited output pipe can keep Wait
// blocked after the step exits, while allowing ordinary child shutdown.
const backgroundPipeWait = 2 * time.Second

// measureWithCapture is measure; with capture the step's output also goes to
// a bounded tail, the reason of its stats line when it fails.
func (r *Runner) measureWithCapture(cmd *exec.Cmd, source string, capture bool) (stats.Usage, error) {
	var stdout, stderr *tailCapture
	autoWaitDelay := false
	if capture {
		stdout, stderr = &tailCapture{}, &tailCapture{}
		var outPiped, errPiped bool
		cmd.Stdout, outPiped = teeWriter(cmd.Stdout, stdout)
		cmd.Stderr, errPiped = teeWriter(cmd.Stderr, stderr)
		if (outPiped || errPiped) && cmd.WaitDelay == 0 {
			cmd.WaitDelay, autoWaitDelay = backgroundPipeWait, true
		}
	}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return stats.Usage{}, fmt.Errorf("start %s: %w", cmd.Path, err)
	}
	watch := stats.WatchProcess(cmd.Process.Pid)
	err := cmd.Wait()
	if autoWaitDelay && errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		err = nil
	}
	seen := watch.Stop()
	if len(r.frames) == 0 {
		return stats.Usage{}, err
	}
	use := stats.FromProcess(cmd.ProcessState, time.Since(start))
	if werr := use.Watched(seen); werr != nil && r.Stats != nil {
		r.Stats.Warn(werr)
	}
	code := cmd.ProcessState.ExitCode()
	var reason string
	if stderr != nil {
		reason = stderr.reason()
	}
	if reason == "" && stdout != nil {
		reason = stdout.reason()
	}
	r.recordStep(source, use, code, reason)
	return use, err
}

// teeWriter copies output to capture while preserving the original receiver;
// a terminal file stays direct so the child keeps its terminal behavior.
func teeWriter(dst, capture io.Writer) (io.Writer, bool) {
	if file, ok := dst.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		return dst, false
	}
	if dst == nil {
		return capture, true
	}
	return io.MultiWriter(dst, capture), true
}

// tailCapture stores only the latest output bytes, so verbose steps use
// bounded memory while their reason is collected.
type tailCapture struct {
	mu   sync.Mutex
	data []byte
}

func (c *tailCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = append(c.data, p...)
	if len(c.data) > 64<<10 {
		c.data = append([]byte(nil), c.data[len(c.data)-(64<<10):]...)
	}
	return len(p), nil
}

func (c *tailCapture) reason() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	text := string(c.data)
	lines := strings.Split(strings.TrimRight(text, "\r\n"), "\n")
	if len(lines) > 10 {
		lines = lines[len(lines)-10:]
	}
	return stats.LimitReason(strings.Join(lines, "\n"))
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
		rec.Reason = f.reason
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
		v := vars[k]
		if k == goFlagsEnv && t.Env[k] != "" {
			if v, err = r.goFlags(v); err != nil {
				return nil, err
			}
		}
		env = append(env, k+"="+v)
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

const goFlagsEnv = "GOFLAGS"

// goFlags is v, the GOFLAGS a task's env sets, after the user's: GOFLAGS in
// the environment replaces the go env file's, and the task's would replace
// both, dropping what the user set for every build (-p of a shared machine).
// A later flag wins in GOFLAGS, so the task's flags still override the
// user's, and a value with ${GOFLAGS}, which sees the environment only, just
// repeats some of them.
func (r *Runner) goFlags(v string) (string, error) {
	// An empty GOFLAGS, as go has it, is none: the go env file's holds.
	user, _ := lookupEnv(r.base, goFlagsEnv)
	if user == "" {
		if !r.goEnvRead {
			// The go env file's, read by the go of this machine:
			// GOTOOLCHAIN=local keeps a go.mod from switching it.
			cmd := exec.Command("go", "env", goFlagsEnv)
			cmd.Dir, cmd.Env = r.Root, append(slices.Clone(r.base), "GOTOOLCHAIN=local")
			out, err := cmd.Output()
			if err != nil && !errors.Is(err, exec.ErrNotFound) {
				return "", fmt.Errorf("go env GOFLAGS, the user's flags that env GOFLAGS keeps: %w", err)
			}
			r.goEnvFlags, r.goEnvRead = strings.TrimSpace(string(out)), true
		}
		user = r.goEnvFlags
	}
	if user = strings.TrimSpace(user); user == "" {
		return v, nil
	}
	return user + " " + v, nil
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

// enterHeavy admits a heavy task run on this machine, unless a running one
// was admitted and took it in, and counts it running; leave, when the task
// ends, gives its admission up.
func (r *Runner) enterHeavy(ctx context.Context, name string) (leave func() error, err error) {
	release := func() error { return nil }
	if r.heavy == 0 {
		waits, admitted, err := r.admit(ctx, name)
		r.waitedFor(waits)
		if err != nil {
			return nil, err
		}
		release = admitted
	}
	r.heavy++
	return func() error {
		r.heavy--
		return release()
	}, nil
}

// AdmittedEnv marks the steps of a heavy task this machine admitted: a graft
// they start was admitted with them and does not queue again.
const AdmittedEnv = "GRAFT_ADMITTED"

// admit waits until the machine takes a heavy task and returns how long it
// waited, by why, and the release the task calls when it ends.
func (r *Runner) admit(ctx context.Context, name string) (loadgate.Waits, func() error, error) {
	none := func() error { return nil }
	if os.Getenv(AdmittedEnv) != "" {
		return nil, none, nil
	}
	l, err := r.Load()
	if err != nil {
		return nil, none, err
	}
	lockPath, err := loadLock()
	if err != nil {
		return nil, none, err
	}
	var priority *loadgate.Priority
	p, ok, err := loadgate.PriorityFromEnv()
	if err != nil {
		return nil, none, err
	}
	if ok {
		priority = &p
	}
	return loadgate.Wait(ctx, name, priority, l, lockPath, r.Stderr)
}

// loadLock is the lock of this machine's heavy queue.
func loadLock() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locating the cache for the load queue: %w", err)
	}
	return filepath.Join(cache, "graft", "load.lock"), nil
}

// heavyEnv adds to the environment of a step what an admitted heavy task
// gives: GRAFT_THREADS and GOMAXPROCS of threads, one Go program not taking
// every thread of the machine (the go command runs as many compiles at once,
// each as many threads), unless the task's env or the environment sets
// GOMAXPROCS; and AdmittedEnv when the task runs on this machine.
func (r *Runner) heavyEnv(env []string, t *config.Task, threads string) []string {
	if threads != "" {
		if t.Heavy || r.heavy > 0 {
			env = append(env, ThreadsEnv+"="+threads)
		}
		if _, set := lookupEnv(env, "GOMAXPROCS"); !set {
			env = append(env, "GOMAXPROCS="+threads)
		}
	}
	if r.heavy > 0 && !r.frames[len(r.frames)-1].remote {
		env = append(env, AdmittedEnv+"=1")
	}
	return env
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
	if !uses && !t.Heavy && r.heavy == 0 {
		return s, "", nil
	}
	var n string
	if len(r.frames) > 0 && r.frames[len(r.frames)-1].remote {
		rh := r.remoteHeavy
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

// install expands source patterns, validates each binary, then installs it.
func (r *Runner) install(ctx context.Context, s config.Step, t *config.Task) error {
	patterns := strings.Fields(s.Install.Path)
	if len(patterns) == 0 {
		return errors.New("install: empty path")
	}
	var files []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(filepath.Join(r.taskDir(t), filepath.FromSlash(pattern)))
		if err != nil {
			return fmt.Errorf("install glob %q: %w", pattern, err)
		}
		if len(matches) == 0 {
			return fmt.Errorf("install glob %q matched no files", pattern)
		}
		files = append(files, matches...)
	}
	repo, err := r.repo()
	if err != nil {
		return fmt.Errorf("install: opening repository: %w", err)
	}
	head, err := repo.Git("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("install: reading HEAD: %w", err)
	}
	head = strings.TrimSpace(head)
	candidates := make([]installCandidate, 0, len(files))
	for _, file := range files {
		candidate, err := prepareInstall(ctx, repo, head, file, s.Install.To)
		if err != nil {
			return err
		}
		candidates = append(candidates, candidate)
	}
	var hosts []string
	switch cfg, _, err := userconfig.Load(); {
	case errors.Is(err, userconfig.ErrNotFound):
	case err != nil:
		return fmt.Errorf("reading install hosts: %w", err)
	default:
		hosts = cfg.Install.Hosts
	}
	for _, candidate := range candidates {
		if err := r.writeInstall(candidate); err != nil {
			return err
		}
		for _, host := range hosts {
			cmd := exec.CommandContext(ctx, "rsync", "--archive", "--mkpath", candidate.source, host+":"+candidate.destination)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("install %s to %s: rsync: %w: %s", candidate.name, host, err, strings.TrimSpace(string(out)))
			}
			fmt.Fprintf(r.Stderr, "graft: install: %s %s -> %s:%s\n", candidate.name, shortRevision(candidate.revision), host, candidate.destination)
		}
	}
	return nil
}

type installCandidate struct {
	name, revision, destination, source string
	data                                []byte
	unchanged                           bool
}

func prepareInstall(ctx context.Context, repo *gitx.Repo, head, file, to string) (installCandidate, error) {
	name := filepath.Base(file)
	info, err := os.Stat(file)
	if err != nil {
		return installCandidate{}, fmt.Errorf("install %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return installCandidate{}, fmt.Errorf("install %s: not a regular file", name)
	}
	build, buildErr := buildinfo.ReadFile(file)
	revision, hasRevision := installSetting(build, "vcs.revision")
	rev := head
	if buildErr == nil && hasRevision {
		if modified, ok := installSetting(build, "vcs.modified"); ok && modified == "true" {
			return installCandidate{}, fmt.Errorf("install %s: refusing uncommitted binary (revision %s)", name, shortRevision(revision))
		}
		if revision != head {
			return installCandidate{}, fmt.Errorf("install %s: binary revision %s does not match HEAD %s", name, shortRevision(revision), shortRevision(head))
		}
		rev = revision
	} else {
		if buildErr == nil && filepath.Clean(repo.GitDir) != filepath.Clean(repo.CommonDir) {
			worktrees, err := repo.Git("worktree", "list", "--porcelain")
			if err != nil {
				return installCandidate{}, fmt.Errorf("install %s: listing worktrees: %w", name, err)
			}
			mainWorktree := strings.TrimPrefix(strings.SplitN(worktrees, "\n", 2)[0], "worktree ")
			if mainWorktree == worktrees || mainWorktree == "" {
				return installCandidate{}, fmt.Errorf("install %s: unexpected git worktree list output: %q", name, worktrees)
			}
			return installCandidate{}, fmt.Errorf("install %s: refusing Go binary without vcs.revision from linked worktree; go build does not embed VCS metadata there, install from the main worktree (%s) after merging", name, filepath.FromSlash(mainWorktree))
		}
		clean, err := repo.IsClean()
		if err != nil {
			return installCandidate{}, fmt.Errorf("install %s: checking clean tree: %w", name, err)
		}
		if !clean {
			return installCandidate{}, fmt.Errorf("install %s: refusing binary without Go revision; working tree must be clean", name)
		}
	}
	destination, err := installDestination(ctx, to)
	if err != nil {
		return installCandidate{}, fmt.Errorf("install destination for %s: %w", name, err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return installCandidate{}, fmt.Errorf("install %s: reading file: %w", name, err)
	}
	dest := filepath.Join(destination, name)
	unchanged, err := sameFile(dest, data)
	if err != nil {
		return installCandidate{}, fmt.Errorf("install destination %s: %w", dest, err)
	}
	return installCandidate{name: name, revision: rev, destination: dest, source: file, data: data, unchanged: unchanged}, nil
}

// sameFile tells whether dest is a regular file holding data. A symlink is
// never the same: it may lead to the build output, which the next build
// changes behind the install, so the rename replaces the link with a file.
func sameFile(dest string, data []byte) (bool, error) {
	fi, err := os.Lstat(dest)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking the installed file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return false, nil
	}
	existing, err := os.ReadFile(dest)
	if err != nil {
		return false, fmt.Errorf("reading the installed file: %w", err)
	}
	return bytes.Equal(existing, data), nil
}

func (r *Runner) writeInstall(candidate installCandidate) error {
	if candidate.unchanged {
		fmt.Fprintf(r.Stderr, "graft: install: %s %s unchanged: %s\n", candidate.name, shortRevision(candidate.revision), candidate.destination)
		return nil
	}
	if err := installAtomic(candidate.destination, candidate.data); err != nil {
		return fmt.Errorf("install %s to %s: %w", candidate.name, candidate.destination, err)
	}
	fmt.Fprintf(r.Stderr, "graft: install: %s %s -> %s\n", candidate.name, shortRevision(candidate.revision), candidate.destination)
	return nil
}

func installSetting(info *debug.BuildInfo, key string) (string, bool) {
	if info == nil {
		return "", false
	}
	for _, setting := range info.Settings {
		if setting.Key == key {
			return setting.Value, true
		}
	}
	return "", false
}

func installDestination(ctx context.Context, to string) (string, error) {
	if to == "" {
		return tools.InstallDir(ctx)
	}
	if to == "~" || strings.HasPrefix(to, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("getting home directory: %w", err)
		}
		to = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(to, "~"), string(filepath.Separator)))
	}
	return to, nil
}

func shortRevision(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}

func installAtomic(dest string, data []byte) (err error) {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating destination directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".graft-install-*")
	if err != nil {
		return fmt.Errorf("creating temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if e := os.Remove(tmpName); e != nil && !errors.Is(e, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("removing temporary file: %w", e))
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return errors.Join(fmt.Errorf("writing temporary file: %w", err), tmp.Close())
	}
	if err = tmp.Chmod(0o755); err != nil {
		return errors.Join(fmt.Errorf("setting executable mode: %w", err), tmp.Close())
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("closing temporary file: %w", err)
	}
	if err = os.Rename(tmpName, dest); err == nil {
		return nil
	}
	if runtime.GOOS != "windows" {
		return fmt.Errorf("replacing destination: %w", err)
	}
	// Windows does not replace a running program, but renames it: the old
	// file moves aside under a name of its own and is removed when nothing
	// runs it any more, by this install or a later one.
	oldPrefix := "." + filepath.Base(dest) + ".old-"
	if removeErr := removeOld(dir, oldPrefix); removeErr != nil {
		return errors.Join(fmt.Errorf("replacing destination: %w", err), removeErr)
	}
	old := filepath.Join(dir, fmt.Sprintf("%s%d", oldPrefix, time.Now().UnixNano()))
	if moveErr := os.Rename(dest, old); moveErr != nil {
		return errors.Join(fmt.Errorf("replacing destination: %w", err), fmt.Errorf("moving current destination: %w", moveErr))
	}
	if moveErr := os.Rename(tmpName, dest); moveErr != nil {
		restoreErr := os.Rename(old, dest)
		return errors.Join(fmt.Errorf("replacing destination: %w", err), fmt.Errorf("installing new file: %w", moveErr), restoreErr)
	}
	if removeErr := removeOld(dir, oldPrefix); removeErr != nil {
		return fmt.Errorf("installed %s, but: %w", dest, removeErr)
	}
	return nil
}

// removeOld removes the replaced programs in dir named with prefix that
// nothing runs any more. Windows refuses to remove a running one with a
// permission error: that one stays for a later install to remove, and any
// other failure is returned.
func removeOld(dir, prefix string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("listing replaced programs in %s: %w", dir, err)
	}
	var errs []error
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		err := os.Remove(filepath.Join(dir, e.Name()))
		if err != nil && !errors.Is(err, fs.ErrPermission) && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("removing the replaced program: %w", err))
		}
	}
	return errors.Join(errs...)
}

// taskDir is the directory a task's steps run in.
func (r *Runner) taskDir(t *config.Task) string {
	if t.Dir != "" {
		return filepath.Join(r.Root, filepath.FromSlash(t.Dir))
	}
	return r.Root
}
