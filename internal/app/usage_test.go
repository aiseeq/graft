package app

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
)

func TestProjectHelpHintsGoTestsWithoutHeavy(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{
			name: "go tests without heavy",
			cfg: &config.Config{Tasks: map[string]*config.Task{
				"test": {Tests: config.TestsGo},
			}},
			want: true,
		},
		{
			name: "heavy go tests",
			cfg: &config.Config{Tasks: map[string]*config.Task{
				"test": {Tests: config.TestsGo, Heavy: true},
			}},
		},
		{
			name: "other test reporter",
			cfg: &config.Config{Tasks: map[string]*config.Task{
				"test": {Tests: config.TestsJest},
			}},
		},
		{
			name: "task inside the gate is hinted",
			cfg: &config.Config{
				Tasks: map[string]*config.Task{"test": {Tests: config.TestsGo}},
				Gate:  []config.Step{{Task: "test"}},
			},
			want: true,
		},
		{
			name: "dependency of heavy task runs before admission",
			cfg: &config.Config{Tasks: map[string]*config.Task{
				"test":  {Tests: config.TestsGo},
				"build": {Heavy: true, Deps: []string{"test"}},
			}},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := (&App{Stdout: &out}).ProjectHelp(tt.cfg); err != nil {
				t.Fatal(err)
			}
			got := strings.Contains(out.String(), "use tests: go without heavy: true")
			if got != tt.want {
				t.Fatalf("hint present = %v, want %v; help:\n%s", got, tt.want, out.String())
			}
		})
	}
}

func TestProjectHelpListsGoTestHintsOnOneLine(t *testing.T) {
	var out bytes.Buffer
	cfg := &config.Config{Tasks: map[string]*config.Task{
		"z-test": {Tests: config.TestsGo},
		"a-test": {Tests: config.TestsGo},
	}}
	if err := (&App{Stdout: &out}).ProjectHelp(cfg); err != nil {
		t.Fatal(err)
	}
	if want := "graft: tasks a-test, z-test use tests: go without heavy: true"; !strings.Contains(out.String(), want) {
		t.Fatalf("help = %q, want one sorted hint containing %q", out.String(), want)
	}
}
