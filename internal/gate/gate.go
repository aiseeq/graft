// Package gate runs the project's pre-commit checks: the commands listed under
// gate in .graft.yaml, in order, stopping at the first failure.
package gate

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/gitx"
)

// MarkerEnv is set for git commands graft itself runs, so the pre-commit hook
// lets them through. It is never passed on to gate commands: a test that runs
// git commit must not inherit the pass.
const MarkerEnv = "GRAFT_COMMIT"

// Run executes the gate commands in root. Output goes straight to out; stdin
// is closed, so a command waiting for input fails instead of hanging.
func Run(ctx context.Context, root string, commands []config.Command, out io.Writer) error {
	if len(commands) == 0 {
		fmt.Fprintln(out, "graft: gate is empty, nothing to run")
		return nil
	}
	env, err := childEnv()
	if err != nil {
		return err
	}
	for i, c := range commands {
		fmt.Fprintf(out, "graft: gate %d/%d: %s\n", i+1, len(commands), c.Source)
		cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
		cmd.Dir = root
		cmd.Env = env
		cmd.Stdout = out
		cmd.Stderr = out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("gate failed at %q: %w", c.Source, err)
		}
	}
	return nil
}

// childEnv is the process environment without graft's commit marker and
// without the variables git sets for hooks (GIT_DIR, GIT_INDEX_FILE, ...):
// a gate started from a hook must not have its own git calls, or those of the
// tests it runs, silently redirected to this repository's index.
func childEnv() ([]string, error) {
	gitVars, err := gitx.LocalEnvVars()
	if err != nil {
		return nil, fmt.Errorf("listing git environment variables: %w", err)
	}
	drop := append(gitVars, MarkerEnv)
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		// Windows treats variable names case-insensitively.
		if !slices.ContainsFunc(drop, func(d string) bool { return strings.EqualFold(d, name) }) {
			env = append(env, kv)
		}
	}
	return env, nil
}
