package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/app"
)

func TestGuideSections(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "# Working with graft"},
		{[]string{"HEAVY"}, "## Heavy\n"},
		{[]string{"stats and limits"}, "## Stats and limits\n"},
		{[]string{"stats"}, "## Stats and limits\n"},
	} {
		var out bytes.Buffer
		if err := guideCmd(&app.App{Stdout: &out}, tc.args); err != nil {
			t.Fatalf("guide %v: %v", tc.args, err)
		}
		if !strings.HasPrefix(out.String(), tc.want) {
			t.Errorf("guide %v: got %q, want prefix %q", tc.args, out.String(), tc.want)
		}
	}
	var deploy bytes.Buffer
	if err := guideCmd(&app.App{Stdout: &deploy}, []string{"deploy"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Build the user-facing summary from its output", "what was done and why", "risks the configuration cannot know"} {
		if !strings.Contains(deploy.String(), want) {
			t.Errorf("deploy guide missing %q", want)
		}
	}
	if err := guideCmd(&app.App{}, []string{"missing"}); !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "Stats and limits") {
		t.Fatalf("unknown section: %v", err)
	}
}
