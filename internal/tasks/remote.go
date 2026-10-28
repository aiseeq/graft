package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// project has one.
func (r *Runner) placeHeavy(ctx context.Context, name string, t *config.Task, dir string) (bool, error) {
	rh := r.Config.RemoteHeavy
	if rh == nil {
		var err error
		rh, err = userconfig.RemoteHeavySettings()
		if err != nil {
			return false, fmt.Errorf("reading user remote_heavy: %w", err)
		}
		r.userRemote = rh != nil
	}
	if rh == nil {
		return false, nil
	}
	r.remoteHeavy = rh
	why, err := r.stayHere(ctx, t, dir)
	if err != nil {
		return false, err
	}
	if why != "" {
		fmt.Fprintf(r.Stderr, "graft: %s: heavy, on this machine: %s\n", name, why)
		return false, nil
	}
	if r.userRemote && t.Remote == nil && !userRemoteEligible(t) {
		fmt.Fprintf(r.Stderr, "graft: %s: heavy, on this machine: only go test and go vet steps that write no file can use user remote_heavy\n", name)
		return false, nil
	}
	if rh.When == "busy" {
		l, err := r.Load()
		if err != nil {
			return false, err
		}
		busy, err := loadgate.BusyNow(ctx, name, l, r.Stderr)
		if err != nil {
			return false, err
		}
		if !busy {
			return false, nil
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
				return false, err
			}
		} else {
			fmt.Fprintf(r.Stderr, "graft: %s: heavy, on this machine: %s did not answer (%s)\n", name, rh.Host, r.remoteDown)
		}
		return false, nil
	}
	fmt.Fprintf(r.Stderr, "graft: %s: heavy, on %s\n", name, rh.Host)
	return true, nil
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
func userRemoteEligible(t *config.Task) bool {
	if t.Remote != nil && *t.Remote {
		return true
	}
	if len(t.Run) == 0 {
		return false
	}
	for _, step := range t.Run {
		if step.Shell != "" || step.Install != nil || step.Task != "" || step.ServiceAction != "" ||
			!goCheckNoOutput(step.Argv) {
			return false
		}
	}
	return true
}

// outputFlags are the go test and go vet flags that leave a file on the
// machine the step runs on.
var outputFlags = []string{"-o", "-c", "-coverprofile", "-cpuprofile", "-memprofile",
	"-blockprofile", "-mutexprofile", "-trace", "-outputdir"}

func goCheckNoOutput(argv []string) bool {
	if len(argv) < 2 || argv[0] != "go" || (argv[1] != "test" && argv[1] != "vet") {
		return false
	}
	for _, arg := range argv[2:] {
		name, _, _ := strings.Cut(arg, "=")
		if arg == ArgsPlaceholder || strings.HasPrefix(arg, "-") && slices.Contains(outputFlags, "-"+strings.TrimLeft(name, "-")) {
			return false
		}
	}
	return true
}

// stayHere is why the heavy task stays on this machine, "" when it goes to
// the host: what it needs here, a test database, a report file, .env
// values, the commit message file, or a host that does not answer.
func (r *Runner) stayHere(ctx context.Context, t *config.Task, dir string) (string, error) {
	switch v := os.Getenv(HeavyEnv); v {
	case "":
	case "local":
		return HeavyEnv + "=local", nil
	default:
		return "", fmt.Errorf("%s=%q: want local, or leave it unset", HeavyEnv, v)
	}
	switch {
	case t.Remote != nil && !*t.Remote:
		return "the task has remote: false", nil
	case t.TestDB:
		return "the task uses the test database", nil
	case reportFile(t):
		return "the " + t.Tests + " report is a file on this machine", nil
	case len(t.Keys) > 0:
		return "the task takes .env keys", nil
	case t.Dir != "" && !r.runNamesRoot():
		return "the task runs in " + t.Dir + ", not the work tree root, and remote_heavy.run names no " + config.RootPlaceholder, nil
	case dir != r.Root && !r.runNamesRoot():
		return "it runs in a subdirectory, not the work tree root, and remote_heavy.run names no " + config.RootPlaceholder, nil
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

// dotenvRef is the first ${KEY} of the task's env and argv that is no
// variable of graft's own, "" when there is none.
func dotenvRef(t *config.Task) (string, error) {
	var texts []string
	for _, k := range sortedKeys(t.Env) {
		texts = append(texts, t.Env[k])
	}
	for _, s := range t.Run {
		texts = append(texts, s.Argv...)
	}
	for _, text := range texts {
		refs, err := dotenv.Refs(text)
		if err != nil {
			return "", err
		}
		for _, key := range refs {
			if _, own := t.Env[key]; !own && key != VersionEnv {
				return key, nil
			}
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

// remoteStopGrace is how long a step on remote_heavy's host has after
// SIGTERM to take its work off the host before it is killed.
const remoteStopGrace = 30 * time.Second

// remoteCmd is cmd run on remote_heavy's host: its run with env and the
// command appended, from the work tree root, or from cmd's directory when
// run names {root}. The variables graft sets go as env K=V, since the host
// does not get this machine's environment, and GRAFT_HEAVY=local keeps a
// graft the step starts there from sending its heavy work on again. A
// cancelled step is asked to stop first: killed, run could not clear the
// host.
func (r *Runner) remoteCmd(ctx context.Context, cmd *exec.Cmd, vars map[string]string, threads string) *exec.Cmd {
	words := make([]string, 0, len(r.remoteHeavy.Run)+len(vars)+len(cmd.Args)+4)
	for _, w := range r.remoteHeavy.Run {
		words = append(words, strings.ReplaceAll(w, config.RootPlaceholder, r.Root))
	}
	words = append(words, "env", HeavyEnv+"=local")
	for _, k := range sortedKeys(vars) {
		words = append(words, k+"="+vars[k])
	}
	if threads != "" {
		words = append(words, ThreadsEnv+"="+threads)
		if _, set := vars["GOMAXPROCS"]; !set {
			words = append(words, "GOMAXPROCS="+threads)
		}
	}
	words = append(words, cmd.Args...)
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
