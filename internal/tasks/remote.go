package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/dotenv"
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
		return false, nil
	}
	why, err := r.stayHere(ctx, t, dir)
	if err != nil {
		return false, err
	}
	if why != "" {
		fmt.Fprintf(r.Stderr, "graft: %s: heavy, on this machine: %s\n", name, why)
		return false, nil
	}
	fmt.Fprintf(r.Stderr, "graft: %s: heavy, on %s\n", name, rh.Host)
	return true, nil
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
	case t.Dir != "":
		return "the task runs in " + t.Dir + ", not the work tree root", nil
	case dir != r.Root:
		return "it runs in a subdirectory, not the work tree root", nil
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
	if !r.remoteAsked {
		r.remoteAsked = true
		r.remoteDown = r.remoteUp(ctx)
	}
	if r.remoteDown != "" {
		return r.Config.RemoteHeavy.Host + " did not answer (" + r.remoteDown + ")", nil
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
	up := r.Config.RemoteHeavy.Up
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
		return fmt.Sprintf("%s: %v: %s", strings.Join(up, " "), err, line)
	}
	return fmt.Sprintf("%s: %v", strings.Join(up, " "), err)
}

// remoteCmd is cmd run on remote_heavy's host: its run with env and the
// command appended, from the work tree root. The variables graft sets go
// as env K=V, since the host does not get this machine's environment, and
// GRAFT_HEAVY=local keeps a graft the step starts there from sending its
// heavy work on again.
func (r *Runner) remoteCmd(ctx context.Context, cmd *exec.Cmd, vars map[string]string, threads string) *exec.Cmd {
	words := append(slices.Clone(r.Config.RemoteHeavy.Run), "env", HeavyEnv+"=local")
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
	return remote
}

// hostOf is the host the frame's steps run on, "" for this machine.
func (r *Runner) hostOf(f *frame) string {
	if !f.remote {
		return ""
	}
	return r.Config.RemoteHeavy.Host
}
