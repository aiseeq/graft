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
			plan, err := prepareCommit(repo, cfg, test.message, version.Patch, false)
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

func TestPrepareCommitAllowEmptyOnlySkipsBumpForCleanTree(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) string { return gittest.Run(t, root, args...) }
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte("1.2.3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked.txt")
	git("add", "VERSION")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-q", "-m", "base")
	repo, err := gitx.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Version: config.Version{Mode: config.ModeFile, File: "VERSION"}}
	t.Run("clean empty commit skips version bump", func(t *testing.T) {
		plan, err := prepareCommit(repo, cfg, "chore: link PROJ-123 source", version.Patch, true)
		if err != nil {
			t.Fatalf("prepareCommit: %v", err)
		}
		if !plan.allowEmpty || plan.bump.next != nil {
			t.Fatalf("plan = %+v, want empty commit without version bump", plan)
		}
	})
	for _, tc := range []struct {
		name  string
		level version.Level
		want  string
	}{{"minor", version.Minor, "1.3.0"}, {"major", version.Major, "2.0.0"}} {
		t.Run(tc.name+" with work keeps ordinary bump", func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("changed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			plan, err := prepareCommit(repo, cfg, "feat: PROJ-123 work", tc.level, true)
			if err != nil {
				t.Fatalf("prepareCommit: %v", err)
			}
			if plan.allowEmpty || plan.bump.next == nil || plan.bump.next.String() != tc.want {
				t.Fatalf("plan = %+v, want ordinary %s bump", plan, tc.want)
			}
			if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCommitGateAllowsEmptyResultOnlyWhenRequested(t *testing.T) {
	for _, tc := range []struct {
		name      string
		empty     bool
		allow     bool
		wantAllow bool
	}{
		{name: "gate removed all changes", empty: true, allow: false, wantAllow: false},
		{name: "explicit empty commit", empty: true, allow: true, wantAllow: true},
		{name: "ordinary non-empty commit", empty: false, allow: false, wantAllow: true},
		{name: "flag on non-empty commit", empty: false, allow: true, wantAllow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := commitGateAllowsEmptyResult(tc.empty, tc.allow); got != tc.wantAllow {
				t.Fatalf("commitGateAllowsEmptyResult(%t, %t) = %t, want %t", tc.empty, tc.allow, got, tc.wantAllow)
			}
		})
	}
}
