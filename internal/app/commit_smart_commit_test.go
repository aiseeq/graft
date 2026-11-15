package app

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gittest"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/version"
)

func TestSmartCommitToken(t *testing.T) {
	tests := []struct {
		message string
		want    string
	}{
		{"PROJ-123 #done fix", "#done"},
		{"#comment added", "#comment"},
		{"#time 1h", "#time"},
		{"#in-progress", "#in-progress"},
		{"PROJ-123 see https://example.org/#done", ""},
		{"PROJ-123 use `#done`", ""},
		{"fix #1 issue", ""},
		{"fix #123 issue", ""},
		{"fix # заголовок", ""},
		{"C# fix", ""},
		{"a#b", ""},
		{"!#done", ""},
		{"! #done", "#done"},
	}
	for _, test := range tests {
		t.Run(test.message, func(t *testing.T) {
			if got := smartCommitToken(test.message); got != test.want {
				t.Fatalf("smartCommitToken(%q) = %q, want %q", test.message, got, test.want)
			}
		})
	}
}

func TestPrepareCommitRefusesSmartCommitAfterBranchKey(t *testing.T) {
	tests := []struct {
		name     string
		branch   string
		message  string
		ticket   bool
		pattern  string
		projects []string
		wantErr  string
	}{
		{name: "message token", branch: "work", message: "PROJ-123 #done fix", ticket: true, wantErr: "#done"},
		{name: "numeric issue marker", branch: "work", message: "PROJ-123 fix #1 issue", ticket: true},
		{name: "command without ticket key", branch: "work", message: "fix #done", ticket: true},
		{name: "branch key is included", branch: "PROJ-123-x", message: "fix #done", ticket: true, wantErr: "#done"},
		{name: "project without ticket", branch: "work", message: "fix #done"},
		{name: "url fragment", branch: "work", message: "PROJ-123 see https://example.org/#done", ticket: true},
		{name: "inline code", branch: "work", message: "PROJ-123 use `#done`", ticket: true},
		{name: "other project key", branch: "work", message: "OTHER-123 #done", ticket: true, pattern: `[A-Z]+-[0-9]+`, projects: []string{"PROJ"}, wantErr: "#done"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			git := func(args ...string) string { return gittest.Run(t, root, args...) }
			git("init", "-q", "-b", test.branch)
			if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git("add", "tracked.txt")
			git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-q", "-m", "base")
			if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("changed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			repo, err := gitx.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{Version: config.Version{Mode: config.ModeNone}}
			if test.ticket {
				pattern := test.pattern
				if pattern == "" {
					pattern = `PROJ-[0-9]+`
				}
				cfg.Ticket = &config.Ticket{Pattern: pattern, Regexp: regexp.MustCompile(pattern)}
				if test.projects != nil {
					cfg.ReleaseNotes = &config.ReleaseNotes{Jira: &config.JiraNotes{ProjectKeys: test.projects}}
				}
			}
			plan, err := prepareCommit(repo, cfg, test.message, version.Patch)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("prepareCommit: %v", err)
				}
				if test.branch == "PROJ-123-x" && !strings.Contains(plan.message, "PROJ-123") {
					t.Fatalf("branch key missing from prepared message %q", plan.message)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) || !strings.Contains(err.Error(), "ticket itself") || !strings.Contains(err.Error(), "without #") {
				t.Fatalf("prepareCommit error = %v; want Smart Commit refusal with rewrite hint", err)
			}
		})
	}
}
