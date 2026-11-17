package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/tasks"
)

func TestLocalExecWarningDecision(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// the gate itself may run on the remote_heavy host, whose origin marker suppresses the warning
	t.Setenv(tasks.HeavyOriginEnv, "")
	project := &config.Config{RemoteHeavy: &config.RemoteHeavy{Host: "fake", Up: []string{"true"}, Run: []string{"true"}}}
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"go", "test", "./..."}, "go test"},
		{[]string{"go", "vet", "./..."}, "go vet"},
		{[]string{"mut", "./..."}, "mut"},
		{[]string{"graft", "test"}, "graft test"},
		{[]string{"/tools/graft", "test"}, "graft test"},
	} {
		got, err := tasks.LocalExecWarning(project, true, tc.argv)
		if err != nil || got != tc.want {
			t.Errorf("argv %q: warning=%q err=%v, want %q", tc.argv, got, err, tc.want)
		}
	}
	for _, argv := range [][]string{{"go", "build"}, {"sh", "-c", "go test"}, {"graft", "gate"}, {"mutate", "./..."}} {
		if got, err := tasks.LocalExecWarning(project, true, argv); err != nil || got != "" {
			t.Errorf("unexpected warning for argv %q: %q, %v", argv, got, err)
		}
	}
	if got, err := tasks.LocalExecWarning(project, false, []string{"go", "test"}); err != nil || got != "" {
		t.Errorf("warning without --local = %q, %v", got, err)
	}
	if got, err := tasks.LocalExecWarning(&config.Config{}, true, []string{"go", "test"}); err != nil || got != "" {
		t.Errorf("warning without remote_heavy = %q, %v", got, err)
	}
	t.Setenv(tasks.HeavyOriginEnv, "host")
	if got, err := tasks.LocalExecWarning(project, true, []string{"go", "test"}); err != nil || got != "" {
		t.Errorf("warning on remote_heavy host = %q, %v", got, err)
	}
	t.Setenv(tasks.HeavyOriginEnv, "")

	userConfig := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "graft", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(userConfig), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userConfig, []byte("remote_heavy: {host: fake, up: [true], run: [true]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := tasks.LocalExecWarning(&config.Config{}, true, []string{"go", "test"}); err != nil || got != "go test" {
		t.Errorf("user remote_heavy warning = %q, %v", got, err)
	}
}
