package app

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/jira"
)

func TestApprovedReleaseRevertOfRevertUsesItsOwnKey(t *testing.T) {
	for _, tc := range []struct {
		name       string
		r2Key      string
		gitR2      bool
		noR2       bool
		wantFail   bool
		wantPassed string
	}{
		{name: "conventional return without key is refused", wantFail: true},
		{name: "conventional return with approved key passes", r2Key: "PROJ-2", wantPassed: "PROJ-2"},
		{name: "one revert still removes work", noR2: true},
		{name: "git revert of revert is checked by its key", gitR2: true, wantFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newEmptyWithheldRepo(t)
			prev := r.commitText("work.txt", "base\n", "base")
			w := r.commitText("work.txt", "work\n", "fix: PROJ-1 work")
			r.revert(w)
			r1 := r.head()
			if !tc.noR2 {
				if tc.gitR2 {
					r.revert(r1)
				} else {
					subject := "revert: restore"
					if tc.r2Key != "" {
						subject += " " + tc.r2Key
					}
					r.git("commit", "--allow-empty", "-m", subject, "-m", fmt.Sprintf("This reverts commit %s.", r1))
				}
			}
			conf := &config.ApprovedRelease{Statuses: []string{"Ready for release"}}
			var output bytes.Buffer
			gate := &gateRun{
				app:   &App{Stdout: &output},
				repo:  r.repo,
				cfg:   &config.Config{},
				conf:  conf,
				keyRe: regexp.MustCompile(`PROJ-[0-9]+`),
				gate: &approvalGate{conf: conf, issues: map[string]jira.Issue{
					"PROJ-1": {Key: "PROJ-1", Status: "In Progress"},
					"PROJ-2": {Key: "PROJ-2", Status: "Ready for release"},
				}},
			}
			out, err := gate.run(context.Background(), prev, r.head(), nil, false)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(out.failures) > 0; got != tc.wantFail {
				t.Fatalf("failures = %+v, want failure %t; output:\n%s", out.failures, tc.wantFail, output.String())
			}
			if tc.noR2 && !strings.Contains(output.String(), "reverted by "+r.git("rev-parse", "--short", r1)) {
				t.Errorf("output = %q, want W reported as reverted by R1", output.String())
			}
			if tc.wantPassed != "" && !contains(out.passed, tc.wantPassed) {
				t.Errorf("passed = %v, want %s", out.passed, tc.wantPassed)
			}
		})
	}
}

func TestApprovedReleaseConventionalRevertOfOrdinaryCommitPasses(t *testing.T) {
	r := newEmptyWithheldRepo(t)
	prev := r.commitText("work.txt", "base\n", "base")
	w := r.commitText("work.txt", "work\n", "ordinary change")
	r.git("commit", "--allow-empty", "-m", "revert: x", "-m", fmt.Sprintf("This reverts commit %s.", w))
	conf := &config.ApprovedRelease{Statuses: []string{"Ready for release"}}
	gate := &gateRun{
		app:   &App{Stdout: &bytes.Buffer{}},
		repo:  r.repo,
		cfg:   &config.Config{},
		conf:  conf,
		keyRe: regexp.MustCompile(`PROJ-[0-9]+`),
		gate:  &approvalGate{conf: conf, issues: map[string]jira.Issue{}},
	}
	out, err := gate.run(context.Background(), prev, r.head(), nil, false)
	if err != nil {
		t.Fatalf("approved release = (%+v, %v), want pass", out, err)
	}
	if len(out.failures) != 0 {
		t.Fatalf("failures = %+v, want none", out.failures)
	}
}

func contains(values []string, value string) bool {
	return slices.Contains(values, value)
}
