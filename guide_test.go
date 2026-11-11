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
	if err := guideCmd(&app.App{}, []string{"missing"}); !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "Stats and limits") {
		t.Fatalf("unknown section: %v", err)
	}
}
