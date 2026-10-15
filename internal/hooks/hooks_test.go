package hooks

import "testing"

// TestInstallHint: the hint installs a version the v2 module path reaches.
func TestInstallHint(t *testing.T) {
	for min, want := range map[string]string{
		"":      "go install github.com/aiseeq/graft/v2@latest",
		"1.3.0": "go install github.com/aiseeq/graft/v2@latest",
		"2.3.1": "go install github.com/aiseeq/graft/v2@v2.3.1",
	} {
		if got := InstallHint(min); got != want {
			t.Errorf("InstallHint(%q) = %q, want %q", min, got, want)
		}
	}
}
