package tasks

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/lintcache"
)

// CallerSetEnv names, comma-separated, the variables that the caller of an
// outer graft exec --heavy set: the inner graft, whose caller is that exec,
// keeps its task here for them as the exec would have.
const CallerSetEnv = "GRAFT_CALLER_SET"

// callerSet names the variables that graft's caller set for it, the ones
// graft's environment has and its parent's does not, or has with another
// value: a command sent to remote_heavy's host would not get them. graft's
// own variables, git's and the shell's bookkeeping are left out.
func callerSet() ([]string, error) {
	// A graft started by graft exec --heavy has the exec's word: its parent
	// is that graft, whose own variables are no input of the command.
	if v, ok := os.LookupEnv(CallerSetEnv); ok {
		switch v {
		case "":
			return nil, nil
		case callerUnknown:
			return nil, errors.New("the graft exec that started this graft could not tell them")
		}
		return strings.Split(v, ","), nil
	}
	parent, err := parentEnv()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(parent, kv) || bookkeeping(name) || slices.Contains(names, name) {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names, nil
}

// bookkeeping tells whether the variable is one that graft, git or the shell
// sets for itself, not one a command reads as its input.
func bookkeeping(name string) bool {
	switch name {
	case "PWD", "OLDPWD", "SHLVL", "_", "GOMAXPROCS", lintcache.Env:
		return true
	}
	return strings.HasPrefix(name, "GRAFT_") || strings.HasPrefix(name, "GIT_")
}

// callerUnknown in CallerSetEnv says the outer graft could not tell the
// variables its caller set.
const callerUnknown = "?"

// callerSetValue is CallerSetEnv's value for an inner graft: the variables
// graft's caller set, or callerUnknown.
func (r *Runner) callerSetValue() string {
	r.readCallerSet()
	if r.callerErr != nil {
		return callerUnknown
	}
	return strings.Join(r.callerNames, ",")
}

// readCallerSet reads callerSet once.
func (r *Runner) readCallerSet() {
	if !r.callerAsked {
		r.callerAsked = true
		r.callerNames, r.callerErr = callerSet()
	}
}

// callerSetWhy is why a heavy task stays here for the variables its caller
// set, "" when the caller set none.
func (r *Runner) callerSetWhy(host string) string {
	r.readCallerSet()
	if r.callerErr != nil {
		return fmt.Sprintf("graft cannot tell the variables its caller set, which %s would not get: %v", host, r.callerErr)
	}
	if len(r.callerNames) == 0 {
		return ""
	}
	return fmt.Sprintf("the caller set %s, which %s would not get", strings.Join(r.callerNames, ", "), host)
}
