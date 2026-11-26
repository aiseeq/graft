package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/stats"
)

const remoteCapProgram = `import subprocess,sys
limit=int(sys.argv[1]); head=int(sys.argv[2]); mode=sys.argv[3]
p=subprocess.run(sys.argv[4:],stdout=subprocess.PIPE,stderr=subprocess.PIPE)
sys.stderr.buffer.write(p.stderr)
data=p.stdout
if mode == "cut" and len(data)>limit:
    prefix=data[:head]
    prefix=prefix[:prefix.rfind(b"\n")+1]
    omitted=len(data)-limit
    marker=("[вывод обрезан: пропущено "+str(omitted)+" байт из "+str(len(data))+", предел "+str(limit)+" байт]\n").encode()
    while True:
        start=max(len(prefix),len(data)-(limit-len(prefix)-len(marker)))
        newline=data.find(b"\n",start)
        suffix=data[newline+1:]
        omitted=len(data)-len(prefix)-len(suffix)
        next_marker=("[вывод обрезан: пропущено "+str(omitted)+" байт из "+str(len(data))+", предел "+str(limit)+" байт]\n").encode()
        if next_marker==marker: break
        marker=next_marker
    sys.stdout.buffer.write(prefix+marker+suffix)
elif mode == "abrupt":
    sys.stdout.buffer.write(data[:head])
else:
    sys.stdout.buffer.write(data)
sys.exit(p.returncode)`

