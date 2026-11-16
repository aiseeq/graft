package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	akitproc "github.com/aiseeq/akit/proc"
)

func TestPilotGateUsesCleanHeadWorktree(t *testing.T) {
	t.Run("gate changes tracked file without touching pilot", func(t *testing.T) {
		pilot := newPilotRepo(t)
		newGraft := newPilotGraft(t)
		tmpRoot := setPilotGateEnv(t, pilot)

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
		assertPilotGateCleanup(t, pilot, tmpRoot)
	})

	t.Run("dirty gate is ignored and preserved", func(t *testing.T) {
		pilot := newPilotRepo(t)
		newGraft := newPilotGraft(t)
		tmpRoot := setPilotGateEnv(t, pilot)
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
		assertPilotGateCleanup(t, pilot, tmpRoot)
	})
}

func TestPilotGateClassifiesNewAndInstalledFailures(t *testing.T) {
	tests := []struct {
		name        string
		newOutput   string
		newExit     int
		oldOutput   string
		oldExit     int
		wantCode    int
		wantText    string
		oldInvoked  bool
		noInstalled bool
	}{
		{name: "old fails diagnoses pilot failure but refuses", newOutput: pilotGateFailure("lex check", "new output"), newExit: 1, oldOutput: pilotGateFailure("test", "old output"), oldExit: 1, wantText: "the installed graft fails the pilot's gate too: likely the pilot's own failure", wantCode: 1, oldInvoked: true},
		{name: "old passes diagnoses graft regression and refuses", newOutput: `gate failed at "lex check"` + "\n", newExit: 1, wantText: "the installed graft passes it: a regression of this graft", wantCode: 1, oldInvoked: true},
		{name: "old command cannot start refuses with diagnosis", newOutput: pilotGateFailure("lex check", "new output"), newExit: 1, oldExit: 127, wantText: "the installed graft could not be run for comparison: gate command exited 127", wantCode: 1, oldInvoked: true},
		{name: "installed target missing refuses with diagnosis", newOutput: pilotGateFailure("lex check", "pilot output"), newExit: 1, wantText: "the installed graft could not be run for comparison: not found", wantCode: 1, noInstalled: true},
		{name: "new pass does not run old", newOutput: "", oldExit: 0, wantCode: 0, oldInvoked: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pilot := newPilotRepo(t)
			newGraft := newPilotGraftResult(t, tt.newOutput, tt.newExit)
			oldDir := filepath.Join(newGraft, "installed")
			if err := os.MkdirAll(oldDir, 0o755); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "old-called")
			oldPath := filepath.Join(oldDir, "graft")
			oldScript := "#!/bin/sh\nset -eu\nif [ \"$1\" = gate ]; then test \"$(command -v graft)\" = '" + oldPath + "'; printf called > '" + marker + "'; else exit 2; fi\n"
			if tt.oldOutput != "" {
				oldScript += "printf '%s\\n' '" + strings.TrimSpace(tt.oldOutput) + "'\n"
			}
			oldScript += "exit " + fmt.Sprint(tt.oldExit) + "\n"
			if !tt.noInstalled {
				if err := os.WriteFile(oldPath, []byte(oldScript), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			tmpRoot := setPilotGateEnv(t, pilot)
			decoyDir := filepath.Join(filepath.Dir(newGraft), "decoy")
			if err := os.MkdirAll(decoyDir, 0o755); err != nil {
				t.Fatal(err)
			}
			decoyMarker := filepath.Join(t.TempDir(), "decoy-called")
			decoy := "#!/bin/sh\nprintf called > '" + decoyMarker + "'\nprintf 'gate failed at \"lex check\"\\n'\nexit 1\n"
			if err := os.WriteFile(filepath.Join(decoyDir, "graft"), []byte(decoy), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", decoyDir+":"+newGraft+":"+os.Getenv("PATH"))
			out, err := runPilotScript(t, newGraft, nil)
			code := 0
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					code = exitErr.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if code != tt.wantCode || !strings.Contains(string(out), tt.wantText) {
				t.Errorf("exit=%d output:\n%s\nwant exit=%d containing %q", code, out, tt.wantCode, tt.wantText)
			}
			_, statErr := os.Stat(marker)
			if got := statErr == nil; got != tt.oldInvoked {
				t.Errorf("installed graft invoked = %v, want %v", got, tt.oldInvoked)
			}
			if _, err := os.Stat(decoyMarker); err == nil {
				t.Errorf("post-first-in-PATH graft was invoked, want installed target %s", oldPath)
			}
			assertPilotGateCleanup(t, pilot, tmpRoot)
		})
	}
}

func TestPilotGateInstallDirFailurePreservesError(t *testing.T) {
	pilot := newPilotRepo(t)
	gateRan := filepath.Join(t.TempDir(), "gate-ran")
	newGraft := newPilotGraftScript(t, "#!/bin/sh\nif [ \"$1\" = tools ]; then printf 'config-error\\n' >&2; exit 9; fi\nprintf gate-ran > '"+gateRan+"'\nexit 0\n")
	if err := os.WriteFile(filepath.Join(newGraft, "graft"), []byte("#!/bin/sh\nif [ \"$1\" = tools ]; then printf 'config-error\\n' >&2; exit 9; fi\nprintf gate-ran > '"+gateRan+"'\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	setPilotGateEnv(t, pilot)
	out, err := runPilotScript(t, newGraft, nil)
	if err == nil || !strings.Contains(string(out), "config-error") || !strings.Contains(string(out), "could not get the installed graft directory (exit 9)") {
		t.Fatalf("install-dir error: err=%v\n%s", err, out)
	}
	if _, err := os.Stat(gateRan); !os.IsNotExist(err) {
		t.Errorf("pilot gate ran despite install-dir failure; stat error = %v", err)
	}
}

func TestPilotGateRejectsInstalledLinkToNewBinary(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			pilot := newPilotRepo(t)
			newGraft := newPilotGraftResult(t, pilotGateFailure("lex check", "same"), 1)
			installedDir := filepath.Join(newGraft, "installed")
			if err := os.MkdirAll(installedDir, 0o755); err != nil {
				t.Fatal(err)
			}
			installed := filepath.Join(installedDir, "graft")
			switch kind {
			case "symlink":
				if err := os.Symlink(filepath.Join(newGraft, "graft"), installed); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(filepath.Join(newGraft, "graft"), installed); err != nil {
					t.Fatal(err)
				}
			}
			setPilotGateEnv(t, pilot)
			out, err := runPilotScript(t, newGraft, nil)
			if err == nil || !strings.Contains(string(out), "is the new graft") {
				t.Fatalf("expected same-file refusal: err=%v\n%s", err, out)
			}
		})
	}
}

