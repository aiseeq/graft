package npmmods

import "testing"

func TestNormalizeGitPath(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"git+ssh://git@github.com/org/repo.git", "github.com/org/repo"},
		{"git@github.com:org/repo.git", "github.com/org/repo"},
		{"ssh://git@ssh.github.com/org/repo.git", "github.com/org/repo"},
		{"https://github.com/org/repo.git", "github.com/org/repo"},
		{"https://user@github.com/org/repo", "github.com/org/repo"},
	} {
		if got := normalizeGitPath(tc.input); got != tc.want {
			t.Errorf("normalizeGitPath(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestParseGitTagSpec(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec string
		want bool
	}{
		{"plain semver tag", "git+ssh://git@example.org/projectA/dep.git#v1.2.3", true},
		{"branch", "git+ssh://git@example.org/projectA/dep.git#main", false},
		{"commit", "git+ssh://git@example.org/projectA/dep.git#0123456789abcdef", false},
		{"range", "git+ssh://git@example.org/projectA/dep.git#^1.2.3", false},
		{"no selector", "git+ssh://git@example.org/projectA/dep.git", false},
		{"github shorthand", "github:projectA/dep#v1.2.3", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, got := parseGitSpec(tc.spec)
			if got != tc.want {
				t.Errorf("parseGitSpec(%q) accepted = %v, want %v", tc.spec, got, tc.want)
			}
		})
	}
}

func TestShouldRaise(t *testing.T) {
	for _, tc := range []struct {
		current, latest string
		want            bool
	}{
		{"v1.2.3", "v1.2.4", true},
		{"v1.2.3", "v1.2.3", false},
		{"v1.2.4", "v1.2.3", false},
	} {
		if got := shouldRaise(tc.current, tc.latest); got != tc.want {
			t.Errorf("shouldRaise(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
		}
	}
}

func TestTagPreReleasePolicy(t *testing.T) {
	for _, tc := range []struct {
		current, candidate string
		want               bool
	}{
		{"v1.2.0", "v1.3.0-rc.1", false},
		{"v1.2.0-rc.1", "v1.2.0-rc.2", true},
		{"v1.2.0-rc.1", "v1.2.0", true},
		{"v1.2.0", "v1.3.0", true},
	} {
		if got := includesTag(tc.current, tc.candidate); got != tc.want {
			t.Errorf("includesTag(%q, %q) = %v, want %v", tc.current, tc.candidate, got, tc.want)
		}
	}
}
