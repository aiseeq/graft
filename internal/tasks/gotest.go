package tasks

import (
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