func TestRemoteGoTestTruncationKeepsStatisticsWhole(t *testing.T) {
	t.Setenv(HeavyEnv, "")
	t.Setenv(HeavyByEnv, "")
	t.Setenv(HeavyOriginEnv, "")
	stream := strings.Join([]string{
		`{"Action":"output","Package":"example.com/projectA/cached","Output":"ok example.com/projectA/cached (cached)\n"}`,
		`{"Action":"run","Package":"example.com/projectA/open","Test":"TestOpen"}`,
		`{"Action":"run","Package":"example.com/projectA/fails","Test":"TestRealFailure"}`,
		`{"Action":"output","Package":"example.com/projectA/fails","Test":"TestRealFailure","Output":"    synthetic_test.go:1: synthetic failure\n"}`,
		`{"Action":"fail","Package":"example.com/projectA/fails","Test":"TestRealFailure","Elapsed":0.1}`,
		`{"Action":"fail","Package":"example.com/projectA/fails","Elapsed":0.1}`,
		`{"Action":"run","Package":"example.com/projectA/after","Test":"TestAfter"}`,
		`{"Action":"pass","Package":"example.com/projectA/after","Test":"TestAfter","Elapsed":0.1}`,
		`{"Action":"pass","Package":"example.com/projectA/after","Elapsed":0.1}`,
	}, "\n") + "\n"
	padding := strings.Repeat("{\"Action\":\"noise\"}\n", 40)
	stream = strings.Replace(stream, `{"Action":"run","Package":"example.com/projectA/fails","Test":"TestRealFailure"}`, padding+`{"Action":"pass","Package":"example.com/projectA/cached","Elapsed":0}`+"\n"+`{"Action":"pass","Package":"example.com/projectA/open","Test":"TestOpen","Elapsed":0.1}`+"\n"+`{"Action":"pass","Package":"example.com/projectA/open","Elapsed":0.1}`+"\n"+`{"Action":"run","Package":"example.com/projectA/fails","Test":"TestRealFailure"}`, 1)
	stream = strings.Replace(stream, `{"Action":"run","Package":"example.com/projectA/after","Test":"TestAfter"}`, strings.Repeat("{\"Action\":\"noise\"}\n", 40)+`{"Action":"run","Package":"example.com/projectA/after","Test":"TestAfter"}`, 1)

	for _, tc := range []struct {
		name, mode string
		limit      int
		wantCut    bool
	}{
		{name: "marked truncation", mode: "cut", limit: 700, wantCut: true},
		{name: "full stream", mode: "cut", limit: 10000},
		{name: "abrupt without marker", mode: "abrupt", limit: 700},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			root := t.TempDir()
			stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			python, err := exec.LookPath("python3")
			if err != nil {
				t.Skip("python3 is needed by the synthetic remote wrapper")
			}
			shell := "printf '%s' '" + stream + "'; exit 7"
			cfg := &config.Config{
				RemoteHeavy: &config.RemoteHeavy{
					Host: "synthetic",
					Up:   []string{"/bin/true"},
					Run:  []string{python, "-c", remoteCapProgram, fmt.Sprint(tc.limit), "256", tc.mode},
				},
				Tasks: map[string]*config.Task{
					"tests": {
						Heavy: true,
						Tests: config.TestsGo,
						Run:   []config.Step{{Shell: shell, Source: "tests: go"}},
					},
				},
			}
			r := NewRunner(root, cfg, stdout, stderr)
			r.noRepoName = true
			r.Stats = stats.NewRecorder(stderr)
			err = r.Run(context.Background(), "tests", nil)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
				t.Fatalf("step error = %v, want exit 7; stdout=%s stderr=%s", err, stdout, stderr)
			}
			path, err := stats.Path()
			if err != nil {
				t.Fatal(err)
			}
			records, readErr := os.ReadFile(stats.TestsPath(path))
			if tc.wantCut {
				if readErr == nil && len(bytes.TrimSpace(records)) != 0 {
					t.Fatalf("truncated step wrote test records: %s", records)
				}
				if !strings.Contains(stderr.String(), "bytes omitted)") || !strings.Contains(stderr.String(), "its test statistics are not recorded") || !strings.Contains(stderr.String(), "may be missing from the output") || !strings.Contains(stderr.String(), "GRAFT_HEAVY=local") {
					t.Fatalf("truncation warning missing details: %s", stderr)
				}
				if strings.Count(stderr.String(), "remote output of this tests: go step was cut") != 1 {
					t.Fatalf("got other than one truncation warning: %s", stderr)
				}
				if strings.Contains(stdout.String(), "TestOpen: fail") || strings.Contains(stdout.String(), "skipped ") {
					t.Fatalf("truncated stream printed an incomplete test conclusion: %s", stdout)
				}
				statsBytes, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var stepRecords []map[string]any
				for line := range bytes.SplitSeq(bytes.TrimSpace(statsBytes), []byte("\n")) {
					var record map[string]any
					if err := json.Unmarshal(line, &record); err != nil {
						t.Fatal(err)
					}
					if record["kind"] == stats.KindStep {
						stepRecords = append(stepRecords, record)
					}
				}
				if len(stepRecords) != 1 || stepRecords[0]["reason"] != nil {
					t.Fatalf("truncated step reason = %v, want none", stepRecords)
				}
				return
			}
			if readErr != nil {
				t.Fatal(readErr)
			}
			var lines []map[string]any
			for line := range bytes.SplitSeq(bytes.TrimSpace(records), []byte("\n")) {
				var record map[string]any
				if err := json.Unmarshal(line, &record); err != nil {
					t.Fatal(err)
				}
				lines = append(lines, record)
			}
			if tc.mode == "abrupt" {
				if len(lines) != 1 || lines[0]["result"] != stats.ResultFail {
					t.Fatalf("abrupt stream records = %v, want unfinished package fail", lines)
				}
			} else if len(lines) != 3 || lines[0]["package"] != "example.com/projectA/open" || lines[1]["package"] != "example.com/projectA/fails" || lines[2]["package"] != "example.com/projectA/after" {
				t.Fatalf("complete stream records = %v, want open, fail and post-cut packages (cached omitted)", lines)
			}
		})
	}
}

