package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/app"
)

type registryCommand struct {
	Path       []string `json:"path"`
	Flags      []string `json:"flags"`
	ValueFlags []string `json:"value_flags"`
	Desc       string   `json:"desc"`
	Rest       string   `json:"rest"`
}

type registryDocument struct {
	Tool             string            `json:"tool"`
	Version          string            `json:"version"`
	GlobalFlags      []string          `json:"global_flags"`
	GlobalValueFlags []string          `json:"global_value_flags"`
	Commands         []registryCommand `json:"commands"`
	Tasks            []string          `json:"tasks"`
	TasksError       string            `json:"tasks_error,omitempty"`
}

// flagRecorder stops a handler immediately before parsing, after it has
// registered its flags. It cannot be supplied by a command-line caller.
type flagRecorder struct{ flags *flag.FlagSet }

func (*flagRecorder) Write(p []byte) (int, error) { return len(p), nil }

var errFlagRecorded = errors.New("flag set recorded")

func recordFlags(fs *flag.FlagSet) bool {
	out, ok := fs.Output().(*heldOutput)
	if !ok {
		return false
	}
	recorder, ok := out.w.(*flagRecorder)
	if ok {
		recorder.flags = fs
	}
	return ok
}

func recordedFlags(a *app.App, name string) (*flag.FlagSet, error) {
	copyApp := *a
	recorder := &flagRecorder{}
	copyApp.Stdout = recorder
	ctx := context.Background()
	var err error
	switch name {
	case "commit":
		err = commitCmd(ctx, &copyApp, nil)
	case "release":
		err = releaseCmd(ctx, &copyApp, nil)
	case "version":
		err = versionCmd(ctx, &copyApp, nil)
	case "version bump":
		err = versionCmd(ctx, &copyApp, []string{"bump"})
	case "stats":
		err = statsCmd(&copyApp, nil)
	case "logs":
		err = logsCmd(ctx, &copyApp, nil)
	case "flags":
		err = flagsCmd(ctx, &copyApp, nil)
	case "deploy":
		err = deployCmd(ctx, &copyApp, nil)
	case "gocache trim":
		err = gocacheCmd(ctx, &copyApp, []string{"trim"})
	case "worktree add", "worktree check", "worktree clean", "worktree sync":
		err = worktreeCmd(ctx, &copyApp, []string{strings.TrimPrefix(name, "worktree ")})
	default:
		return nil, fmt.Errorf("no flag recorder for %s", name)
	}
	if err == nil {
		return nil, fmt.Errorf("recording flags for %s: handler returned without recording", name)
	}
	if !errors.Is(err, errFlagRecorded) {
		return nil, fmt.Errorf("recording flags for %s: %w", name, err)
	}
	if recorder.flags == nil {
		return nil, fmt.Errorf("recording flags for %s: no flag set", name)
	}
	return recorder.flags, nil
}

type registrySpec struct {
	path, rest, source, selectFlags string
	manual, values                  string
}

