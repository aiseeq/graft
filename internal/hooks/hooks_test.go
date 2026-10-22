package hooks

import (
	"strings"
	"testing"
)

// TestShimInstallsTheLatest: the install hint of a shim installs the latest
// from a checkout: graft versions are not pinned, a project's minimum
// included.
func TestShimInstallsTheLatest(t *testing.T) {
	for _, name := range Names {
		if shim := string(Shim(name)); !strings.Contains(shim, "git pull && go run . install") {
			t.Errorf("the %s shim:\n%s", name, shim)
		}
	}
}