func TestRemoteGoTestTruncationGuards(t *testing.T) {
	t.Setenv(HeavyEnv, "")
	t.Setenv(HeavyByEnv, "")
	t.Setenv(HeavyOriginEnv, "")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	root := t.TempDir()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is needed by the synthetic remote wrapper")
	}
	remote := &config.RemoteHeavy{Host: "synthetic", Up: []string{"/bin/true"}, Run: []string{python, "-c", remoteCapProgram, "2600", "2400", "cut"}}
	for _, tc := range []struct {
		name, stream, step, source string
		wantExit                   int
		wantNoJSON                 bool
		forbiddenReason            string
	}{
		{
			name: "truncated stream omits parsed failure reason",
			stream: `{"Action":"build-output","Package":"example.com/projectA/early-fail","Output":"synthetic build failure\n"}` + "\n" +
				`{"Action":"fail","Package":"example.com/projectA/early-fail","FailedBuild":"compile"}` + "\n" + strings.Repeat("noise\n", 800),
			step: "go test example.com/projectA/...", source: "tests: go", wantExit: 7, forbiddenReason: "synthetic build failure",
		},
		{
			name: "missing run keeps successful exit",
			stream: `{"Action":"run","Package":"example.com/projectA/present","Test":"TestPresent"}` + "\n" +
				`{"Action":"pass","Package":"example.com/projectA/present","Test":"TestPresent"}` + "\n" +
				`{"Action":"pass","Package":"example.com/projectA/present"}` + "\n" + strings.Repeat("noise\n", 800),
			step: "go test -run Missing example.com/projectA/...", source: "tests: go", wantExit: 0,
		},
		{
			name:   "no json does not add warning",
			stream: strings.Repeat("plain output\n", 2000), step: "yes x | head -n 2000; exit 7",
			source: "go test -json example.com/projectA/...", wantExit: 7, wantNoJSON: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			step := config.Step{Source: tc.source}
			if strings.HasPrefix(tc.step, "go test") {
				bin := t.TempDir()
				goFake := filepath.Join(bin, "go")
				if err := os.WriteFile(goFake, []byte("#!/bin/sh\nprintf '%s' '"+tc.stream+"'\nexit "+fmt.Sprint(tc.wantExit)+"\n"), 0o700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
				step.Argv = strings.Fields(tc.step)
			} else {
				step.Shell = tc.step
			}
			cfg := &config.Config{
				RemoteHeavy: remote,
				Tasks:       map[string]*config.Task{"tests": {Heavy: true, Tests: config.TestsGo, Run: []config.Step{step}}},
			}
			r := NewRunner(root, cfg, stdout, stderr)
			r.noRepoName = true
			r.Stats = stats.NewRecorder(stderr)
			err := r.Run(context.Background(), "tests", nil)
			if tc.wantExit == 0 && err != nil {
				t.Fatalf("step error = %v, want success; stderr=%s", err, stderr)
			}
			var exitErr *exec.ExitError
			if tc.wantExit != 0 && (!errors.As(err, &exitErr) || exitErr.ExitCode() != tc.wantExit) {
				t.Fatalf("step error = %v, want exit %d; stderr=%s", err, tc.wantExit, stderr)
			}
			if tc.wantNoJSON && strings.Contains(stderr.String(), "printed no -json events") {
				t.Fatalf("truncated stream got the no-JSON warning: %s", stderr)
			}
			path, err := stats.Path()
			if err != nil {
				t.Fatal(err)
			}
			statsBytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var reason string
			for line := range bytes.SplitSeq(bytes.TrimSpace(statsBytes), []byte("\n")) {
				var record map[string]any
				if err := json.Unmarshal(line, &record); err != nil {
					t.Fatal(err)
				}
				if record["kind"] == stats.KindStep && record["reason"] != nil {
					reason = fmt.Sprint(record["reason"])
				}
			}
			if tc.forbiddenReason != "" && strings.Contains(reason, tc.forbiddenReason) {
				t.Fatalf("truncated step reason contains %q: %q", tc.forbiddenReason, reason)
			}
			if strings.Contains(reason, "-run names") {
				t.Fatalf("truncated step reason contains missing -run conclusion: %q", reason)
			}
			if !strings.Contains(stderr.String(), "remote output of this tests: go step was cut") {
				t.Fatalf("missing truncation warning: %s", stderr)
			}
		})
	}
}

func TestLocalGoTestMarkerIsOrdinaryOutput(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	marker := "[вывод обрезан: пропущено 12 байт из 99, предел 80 байт]"
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	step := config.Step{Source: "tests: go", Shell: "printf '%s\\n' '" + marker + "'; printf '%s\\n' '{\"Action\":\"run\",\"Package\":\"example.com/projectA/local\",\"Test\":\"TestLocal\"}' '{\"Action\":\"pass\",\"Package\":\"example.com/projectA/local\",\"Test\":\"TestLocal\"}' '{\"Action\":\"pass\",\"Package\":\"example.com/projectA/local\"}'"}
	cfg := &config.Config{RemoteHeavy: &config.RemoteHeavy{Host: "synthetic", Up: []string{"/bin/true"}, Run: []string{"/bin/true"}}, Tasks: map[string]*config.Task{"tests": {Tests: config.TestsGo, Run: []config.Step{step}}}}
	r := NewRunner(t.TempDir(), cfg, stdout, stderr)
	r.noRepoName = true
	r.Stats = stats.NewRecorder(stderr)
	if err := r.Run(context.Background(), "tests", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), marker) || strings.Contains(stderr.String(), "remote output of this tests: go step was cut") {
		t.Fatalf("local marker handling: stdout=%s stderr=%s", stdout, stderr)
	}
}
