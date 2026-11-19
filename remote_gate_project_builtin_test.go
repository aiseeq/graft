package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/tasks"
)

func projectBuiltinRepo(t *testing.T) (*fixture, string) {
	t.Helper()
	f, state := userRemoteRepo(t, "")
	script := filepath.Join(state, "project.sh")
	writeFile(t, script, "exec \"$@\"\n")
	cfg := "schema: 1\nversion: {mode: none}\ngo_fix: {enabled: true}\ngo_fmt: {enabled: false, reason: synthetic test}\n" +
		"remote_heavy: {host: box, up: [/bin/true], run: [sh, " + yamlQuote(script) + "], threads: 4, broken_exit: 125}\n" +
		"gate: [{task: unit}, {task: script}]\ntasks:\n  unit: {heavy: true, run: [[sh, -c, 'echo task']]}\n  script: {run: [[sh, -c, 'echo local']]}\n"
	f.write(".graft.yaml", cfg)
	return f, state
}

func TestProjectRemoteGateBuiltinPlacement(t *testing.T) {
	f, state := projectBuiltinRepo(t)
	script := filepath.Join(state, "project.sh")
	writeFile(t, script, "echo \"$*\" >> "+yamlQuote(filepath.Join(state, "project-calls"))+"\nexec \"$@\"\n")
	out := f.mustGraft("gate")
	calls := readFile(t, filepath.Join(state, "project-calls"))
	for _, want := range []string{"go vet ./...", "go fix -diff ./...", "echo task"} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing host call %q: %q\n%s", want, calls, out)
		}
	}
	if strings.Contains(calls, "echo local") || strings.Contains(out, "busy here:") {
		t.Errorf("wrong placement: %q\n%s", calls, out)
	}
	found := map[string]bool{}
	for _, rec := range readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl")) {
		if rec.Kind == "step" && rec.Host == "box" {
			found[rec.Step] = true
		}
	}
	for _, step := range []string{tasks.GoVetSource, tasks.GoFixSource} {
		if !found[step] {
			t.Errorf("missing host stats for %s: %+v", step, found)
		}
	}
}

func TestProjectRemoteGateBuiltinVetFailure(t *testing.T) {
	f, state := projectBuiltinRepo(t)
	script := filepath.Join(state, "project.sh")
	writeFile(t, script, "shift\nexec \"$@\"\n")
	f.write(".graft.yaml", strings.Replace(f.read(".graft.yaml"), yamlQuote(script)+"], threads", yamlQuote(script)+", '{root}'], threads", 1))
	f.write("sub/go.mod", "module example.com/sub\n\ngo 1.22\n")
	f.write("sub/a.go", "package sub\nimport \"fmt\"\nfunc bad() { fmt.Printf(\"%d\", \"wrong\") }\n")
	out, code := f.graft("", "gate")
	if code == 0 || !strings.Contains(out, "Printf format %d") || !strings.Contains(out, `gate failed at "go vet ./... (sub)"`) || strings.Contains(out, "path broken") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	counts := map[string]int{}
	for _, rec := range readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl")) {
		if rec.Kind == "step" && (rec.Step == tasks.GoVetSource || rec.Step == tasks.GoVetSource+" (sub)") {
			if rec.Host != "box" {
				t.Errorf("vet %q ran on %q: %+v", rec.Step, rec.Host, rec)
			} else {
				counts[rec.Step]++
			}
		}
	}
	for _, step := range []string{tasks.GoVetSource, tasks.GoVetSource + " (sub)"} {
		if counts[step] != 1 {
			t.Errorf("host ran %q %d times, want once", step, counts[step])
		}
	}
}

func TestProjectRemoteGateBuiltinFallback(t *testing.T) {
	for _, mode := range []string{"broken", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			f, state := projectBuiltinRepo(t)
			script := filepath.Join(state, "project.sh")
			writeFile(t, script, "echo transport down >&2\nexit 125\n")
			cfg := f.read(".graft.yaml")
			if mode == "unavailable" {
				cfg = strings.Replace(cfg, "up: [/bin/true]", "up: [/bin/false]", 1)
			}
			f.write(".graft.yaml", cfg)
			out := f.mustGraft("gate")
			want := "box path broken: run exited 125"
			if mode == "unavailable" {
				want = "box did not answer"
			}
			if !strings.Contains(out, want) {
				t.Errorf("missing transport fallback %q:\n%s", want, out)
			}
			found := map[string]bool{}
			gateLines := 0
			for _, rec := range readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl")) {
				if rec.Kind == "task" && rec.Task == "gate" {
					gateLines++
					if strings.Contains(rec.Here, "box") {
						t.Errorf("gate inherited host failure: %+v", rec)
					}
				}
				if rec.Kind == "step" && rec.Host == "" && strings.Contains(rec.Here, want) {
					found[rec.Step] = true
				}
			}
			if gateLines != 1 {
				t.Errorf("got %d gate task records, want one", gateLines)
			}
			for _, step := range []string{tasks.GoVetSource, tasks.GoFixSource} {
				if !found[step] {
					t.Errorf("missing local %s: %+v\n%s", step, found, out)
				}
			}
		})
	}
}

