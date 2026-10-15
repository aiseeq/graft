package userconfig

import "testing"

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
	if err := l.normalize("cfg"); err != nil || l.FreeBelow != 16 || l.WaitAbove != 22 || l.Threads != 8 || l.Settle != DefaultLoad.Settle {
		t.Errorf("defaults: %+v, %v", l, err)
	}
	for _, bad := range []MachineLoad{{FreeBelow: 22, WaitAbove: 16}, {Threads: -1}, {Poll: -1}} {
		if err := bad.normalize("cfg"); err == nil {
			t.Errorf("%+v: want an error", bad)
		}
	}
}
