package app

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/jira"
)

func TestSweepDroppedSubtasks(t *testing.T) {
	const parentKey, doneKey, droppedKey = "PROJ-123", "PROJ-124", "PROJ-125"
	parent := jira.Issue{Key: parentKey, Status: "Ready for release", AssigneeID: "owner", Subtasks: []string{doneKey, droppedKey}}
	makeSweep := func(commits []sweptCommit) (*sweepRun, *bytes.Buffer) {
		var output bytes.Buffer
		transition := config.NoteTransition{From: []string{"Ready for release"}, To: "Done"}
		target := &config.DeployTarget{ReleaseNotes: &config.TargetNotes{Transitions: []config.NoteTransition{transition}}}
		cfg := &config.Config{Deploy: &config.Deploy{Targets: map[string]*config.DeployTarget{"prod": target}}}
		s := &sweepRun{
			app:   &App{Stdout: &output, Stderr: &output},
			r:     &deployRun{name: "prod", target: target, cfg: cfg},
			conf:  &config.Sweep{Days: 30, Done: "Done", Dropped: []string{"Won't do"}},
			notes: &config.JiraNotes{},
			me:    "owner",
			items: map[string][]sweptCommit{doneKey: {{sha: strings.Repeat("a", 40), onTarget: true}}, droppedKey: commits},
			win:   &sweepWindow{read: true, reverted: map[string]bool{}, older: map[string]bool{}, local: map[string]bool{}},
			issues: map[string]jira.Issue{
				doneKey:    {Key: doneKey, Status: "Done", Done: true},
				droppedKey: {Key: droppedKey, Status: "Won't do", Done: true},
			},
			judged: map[string]bool{}, glint: &glintCheck{},
		}
		return s, &output
	}
	assertHeld := func(t *testing.T, s *sweepRun, output *bytes.Buffer, want string) {
		t.Helper()
		s.finishParent(context.Background(), parent)
		got := strings.Join(s.lines, "\n")
		if !strings.Contains(got, want) {
			t.Fatalf("sweep lines do not contain %q:\n%s\noutput:\n%s", want, got, output.String())
		}
		if strings.Contains(output.String(), "would move to Done") {
			t.Fatalf("held parent was listed for a move:\n%s", output.String())
		}
	}

	t.Run("no commits at all allows parent transition", func(t *testing.T) {
		s, output := makeSweep(nil)
		s.finishParent(context.Background(), parent)
		if !strings.Contains(output.String(), "PROJ-123 would move to Done") {
			t.Fatalf("parent was not listed for transition:\n%s", output.String())
		}
	})
	t.Run("older work still holds parent", func(t *testing.T) {
		s, output := makeSweep(nil)
		s.win.older[droppedKey] = true
		assertHeld(t, s, output, "PROJ-125 has work older than the last 30 days")
	})
	t.Run("local work still holds parent", func(t *testing.T) {
		s, output := makeSweep(nil)
		s.win.local[droppedKey] = true
		assertHeld(t, s, output, "PROJ-125 has work only on a local branch")
	})
	t.Run("window commit off target still holds parent", func(t *testing.T) {
		s, output := makeSweep([]sweptCommit{{sha: strings.Repeat("b", 40), onTarget: false}})
		assertHeld(t, s, output, "its subtask PROJ-125 has work not on prod")
	})
	t.Run("reverted work still holds parent", func(t *testing.T) {
		s, output := makeSweep(nil)
		s.win.reverted[droppedKey] = true
		assertHeld(t, s, output, "a revert of the last 30 days took work of PROJ-125 out")
	})
	t.Run("won't do subtask without the dropped setting still holds parent", func(t *testing.T) {
		s, output := makeSweep(nil)
		s.conf.Dropped = nil
		assertHeld(t, s, output, "its subtask PROJ-125 has no commit")
	})
	t.Run("ordinary done subtask without commits still holds parent", func(t *testing.T) {
		s, output := makeSweep(nil)
		s.conf.Dropped = nil
		s.issues[droppedKey] = jira.Issue{Key: droppedKey, Status: "Done", Done: true}
		assertHeld(t, s, output, "its subtask PROJ-125 has no commit")
	})
}
