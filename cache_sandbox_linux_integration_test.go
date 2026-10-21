//go:build linux

package main

import (
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/proc/proctest"
)

// TestCacheCommandRefusedInASandbox: a cache command SELinux would refuse
// to start under no_new_privs is not started, an alert each time; the task
// runs without the cache and says why.
func TestCacheCommandRefusedInASandbox(t *testing.T) {
	if !proctest.Sandboxed(t, "TestCacheCommandRefusedInASandbox") {
		return
	}
	f := newRepo(t, map[string]string{".graft.yaml": `schema: 1
version: {mode: none}
tasks:
  verify:
    cache:
      commands: [[docker, image, inspect, -f, '{{.Id}}', 'postgres:18']]
    run: [` + helperStep("ok") + `]
`})
	out, code := f.graft("", "verify")
	if code != 0 || !strings.Contains(out, "graft: verify: not looked up in the cache, running: cache.commands docker image inspect") ||
		!strings.Contains(out, "cannot start here: this process runs with no_new_privs") {
		t.Errorf("exit %d\n%s", code, out)
	}
}
