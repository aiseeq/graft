package config

import (
	"strings"
	"testing"
)

// release_notes.jira.work_keys is no longer a switch: graft always counts a
// commit for the items it is the work of. The key left in a config, either
// value, is an error that names it and says to delete it.
func TestWorkKeysKeyRemoved(t *testing.T) {
	for _, v := range []string{"true", "false"} {
		_, err := Parse([]byte("schema: 1\nversion: {mode: file}\nrelease_notes: {jira: {project_keys: [PROJ], work_keys: " + v + "}}\n"))
		if err == nil || !strings.Contains(err.Error(), "release_notes.jira.work_keys") || !strings.Contains(err.Error(), "delete") {
			t.Errorf("work_keys: %s: want an error that names release_notes.jira.work_keys and says to delete it, got %v", v, err)
		}
	}
}
