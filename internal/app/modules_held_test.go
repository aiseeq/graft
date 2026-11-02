package app

import (
	"regexp"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
)

func TestModulesHeldBy(t *testing.T) {
	release := regexp.MustCompile(`^release/.*$`)
	hotfix := regexp.MustCompile(`^hotfix/.*$`)
	withDeploy := &config.Config{
		Version: config.Version{Build: []*regexp.Regexp{release}},
		Deploy:  &config.Deploy{Hotfix: &config.Hotfix{Match: []*regexp.Regexp{hotfix}}},
	}
	noDeploy := &config.Config{Version: config.Version{Build: []*regexp.Regexp{release}}}
	for _, tc := range []struct {
		cfg    *config.Config
		branch string
		want   string
	}{
		{withDeploy, "release/2.19", "version.build_branches"},
		{withDeploy, "hotfix/PROJ-1", "deploy.hotfix.branches"},
		{withDeploy, "main", ""},
		{withDeploy, "", ""},
		{noDeploy, "release/2.19", "version.build_branches"},
		{noDeploy, "hotfix/PROJ-1", ""},
		{&config.Config{}, "release/2.19", ""},
	} {
		if got := modulesHeldBy(tc.cfg, tc.branch); got != tc.want {
			t.Errorf("modulesHeldBy(%q) = %q, want %q", tc.branch, got, tc.want)
		}
	}
}
