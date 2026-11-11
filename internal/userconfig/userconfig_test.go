package userconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/pressure"
	"github.com/aiseeq/graft/v2/internal/config"
)

func TestUserconfigRemoteIsPublic(t *testing.T) {
	l := &Leaks{PublicRemotes: []string{"github.com/someone/"}}
	for url, want := range map[string]bool{
		"git@github.com:someone/tool.git":           true,
		"https://github.com/someone/tool":           true,
		"ssh://git@github.com/someone/tool.git":     true,
		"https://token@github.com/someone/tool.git": true,
		"git@github.com:other/tool.git":             false,
		"git@bitbucket.org:someone/tool.git":        false,
		"/srv/git/someone/tool.git":                 false,
		"ssh://git@github.com:22/someone/tool.git":  true,
		"https://GitHub.com/someone/tool.git":       true,
		"git@github.com:/someone/tool.git":          true,
		"git@GITHUB.COM:someone/tool":               true,
		"https://github.com/SomeOne/tool.git":       true,
	} {
		if got := l.RemoteIsPublic(url); got != want {
			t.Errorf("%s: %v, want %v", url, got, want)
		}
	}
}

func TestPinnedOwnTool(t *testing.T) {
	modules := Modules{Latest: []string{"example.org/team/"}}
	for _, tc := range []struct {
		name, install, want string
	}{
		{"pinned own", "example.org/team/tool/cmd/x@v1.2.3", "example.org/team/tool/cmd/x"},
		{"latest", "example.org/team/tool@latest", ""},
		{"unpinned", "example.org/team/tool", ""},
		{"foreign", "example.org/other/tool@v1.2.3", ""},
		{"prefix is not a path segment", "example.org/teamX/tool@v1.2.3", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := modules.PinnedOwnTool(config.Tool{GoInstall: tc.install}); got != tc.want {
				t.Errorf("PinnedOwnTool(%q) = %q, want %q", tc.install, got, tc.want)
			}
		})
	}
}

func TestModulesIsLatest(t *testing.T) {
	m := Modules{Latest: []string{"example.com/own/", "example.org/team"}}
	for path, want := range map[string]bool{
		"example.com/own":       true,
		"example.com/own/lib":   true,
		"example.com/ownish":    false,
		"example.org/team/tool": true,
		"example.org/teams":     false,
	} {
		if got := m.IsLatest(path); got != want {
			t.Errorf("IsLatest(%q): %v, want %v", path, got, want)
		}
	}
}

// A narrower private prefix wins over the public prefix it lies under.
func TestUserconfigRemoteIsPublicPrivateOverrides(t *testing.T) {
	l := &Leaks{PublicRemotes: []string{"github.com/someone/"}, PrivateRemotes: []string{"github.com/someone/secret-tool"}}
	for url, want := range map[string]bool{
		"git@github.com:someone/secret-tool.git":       false,
		"https://github.com/someone/secret-tool":       false,
		"ssh://git@github.com/someone/secret-tool.git": false,
		"git@github.com:someone/tool.git":              true,
		"https://github.com/someone/tool":              true,
	} {
		if got := l.RemoteIsPublic(url); got != want {
			t.Errorf("%s: %v, want %v", url, got, want)
		}
	}
}

