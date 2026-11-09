package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/dotenv"
	"github.com/aiseeq/graft/v2/internal/loadgate"
	"github.com/aiseeq/graft/v2/internal/proc"
	"github.com/aiseeq/graft/v2/internal/userconfig"
)

// HeavyEnv set to "local" keeps heavy work on this machine though the
// project has remote_heavy, for the graft that reads it and those it starts;
// graft exec --heavy --local sets it.
const HeavyEnv = "GRAFT_HEAVY"

// remoteUpTimeout bounds remote_heavy's up: a host that takes longer to say
// it is there is not one to wait for.
const remoteUpTimeout = 30 * time.Second

// placeHeavy says whether a heavy task, or graft exec --heavy run in dir,
// runs its steps on remote_heavy's host, and says where it runs when the
// project has one; args are the arguments given to the task. here is why
// it stays on this machine when there is a host, for the stats.
func (r *Runner) placeHeavy(ctx context.Context, name string, t *config.Task, dir string, args []string) (remote bool, here string, err error) {
	rh := r.Config.RemoteHeavy
	if rh == nil {
		rh, err = userconfig.RemoteHeavySettings()
		if err != nil {
			return false, "", fmt.Errorf("reading user remote_heavy: %w", err)
		}
		r.userRemote = rh != nil
	}
	if rh == nil {
		return false, "", nil
	}
	r.remoteHeavy = rh
	stay := func(why string) (bool, string, error) {
		fmt.Fprintf(r.Stderr, "graft: %s: heavy, on this machine: %s\n", name, why)
		return false, why, nil
	}
	why, err := r.stayHere(t, dir)
	if err != nil {
		return false, "", err
	}
	if why != "" {
		return stay(why)
	}
	if r.userRemote && t.Remote == nil && !r.userRemoteEligible(t, args) {
		if argv0 := r.glintMismatch(t, args); argv0 != "" {
			return stay(r.glintStayReason(argv0))
		}
		return stay("only go test and go vet steps that write no file can use user remote_heavy")
	}
	if rh.When == "busy" {
		if os.Getenv(AdmittedEnv) != "" || r.heavy > 0 {
			// Not said: such a task never waited here either.
			return false, WhyInsideHeavy, nil
		}
		busy, err := r.wouldWait(ctx, name)
		if err != nil {
			return false, "", err
		}
		if !busy {
			return false, WhyNotBusy, nil
		}
	}
	if !r.remoteAsked {
		r.remoteAsked = true
		r.remoteDown = r.remoteUp(ctx)
	}
	if r.remoteDown != "" {
		if r.userRemote {
			fmt.Fprintf(r.Stderr, "graft: %s: %s path broken: %s, running here\n", name, rh.Host, r.remoteDown)
			if err := r.journalRemoteBroken(name, r.remoteDown); err != nil {
				return false, "", err
			}
			return false, rh.Host + " path broken: " + r.remoteDown, nil
		}
		return stay(rh.Host + " did not answer (" + r.remoteDown + ")")
	}
	if name == ExecTask {
		// The command line is the caller's: say what of its environment does
		// not travel, as a test that reads TMPDIR or a flag would differ there.
		fmt.Fprintf(r.Stderr, "graft: %s: heavy, on %s (the caller's variables, TMPDIR too, stay here: --env NAME passes one, --local runs here)\n", name, rh.Host)
	} else {
		fmt.Fprintf(r.Stderr, "graft: %s: heavy, on %s\n", name, rh.Host)
	}
	return true, "", nil
}

// Reasons a heavy task with a host stays on this machine without saying so:
// under when: busy, it would not wait here.
const (
	WhyInsideHeavy = "inside an admitted heavy task, which waits for the machine"
	WhyNotBusy     = "this machine would not make it wait"
	WhyGateSplit   = "in a gate split by step, each step placed for itself"
	// A busy gate stays here whole when none of its steps may go to the host,
	// or each that may is cheaper here.
	WhyGateNoStepForHost = "no step of the gate may go to the host"
	WhyGateCheaperHere   = "each step of the gate that may go to the host is cheaper here"
	// A gate is split by step only for the user's remote_heavy with when:
	// busy; otherwise its heavy tasks are placed one by one.
	WhyGateProjectHost = "the project's remote_heavy places the gate's heavy tasks, not its steps"
	WhyGateAlways      = "when: always places the gate's heavy tasks, not its steps"
)

