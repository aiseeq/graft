package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPilotGateUsesCleanHeadWorktree(t *testing.T) {
	t.Run("gate changes tracked file without touching pilot", func(t *testing.T) {
		pilot := newPilotRepo(t)
		newGraft := newPilotGraft(t)
		setPilotGateEnv(t, pilot)

		if out, err := runPilotScript(t, newGraft, nil); err != nil {
			t.Fatalf("pilot gate failed: %v\n%s", err, out)
		}
		if got := gitOutput(t, pilot, "status", "--porcelain"); got != "" {
			t.Errorf("pilot status = %q, want clean", got)
		}
		if got := gitOutput(t, pilot, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
			t.Errorf("pilot worktrees = %q, want only the main worktree", got)
		}
		if got := readPilotFile(t, pilot, "go.mod"); got != "module example.com/projectA\n" {
			t.Errorf("pilot go.mod = %q, want original content", got)
		}
	})

	t.Run("dirty gate is ignored and preserved", func(t *testing.T) {
		pilot := newPilotRepo(t)
		newGraft := newPilotGraft(t)
		setPilotGateEnv(t, pilot)
		dirtyGate := "exit 1\n"
		writePilotFile(t, pilot, "gate.sh", dirtyGate)

		if out, err := runPilotScript(t, newGraft, nil); err != nil {
			t.Fatalf("HEAD pilot gate failed: %v\n%s", err, out)
		}
		if got := readPilotFile(t, pilot, "gate.sh"); got != dirtyGate {
			t.Errorf("dirty gate = %q, want it preserved", got)
		}
		if got := gitOutput(t, pilot, "status", "--porcelain"); !strings.Contains(got, "M gate.sh") {
			t.Errorf("pilot status = %q, want dirty gate.sh preserved", got)
		}
		if got := readPilotFile(t, pilot, "go.mod"); got != "module example.com/projectA\n" {
			t.Errorf("pilot go.mod = %q, want original content", got)
		}
		if got := gitOutput(t, pilot, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
			t.Errorf("pilot worktrees = %q, want only the main worktree", got)
		}
	})
}

func TestPilotGateTaskWiring(t *testing.T) {
	config, err := os.ReadFile(".graft.yaml")
	if err != nil {
		t.Fatal(err)
	}
	step := `- sh: 'sh scripts/pilot-gate.sh "$PWD/bin" {args}'`
	if !strings.Contains(string(config), step) {
		t.Fatalf(".graft.yaml does not wire pilot-gate through %q", step)
	}
	script, err := os.ReadFile(filepath.Join("scripts", "pilot-gate.sh"))
	if err != nil {
		t.Fatal(err)
	}
	f := tasksRepo(t, `tasks:
  pilot-gate:
    usage: '[--no-pilot]'
    dotenv: ['GRAFT_PILOT?']
    run:
      `+step+`
`, map[string]string{"scripts/pilot-gate.sh": string(script)})
	pilot := newPilotRepo(t)
	t.Setenv("GRAFT_PILOT", pilot)
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("GOWORK", "off")
	f.env = []string{"GRAFT_PILOT=" + pilot, "TMPDIR=" + os.Getenv("TMPDIR"), "GOWORK=off"}
	if err := os.MkdirAll(f.path("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.write("bin/graft", "#!/bin/sh\nset -eu\ntest \"$1\" = gate\nexec sh gate.sh\n")
	if err := os.Chmod(f.path("bin/graft"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := f.graft("", "pilot-gate")
	if code != 0 {
		t.Fatalf("graft pilot-gate: exit %d\n%s", code, out)
	}
	if got := gitOutput(t, pilot, "status", "--porcelain"); got != "" {
		t.Errorf("pilot status after graft pilot-gate = %q, want clean", got)
	}
	if got := gitOutput(t, pilot, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
		t.Errorf("pilot worktrees after graft pilot-gate = %q, want only the main worktree", got)
	}
	out, code = f.graft("", "pilot-gate", "--", "--no-pilot")
	if code != 0 || !strings.Contains(out, "install: no pilot, as asked") {
		t.Errorf("graft pilot-gate -- --no-pilot: exit %d\n%s", code, out)
	}
	out, code = f.graft("", "pilot-gate", "--", "--unknown")
	if code == 0 || !strings.Contains(out, "install: unknown arguments: --unknown") {
		t.Errorf("graft pilot-gate -- --unknown: exit %d\n%s", code, out)
	}
}

func TestPilotGateScriptArguments(t *testing.T) {
	newGraft := newPilotGraft(t)
	t.Setenv("GRAFT_PILOT", "")

	out, err := runPilotScript(t, newGraft, []string{"--no-pilot"})
	if err != nil || !strings.Contains(string(out), "install: no pilot, as asked") {
		t.Errorf("--no-pilot: error %v, output %q", err, out)
	}
	out, err = runPilotScript(t, newGraft, []string{"--unknown"})
	if err == nil || !strings.Contains(string(out), "install: unknown arguments: --unknown") {
		t.Errorf("unknown argument: error %v, output %q", err, out)
	}
}

func newPilotRepo(t *testing.T) string {
	t.Helper()
	pilot := filepath.Join(t.TempDir(), "pilot")
	if err := os.MkdirAll(pilot, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, pilot, "init", "-q")
	runGit(t, pilot, "config", "user.name", "projectA")
	runGit(t, pilot, "config", "user.email", "projectA@example.com")
	writePilotFile(t, pilot, ".gitignore", ".env\n")
	writePilotFile(t, pilot, ".graft.yaml", "gate: [synthetic]\n")
	writePilotFile(t, pilot, ".env", "PILOT_SETTING=present\n")
	writePilotFile(t, pilot, "go.mod", "module example.com/projectA\n")
	writePilotFile(t, pilot, "gate.sh", "test -f .env && grep -q PILOT_SETTING=present .env && printf 'module example.com/projectA v2\\n' > go.mod\n")
	runGit(t, pilot, "add", ".gitignore", ".graft.yaml", "go.mod", "gate.sh")
	runGit(t, pilot, "commit", "-qm", "test: create synthetic pilot")
	return pilot
}

func newPilotGraft(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(bin, "graft")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\ntest \"$1\" = gate\nexec sh gate.sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func setPilotGateEnv(t *testing.T, pilot string) {
	t.Helper()
	t.Setenv("GRAFT_PILOT", pilot)
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("GOWORK", "off")
}

func runPilotScript(t *testing.T, newGraft string, args []string) ([]byte, error) {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "scripts", "pilot-gate.sh")
	commandArgs := append([]string{script, newGraft}, args...)
	cmd := exec.Command("sh", commandArgs...)
	return cmd.CombinedOutput()
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(string(runGit(t, dir, args...)))
}

func runGit(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func writePilotFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readPilotFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
