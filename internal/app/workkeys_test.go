package app

import (
	"regexp"
	"slices"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
)

func TestWorkKeys(t *testing.T) {
	re := regexp.MustCompile(`\bPROJ-[0-9]+\b`)
	attributed := &config.ApprovedRelease{Attributed: map[string]config.Attribution{"abc1234": {Item: "PROJ-9"}}}
	for _, tc := range []struct {
		name          string
		sha           string
		subject, body string
		conf          *config.ApprovedRelease
		want          []string
	}{
		{name: "a mention in the body", subject: "fix(balance): PROJ-1 no error", body: "Undoes what PROJ-2 added.\n", want: []string{"PROJ-1"}},
		{name: "only a mention in the body", subject: "docs: notes", body: "After PROJ-2.\n", want: nil},
		{name: "a run of keys", subject: "feat(cards): PROJ-3 PROJ-1 PROJ-2 the screen", want: []string{"PROJ-3", "PROJ-1", "PROJ-2"}},
		{name: "commas and words", subject: "fix: PROJ-1, PROJ-2 and PROJ-3 и PROJ-4 + PROJ-5 / PROJ-6 & PROJ-7 x", want: []string{"PROJ-1", "PROJ-2", "PROJ-3", "PROJ-4", "PROJ-5", "PROJ-6", "PROJ-7"}},
		{name: "a key in brackets at the end", subject: "docs(instructions): night edits (PROJ-1, PROJ-2)", want: []string{"PROJ-1", "PROJ-2"}},
		{name: "a mention in the subject", subject: "chore(deploy): PROJ-1 the standing PROJ-2 in skip_keys", want: []string{"PROJ-1"}},
		{name: "a clause of its own", subject: "fix(rates): PROJ-1 the rate; PROJ-2 the name", want: []string{"PROJ-1", "PROJ-2"}},
		{name: "a run after a clause, then a mention", subject: "fix: PROJ-1 a; PROJ-2 PROJ-3 b after PROJ-4", want: []string{"PROJ-1", "PROJ-2", "PROJ-3"}},
		{name: "a semicolon not right before the key", subject: "fix: PROJ-1 a; after PROJ-2 b", want: []string{"PROJ-1"}},
		{name: "a key line of the body", subject: "fix: no key", body: "Why.\n\nPROJ-1\n", want: []string{"PROJ-1"}},
		{name: "key lines of the body", subject: "fix: no key", body: "PROJ-1\nPROJ-2\nPROJ-1\n", want: []string{"PROJ-1", "PROJ-2"}},
		{name: "the subject before the body", subject: "fix: PROJ-1", body: "PROJ-2\n", want: []string{"PROJ-1"}},
		{name: "repeated", subject: "fix: PROJ-1 PROJ-1 x", want: []string{"PROJ-1"}},
		{name: "attribution over the message", sha: "abc1234", subject: "fix: PROJ-1 x", conf: attributed, want: []string{"PROJ-9"}},
		{name: "a revert of an attributed commit", subject: `Revert "fix: x"`, body: "This reverts commit abc1234.\n", conf: attributed, want: []string{"PROJ-9"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := workKeys(re, tc.conf, deliveredCommit{sha: tc.sha, subject: tc.subject, body: tc.body})
			if !slices.Equal(got, tc.want) {
				t.Errorf("workKeys = %v, want %v", got, tc.want)
			}
		})
	}
}