// A blank selectFlags uses every flag of source; otherwise it selects the
// flags applicable to that subcommand. Manual parsers are listed explicitly.
var registrySpecs = []registrySpec{
	{path: "commit", rest: "none", source: "commit"},
	{path: "amend", rest: "none"},
	{path: "release", rest: "none", source: "release"},
	{path: "version", rest: "none", source: "version"},
	{path: "version bump", rest: "none", source: "version bump"},
	{path: "init", rest: "none", manual: "--global"},
	{path: "check", rest: "none"}, {path: "leaks", rest: "args", manual: "--all"},
	{path: "gate", rest: "none"}, {path: "hook", rest: "none"},
	{path: "hook pre-commit", rest: "args"}, {path: "hook prepare-commit-msg", rest: "args"},
	{path: "hook post-merge", rest: "args"}, {path: "hook pre-push", rest: "args"},
	{path: "hook global-pre-commit", rest: "args"},
	{path: "flags", rest: "none", source: "flags", selectFlags: "env"},
	{path: "flags status", rest: "none", source: "flags", selectFlags: "env"},
	{path: "flags show", rest: "args", source: "flags", selectFlags: "env"},
	{path: "flags ack", rest: "args", source: "flags", selectFlags: "env reason"},
	{path: "flags mute", rest: "args", source: "flags"},
	{path: "deploy", rest: "after-dashdash", source: "deploy", selectFlags: "redeploy unapproved past-freeze glint-ticket no-cache"},
	{path: "deploy freeze", rest: "args", source: "deploy", selectFlags: "no-cache"},
	{path: "deploy thaw", rest: "args", source: "deploy", selectFlags: "reason no-cache"},
	{path: "deploy status", rest: "after-dashdash", source: "deploy", selectFlags: "no-cache"},
	{path: "deploy logs", rest: "after-dashdash", source: "deploy", selectFlags: "lines grep no-cache"},
	{path: "deploy check", rest: "args", source: "deploy", selectFlags: "no-cache"},
	{path: "deploy versions", rest: "none", source: "deploy", selectFlags: "no-cache"},
	{path: "deploy cutoff", rest: "args", source: "deploy", selectFlags: "no-cache"},
	{path: "deploy sweep", rest: "args", source: "deploy", selectFlags: "apply no-cache"},
	{path: "deploy handover", rest: "args", source: "deploy", selectFlags: "glint-ticket no-cache"},
	{path: "deploy check-head", rest: "none", source: "deploy", selectFlags: "no-cache"},
	{path: "deploy check-integrations", rest: "args", source: "deploy", selectFlags: "no-cache"},
	{path: "run", rest: "after-dashdash", manual: "--no-cache"},
	{path: "tasks", rest: "none"}, {path: "start", rest: "args"},
	{path: "stop", rest: "args"}, {path: "restart", rest: "args"},
	{path: "logs", rest: "args", source: "logs"},
	{path: "exec", rest: "after-dashdash", manual: "--heavy --local --test-db --env --settle --cores --memory-max", values: "--env --settle --cores --memory-max"},
	{path: "reap", rest: "after-dashdash"},
	{path: "stats", rest: "none", source: "stats"},
	{path: "status", rest: "none"}, {path: "locks", rest: "none"},
	{path: "testdb", rest: "none"}, {path: "testdb up", rest: "none"},
	{path: "testdb down", rest: "none"}, {path: "testdb status", rest: "none"},
	{path: "testdb recreate", rest: "none"},
	{path: "tools", rest: "none"}, {path: "tools install", rest: "none"},
	{path: "worktree", rest: "none"},
	{path: "worktree add", rest: "args", source: "worktree add"},
	{path: "worktree sync", rest: "none", source: "worktree sync"},
	{path: "worktree check", rest: "args", source: "worktree check"},
	{path: "worktree clean", rest: "none", source: "worktree clean"},
	{path: "gocache", rest: "none"}, {path: "gocache trim", rest: "none", source: "gocache trim"},
	{path: "guide", rest: "args"}, {path: "help", rest: "args", manual: "--json"},
}

func registry(a *app.App) ([]registryCommand, error) {
	entries := make([]registryCommand, 0, len(registrySpecs))
	sets := make(map[string]*flag.FlagSet)
	for _, spec := range registrySpecs {
		path := strings.Fields(spec.path)
		section, ok := sectionOf(path[0])
		if !ok {
			return nil, fmt.Errorf("registry: no help for %s", spec.path)
		}
		entry := registryCommand{Path: path, Flags: []string{}, ValueFlags: []string{}, Desc: section.summary(nil), Rest: spec.rest}
		entry.Flags = append(entry.Flags, strings.Fields(spec.manual)...)
		entry.ValueFlags = append(entry.ValueFlags, strings.Fields(spec.values)...)
		if spec.source != "" {
			fs := sets[spec.source]
			if fs == nil {
				var err error
				fs, err = recordedFlags(a, spec.source)
				if err != nil {
					return nil, err
				}
				sets[spec.source] = fs
			}
			selected := strings.Fields(spec.selectFlags)
			fs.VisitAll(func(f *flag.Flag) {
				if len(selected) != 0 && !slices.Contains(selected, f.Name) {
					return
				}
				for _, prefix := range []string{"-", "--"} {
					name := prefix + f.Name
					entry.Flags = append(entry.Flags, name)
					if b, ok := f.Value.(interface{ IsBoolFlag() bool }); !ok || !b.IsBoolFlag() {
						entry.ValueFlags = append(entry.ValueFlags, name)
					}
				}
			})
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func helpJSON(a *app.App) error {
	entries, err := registry(a)
	if err != nil {
		return err
	}
	doc := registryDocument{Tool: "graft", Version: toolVersion(), GlobalFlags: []string{"-C", "--help", "-h", "--version", "-v"}, GlobalValueFlags: []string{"-C"}, Commands: entries, Tasks: []string{}}
	cfg, found, configErr := a.ProjectConfig()
	switch {
	case configErr != nil:
		doc.TasksError = configErr.Error()
	case !found:
		doc.TasksError = "no project or .graft.yaml found"
	default:
		doc.Tasks = slices.Sorted(maps.Keys(cfg.Tasks))
	}
	return json.NewEncoder(a.Stdout).Encode(doc)
}

var _ io.Writer = (*flagRecorder)(nil)
