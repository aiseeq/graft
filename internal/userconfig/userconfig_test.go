package userconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/pressure"
)

func TestRemoteIsPublic(t *testing.T) {
	l := &Leaks{PublicRemotes: []string{"github.com/someone/"}}
	for url, want := range map[string]bool{
		"git@github.com:someone/tool.git":           true,
		"https://github.com/someone/tool":           true,
		"ssh://git@github.com/someone/tool.git":     true,
		"https://token@github.com/someone/tool.git": true,
		"git@github.com:other/tool.git":             false,
		"git@bitbucket.org:someone/tool.git":        false,
		"/srv/git/someone/tool.git":                 false,
	} {
		if got := l.RemoteIsPublic(url); got != want {
			t.Errorf("%s: %v, want %v", url, got, want)
		}
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
func TestRemoteIsPublicPrivateOverrides(t *testing.T) {
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
