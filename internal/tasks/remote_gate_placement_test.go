package tasks

import (
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
)

func TestRemoteVouchedSingleStepPlacement(t *testing.T) {
	t.Setenv(HeavyEnv, "")
	vouched := true
	steps := []config.Step{
		{Shell: "echo one", Source: "echo one"},
		{Argv: []string{"sh", "-c", "echo one"}},
	}
	r := &Runner{Root: t.TempDir(), Config: &config.Config{}}
	for name, tc := range map[string]struct {
		task   *config.Task
		step   config.Step
		remote bool
		why    string
	}{
		"one shell step": {
			task: &config.Task{Remote: &vouched, Run: steps[:1]}, step: steps[0], remote: true,
		},
		"one process step": {
			task: &config.Task{Remote: &vouched, Run: steps[1:]}, step: steps[1], remote: true,
		},
		"two steps": {
			task: &config.Task{Remote: &vouched, Run: steps}, step: steps[0],
			why: "a split gate sends a step of a remote: true task only when it is the whole task",
		},
		"with deps": {
			task: &config.Task{Remote: &vouched, Deps: []string{"dep"}, Run: steps[:1]}, step: steps[0],
			why: "a split gate sends a step of a remote: true task only when it is the whole task",
		},
		"nested task": {
			task: &config.Task{Remote: &vouched, Run: []config.Step{{Task: "child"}}}, step: config.Step{Task: "child"},
			why: "a split gate sends a step of a remote: true task only when it is the whole task",
		},
		"install": {
			task: &config.Task{Remote: &vouched, Run: []config.Step{{Install: &config.InstallStep{Path: "bin/tool"}}}},
			step: config.Step{Install: &config.InstallStep{Path: "bin/tool"}}, why: "install steps run on this machine",
		},
		"service": {
			task: &config.Task{Remote: &vouched, Run: []config.Step{{ServiceAction: "restart"}}},
			step: config.Step{ServiceAction: "restart"}, why: "service steps run on this machine",
		},
	} {
		t.Run(name, func(t *testing.T) {
			remote, why, err := r.gateStepPlacement(tc.step, tc.task, name, false)
			if err != nil || remote != tc.remote || why != tc.why {
				t.Fatalf("placement = (%v, %q, %v), want (%v, %q, nil)", remote, why, err, tc.remote, tc.why)
			}
		})
	}
}

func TestGateDoesNotSplitForRemoteInstallOrServiceOnly(t *testing.T) {
	t.Setenv(HeavyEnv, "")
	vouched := true
	for name, step := range map[string]config.Step{
		"install": {Install: &config.InstallStep{Path: "bin/tool"}},
		"service": {ServiceAction: "restart"},
	} {
		t.Run(name, func(t *testing.T) {
			task := &config.Task{Remote: &vouched, Run: []config.Step{step}}
			r := &Runner{Root: t.TempDir(), Config: &config.Config{Tasks: map[string]*config.Task{"only-local": task}}, remoteHeavy: &config.RemoteHeavy{Host: "pc"}}
			goSteps, glintSteps, singleSteps, err := r.remoteGateTaskKinds([]gateStep{{step: config.Step{Task: "only-local"}}})
			if err != nil || goSteps || glintSteps || singleSteps {
				t.Fatalf("remote task with %s step qualified for a split: go=%v glint=%v single=%v err=%v", name, goSteps, glintSteps, singleSteps, err)
			}
		})
	}
}
