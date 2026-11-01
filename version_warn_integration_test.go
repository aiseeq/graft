package main

import (
	"strings"
	"testing"
)

// TestCommitWarnsWithoutVersion: graft commit warns on a project in mode
// none that does not say why, and on one in git-tag mode before its first
// version tag; a reason or a tag silences it.
func TestCommitWarnsWithoutVersion(t *testing.T) {
	gate := "gate:\n  - [sh, -c, 'true']\n"
	for _, c := range []struct {
		name, version, want string
	}{
		{"none without reason", "{mode: none}", "the project has no version (version.mode none)"},
		{"none with reason", "{mode: none, reason: 'notes, never released'}", ""},
		{"git-tag without tags", "{mode: git-tag}", "no v* version tag; graft release tags the first, v0.1.0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: " + c.version + "\n" + gate})
			f.write("a.txt", "a\n")
			out := f.mustGraft("commit", "-m", "feat: a")
			if c.want == "" {
				if strings.Contains(out, "has no version") {
					t.Errorf("warned:\n%s", out)
				}
				return
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("no %q in:\n%s", c.want, out)
			}
			if c.version != "{mode: git-tag}" {
				return
			}
			f.git("tag", "-a", "v0.1.0", "-m", "v0.1.0")
			f.write("b.txt", "b\n")
			if out := f.mustGraft("commit", "-m", "feat: b"); strings.Contains(out, "has no version") {
				t.Errorf("warned after the first tag:\n%s", out)
			}
		})
	}
}