func TestProjectRemoteGateBuiltinLocal(t *testing.T) {
	f, state := projectBuiltinRepo(t)
	f.env = append(f.env, "GRAFT_HEAVY=local")
	out := f.mustGraft("gate")
	found := map[string]bool{}
	for _, rec := range readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl")) {
		if rec.Kind == "step" && (rec.Step == tasks.GoVetSource || rec.Step == tasks.GoFixSource) {
			if rec.Host != "" {
				t.Errorf("local check on %s: %s", rec.Host, out)
			} else {
				found[rec.Step] = true
			}
		}
	}
	for _, step := range []string{tasks.GoVetSource, tasks.GoFixSource} {
		if !found[step] {
			t.Errorf("missing local %s: %+v\n%s", step, found, out)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "calls")); !os.IsNotExist(err) {
		t.Errorf("unexpected user host calls: %v", err)
	}
}

func TestProjectRemoteGateBuiltinExecLocal(t *testing.T) {
	f, state := projectBuiltinRepo(t)
	out := f.mustGraft("exec", "--heavy", "--local", "--", graftBin, "gate")
	found := map[string]bool{}
	for _, rec := range readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl")) {
		if rec.Kind == "step" && (rec.Step == tasks.GoVetSource || rec.Step == tasks.GoFixSource) && rec.Host == "" {
			found[rec.Step] = true
		}
	}
	for _, step := range []string{tasks.GoVetSource, tasks.GoFixSource} {
		if !found[step] {
			t.Errorf("missing local %s: %+v\n%s", step, found, out)
		}
	}
}

func TestProjectRemoteGateBuiltinFixDiff(t *testing.T) {
	f, state := projectBuiltinRepo(t)
	f.write("go.mod", fixGoMod)
	f.write("a.go", fixDue)
	out, code := f.graft("", "gate")
	if code == 0 || !strings.Contains(out, `gate failed at "go fix -diff ./..."`) || !strings.Contains(out, "+\tfor i := range 3 {") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, rec := range readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl")) {
		if rec.Kind == "step" && rec.Step == tasks.GoFixSource && rec.Host == "box" {
			return
		}
	}
	t.Fatal("fix diff did not run on box")
}

func TestProjectRemoteGateBuiltinModules(t *testing.T) {
	f, state := projectBuiltinRepo(t)
	script := filepath.Join(state, "project.sh")
	writeFile(t, script, "shift\necho \"$PWD $*\" >> "+yamlQuote(filepath.Join(state, "module-calls"))+"\nexec \"$@\"\n")
	cfg := strings.Replace(f.read(".graft.yaml"), yamlQuote(script)+"], threads", yamlQuote(script)+", '{root}'], threads", 1)
	f.write(".graft.yaml", cfg)
	f.write("sub/go.mod", "module example.com/sub\n\ngo 1.22\n")
	f.write("sub/a.go", "package sub\n")
	out := f.mustGraft("gate")
	calls := readFile(t, filepath.Join(state, "module-calls"))
	for _, step := range []string{"go vet ./...", "go fix -diff ./..."} {
		for _, dir := range []string{f.work, filepath.Join(f.work, "sub")} {
			found := false
			for line := range strings.SplitSeq(calls, "\n") {
				if strings.HasPrefix(line, dir+" ") && strings.Contains(line, step) {
					found = true
				}
			}
			if !found {
				t.Errorf("missing %s in %s: %q\n%s", step, dir, calls, out)
			}
		}
	}
}

func TestProjectRemoteGateBuiltinWhenBusy(t *testing.T) {
	f, state := projectBuiltinRepo(t)
	f.write(".graft.yaml", strings.Replace(f.read(".graft.yaml"), "threads: 4, broken_exit", "threads: 4, when: busy, broken_exit", 1))
	out := f.mustGraft("gate")
	seen := map[string]bool{}
	for _, rec := range readStats(t, filepath.Join(state, "cache", "graft", "stats.jsonl")) {
		if rec.Kind == "step" && (rec.Step == tasks.GoVetSource || rec.Step == tasks.GoFixSource) && rec.Host == "" && rec.Here == tasks.WhyInsideHeavy {
			seen[rec.Step] = true
		}
		if rec.Kind == "task" && rec.Task == "unit" && rec.Host == "" && rec.Here == tasks.WhyInsideHeavy {
			seen["unit"] = true
		}
	}
	for _, name := range []string{tasks.GoVetSource, tasks.GoFixSource, "unit"} {
		if !seen[name] {
			t.Errorf("%s did not use admitted heavy placement: %+v\n%s", name, seen, out)
		}
	}
}
