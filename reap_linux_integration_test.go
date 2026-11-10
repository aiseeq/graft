//go:build linux

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// orphanScripts leave an orphan that writes its parent's command line to
// the file named by orphan.sh's argument: the parent it has once the shell
// that started it is gone. Nothing here sends a signal.
var orphanScripts = map[string]string{
	"orphan.sh": `out=$1
sh -c '(sleep 0.1; exec sh ./parent.sh "$0") & exit 0' "$out"
i=0
while [ ! -s "$out" ] && [ $i -lt 200 ]; do sleep 0.05; i=$((i+1)); done
`,
	"parent.sh": `tr '\0' ' ' < /proc/$PPID/cmdline > "$1.tmp" && mv "$1.tmp" "$1"
`,
}

// orphanParent is the command line of the orphan's parent that orphan.sh
// wrote to out.
func orphanParent(t *testing.T, out string) string {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if data, err := os.ReadFile(out); err == nil && len(data) > 0 {
			return strings.TrimSpace(string(data))
		}
	}
	t.Fatalf("the orphan wrote no parent to %s", out)
	return ""
}

// TestOrphansOfCommandsStayWithGraft: the orphan a task step, graft exec
// or graft reap leaves has graft's reaper for its parent, not the session
// manager, so its kill "$PPID" cannot end the user's session.
func TestOrphansOfCommandsStayWithGraft(t *testing.T) {
	f := newRepo(t, orphanScripts)
	taskOut := filepath.Join(t.TempDir(), "parent")
	f.write(".graft.yaml", `schema: 1
version: {mode: none}
tasks:
  orphan:
    run: [[sh, orphan.sh, '`+taskOut+`']]
`)
	for _, c := range []struct {
		name   string
		args   []string
		parent string
	}{
		{"task", []string{"orphan"}, "graft-reap "},
		{"exec", []string{"exec", "--", "sh", "orphan.sh"}, "graft-reap "},
		{"reap", []string{"reap", "--", "sh", "orphan.sh"}, graftBin + " reap -- sh orphan.sh"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, args := taskOut, c.args
			if c.name != "task" {
				out = filepath.Join(t.TempDir(), "parent")
				args = append(slices.Clone(c.args), out)
			}
			if got, code := f.graft("", args...); code != 0 {
				t.Fatalf("exit %d\n%s", code, got)
			}
			if parent := orphanParent(t, out); !strings.HasPrefix(parent, c.parent) {
				t.Errorf("the orphan's parent is %q, want %q…", parent, c.parent)
			}
		})
	}
}
