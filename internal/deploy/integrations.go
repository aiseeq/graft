package deploy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/envs"
)

// Integrations runs the integrations command in env, {staged} filled with
// staged, and returns the integrations it prints, one a line.
func Integrations(ctx context.Context, env *envs.Env, c config.EnvCommand, staged string) ([]string, error) {
	fill := func(s string, escape func(string) string) string {
		return strings.ReplaceAll(s, config.StagedPlaceholder, escape(staged))
	}
	filled := config.EnvCommand{Source: c.Source}
	if c.Shell != "" {
		filled.Shell = fill(c.Shell, envs.Quote)
	}
	for _, w := range c.Argv {
		filled.Argv = append(filled.Argv, fill(w, func(s string) string { return s }))
	}
	cmd, err := env.Command(ctx, filled)
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("reading the integrations on %s: %w: %s", env.Name, err, strings.TrimSpace(stderr.String()))
	}
	var names []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" && !slices.Contains(names, line) {
			names = append(names, line)
		}
	}
	return names, nil
}

// ReadIntegrationExceptions reads an exceptions file: "integration|reason"
// lines, blank lines and # comments skipped. A line without its reason is an
// error: an exception nobody explained is a gap nobody decided on. No file
// name is no exceptions; a named file that is missing is an error.
func ReadIntegrationExceptions(path string) (map[string]string, error) {
	out := map[string]string{}
	if path == "" {
		return out, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("integration exceptions: %w", err)
	}
	var errs []error
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, reason, ok := strings.Cut(line, "|")
		name, reason = strings.TrimSpace(name), strings.TrimSpace(reason)
		switch _, twice := out[name]; {
		case !ok || name == "" || reason == "":
			errs = append(errs, fmt.Errorf("%s:%d: expected \"integration|reason\", got %q", path, n, line))
		case twice:
			errs = append(errs, fmt.Errorf("%s:%d: %s is listed twice", path, n, name))
		default:
			out[name] = reason
		}
	}
	if err := sc.Err(); err != nil {
		errs = append(errs, fmt.Errorf("reading %s: %w", path, err))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

// IntegrationGaps compares what the required target runs with what this one
// configures: missing are the integrations it lacks that no exception
// excuses, excused the ones an exception does, and stale the exceptions for
// integrations it has or the required target does not run.
func IntegrationGaps(required, here []string, exceptions map[string]string) (missing, excused, stale []string) {
	for _, name := range required {
		switch _, ok := exceptions[name]; {
		case slices.Contains(here, name):
		case ok:
			excused = append(excused, name)
		default:
			missing = append(missing, name)
		}
	}
	for name := range exceptions {
		if !slices.Contains(excused, name) {
			stale = append(stale, name)
		}
	}
	slices.Sort(stale)
	return missing, excused, stale
}