func TestPilotGateSecondWorktreeFailureCleansFirst(t *testing.T) {
	pilot := newPilotRepo(t)
	newGraft := newPilotGraftResult(t, pilotGateFailure("lex check", "same"), 1)
	installedDir := filepath.Join(newGraft, "installed")
	if err := os.MkdirAll(installedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldScript := "#!/bin/sh\nset -eu\ntest \"$(command -v graft)\" = '" + filepath.Join(installedDir, "graft") + "'\ncat <<'PILOT_GATE_OUTPUT'\n" + pilotGateFailure("lex check", "same") + "PILOT_GATE_OUTPUT\nexit 1\n"
	if err := os.WriteFile(filepath.Join(installedDir, "graft"), []byte(oldScript), 0o755); err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gitBin := filepath.Join(t.TempDir(), "git-bin")
	if err := os.MkdirAll(gitBin, 0o755); err != nil {
		t.Fatal(err)
	}
	counter := filepath.Join(t.TempDir(), "worktree-add-count")
	gitWrapper := "#!/bin/sh\ncase \" $* \" in *' worktree add '*) n=0; test ! -f '" + counter + "' || n=$(cat '" + counter + "'); n=$((n+1)); printf '%s\\n' \"$n\" > '" + counter + "'; if [ \"$n\" -eq 2 ]; then exit 1; fi;; esac\nexec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(gitBin, "git"), []byte(gitWrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	setPilotGateEnv(t, pilot)
	tmpRoot := filepath.Join(t.TempDir(), "pilot-temp")
	if err := os.MkdirAll(tmpRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmpRoot)
	t.Setenv("PATH", gitBin+":"+newGraft+":"+os.Getenv("PATH"))
	out, err := runPilotScript(t, newGraft, nil)
	if err == nil || !strings.Contains(string(out), "cannot make a fresh worktree at ") {
		t.Fatalf("second worktree failure: err=%v\n%s", err, out)
	}
	if got := gitOutput(t, pilot, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
		t.Errorf("pilot worktrees after failed second add = %q, want only the main worktree", got)
	}
	assertPilotGateCleanup(t, pilot, tmpRoot)
}

func TestPilotGateRunsInstalledGraftInFreshHeadWorktree(t *testing.T) {
	pilot := newPilotRepo(t)
	writePilotFile(t, pilot, "go.work", "go 1.23.0\n\nuse .\n")
	newGraft := newPilotGraftScript(t, `#!/bin/sh
set -eu
test "$1" = gate
test -f .env
grep -q PILOT_SETTING=present .env
test "$GOWORK" = '`+filepath.Join(pilot, "go.work")+`'
printf 'module example.com/projectA v2\n' > go.mod
printf 'graft: gate 1/1: lint\npilot output: %s\n' "$PWD"
printf 'graft: gate failed at "lint" (see graft stats --failures): step failed\n'
exit 1
`)
	oldDir := filepath.Join(newGraft, "installed")
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := `#!/bin/sh
set -eu
test "$1" = gate
test -f .env
grep -q PILOT_SETTING=present .env
test "$GOWORK" = '` + filepath.Join(pilot, "go.work") + `'
if grep -q ' v2' go.mod; then printf 'unexpected modified tree\n'; exit 0; fi
test "$(command -v graft)" = '` + oldDir + `/graft'
printf 'graft: gate 1/1: lint\npilot output: %s\n' "$PWD"
printf 'graft: gate failed at "lint" (see graft stats --failures): step failed\n'
exit 1
`
	oldPath := filepath.Join(oldDir, "graft")
	if err := os.WriteFile(oldPath, []byte(old), 0o755); err != nil {
		t.Fatal(err)
	}
	tmpRoot := setPilotGateEnv(t, pilot)
	t.Setenv("GOWORK", "")
	t.Setenv("PATH", newGraft+":"+oldDir+":"+os.Getenv("PATH"))
	out, err := runPilotScript(t, newGraft, nil)
	if err == nil || !strings.Contains(string(out), "the installed graft fails the pilot's gate too: likely the pilot's own failure") {
		t.Fatalf("expected pilot diagnosis from fresh HEAD worktree and refusal: err=%v\n%s", err, out)
	}
	if got := readPilotFile(t, pilot, "go.mod"); got != "module example.com/projectA\n" {
		t.Errorf("pilot go.mod = %q, want original content", got)
	}
	if got := gitOutput(t, pilot, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
		t.Errorf("pilot worktrees = %q, want only the main worktree", got)
	}
	assertPilotGateCleanup(t, pilot, tmpRoot)
}

func TestPilotGatePinsOnePilotCommitForBothRuns(t *testing.T) {
	pilot := newPilotRepo(t)
	writePilotFile(t, pilot, "revision", "A\n")
	runGit(t, pilot, "add", "revision")
	runGit(t, pilot, "commit", "-m", "synthetic revision A")
	initialHead := gitOutput(t, pilot, "rev-parse", "HEAD")

	started := filepath.Join(t.TempDir(), "new-gate-started")
	release := filepath.Join(t.TempDir(), "release-new-gate")
	newGraft := newPilotGraftScript(t, `#!/bin/sh
set -eu
test "$1" = gate
printf started > '`+started+`'
while [ ! -f '`+release+`' ]; do sleep 0.02; done
printf 'graft: gate failed at "lint" (see graft stats --failures): step failed\n'
exit 1
`)
	installedDir := filepath.Join(newGraft, "installed")
	oldRevision := filepath.Join(t.TempDir(), "installed-revision")
	oldScript := `#!/bin/sh
set -eu
test "$1" = gate
cat revision > '` + oldRevision + `'
if grep -qx A revision; then
	printf 'graft: gate failed at "lint" (see graft stats --failures): step failed\n'
	exit 1
fi
if grep -qx B revision; then
	exit 0
fi
exit 2
`
	if err := os.WriteFile(filepath.Join(installedDir, "graft"), []byte(oldScript), 0o755); err != nil {
		t.Fatal(err)
	}
	tmpRoot := setPilotGateEnv(t, pilot)
	t.Setenv("PATH", newGraft+":"+os.Getenv("PATH"))
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", filepath.Join(root, "scripts", "pilot-gate.sh"), newGraft)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = os.WriteFile(release, []byte("go\n"), 0o600)
			_ = cmd.Wait()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = os.WriteFile(release, []byte("go\n"), 0o600)
			_ = cmd.Wait()
			t.Fatal("new pilot gate did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	writePilotFile(t, pilot, "revision", "B\n")
	runGit(t, pilot, "add", "revision")
	runGit(t, pilot, "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "commit", "-m", "synthetic revision B")
	if err := os.WriteFile(release, []byte("go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	out := output.Bytes()
	if err == nil || !strings.Contains(string(out), "the installed graft fails the pilot's gate too: likely the pilot's own failure") {
		t.Fatalf("expected both grafts to fail on pinned commit A: err=%v\n%s", err, out)
	}
	if got := readPilotFile(t, pilot, "revision"); got != "B\n" {
		t.Errorf("main pilot revision = %q, want B after new graft advanced HEAD", got)
	}
	if got := gitOutput(t, pilot, "rev-parse", "HEAD"); got == initialHead {
		t.Errorf("new graft left pilot HEAD at %s; expected it to advance to B", got)
	}
	if got, err := os.ReadFile(oldRevision); err != nil || string(got) != "A\n" {
		t.Errorf("installed graft revision = %q, err=%v; want pinned commit A", got, err)
	}
	assertPilotGateCleanup(t, pilot, tmpRoot)
}

func TestPilotGateTermStopsGateWithoutPstop(t *testing.T) {
	pilot := newPilotRepo(t)
	started := filepath.Join(t.TempDir(), "gate-pid")
	newGraft := newPilotGraftScript(t, "#!/bin/sh\nprintf '%s\\n' \"$$\" > '"+started+"'\nexec sleep 20\n")
	tmpRoot := setPilotGateEnv(t, pilot)
	// /usr/bin and /bin provide the normal utilities and setsid but exclude
	// the user's ~/bin/pstop, exercising the portable process-group path.
	t.Setenv("PATH", newGraft+":/usr/bin:/bin")
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", filepath.Join(root, "scripts", "pilot-gate.sh"), newGraft)
	cmd.Env = append(os.Environ(), "GRAFT_PILOT="+pilot)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_, _ = akitproc.StopChild(cmd.Process.Pid, 0, func() error { return cmd.Process.Kill() })
			_ = cmd.Wait()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if cmd.ProcessState != nil || time.Now().After(deadline) {
			t.Fatalf("pilot gate did not start; output: %s", output.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	pidBytes, err := os.ReadFile(started)
	if err != nil {
		t.Fatal(err)
	}
	var gatePID int
	if _, err := fmt.Sscanf(string(pidBytes), "%d", &gatePID); err != nil {
		t.Fatal(err)
	}
	stopStarted := time.Now()
	_, stopErr := akitproc.StopChild(cmd.Process.Pid, 4*time.Second, func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	})
	if elapsed := time.Since(stopStarted); elapsed > 3*time.Second {
		t.Fatalf("pilot-gate process tree did not stop within 3 seconds after TERM (took %s)", elapsed)
	}
	if stopErr != nil {
		t.Fatalf("stop pilot-gate process tree after TERM: %v", stopErr)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("pilot-gate exited successfully after TERM")
	}
	if err := syscall.Kill(gatePID, 0); err == nil {
		t.Errorf("pilot gate process %d is still alive", gatePID)
	}
	assertPilotGateCleanup(t, pilot, tmpRoot)
}

func TestPilotGateUsesPstopWhenAvailable(t *testing.T) {
	pilot := newPilotRepo(t)
	started := filepath.Join(t.TempDir(), "gate-pid")
	newGraft := newPilotGraftScript(t, "#!/bin/sh\nprintf '%s\\n' \"$$\" > '"+started+"'\nexec sleep 20\n")
	tmpRoot := setPilotGateEnv(t, pilot)
	pstopDir := t.TempDir()
	pstopMarker := filepath.Join(t.TempDir(), "pstop-called")
	pstop := "#!/bin/sh\nprintf '%s\\n' \"$*\" > '" + pstopMarker + "'\ntest \"$1\" = --grace && test \"$2\" = 1s || exit 2\nkill -TERM \"$3\"\n"
	if err := os.WriteFile(filepath.Join(pstopDir, "pstop"), []byte(pstop), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pstopDir+":"+newGraft+":/usr/bin:/bin")
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", filepath.Join(root, "scripts", "pilot-gate.sh"), newGraft)
	cmd.Env = append(os.Environ(), "GRAFT_PILOT="+pilot)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_, _ = akitproc.StopChild(cmd.Process.Pid, 0, func() error { return cmd.Process.Kill() })
			_ = cmd.Wait()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if cmd.ProcessState != nil || time.Now().After(deadline) {
			t.Fatalf("pilot gate did not start; output: %s", output.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	stopStarted := time.Now()
	_, stopErr := akitproc.StopChild(cmd.Process.Pid, 4*time.Second, func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	})
	if elapsed := time.Since(stopStarted); elapsed > 3*time.Second {
		t.Fatalf("pilot-gate process tree did not stop within 3 seconds after TERM (took %s)", elapsed)
	}
	if stopErr != nil {
		t.Fatalf("stop pilot-gate process tree after TERM: %v", stopErr)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("pilot-gate exited successfully after TERM")
	}
	called, err := os.ReadFile(pstopMarker)
	if err != nil || !strings.HasPrefix(string(called), "--grace 1s ") {
		t.Errorf("pstop marker = %q, err=%v; want invocation with --grace 1s and gate PID", called, err)
	}
	assertPilotGateCleanup(t, pilot, tmpRoot)
}

func TestPilotGatePreservesStdin(t *testing.T) {
	pilot := newPilotRepo(t)
	newGraft := newPilotGraftScript(t, "#!/bin/sh\nIFS= read -r line || exit 7\nprintf 'input:%s\\n' \"$line\"\nexit 0\n")
	tmpRoot := setPilotGateEnv(t, pilot)
	t.Setenv("PATH", newGraft+":"+os.Getenv("PATH"))
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", filepath.Join(root, "scripts", "pilot-gate.sh"), newGraft)
	cmd.Stdin = strings.NewReader("hello\n")
	cmd.Env = append(os.Environ(), "GRAFT_PILOT="+pilot)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "input:hello\n") {
		t.Fatalf("pilot gate stdin: err=%v, output=%q; want input:hello and exit 0", err, out)
	}
	assertPilotGateCleanup(t, pilot, tmpRoot)
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
	installDir := f.path("installed")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.write("bin/graft", "#!/bin/sh\nset -eu\nif [ \"$1\" = tools ]; then test \"$2\" = install-dir; printf '%s\\n' '"+installDir+"'; exit 0; fi\ntest \"$1\" = gate\nexec sh gate.sh\n")
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

func TestToolsInstallDirUsesUserConfig(t *testing.T) {
	f := tasksRepo(t, "", nil)
	configured := f.path("configured-bin")
	cfgHome := filepath.Join(f.base, "userconfig")
	writeUserConfig(t, cfgHome, "tools:\n  bin_dir: "+yamlQuote(configured)+"\n")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgHome)
	out, code := f.graft("", "tools", "install-dir")
	if code != 0 || strings.TrimSpace(out) != configured {
		t.Fatalf("graft tools install-dir: exit %d, output %q; want %q", code, out, configured)
	}
}

func TestToolsInstallDirHelp(t *testing.T) {
	f := tasksRepo(t, "tools: {}\n", nil)
	out, code := f.graft("", "help", "tools")
	if code != 0 || !strings.Contains(out, "graft tools install-dir") {
		t.Fatalf("graft help tools: exit %d, output %q; want install-dir usage", code, out)
	}
}

func TestPilotGateInstallDirUsesUserConfig(t *testing.T) {
	f := tasksRepo(t, "", nil)
	configured := f.path("configured-bin")
	cfgHome := filepath.Join(f.base, "userconfig")
	writeUserConfig(t, cfgHome, "tools:\n  bin_dir: "+yamlQuote(configured)+"\n")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgHome)
	out, code := f.graft("", "tools", "install-dir")
	if code != 0 || strings.TrimSpace(out) != configured {
		t.Fatalf("graft tools install-dir: exit %d, output %q; want %q", code, out, configured)
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
	out, err = runPilotScript(t, newGraft, nil)
	if err == nil || !strings.Contains(string(out), "install: no pilot:") {
		t.Errorf("empty GRAFT_PILOT without arguments: error %v, output %q", err, out)
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
	return newPilotGraftResult(t, "", 0)
}

func newPilotGraftResult(t *testing.T, output string, exit int) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(bin, "graft")
	script := "#!/bin/sh\nset -eu\ntest \"$1\" = gate\n"
	if output != "" {
		script += "cat <<'PILOT_GATE_OUTPUT'\n" + output
		if !strings.HasSuffix(output, "\n") {
			script += "\n"
		}
		script += "PILOT_GATE_OUTPUT\n"
	}
	if exit == 0 && output == "" {
		script += "exec sh gate.sh\n"
	} else {
		script += "exit " + string(rune('0'+exit)) + "\n"
	}
	return newPilotGraftScriptAt(t, path, script)
}

func newPilotGraftScript(t *testing.T, script string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	return newPilotGraftScriptAt(t, filepath.Join(bin, "graft"), script)
}

func newPilotGraftScriptAt(t *testing.T, path, script string) string {
	t.Helper()
	installedDir := filepath.Join(filepath.Dir(path), "installed")
	if err := os.MkdirAll(installedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if after, ok := strings.CutPrefix(script, "#!/bin/sh\n"); ok {
		script = "#!/bin/sh\nif [ \"${1:-}\" = tools ]; then test \"${2:-}\" = install-dir; printf '%s\\n' '" + installedDir + "'; exit 0; fi\n" + after
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(path)
}

func pilotGateFailure(step, output string) string {
	return fmt.Sprintf("graft: gate 1/1: %s\n%s\ngraft: gate failed at %q (see graft stats --failures): step failed\n", step, output, step)
}

func setPilotGateEnv(t *testing.T, pilot string) string {
	t.Helper()
	t.Setenv("GRAFT_PILOT", pilot)
	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot)
	t.Setenv("GOWORK", "off")
	return tmpRoot
}

func assertPilotGateCleanup(t *testing.T, pilot, tmpRoot string) {
	t.Helper()
	if got := gitOutput(t, pilot, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
		t.Errorf("pilot worktrees = %q, want only the main worktree", got)
	}
	entries, err := os.ReadDir(tmpRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("temporary entries = %v, want empty", entries)
	}
}

func runPilotScript(t *testing.T, newGraft string, args []string) ([]byte, error) {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "scripts", "pilot-gate.sh")
	installedDir := filepath.Join(newGraft, "installed")
	if err := os.MkdirAll(installedDir, 0o755); err != nil {
		t.Fatal(err)
	}
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
	// The fixture's git config, not the machine's: a global graft pre-commit
	// there would judge the synthetic pilot's raw commit.
	cmd.Env = testEnv
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

// A warning git prints on stderr while it resolves HEAD (an ambiguous
// refname) does not become part of the pinned commit: the pilot's gate runs.
func TestPilotGateRevisionIgnoresGitWarnings(t *testing.T) {
	pilot := newPilotRepo(t)
	runGit(t, pilot, "update-ref", "refs/tags/HEAD", "HEAD")
	newGraft := newPilotGraft(t)
	tmpRoot := setPilotGateEnv(t, pilot)
	out, err := runPilotScript(t, newGraft, nil)
	if err != nil {
		t.Fatalf("pilot gate with an ambiguous HEAD refname: %v\n%s", err, out)
	}
	assertPilotGateCleanup(t, pilot, tmpRoot)
}

// A warning graft prints on stderr while it reports its install directory
// does not become part of the path: the installed graft is found and run
// for the diagnosis.
func TestPilotGateInstallDirIgnoresWarnings(t *testing.T) {
	pilot := newPilotRepo(t)
	newGraft := newPilotGraftScript(t, "#!/bin/sh\nexit 0\n")
	installedDir := filepath.Join(newGraft, "installed")
	newScript := "#!/bin/sh\nif [ \"$1\" = tools ]; then printf 'graft: warning: graft is stale\\n' >&2; printf '%s\\n' '" + installedDir + "'; exit 0; fi\nexit 1\n"
	if err := os.WriteFile(filepath.Join(newGraft, "graft"), []byte(newScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installedDir, "graft"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tmpRoot := setPilotGateEnv(t, pilot)
	out, err := runPilotScript(t, newGraft, nil)
	if err == nil || !strings.Contains(string(out), "the installed graft passes it: a regression of this graft") {
		t.Fatalf("pilot gate with a warning from tools install-dir: err=%v\n%s", err, out)
	}
	assertPilotGateCleanup(t, pilot, tmpRoot)
}