func TestLeaksNormalize(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	l := &Leaks{PublicRemotes: []string{"github.com/someone/"}, PrivateSources: []string{"~/work/a"}, Allow: []LeakAllow{{Term: "X", Reason: "why"}}}
	if err := l.normalize("cfg"); err != nil {
		t.Fatal(err)
	}
	if l.PrivateSources[0] != "/home/u/work/a" || l.MinNameLength != DefaultMinNameLength {
		t.Errorf("%+v", l)
	}
	for _, bad := range []*Leaks{
		{PrivateSources: []string{"/a"}},
		{PublicRemotes: []string{"https://github.com/x/"}, PrivateSources: []string{"/a"}},
		{PublicRemotes: []string{"github.com/x/"}},
		{PublicRemotes: []string{"github.com/x/"}, PrivateSources: []string{"rel/dir"}},
		{PublicRemotes: []string{"github.com/x/"}, PrivateSources: []string{"/a"}, Allow: []LeakAllow{{Term: "X"}}},
		{PublicRemotes: []string{"github.com/x/"}, PrivateRemotes: []string{"git@github.com:x/y"}, PrivateSources: []string{"/a"}},
		{PublicRemotes: []string{"github.com/x/"}, PrivateRemotes: []string{"https://github.com/x/y"}, PrivateSources: []string{"/a"}},
	} {
		if err := bad.normalize("cfg"); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestLoadNormalize(t *testing.T) {
	l := MachineLoad{Threads: 8}
	if err := l.normalize("cfg"); err != nil || l.Threads != 8 || l.Settle != DefaultLoad.Settle {
		t.Errorf("defaults: %+v, %v", l, err)
	}
	for _, bad := range []MachineLoad{{Threads: -1}, {Poll: -1}, {Settle: -1}} {
		if err := bad.normalize("cfg"); err == nil {
			t.Errorf("%+v: want an error", bad)
		}
	}
}

func TestLoadPressureLimitsMustMoveToAkit(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	for _, key := range []string{"max_io_full", "wait_above"} {
		path := filepath.Join(cfgDir, "graft", "config.yaml")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("load: {"+key+": 10}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, err := Load()
		pressurePath, pathErr := pressure.LimitsPath()
		if pathErr != nil {
			t.Fatal(pathErr)
		}
		if err == nil || !strings.Contains(err.Error(), path+": load: "+key+" moved to "+pressurePath) || !strings.Contains(err.Error(), strings.Join(pressure.LimitKeys, ", ")) {
			t.Errorf("load key %s: got %v, want moved-key error naming %s and keys %v", key, err, pressurePath, pressure.LimitKeys)
		}
	}
}

func TestLimitSettingsForPrecedence(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	got, err := LimitSettingsFor(config.ProjectLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Values != DefaultLimits || got.GateSource != "built-in limits.gate" || got.TaskSource != "built-in limits.task" {
		t.Fatalf("built-in: %+v", got)
	}
	path := filepath.Join(cfgDir, "graft", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("limits: {gate: 2m, task: 4m}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = LimitSettingsFor(config.ProjectLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Values.Gate != 2*time.Minute || got.Values.Task != 4*time.Minute || got.GateSource != "user config limits.gate" {
		t.Fatalf("user config: %+v", got)
	}
	gate, task := 30*time.Second, 6*time.Minute
	got, err = LimitSettingsFor(config.ProjectLimits{Gate: &gate})
	if err != nil {
		t.Fatal(err)
	}
	if got.Values.Gate != gate || got.Values.Task != 4*time.Minute || got.GateSource != "project .graft.yaml limits.gate" || got.TaskSource != "user config limits.task" {
		t.Fatalf("project gate: %+v", got)
	}
	got, err = LimitSettingsFor(config.ProjectLimits{Task: &task})
	if err != nil {
		t.Fatal(err)
	}
	if got.Values.Gate != 2*time.Minute || got.Values.Task != task || got.TaskSource != "project .graft.yaml limits.task" {
		t.Fatalf("project task: %+v", got)
	}
	// A key the user config leaves out is the built-in one, and says so.
	if err := os.WriteFile(path, []byte("limits: {gate: 2m}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = LimitSettingsFor(config.ProjectLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Values.Task != DefaultLimits.Task || got.TaskSource != "built-in limits.task" || got.GateSource != "user config limits.gate" {
		t.Fatalf("user config without task: %+v", got)
	}
}

func TestLimitsNormalize(t *testing.T) {
	l := Limits{Task: time.Hour}
	if err := l.normalize("cfg"); err != nil || l.Gate != time.Minute || l.Task != time.Hour {
		t.Errorf("defaults: %+v, %v", l, err)
	}
	for _, bad := range []Limits{{Gate: -1}, {Task: -1}} {
		if err := bad.normalize("cfg"); err == nil {
			t.Errorf("%+v: want an error", bad)
		}
	}
}