// wouldWait tells whether this machine's queue would make the heavy task wait
// now, as admit and enterHeavy decide for a task outside an admitted one
// (placeHeavy keeps those here): one with a priority waits for the machine
// only, not for the queue.
func (r *Runner) wouldWait(ctx context.Context, name string) (bool, error) {
	l, err := r.Load()
	if err != nil {
		return false, err
	}
	_, priority, err := loadgate.PriorityFromEnv()
	if err != nil {
		return false, err
	}
	lockPath, err := loadLock()
	if err != nil {
		return false, err
	}
	if priority {
		return loadgate.BusyNowWithReserves(ctx, name, r.kind(name), l, lockPath, false, r.Stderr)
	}
	return loadgate.BusyNow(ctx, name, r.kind(name), l, lockPath, r.Stderr)
}

func (r *Runner) journalRemoteBroken(task, reason string) error {
	cache, err := os.UserCacheDir()
	if err != nil {
		return fmt.Errorf("locating remote failure journal: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting current directory for remote failure journal: %w", err)
	}
	line, err := json.Marshal(struct {
		At        string `json:"at"`
		Project   string `json:"project"`
		Task      string `json:"task"`
		Host      string `json:"host"`
		Reason    string `json:"reason"`
		PID       int    `json:"pid"`
		PPID      int    `json:"ppid"`
		Cwd       string `json:"cwd"`
		ClaudePID string `json:"claude_pid"`
		Worker    string `json:"worker"`
	}{time.Now().Format(time.RFC3339), r.Root, task, r.remoteHeavy.Host, reason, os.Getpid(), os.Getppid(), cwd, os.Getenv("CLAUDE_PID"), os.Getenv("WORKER_JOB")})
	if err != nil {
		return fmt.Errorf("encoding remote failure journal entry: %w", err)
	}
	path := filepath.Join(cache, "graft", "remote-broken.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating remote failure journal: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening remote failure journal: %w", err)
	}
	_, writeErr := f.Write(append(line, '\n'))
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return fmt.Errorf("writing remote failure journal: %w", err)
	}
	return nil
}

// userRemoteEligible accepts only go test and go vet invocations whose
// arguments cannot request an output file.
func (r *Runner) userRemoteEligible(t *config.Task, args []string) bool {
	if t.Remote != nil && *t.Remote {
		return true
	}
	if len(t.Run) == 0 {
		return false
	}
	for _, step := range t.Run {
		if step.Shell != "" || step.Install != nil || step.Task != "" || step.ServiceAction != "" ||
			!(goCheckNoOutput(step.Argv, args) || glintCheckNoOutput(step.Argv, args) && r.glintEligible(step.Argv, args)) {
			return false
		}
	}
	return true
}

// glintMismatch names the glint of the first glint check step whose copy on
// the host is not the one here, when that alone keeps the task here: every
// other step is a go check or a glint check that may go. Otherwise "".
func (r *Runner) glintMismatch(t *config.Task, args []string) string {
	mismatch := ""
	for _, step := range t.Run {
		if step.Shell != "" || step.Install != nil || step.Task != "" || step.ServiceAction != "" {
			return ""
		}
		switch {
		case goCheckNoOutput(step.Argv, args):
		case glintCheckNoOutput(step.Argv, args):
			if mismatch == "" && !r.glintCopy(step.Argv[0]).ok {
				mismatch = step.Argv[0]
			}
		default:
			return ""
		}
	}
	return mismatch
}

