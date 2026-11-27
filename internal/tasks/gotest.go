package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/execx"
)

// goBoolFlags are the go test and build flags that take no separate value;
// any other flag written without = takes the next word as its value.
var goBoolFlags = []string{
	"a", "asan", "benchmem", "cover", "failfast", "fullpath", "i", "json",
	"linkshared", "msan", "n", "race", "short", "trimpath", "v", "work", "x",
}

// ownPackages drops the packages a go test step names itself (./... after
// {args}) when the arguments name packages of their own: graft test --
// ./cmd/x runs ./cmd/x alone, as go test ./cmd/x would. It returns the step
// unchanged otherwise; dropped tells what went.
func ownPackages(argv, args []string) (out, dropped []string) {
	if len(argv) < 2 || argv[0] != "go" || argv[1] != "test" ||
		len(goPackages(args)) == 0 || !slices.Contains(argv, config.ArgsPlaceholder) {
		return argv, nil
	}
	own := goPackages(argv)
	if len(own) == 0 {
		return argv, nil
	}
	for i, w := range argv {
		if slices.Contains(own, i) {
			dropped = append(dropped, w)
			continue
		}
		out = append(out, w)
	}
	return out, dropped
}

// goPackages returns the indexes of the words go test reads as packages:
// those that are neither flags nor flag values, up to -args, which hands
// the rest to the test binary. go test at the start of a step and {args}
// are not packages either.
func goPackages(words []string) []int {
	var pkgs []int
	value := false
	for i, w := range words {
		switch {
		case value:
			value = false
		case w == "-args" || w == "--args":
			return pkgs
		case strings.HasPrefix(w, "-"):
			name := strings.TrimLeft(w, "-")
			value = !strings.Contains(name, "=") && !slices.Contains(goBoolFlags, name)
		case w == config.ArgsPlaceholder:
		case i < 2 && (w == "go" || w == "test"):
		default:
			pkgs = append(pkgs, i)
		}
	}
	return pkgs
}

// trimBuilds are the go commands that build the module's packages into the
// build cache.
var trimBuilds = []string{"build", "install", "run", "test"}

// splitGoFlags follows cmd/internal/quoted.Split (src/cmd/internal/quoted/quoted.go).
func splitGoFlags(s string) ([]string, error) {
	isSpace := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
	var fields []string
	for len(s) > 0 {
		for len(s) > 0 && isSpace(s[0]) {
			s = s[1:]
		}
		if len(s) == 0 {
			break
		}
		if s[0] == '\'' || s[0] == '"' {
			quote := s[0]
			s = s[1:]
			i := strings.IndexByte(s, quote)
			if i < 0 {
				return nil, fmt.Errorf("unterminated %c string", quote)
			}
			fields = append(fields, s[:i])
			s = s[i+1:]
			continue
		}
		i := 0
		for i < len(s) && !isSpace(s[i]) {
			i++
		}
		fields = append(fields, s[:i])
		s = s[i:]
	}
	return fields, nil
}

// addTrimPath puts -trimpath after the selected go commands unless Go's
// resolved GOFLAGS or the command's own flags already name it, =false included.
// Without it the go command keys a package of the module by its directory,
// and every worktree of a repository fills the build cache with its own copy
// of what the others built. The gate's built-in vet and go fix -diff also use
// -trimpath; if they print anything or fail, they repeat without it so the
// result shows paths from this tree (builtinTrimmedStep). Ordinary go fix,
// which rewrites files, is not changed.
func addTrimPath(argv, flags []string, commands []string) []string {
	if len(argv) < 2 || argv[0] != "go" || !slices.Contains(commands, argv[1]) {
		return argv
	}
	words := slices.Clone(flags)
	for _, w := range argv[2:] {
		if w == "-args" || w == "--args" || (argv[1] == "run" && !strings.HasPrefix(w, "-")) {
			break
		}
		words = append(words, w)
	}
	for _, w := range words {
		if name, _, _ := strings.Cut(strings.TrimLeft(w, "-"), "="); name == "trimpath" {
			return argv
		}
	}
	return slices.Concat(argv[:2], []string{"-trimpath"}, argv[2:])
}

// trimRun resolves GOFLAGS with go env in the step environment and, when it
// adds -trimpath to go run or go test and env sets no GOROOT, gets GOROOT from
// go env in dir: a program or test built with
// -trimpath takes runtime.GOROOT() from its environment only, and go/importer
// and go/build find no standard library without it. go env runs in dir with
// env, so a toolchain go.mod switches to answers for itself. On the host of
// remote_heavy, which gets none of this environment, go run and go test go
// without -trimpath: the GOROOT found here is no answer there.
func trimRun(ctx context.Context, dir string, argv, env []string, remote bool) ([]string, []string, error) {
	if len(argv) < 2 || argv[0] != "go" || !slices.Contains(trimBuilds, argv[1]) {
		return argv, env, nil
	}
	if remote {
		if argv[1] == "build" || argv[1] == "install" {
			flags, _ := lookupEnv(env, "GOFLAGS")
			return addTrimPath(argv, strings.Fields(flags), trimBuilds), env, nil
		}
		return argv, env, nil
	}
	needRoot := (argv[1] == "run" || argv[1] == "test") && lookupEnvValue(env, "GOROOT") == ""
	query := []string{"GOFLAGS"}
	if needRoot {
		query = []string{"-json", "GOFLAGS", "GOROOT"}
	}
	out, err := goEnvOutput(ctx, dir, env, query...)
	if err != nil {
		return nil, nil, fmt.Errorf("go env %s for -trimpath: %w", strings.Join(query, " "), err)
	}
	var info struct{ GOROOT, GOFLAGS string }
	if needRoot {
		if err := json.Unmarshal(out, &info); err != nil {
			return nil, nil, fmt.Errorf("go env -json GOFLAGS GOROOT for -trimpath: %w", err)
		}
	} else {
		info.GOFLAGS = strings.TrimSpace(string(out))
	}
	flags, err := splitGoFlags(info.GOFLAGS)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing go env GOFLAGS for -trimpath: %w", err)
	}
	trimmed := addTrimPath(argv, flags, trimBuilds)
	if len(trimmed) == len(argv) || !needRoot {
		return trimmed, env, nil
	}
	if info.GOROOT != "" {
		env = append(slices.Clip(env), "GOROOT="+info.GOROOT)
	}
	return trimmed, env, nil
}

func lookupEnvValue(env []string, key string) string { v, _ := lookupEnv(env, key); return v }

func resolveGoFlags(ctx context.Context, dir string, env []string) (string, error) {
	out, err := goEnvOutput(ctx, dir, env, "GOFLAGS")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func goEnvOutput(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	cmd, err := execx.CommandContext(ctx, execx.ShortGrace, "go", append([]string{"env"}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("go env %s: %w", strings.Join(args, " "), err)
	}
	cmd.Dir, cmd.Env = dir, env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return nil, fmt.Errorf("go env %s: %w: %s", strings.Join(args, " "), err, message)
		}
		return nil, fmt.Errorf("go env %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}
