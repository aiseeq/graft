package hooks

import (
	"strings"
	"testing"
)

// TestShimInstallsTheLatest: the install hint of a shim names the latest
// release: graft versions are not pinned, a project's minimum included.
func TestShimInstallsTheLatest(t *testing.T) {
	for _, name := range Names {
		if shim := string(Shim(name)); !strings.Contains(shim, "go install github.com/aiseeq/graft/v2@latest") {
			t.Errorf("the %s shim:\n%s", name, shim)
		}
	}
}