func glintCheckNoOutput(argv, args []string) bool {
	// A relative path would resolve against the step's directory, which the
	// check against install's record does not see: such a glint stays here.
	if len(argv) < 2 || (argv[0] != "glint" && !(filepath.IsAbs(argv[0]) && strings.HasSuffix(filepath.ToSlash(argv[0]), "/glint"))) || argv[1] != "check" {
		return false
	}
	for _, arg := range expandArgs(argv[2:], args) {
		if arg == "--cpuprofile" || strings.HasPrefix(arg, "--cpuprofile=") {
			return false
		}
	}
	return true
}

func expandArgs(argv, args []string) []string {
	var words []string
	for _, arg := range argv {
		switch {
		case arg == ArgsPlaceholder:
			words = append(words, args...)
		case strings.Contains(arg, ArgsPlaceholder) && len(args) == 1:
			words = append(words, strings.Replace(arg, ArgsPlaceholder, args[0], 1))
		default:
			words = append(words, arg)
		}
	}
	return words
}

// outputFlags are the go test and go vet flags that leave a file on the
// machine the step runs on (-fuzz its corpus in testdata); -mod=mod, which
// may rewrite go.mod and go.sum, keeps a step here too.
var outputFlags = []string{"-o", "-c", "-coverprofile", "-cpuprofile", "-memprofile",
	"-blockprofile", "-mutexprofile", "-trace", "-outputdir", "-fuzz"}

// goCheckNoOutput reports whether argv is go test or go vet with no flag
// that writes a file, args given in place of {args}.
func goCheckNoOutput(argv, args []string) bool {
	if len(argv) < 2 || argv[0] != "go" || (argv[1] != "test" && argv[1] != "vet") {
		return false
	}
	words := expandArgs(argv[2:], args)
	for _, arg := range words {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name, value, _ := strings.Cut(arg, "=")
		// go test passes -test.X to the test binary as -X.
		name = "-" + strings.TrimPrefix(strings.TrimLeft(name, "-"), "test.")
		if slices.Contains(outputFlags, name) || name == "-mod" && value == "mod" {
			return false
		}
	}
	return true
}

// goFixDiffNoOutput admits only the built-in go fix -diff command: go fix
// without -diff rewrites source files on the machine where it runs.
func goFixDiffNoOutput(argv []string) bool {
	return len(argv) == 4 && argv[0] == "go" && argv[1] == "fix" && argv[2] == "-diff" && argv[3] == "./..."
}

// stayHere is why the heavy task stays on this machine, "" when it goes to
// the host: what it needs here, a test database, a report file, .env
// values, the commit message file, or a host that does not answer.
func (r *Runner) stayHere(t *config.Task, dir string) (string, error) {
	switch v := os.Getenv(HeavyEnv); v {
	case "":
	case "local":
		return HeavyEnv + "=local", nil
	default:
		return "", fmt.Errorf("%s=%q: want local, or leave it unset", HeavyEnv, v)
	}
	if why := r.taskStaysHere(t, dir); why != "" {
		return why, nil
	}
	if why, err := r.glintPackagesHere(t); err != nil || why != "" {
		return why, err
	}
	if _, ok := r.Extra[MessageFileEnv]; ok {
		def, err := json.Marshal(t)
		if err != nil {
			return "", fmt.Errorf("reading the task: %w", err)
		}
		if namesMessage(def) {
			return "the commit message file is on this machine", nil
		}
	}
	if key, err := dotenvRef(t); err != nil || key != "" {
		if err != nil {
			return "", err
		}
		return "${" + key + "} would carry a .env value to the host", nil
	}
	return "", nil
}

// glintPackagesHere is why a task that runs glint stays on this machine for
// the packages it reads, "" when it need not: a path of worktree.link that is
// a link out of the work tree reaches the host as a link to nothing, and
// glint there would report the project's packages as not installed.
func (r *Runner) glintPackagesHere(t *config.Task) (string, error) {
	runsGlint := slices.ContainsFunc(t.Run, func(s config.Step) bool {
		return len(s.Argv) > 1 && filepath.Base(s.Argv[0]) == "glint" && s.Argv[1] == "check"
	})
	if !runsGlint {
		return "", nil
	}
	for _, group := range r.Config.Worktree.Link {
		for _, path := range group.Paths {
			out, err := linksOutOf(r.Root, filepath.Join(r.Root, filepath.FromSlash(path)))
			if err != nil {
				return "", fmt.Errorf("reading worktree.link %s: %w", path, err)
			}
			if out {
				return path + " links out of the work tree, and glint on the host would not have it", nil
			}
		}
	}
	return "", nil
}

