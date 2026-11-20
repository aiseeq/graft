package tasks

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"golang.org/x/sys/unix"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gittest"
	"github.com/aiseeq/graft/v2/internal/stats"
)

const snapshotLine = "the tree has uncommitted changes; building HEAD "

// installConfig is projectA's .graft.yaml: install builds bin/tool and puts
// it into DEST. ROOT and DEST in any file of snapshotProject are replaced.
const installConfig = `schema: 1
version: {mode: none}
tasks:
  build:
    run: [[go, build, -o, bin/tool, .]]
  install:
    deps: [build]
    run: [{install: bin/tool, to: 'DEST'}]
`

// snapshotProject is projectA, a committed Go module whose install task
// builds bin/tool and installs it into dest; extra files are committed too.
// TMPDIR is the returned directory, so the clean clone is found there.
func snapshotProject(t *testing.T, extra map[string]string) (root, dest, tmp, head string) {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a synthetic Go binary")
	}
	base := t.TempDir()
	root, dest, tmp = filepath.Join(base, "projectA"), filepath.Join(base, "bin"), filepath.Join(base, "tmp")
	t.Setenv("TMPDIR", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	files := map[string]string{
		".graft.yaml":     installConfig,
		".gitignore":      "bin/\n",
		"go.mod":          "module example.org/projecta\n\ngo 1.22\n",
		"main.go":         "package main\n\nfunc main() { println(\"one\") }\n",
		"docs/tasks/x.md": "# PROJ-123\n",
	}
	maps.Copy(files, extra)
	paths := strings.NewReplacer("ROOT", root, "DEST", dest)
	for name, body := range files {
		writeFile(t, filepath.Join(root, name), paths.Replace(body))
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "init", "-q", "-b", "main")
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-q", "-m", "projectA")
	return root, dest, tmp, gittest.Run(t, root, "rev-parse", "HEAD")
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// snapshotRunner is a runner of projectA with the tree's .graft.yaml.
func snapshotRunner(t *testing.T, root string, out *bytes.Buffer) *Runner {
	t.Helper()
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return NewRunner(root, cfg, out, out)
}

// runInstall runs the install task of projectA.
func runInstall(t *testing.T, root string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := snapshotRunner(t, root, &out).Run(context.Background(), "install", nil)
	return out.String(), err
}

// installedRevision is the vcs.revision and vcs.modified of dest/tool.
func installedRevision(t *testing.T, dest string) (revision, modified string) {
	t.Helper()
	info, err := buildinfo.ReadFile(filepath.Join(dest, "tool"))
	if err != nil {
		t.Fatalf("reading the installed binary: %v", err)
	}
	revision, _ = installSetting(info, "vcs.revision")
	modified, _ = installSetting(info, "vcs.modified")
	return revision, modified
}

// noClone fails when the clean clone outlived the task, in TMPDIR or in
// the repository's worktrees.
func noClone(t *testing.T, root, tmp string) {
	t.Helper()
	if left, err := os.ReadDir(tmp); err != nil || len(left) != 0 {
		t.Errorf("the clean clone is left in %s: %v %v", tmp, left, err)
	}
	if list := gittest.Run(t, root, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Errorf("git worktree list names more than the tree:\n%s", list)
	}
}

// A ticket journal written after the commit makes the tree dirty: install
// builds HEAD in a clean clone and installs a binary of HEAD, not modified.
func TestInstallDirtyJournalBuildsHeadInACleanClone(t *testing.T) {
	root, dest, tmp, head := snapshotProject(t, nil)
	writeFile(t, filepath.Join(root, "docs/tasks/x.md"), "# PROJ-123\n- note\n")
	out, err := runInstall(t, root)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if !strings.Contains(out, "graft: install: "+snapshotLine+shortRevision(head)+" in a clean clone") {
		t.Errorf("install did not say it builds HEAD in a clean clone:\n%s", out)
	}
	if revision, modified := installedRevision(t, dest); revision != head || modified != "false" {
		t.Errorf("installed revision %s modified %s, want %s false", revision, modified, head)
	}
	if got := gittest.Run(t, root, "status", "--porcelain"); got != "M docs/tasks/x.md" {
		t.Errorf("the tree's own changes moved: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "bin", "tool")); !os.IsNotExist(err) {
		t.Errorf("the build ran in the tree, not in the clone: %v", err)
	}
	noClone(t, root, tmp)
}

// An edit of the source after the commit is not built: the installed binary
// is HEAD's.
func TestInstallDirtySourceInstallsHeadWithoutTheEdit(t *testing.T) {
	root, dest, tmp, head := snapshotProject(t, nil)
	writeFile(t, filepath.Join(root, "main.go"), "package main\n\nfunc main() { println(\"two\") }\n")
	out, err := runInstall(t, root)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if !strings.Contains(out, snapshotLine+shortRevision(head)) {
		t.Errorf("install did not say it builds HEAD in a clean clone:\n%s", out)
	}
	if revision, modified := installedRevision(t, dest); revision != head || modified != "false" {
		t.Errorf("installed revision %s modified %s, want %s false", revision, modified, head)
	}
	ran, err := exec.Command(filepath.Join(dest, "tool")).CombinedOutput()
	if err != nil || strings.TrimSpace(string(ran)) != "one" {
		t.Errorf("the installed binary is not HEAD's: %v %q", err, ran)
	}
	noClone(t, root, tmp)
}

// A clean tree builds and installs in place, without a clone.
func TestInstallCleanTreeBuildsInPlace(t *testing.T) {
	root, dest, tmp, head := snapshotProject(t, nil)
	out, err := runInstall(t, root)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if strings.Contains(out, snapshotLine) {
		t.Errorf("a clean tree went to a clone:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(root, "bin", "tool")); err != nil {
		t.Errorf("a clean tree did not build in place: %v", err)
	}
	if revision, modified := installedRevision(t, dest); revision != head || modified != "false" {
		t.Errorf("installed revision %s modified %s, want %s false", revision, modified, head)
	}
	noClone(t, root, tmp)
}

// A build that needs a file git ignores fails in the clone: the refusal names
// the clone of HEAD and what to do, the clone is gone, nothing is installed.
func TestInstallCleanCloneWithoutIgnoredInputFails(t *testing.T) {
	root, dest, tmp, head := snapshotProject(t, map[string]string{
		".gitignore": "bin/\ngen.txt\n",
		"main.go":    "package main\n\nimport _ \"embed\"\n\n//go:embed gen.txt\nvar gen string\n\nfunc main() { println(gen) }\n",
	})
	writeFile(t, filepath.Join(root, "gen.txt"), "generated\n")
	writeFile(t, filepath.Join(root, "docs/tasks/x.md"), "# PROJ-123\n- note\n")
	out, err := runInstall(t, root)
	if err == nil {
		t.Fatalf("install passed without the ignored input:\n%s", out)
	}
	for _, want := range []string{"clean clone of HEAD " + shortRevision(head), "commit what it needs or clean the tree"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal lacks %q: %v", want, err)
		}
	}
	if _, statErr := os.Stat(filepath.Join(dest, "tool")); !os.IsNotExist(statErr) {
		t.Errorf("a failed build installed a binary: %v", statErr)
	}
	noClone(t, root, tmp)
}

// buildsInPlace checks an install of a dirty tree that may not go to a clean
// clone: a line says why, and the binary built in the tree is refused as
// uncommitted, as before the clone.
func buildsInPlace(t *testing.T, root, dest, tmp, why string) {
	t.Helper()
	out, err := runInstall(t, root)
	if err == nil || !strings.Contains(err.Error(), "refusing uncommitted binary") {
		t.Errorf("install of a dirty tree in place was not refused: %v\n%s", err, out)
	}
	if !strings.Contains(out, "is not built in a clean clone: "+why) || strings.Contains(out, snapshotLine) {
		t.Errorf("install did not say why it builds in place (%s):\n%s", why, out)
	}
	if _, err := os.Stat(filepath.Join(dest, "tool")); !os.IsNotExist(err) {
		t.Errorf("a refused install wrote a binary: %v", err)
	}
	noClone(t, root, tmp)
}

// A changed .graft.yaml would give the clone tasks HEAD does not have, or
// HEAD's where the tree's were meant: the tree builds in place.
func TestInstallChangedConfigBuildsInPlace(t *testing.T) {
	root, dest, tmp, _ := snapshotProject(t, nil)
	writeFile(t, filepath.Join(root, ".graft.yaml"), strings.Replace(strings.ReplaceAll(installConfig, "DEST", dest),
		"[[go, build, -o, bin/tool, .]]", "[[go, build, -ldflags, -X=main.mode=dirty, -o, bin/tool, .]]", 1))
	buildsInPlace(t, root, dest, tmp, ".graft.yaml has uncommitted changes")
}

// A clone does not fetch submodules: a repository with them builds in place.
func TestInstallSubmodulesBuildInPlace(t *testing.T) {
	root, dest, tmp, _ := snapshotProject(t, map[string]string{
		".gitmodules": "[submodule \"lib\"]\n\tpath = lib\n\turl = ../lib\n",
	})
	writeFile(t, filepath.Join(root, "docs/tasks/x.md"), "# PROJ-123\n- note\n")
	buildsInPlace(t, root, dest, tmp, "it has submodules")
}

// The clone owns its objects: HEAD amended and pruned in the repository
// while the clone builds does not break the build.
func TestInstallSurvivesPruneOfTheRepository(t *testing.T) {
	root, dest, tmp, head := snapshotProject(t, map[string]string{
		".graft.yaml": strings.Replace(installConfig, "  build:\n", `  prune:
    run:
      - sh: 'git -c user.name=T -c user.email=t@example.com -C ROOT commit -q --amend --allow-empty -m amended && git -C ROOT reflog expire --expire=now --all && git -C ROOT gc -q --prune=now'
  build:
    deps: [prune]
`, 1),
	})
	writeFile(t, filepath.Join(root, "docs/tasks/x.md"), "# PROJ-123\n- note\n")
	out, err := runInstall(t, root)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if amended := gittest.Run(t, root, "rev-parse", "HEAD"); amended == head {
		t.Fatalf("the repository's HEAD was not amended:\n%s", out)
	}
	if gone := exec.Command("git", "-C", root, "cat-file", "-e", head); gone.Run() == nil {
		t.Fatalf("the old HEAD %s was not pruned:\n%s", head, out)
	}
	if revision, modified := installedRevision(t, dest); revision != head || modified != "false" {
		t.Errorf("installed revision %s modified %s, want %s false", revision, modified, head)
	}
	noClone(t, root, tmp)
}

// A clone left by a graft that died (SIGKILL, SIGTERM in a deploy) goes with
// the next clone; one whose graft lives, or that has no owner file yet, stays.
func TestInstallSweepsTheCloneOfADeadGraft(t *testing.T) {
	root, _, tmp, _ := snapshotProject(t, nil)
	old := time.Now().Add(-2 * time.Hour)
	dirs := map[string]string{}
	for _, name := range []string{"dead", "live", "new"} {
		dirs[name] = filepath.Join(tmp, clonePattern+name)
		writeFile(t, filepath.Join(dirs[name], "projectA", "main.go"), "package main\n")
		if name != "new" {
			writeFile(t, filepath.Join(dirs[name], cloneOwnerFile), "")
			if err := os.Chtimes(filepath.Join(dirs[name], cloneOwnerFile), old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	live := flock.New(filepath.Join(dirs["live"], cloneOwnerFile))
	if locked, err := live.TryLock(); err != nil || !locked {
		t.Fatalf("locking the live clone: %t %v", locked, err)
	}
	defer live.Unlock()
	writeFile(t, filepath.Join(root, "docs/tasks/x.md"), "# PROJ-123\n- note\n")
	if out, err := runInstall(t, root); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if _, err := os.Stat(dirs["dead"]); !os.IsNotExist(err) {
		t.Errorf("the clone of a dead graft stays: %v", err)
	}
	for _, name := range []string{"live", "new"} {
		if _, err := os.Stat(dirs[name]); err != nil {
			t.Errorf("the %s clone was removed: %v", name, err)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, clonePattern+"*")); len(left) != 2 {
		t.Errorf("clones in %s: %q, want the live and the new one", tmp, left)
	}
}

// The clone is only on this machine: its heavy tasks stay here whatever
// GRAFT_HEAVY* came from outside (an exec --local leaves GRAFT_HEAVY=local
// with origin exec, which a nested task would not take as its own), and a
// graft a step starts (a pilot's gate) gets the environment it would get
// without the clone.
func TestInstallCloneStepsStayHere(t *testing.T) {
	for what, outside := range map[string]map[string]string{
		"no GRAFT_HEAVY":     {HeavyEnv: "", HeavyOriginEnv: "", HeavyByEnv: ""},
		"exec --local above": {HeavyEnv: "local", HeavyOriginEnv: "exec", HeavyByEnv: "run-parent", stats.EnvRun: "run-child"},
	} {
		t.Run(what, func(t *testing.T) {
			root, _, _, _ := snapshotProject(t, map[string]string{
				".graft.yaml": installConfig + "remote_heavy:\n  host: hostA\n  up: [\"true\"]\n  run: [sh, -c, 'exec \"$@\"', sh]\n",
			})
			for name, value := range outside {
				t.Setenv(name, value)
			}
			writeFile(t, filepath.Join(root, "docs/tasks/x.md"), "# PROJ-123\n- note\n")
			var out bytes.Buffer
			r := snapshotRunner(t, root, &out)
			build := &config.Task{Heavy: true, Run: []config.Step{{Argv: []string{"go", "build", "./..."}}}}
			if remote, _, err := r.placeHeavy(context.Background(), "build", build, r.Root, nil); err != nil || !remote {
				t.Fatalf("the tree's heavy task did not go to the host: %t %v\n%s", remote, err, out.String())
			}
			before, _, err := r.env(build, nil)
			if err != nil {
				t.Fatal(err)
			}
			leave, err := r.enterInstallSnapshot("install", r.Config.Tasks["install"])
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := leave(nil); err != nil {
					t.Error(err)
				}
			}()
			if r.snapshotHead == "" {
				t.Fatalf("install did not move to a clean clone:\n%s", out.String())
			}
			remote, why, err := r.placeHeavy(context.Background(), "build", build, r.Root, nil)
			if err != nil || remote || why != WhyClone {
				t.Errorf("a heavy task of the clone went to the host: remote %t why %q err %v", remote, why, err)
			}
			in, _, err := r.env(build, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{HeavyEnv, HeavyOriginEnv, HeavyByEnv} {
				wantValue, wantSet := lookupEnv(before, name)
				if value, set := lookupEnv(in, name); value != wantValue || set != wantSet {
					t.Errorf("a step in the clone gets %s=%q (set %t), without the clone %q (set %t)", name, value, set, wantValue, wantSet)
				}
			}
		})
	}
}

// A step that outlives graft killed under it (its reaper after SIGTERM)
// holds the clone's lock through its reaper, never through the command: a
// sweep leaves the clone until the step ends.
func TestInstallCloneLockOutlivesGraft(t *testing.T) {
	root, _, tmp, _ := snapshotProject(t, map[string]string{
		".graft.yaml": installConfig + "  wait:\n    run:\n      - sh: 'for f in /proc/self/fd/*; do readlink \"$f\"; done > ROOT.fds; echo started > ROOT.started; exec sleep 60'\n",
	})
	writeFile(t, filepath.Join(root, "docs/tasks/x.md"), "# PROJ-123\n- note\n")
	var out bytes.Buffer
	r := snapshotRunner(t, root, &out)
	if _, err := r.enterInstallSnapshot("install", r.Config.Tasks["install"]); err != nil {
		t.Fatal(err)
	}
	clones, _ := filepath.Glob(filepath.Join(tmp, clonePattern+"*"))
	if len(clones) != 1 {
		t.Fatalf("clones in %s: %q\n%s", tmp, clones, out.String())
	}
	owner := r.snapshotOwner
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.run(ctx, "wait", nil) }()
	for start := time.Now(); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(root + ".started"); err == nil {
			break
		}
		if time.Since(start) > 30*time.Second {
			cancel()
			t.Fatalf("the step did not start:\n%s", out.String())
		}
	}
	// Only the reaper holds the lock: the command and what it starts (a
	// daemon outliving the reaper) do not get the descriptor.
	if fds, err := os.ReadFile(root + ".fds"); err != nil || !strings.Contains(string(fds), root+".fds") || strings.Contains(string(fds), cloneOwnerFile) {
		t.Errorf("the step's command holds the clone's owner file: %v\n%s", err, fds)
	}
	// graft dies: its own descriptor of the owner file goes.
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(clones[0], cloneOwnerFile), old, old); err != nil {
		t.Fatal(err)
	}
	sweepClones(&out)
	_, stayed := os.Stat(clones[0])
	cancel()
	<-done
	if stayed != nil {
		t.Errorf("a sweep removed the clone of a living step: %v\n%s", stayed, out.String())
	}
	sweepClones(&out)
	if _, err := os.Stat(clones[0]); !os.IsNotExist(err) {
		t.Errorf("the clone stays after its last step ended: %v\n%s", err, out.String())
	}
}

// A TMPDIR on another file system takes no hard links of the objects: the
// clone copies them.
func TestInstallCloneOnAnotherFileSystem(t *testing.T) {
	root, dest, _, head := snapshotProject(t, nil)
	var tree, shm syscall.Stat_t
	if syscall.Stat(root, &tree) != nil || syscall.Stat("/dev/shm", &shm) != nil || tree.Dev == shm.Dev {
		t.Skip("no /dev/shm on another file system than the test's temporary directory")
	}
	tmp, err := os.MkdirTemp("/dev/shm", "graft-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	t.Setenv("TMPDIR", tmp)
	writeFile(t, filepath.Join(root, "docs/tasks/x.md"), "# PROJ-123\n- note\n")
	out, err := runInstall(t, root)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if revision, modified := installedRevision(t, dest); revision != head || modified != "false" {
		t.Errorf("installed revision %s modified %s, want %s false", revision, modified, head)
	}
	noClone(t, root, tmp)
}

// A clone does not fetch LFS files: a repository with them builds in place.
func TestInstallLFSBuildsInPlace(t *testing.T) {
	root, dest, tmp, _ := snapshotProject(t, map[string]string{
		"assets/.gitattributes": "*.bin filter=lfs diff=lfs merge=lfs -text\n",
	})
	writeFile(t, filepath.Join(root, "docs/tasks/x.md"), "# PROJ-123\n- note\n")
	buildsInPlace(t, root, dest, tmp, "it has LFS files")
}

// A shallow repository's clone would lack its history: it builds in place.
func TestInstallShallowBuildsInPlace(t *testing.T) {
	full, dest, tmp, _ := snapshotProject(t, nil)
	writeFile(t, filepath.Join(full, "docs/tasks/x.md"), "# PROJ-123\n- second\n")
	gittest.Run(t, full, "commit", "-q", "-am", "second")
	root := filepath.Join(filepath.Dir(full), "shallow", "projectA")
	gittest.Run(t, filepath.Dir(full), "clone", "-q", "--depth", "1", "file://"+full, root)
	writeFile(t, filepath.Join(root, "docs/tasks/x.md"), "# PROJ-123\n- note\n")
	buildsInPlace(t, root, dest, tmp, "the repository is shallow")
}

// A sparse checkout's clone would hold what the tree does not: it builds in
// place.
func TestInstallSparseCheckoutBuildsInPlace(t *testing.T) {
	root, dest, tmp, _ := snapshotProject(t, nil)
	gittest.Run(t, root, "config", "core.sparseCheckout", "true")
	writeFile(t, filepath.Join(root, "docs/tasks/x.md"), "# PROJ-123\n- note\n")
	buildsInPlace(t, root, dest, tmp, "the work tree is a sparse checkout")
}

// After install in a clone, a task of the tree in the same runner is the
// tree's again: it goes to the host by the usual rules, in the tree, without
// the clone's lock.
func TestInstallThenTreeTaskGoesToHostAgain(t *testing.T) {
	t.Setenv(HeavyEnv, "")
	root, dest, _, head := snapshotProject(t, map[string]string{
		".graft.yaml": installConfig + `  probe:
    heavy: true
    run:
      - sh: 'test ! -e /dev/fd/3 && pwd > ROOT.probe'
  all:
    deps: [install, probe]
remote_heavy:
  host: hostA
  up: ["true"]
  run: [sh, -c, 'echo remote > ROOT.remote; exec "$@"', sh]
`,
	})
	writeFile(t, filepath.Join(root, "docs/tasks/x.md"), "# PROJ-123\n- note\n")
	var out bytes.Buffer
	if err := snapshotRunner(t, root, &out).Run(context.Background(), "all", nil); err != nil {
		t.Fatalf("all: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), snapshotLine+shortRevision(head)) {
		t.Errorf("install did not build in a clean clone:\n%s", out.String())
	}
	if revision, _ := installedRevision(t, dest); revision != head {
		t.Errorf("installed revision %s, want %s", revision, head)
	}
	if _, err := os.Stat(root + ".remote"); err != nil {
		t.Errorf("the tree's heavy task did not go to the host: %v\n%s", err, out.String())
	}
	if dir, err := os.ReadFile(root + ".probe"); err != nil || strings.TrimSpace(string(dir)) != root {
		t.Errorf("the tree's task ran in %q, not in the tree, or saw the clone's lock: %v\n%s", dir, err, out.String())
	}
}

// The stats of a task moved to a clean clone, its deps among them, name the
// tree's project, not the clone: the project's history has them.
func TestInstallCloneStatsNameTheTree(t *testing.T) {
	root, dest, _, _ := snapshotProject(t, nil)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	writeFile(t, filepath.Join(root, "docs/tasks/x.md"), "# PROJ-123\n- note\n")
	var out bytes.Buffer
	r := snapshotRunner(t, root, &out)
	r.Stats = stats.NewRecorder(&out)
	if err := r.Run(context.Background(), "install", nil); err != nil {
		t.Fatalf("install: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(dest, "tool")); err != nil {
		t.Fatalf("nothing installed: %v\n%s", err, out.String())
	}
	path, err := stats.Path()
	if err != nil {
		t.Fatal(err)
	}
	records, _, err := stats.Load(path, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	tasks := map[string]bool{}
	for _, rec := range records {
		if rec.Project != root {
			t.Errorf("a %s record of %s names %q, not the tree %s", rec.Kind, rec.Task, rec.Project, root)
		}
		tasks[rec.Task] = true
	}
	if !tasks["build"] || !tasks["install"] {
		t.Errorf("records of build and install missing: %+v", records)
	}
}

// A GRAFT_REAP_HOLD_FD graft gets from its caller names no descriptor for a
// step's reaper: a step outside a clean clone runs, junk in it or not, and
// gets the descriptor it names, inherited from graft's caller.
func TestStepGetsTheFdTheCallerNamesForTheReaper(t *testing.T) {
	inherited, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inherited.Close() })
	fd := int(inherited.Fd())
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC) })
	for _, value := range []string{"junk", strconv.Itoa(fd)} {
		t.Run(value, func(t *testing.T) {
			root, _, _, _ := snapshotProject(t, map[string]string{".graft.yaml": `schema: 1
version: {mode: none}
tasks:
  probe:
    run: [[sh, -c, 'readlink /proc/self/fd/` + strconv.Itoa(fd) + ` >ROOT/fd.txt']]
`, ".gitignore": "bin/\nfd.txt\n"})
			t.Setenv("GRAFT_REAP_HOLD_FD", value)
			var out bytes.Buffer
			if err := snapshotRunner(t, root, &out).Run(context.Background(), "probe", nil); err != nil {
				t.Fatalf("probe with GRAFT_REAP_HOLD_FD=%s: %v\n%s", value, err, out.String())
			}
			if got, err := os.ReadFile(filepath.Join(root, "fd.txt")); err != nil || strings.TrimSpace(string(got)) != os.DevNull {
				t.Errorf("the step's descriptor %d is %q (%v), want %s", fd, got, err, os.DevNull)
			}
		})
	}
}
