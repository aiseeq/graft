package tasks

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"go.yaml.in/yaml/v3"
)

func TestMemoryHighRemoteValidationAndNotApplied(t *testing.T) {
	t.Setenv(HeavyEnv, "")
	t.Setenv(ExecMemoryHighEnv, "")
	t.Setenv(ExecMemoryMaxEnv, "")
	for _, mode := range []string{"task", "exec"} {
		t.Run(mode+" rejects high above ceiling before remote selection", func(t *testing.T) {
			root := t.TempDir()
			cfg := &config.Config{
				RemoteHeavy: &config.RemoteHeavy{Host: "mock", Up: []string{"/bin/true"}, Run: []string{"/usr/bin/env"}},
				Tasks: map[string]*config.Task{"scan": {
					Heavy: true, MemoryMax: yamlScalar("1G"), MemoryHigh: yamlScalar("2G"),
					Run: []config.Step{{Shell: "printf TASK_RAN"}},
				}},
			}
			var stdout, stderr bytes.Buffer
			r := NewRunner(root, cfg, &stdout, &stderr)
			var err error
			if mode == "task" {
				err = r.Run(context.Background(), "scan", nil)
			} else {
				r.ExecMemoryMax, r.ExecMemoryHigh = "1G", "2G"
				err = r.Exec(context.Background(), root, []string{"sh", "-c", "printf EXEC_RAN"}, true, false)
			}
			if err == nil || !strings.Contains(err.Error(), "2.0 GiB") || !strings.Contains(err.Error(), "1.0 GiB") {
				t.Fatalf("run error = %v; want memory high above ceiling", err)
			}
			if r.remoteAsked || strings.Contains(stdout.String(), "TASK_RAN") || strings.Contains(stdout.String(), "EXEC_RAN") {
				t.Fatalf("remote was selected or command ran: asked=%t stdout=%q", r.remoteAsked, stdout.String())
			}
		})

		t.Run(mode+" reports valid high as not applied remotely", func(t *testing.T) {
			root := t.TempDir()
			cfg := &config.Config{
				RemoteHeavy: &config.RemoteHeavy{Host: "mock", Up: []string{"/bin/true"}, Run: []string{"/usr/bin/env"}},
				Tasks: map[string]*config.Task{"scan": {
					Heavy: true, MemoryMax: yamlScalar("2G"), MemoryHigh: yamlScalar("1G"),
					Run: []config.Step{{Shell: "printf TASK_RAN"}},
				}},
			}
			var stdout, stderr bytes.Buffer
			r := NewRunner(root, cfg, &stdout, &stderr)
			var err error
			if mode == "task" {
				err = r.Run(context.Background(), "scan", nil)
			} else {
				r.ExecMemoryMax, r.ExecMemoryHigh = "2G", "1G"
				err = r.Exec(context.Background(), root, []string{"sh", "-c", "printf EXEC_RAN"}, true, false)
			}
			if err != nil {
				t.Fatalf("remote run: %v\n%s", err, stderr.String())
			}
			if want := "graft: memory high 1.0 GiB not applied: remote heavy on mock"; strings.Count(stderr.String(), want) != 1 {
				t.Fatalf("remote not-applied line count: stderr=%q", stderr.String())
			}
			if strings.Contains(stdout.String(), ExecMemoryHighEnv+"=") {
				t.Fatalf("memory high was passed to remote: %q", stdout.String())
			}
		})
	}
}

func TestSplitGateMemoryHighRemotePlacement(t *testing.T) {
	t.Setenv(HeavyEnv, "")
	for _, tc := range []struct {
		name, high string
		wantErr    bool
	}{{"reject above ceiling before remote step", "2G", true}, {"warn when remote step cannot apply high", "1G", false}} {
		t.Run(tc.name, func(t *testing.T) {
			remote := true
			task := &config.Task{
				Heavy: true, Remote: &remote, MemoryMax: yamlScalar("1G"), MemoryHigh: yamlScalar(tc.high),
				Run: []config.Step{{Shell: "printf TASK_RAN"}},
			}
			cfg := &config.Config{RemoteHeavy: &config.RemoteHeavy{Host: "mock"}, Tasks: map[string]*config.Task{"scan": task}}
			var stderr bytes.Buffer
			r := NewRunner(t.TempDir(), cfg, &bytes.Buffer{}, &stderr)
			r.remoteHeavy = cfg.RemoteHeavy
			r.gateSplit = true
			r.frames = []*frame{{task: "scan"}}
			_, err := r.prepareGateStep(context.Background(), task.Run[0], task, nil, false)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "2.0 GiB") || !strings.Contains(err.Error(), "1.0 GiB") {
					t.Fatalf("split placement error = %v", err)
				}
				if r.frames[0].remote || strings.Contains(stderr.String(), "TASK_RAN") {
					t.Fatalf("invalid split task was placed remotely: frame=%+v stderr=%q", r.frames[0], stderr.String())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !r.frames[0].remote || strings.Count(stderr.String(), "graft: memory high 1.0 GiB not applied: remote heavy on mock") != 1 {
				t.Fatalf("remote split placement=%+v stderr=%q", r.frames[0], stderr.String())
			}
			if _, ok := r.placeNoted["memory high 1.0 GiB not applied: remote heavy on mock"]; !ok {
				t.Fatal("remote high warning was not deduplicated")
			}
		})
	}
}

func yamlScalar(value string) (node yaml.Node) {
	node.Kind = yaml.ScalarNode
	node.Tag = "!!str"
	node.Value = value
	return
}