// linksOutOf reports whether path is a symbolic link to a place outside root.
func linksOutOf(root, path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return false, nil
	}
	target, err := os.Readlink(path)
	if err != nil {
		return false, err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	return target != root && !strings.HasPrefix(target, root+string(filepath.Separator)), nil
}

// taskStaysHere is why the task's own settings keep it on this machine, ""
// when they do not.
func (r *Runner) taskStaysHere(t *config.Task, dir string) string {
	switch {
	case t.Remote != nil && !*t.Remote:
		return "the task has remote: false"
	case t.TestDB:
		return "the task uses the test database"
	case reportFile(t):
		return "the " + t.Tests + " report is a file on this machine"
	case len(t.Keys) > 0:
		return "the task takes .env keys"
	case t.Dir != "" && r.taskDir(t) != filepath.Clean(r.Root) && !r.runNamesRoot():
		return "the task runs in " + t.Dir + ", not the work tree root, and remote_heavy.run names no " + config.RootPlaceholder
	case dir != r.Root && !r.runNamesRoot():
		return "it runs in a subdirectory, not the work tree root, and remote_heavy.run names no " + config.RootPlaceholder
	case t.Cache != nil && len(t.Cache.Files) > 0:
		return "the task reads " + t.Cache.Files[0] + " (cache.files), which the host does not have"
	}
	return ""
}

// dotenvRef is the first ${KEY} of the task's env and argv that is no
// variable of graft's own, "" when there is none.
func dotenvRef(t *config.Task) (string, error) {
	var refs []string
	for _, k := range slices.Sorted(maps.Keys(t.Env)) {
		keys, err := dotenv.Refs(t.Env[k])
		if err != nil {
			return "", err
		}
		refs = append(refs, keys...)
	}
	for _, s := range t.Run {
		for _, a := range s.Argv {
			keys, err := dotenv.ShellRefs(a)
			if err != nil {
				return "", err
			}
			refs = append(refs, keys...)
		}
	}
	for _, key := range refs {
		if _, own := t.Env[key]; !own && key != VersionEnv {
			return key, nil
		}
	}
	return "", nil
}

// remoteUp runs remote_heavy's up and returns why the host is not there,
// "" when it is.
func (r *Runner) remoteUp(ctx context.Context) string {
	up := r.remoteHeavy.Up
	ctx, cancel := context.WithTimeout(ctx, remoteUpTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, up[0], up[1:]...)
	cmd.Dir = r.Root
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err == nil {
		return ""
	}
	if line, _, _ := strings.Cut(strings.TrimSpace(out.String()), "\n"); line != "" {
		return line
	}
	return fmt.Sprintf("%s: %v", strings.Join(up, " "), err)
}

// runNamesRoot says whether remote_heavy's run takes the work tree root as
// {root}, so it can start from the task's directory.
func (r *Runner) runNamesRoot() bool {
	return slices.ContainsFunc(r.remoteHeavy.Run, func(w string) bool { return strings.Contains(w, config.RootPlaceholder) })
}

// PassEnvEnv names, comma-separated, the caller's variables a step on
// remote_heavy's host gets besides the task's env: graft exec --env sets it,
// for the grafts the command starts too.
const PassEnvEnv = "GRAFT_PASS_ENV"

// hostEnv are the caller's variables every step on the host gets: they
// choose what the go command builds and checks, the host's result is wrong
// without them, and they hold no secret.
var hostEnv = []string{"GOOS", "GOARCH", "GOFLAGS", "CGO_ENABLED", "GOEXPERIMENT"}

