package tasks

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
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

// trimPaths puts -trimpath after go build, install, run and test unless the
// go command's flags or GOFLAGS in env name -trimpath already, =false
// included; the words after -args, and after the package of go run, are the
// program's.
// Without it the go command keys a package of the module by its directory,
// and every worktree of a repository fills the build cache with its own copy
// of what the others built. go vet and go fix go without: under -trimpath they
// replay a result another worktree cached with that tree's paths, and go fix
// rewrites the files there.
func trimPaths(argv, env []string) []string {
	if len(argv) < 2 || argv[0] != "go" || !slices.Contains(trimBuilds, argv[1]) {
		return argv
	}
	flags, _ := lookupEnv(env, "GOFLAGS")
	words := strings.Fields(flags)
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

// trimRun is trimPaths, and when it adds -trimpath to go run or go test and
// env sets no GOROOT, GOROOT from go env in dir: a program or test built with
// -trimpath takes runtime.GOROOT() from its environment only, and go/importer
// and go/build find no standard library without it. go env runs in dir with
// env, so a toolchain go.mod switches to answers for itself. On the host of
// remote_heavy, which gets none of this environment, go run and go test go
// without -trimpath: the GOROOT found here is no answer there.
func trimRun(ctx context.Context, dir string, argv, env []string, remote bool) ([]string, []string, error) {
	trimmed := trimPaths(argv, env)
	if len(trimmed) == len(argv) || (argv[1] != "run" && argv[1] != "test") {
		return trimmed, env, nil
	}
	if remote {
		return argv, env, nil
	}
	if root, _ := lookupEnv(env, "GOROOT"); root != "" {
		return trimmed, env, nil
	}
	cmd := exec.CommandContext(ctx, "go", "env", "GOROOT")
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.Output()
	if err != nil {
		return nil, nil, fmt.Errorf("go env GOROOT, which a -trimpath build needs to find the standard library: %w", err)
	}
	if root := strings.TrimSpace(string(out)); root != "" {
		env = append(slices.Clip(env), "GOROOT="+root)
	}
	return trimmed, env, nil
}
