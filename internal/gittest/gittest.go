// Package gittest runs git for tests, away from the user's configuration.
package gittest

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// Run runs git in dir away from the user's and the system's git
// configuration, with a fixed author, and returns its trimmed output; a
// failure fails the test.
func Run(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	env, err := isolatedEnv()
	if err != nil {
		t.Fatalf("isolate git test environment: %v", err)
	}
	cmd.Env = append(env, "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// IsolateTestProcess removes Git's repository-local and injected config from
// a test process before its raw git commands run.
func IsolateTestProcess() error {
	vars, err := localEnvVars()
	if err != nil {
		return err
	}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if isGitTestConfig(name) || contains(vars, name) {
			if err := os.Unsetenv(name); err != nil {
				return fmt.Errorf("unset %s: %w", name, err)
			}
		}
	}
	if err := os.Setenv("GIT_CONFIG_GLOBAL", "/dev/null"); err != nil {
		return fmt.Errorf("set GIT_CONFIG_GLOBAL: %w", err)
	}
	if err := os.Setenv("GIT_CONFIG_NOSYSTEM", "1"); err != nil {
		return fmt.Errorf("set GIT_CONFIG_NOSYSTEM: %w", err)
	}
	return nil
}

func isolatedEnv() ([]string, error) {
	vars, err := localEnvVars()
	if err != nil {
		return nil, err
	}
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !isGitTestConfig(name) && !contains(vars, name) {
			env = append(env, entry)
		}
	}
	return append(env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"), nil
}

func localEnvVars() ([]string, error) {
	out, err := exec.Command("git", "rev-parse", "--local-env-vars").Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return []string{
				"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
				"GIT_OBJECT_DIRECTORY", "GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE", "GIT_GRAFT_FILE",
				"GIT_INDEX_FILE", "GIT_NO_REPLACE_OBJECTS", "GIT_REPLACE_REF_BASE", "GIT_PREFIX",
				"GIT_SHALLOW_FILE", "GIT_COMMON_DIR",
			}, nil
		}
		return nil, fmt.Errorf("list git local environment variables: %w", err)
	}
	return strings.Fields(string(out)), nil
}

func isGitTestConfig(name string) bool {
	return name == "GIT_CONFIG_GLOBAL" || name == "GIT_CONFIG_SYSTEM" || name == "GIT_CONFIG_NOSYSTEM" ||
		name == "GIT_CONFIG_PARAMETERS" || name == "GIT_CONFIG_COUNT" ||
		strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_")
}

func contains(values []string, value string) bool {
	return slices.Contains(values, value)
}