// passedEnv lists hostEnv and the names in PassEnvEnv.
func passedEnv() []string {
	names := slices.Clone(hostEnv)
	for name := range strings.SplitSeq(os.Getenv(PassEnvEnv), ",") {
		if name = strings.TrimSpace(name); name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// remoteStopGrace is how long a step on remote_heavy's host has after
// SIGTERM to take its work off the host before it is killed.
const remoteStopGrace = 30 * time.Second

// remoteCmd is cmd run on remote_heavy's host: its run with env and the
// command appended, from the work tree root, or from cmd's directory when
// run names {root}. The variables graft sets, and the caller's passedEnv,
// go as env K=V, since the host does not get this machine's environment
// (the task's own value wins), and GRAFT_HEAVY=local keeps a
// graft the step starts there from sending its heavy work on again. A
// cancelled step is asked to stop first: killed, run could not clear the
// host.
func (r *Runner) remoteCmd(ctx context.Context, cmd *exec.Cmd, t *config.Task, vars map[string]string, threads string) *exec.Cmd {
	words := make([]string, 0, len(r.remoteHeavy.Run)+len(vars)+len(cmd.Args)+4)
	for _, w := range r.remoteHeavy.Run {
		words = append(words, strings.ReplaceAll(w, config.RootPlaceholder, r.Root))
	}
	words = append(words, "env", HeavyEnv+"=local")
	// A heavy step's -p is the host's threads after the caller's GOFLAGS or
	// the task's, unless the task names one (ownsP): the caller's is the
	// default of a command run without graft's admission, left out when the
	// host's threads are not known.
	flags, ownFlags := vars[goFlagsEnv]
	if !ownFlags || strings.TrimSpace(flags) == "" {
		flags = os.Getenv(goFlagsEnv)
	}
	setP := !ownsP(t, nil)
	if setP && threads != "" {
		flags = withP(flags, threads)
	} else if setP {
		flags = withoutP(flags)
	}
	for _, k := range passedEnv() {
		if v, set := os.LookupEnv(k); set {
			if _, own := vars[k]; !own && (k != goFlagsEnv || !setP) {
				words = append(words, k+"="+v)
			}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(vars)) {
		if k != goFlagsEnv || !setP {
			words = append(words, k+"="+vars[k])
		}
	}
	if setP && flags != "" {
		words = append(words, goFlagsEnv+"="+flags)
	}
	if threads != "" {
		words = append(words, ThreadsEnv+"="+threads)
		if _, set := vars["GOMAXPROCS"]; !set {
			words = append(words, "GOMAXPROCS="+threads)
		}
	}
	words = append(words, r.hostArgs(cmd)...)
	remote := exec.CommandContext(ctx, words[0], words[1:]...)
	remote.Dir, remote.Env = r.Root, cmd.Env
	if r.runNamesRoot() {
		remote.Dir = cmd.Dir
	}
	proc.StopOnCancel(remote, remoteStopGrace, func(err error) {
		fmt.Fprintf(r.Stderr, "graft: stopping %s on %s: %v\n", strings.Join(cmd.Args, " "), r.remoteHeavy.Host, err)
	})
	return remote
}

// hostOf is the host the frame's steps run on, "" for this machine.
func (r *Runner) hostOf(f *frame) string {
	if !f.remote {
		return ""
	}
	return r.remoteHeavy.Host
}

// hostArgs is the command line a step runs on the host. A glint check whose
// copy install recorded there names that copy by its path: the host's PATH
// may find another glint first.
func (r *Runner) hostArgs(cmd *exec.Cmd) []string {
	if !glintCheckNoOutput(cmd.Args, nil) || !r.glintCopy(cmd.Args[0]).ok {
		return cmd.Args
	}
	abs, err := filepath.Abs(cmd.Path)
	if err != nil {
		fmt.Fprintf(r.Stderr, "graft: resolving glint path %s: %v; the host finds glint by its PATH\n", cmd.Path, err)
		return cmd.Args
	}
	return append([]string{abs}, cmd.Args[1:]...)
}
