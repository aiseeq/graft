package tasks

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"golang.org/x/sys/unix"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/proc"
)

// clonePattern names the temporary directories of the clean clones.
const clonePattern = "graft-install-"

// cloneOwnerFile, in a clone's directory, is locked (flock) by the graft
// using it, and the reaper of every step gets the locked descriptor
// (proc.HoldInReaper), the command below it not: the lock lives while graft
// or a reaper does, one that outlives graft killed under it (SIGKILL,
// SIGTERM in a deploy, which does not catch it) included, but not with a
// daemon the step left. With the last of them gone the next clone removes
// the directory.
const cloneOwnerFile = ".graft-owner.lock"

// cloneOwnerGrace keeps a sweep off a directory whose owner may still be
// between creating its lock file and taking the lock.
const cloneOwnerGrace = time.Minute

// hasInstallStep says whether the task's own steps install files.
func hasInstallStep(t *config.Task) bool {
	return slices.ContainsFunc(t.Run, func(s config.Step) bool { return s.Install != nil })
}

// enterInstallSnapshot runs a task with an install step, its deps included,
// in a clean clone of HEAD when the main worktree has uncommitted changes,
// untracked files among them: what it builds and installs then comes from
// HEAD alone, whatever is written to the tree before or during the build (a
// ticket journal, an edit undone before the install). A clone and not a
// linked worktree: go build stamps vcs.revision only where .git is a
// directory, so the binary passes prepareInstall's revision check honestly.
// The tasks are the clone's .graft.yaml, the same as the tree's: a tree
// whose .graft.yaml changed builds in place, as does a repository a plain
// clone does not reproduce (submodules, LFS, shallow, sparse); there the
// usual refusal of a binary with uncommitted changes follows a line saying
// why. Files git ignores (generated ones, .env, worktree.copy) are not in the
// clone. A clean tree runs as before, and so do a linked worktree, whose Go
// binaries prepareInstall refuses, and the gate, which guards the tree's
// changes. leave puts the runner back, removes the clone and names it in a
// failure.
func (r *Runner) enterInstallSnapshot(name string, t *config.Task) (leave func(error) error, err error) {
	none := func(err error) error { return err }
	if r.snapshotHead != "" || r.inGate || !hasInstallStep(t) {
		return none, nil
	}
	repo, err := r.repo()
	if err != nil {
		return nil, fmt.Errorf("install: opening repository: %w", err)
	}
	if filepath.Clean(repo.GitDir) != filepath.Clean(repo.CommonDir) {
		return none, nil
	}
	hasHead, err := repo.HasHead()
	if err != nil {
		return nil, fmt.Errorf("install: reading HEAD: %w", err)
	}
	if !hasHead {
		return none, nil
	}
	clean, err := repo.IsClean()
	if err != nil {
		return nil, fmt.Errorf("install: checking clean tree: %w", err)
	}
	if clean {
		return none, nil
	}
	head, err := repo.Git("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, fmt.Errorf("install: reading HEAD: %w", err)
	}
	head = strings.TrimSpace(head)
	why, err := noCloneReason(repo)
	if err != nil {
		return nil, fmt.Errorf("install: %w", err)
	}
	if why != "" {
		fmt.Fprintf(r.Stderr, "graft: %s: the tree has uncommitted changes, and HEAD %s is not built in a clean clone: %s\n", name, shortRevision(head), why)
		return none, nil
	}
	sweepClones(r.Stderr)
	root, owner, remove, err := cloneHead(repo, head)
	if err != nil {
		return nil, fmt.Errorf("install: a clean clone of HEAD %s: %w", shortRevision(head), err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("install: the clean clone of HEAD %s: %w", shortRevision(head), err), remove())
	}
	if cfg.Tasks[name] == nil {
		return nil, errors.Join(fmt.Errorf("install: HEAD %s has no task %s", shortRevision(head), name), remove())
	}
	fmt.Fprintf(r.Stderr, "graft: %s: the tree has uncommitted changes; building HEAD %s in a clean clone (what its steps write there goes with it)\n", name, shortRevision(head))
	// The main tree's golangci-lint cache: the clone's path is new each time.
	r.lookLintCache()
	savedRoot, savedRepo, savedConfig, savedDone, savedVersion, savedNoCache := r.Root, r.Repo, r.Config, r.done, r.version, r.NoCache
	gitDir := filepath.Join(root, ".git")
	// Deps run again in the clone: what they made in the tree is not there.
	// The task cache neither skips them, which would leave their outputs out
	// of the clone, nor keeps their passes under its path.
	r.Root, r.Repo, r.Config, r.done, r.version, r.NoCache = root, &gitx.Repo{Root: root, GitDir: gitDir, CommonDir: gitDir}, cfg, map[string]bool{}, nil, true
	r.snapshotHead, r.snapshotOwner, r.snapshotTree = head, owner, savedRoot
	return func(err error) error {
		ran := r.done[name]
		r.Root, r.Repo, r.Config, r.done, r.version, r.NoCache = savedRoot, savedRepo, savedConfig, savedDone, savedVersion, savedNoCache
		r.snapshotHead, r.snapshotOwner, r.snapshotTree = "", nil, ""
		r.done[name] = ran
		if err != nil {
			err = fmt.Errorf("%w (task %s ran in a clean clone of HEAD %s, without the uncommitted changes and the files git ignores, generated ones, .env and worktree.copy among them: commit what it needs or clean the tree)", err, name, shortRevision(head))
		}
		return errors.Join(err, remove())
	}, nil
}

// noCloneReason is why a plain clone of HEAD would not build what the tree
// at HEAD builds, "" when it would: a .graft.yaml with uncommitted changes
// (the tasks would not be the tree's), submodules or LFS files at HEAD, which
// the clone does not fetch, a shallow repository or a sparse checkout.
func noCloneReason(repo *gitx.Repo) (string, error) {
	status, err := repo.Git("status", "--porcelain", "--", config.FileName)
	if err != nil {
		return "", fmt.Errorf("checking %s: %w", config.FileName, err)
	}
	if strings.TrimSpace(status) != "" {
		return config.FileName + " has uncommitted changes, so its tasks are not HEAD's", nil
	}
	modules, err := repo.Git("ls-tree", "--name-only", "HEAD", "--", ".gitmodules")
	if err != nil {
		return "", fmt.Errorf("looking for submodules: %w", err)
	}
	if strings.TrimSpace(modules) != "" {
		return "it has submodules (.gitmodules), which a clone does not fetch", nil
	}
	lfs, err := repo.Git("grep", "-l", "-e", "filter=lfs", "HEAD", "--", ".gitattributes", ":(glob)**/.gitattributes")
	if err != nil && gitx.ExitCode(err) != 1 {
		return "", fmt.Errorf("looking for LFS files: %w", err)
	}
	if strings.TrimSpace(lfs) != "" {
		return "it has LFS files (filter=lfs in .gitattributes), which a clone does not fetch", nil
	}
	shallow, err := repo.Git("rev-parse", "--is-shallow-repository")
	if err != nil {
		return "", fmt.Errorf("checking for a shallow repository: %w", err)
	}
	if strings.TrimSpace(shallow) == "true" {
		return "the repository is shallow", nil
	}
	sparse, err := repo.Git("config", "--type=bool", "--default=false", "core.sparseCheckout")
	if err != nil {
		return "", fmt.Errorf("checking for a sparse checkout: %w", err)
	}
	if strings.TrimSpace(sparse) == "true" {
		return "the work tree is a sparse checkout", nil
	}
	return "", nil
}

// cloneHead checks head out detached in a local clone of repo under a new
// temporary directory, whose owner file it locks and returns open; remove
// deletes the directory and closes the file, which lets the lock go once no
// step holds it any more. The clone has its own objects, hard links
// or, on another file system, copies: a prune of the repository cannot take
// them away mid-build. Its directory has the tree's name: a build may read
// it.
func cloneHead(repo *gitx.Repo, head string) (root string, owner *os.File, remove func() error, err error) {
	parent, err := os.MkdirTemp("", clonePattern)
	if err != nil {
		return "", nil, nil, fmt.Errorf("a temporary directory: %w", err)
	}
	owner, err = os.OpenFile(filepath.Join(parent, cloneOwnerFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return "", nil, nil, errors.Join(fmt.Errorf("the owner file of the clean clone: %w", err), os.RemoveAll(parent))
	}
	remove = func() error {
		var errs []error
		if err := os.RemoveAll(parent); err != nil {
			errs = append(errs, fmt.Errorf("removing the clean clone %s: %w", parent, err))
		}
		if err := owner.Close(); err != nil {
			errs = append(errs, fmt.Errorf("releasing the clean clone %s: %w", parent, err))
		}
		return errors.Join(errs...)
	}
	if err := unix.Flock(int(owner.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return "", nil, nil, errors.Join(fmt.Errorf("locking %s: %w", owner.Name(), err), remove())
	}
	root = filepath.Join(parent, filepath.Base(repo.Root))
	// No hooks: a global core.hooksPath would run them on the checkout.
	noHooks := []string{"-c", "core.hooksPath=" + os.DevNull}
	clone := func(how string) error {
		_, err := gitx.InOther(parent, nil, append(noHooks, "clone", "--quiet", how, "--no-checkout", "--", repo.Root, root)...)
		return err
	}
	// Hard links where they can be; another file system (a tmpfs TMPDIR)
	// refuses them, and git does not copy instead on its own.
	if linkErr := clone("--local"); linkErr != nil {
		if err := os.RemoveAll(root); err != nil {
			return "", nil, nil, errors.Join(linkErr, err, remove())
		}
		if err := clone("--no-hardlinks"); err != nil {
			return "", nil, nil, errors.Join(linkErr, err, remove())
		}
	}
	if _, err := gitx.InOther(root, nil, append(noHooks, "checkout", "--quiet", "--detach", head)...); err != nil {
		return "", nil, nil, errors.Join(err, remove())
	}
	return root, owner, remove, nil
}

// sweepClones removes the clean clones of grafts that died with them: their
// owner file is unlocked and older than cloneOwnerGrace. A directory without
// one is left: its graft may be creating it. What it cannot check or remove
// it says on stderr and leaves: the clone to make does not depend on it.
func sweepClones(stderr io.Writer) {
	warn := func(dir string, err error) {
		fmt.Fprintf(stderr, "graft: install: the clean clone %s left by a graft that ended stays: %v\n", dir, err)
	}
	dirs, err := filepath.Glob(filepath.Join(os.TempDir(), clonePattern+"*"))
	if err != nil {
		warn(clonePattern+"*", err)
		return
	}
	for _, dir := range dirs {
		path := filepath.Join(dir, cloneOwnerFile)
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue // no owner file yet, or a directory that is not a clone
		}
		if err != nil {
			warn(dir, err)
			continue
		}
		if time.Since(info.ModTime()) < cloneOwnerGrace {
			continue
		}
		owner := flock.New(path)
		locked, err := owner.TryLock()
		if err != nil {
			warn(dir, err)
			continue
		}
		if !locked {
			continue // its graft lives
		}
		if err := errors.Join(os.RemoveAll(dir), owner.Unlock()); err != nil {
			warn(dir, err)
		}
	}
}

// holdSnapshot gives the reaper of a local step in a clean clone the clone's
// locked owner file, so that the lock lives while the step does
// (cloneOwnerFile). It goes after stopLocalCommand, which puts the step
// under its reaper.
func (r *Runner) holdSnapshot(cmd *exec.Cmd, local bool) {
	if local && r.snapshotOwner != nil {
		proc.HoldInReaper(cmd, r.snapshotOwner)
	}
}

// project is the work tree the stats name: the tree a clean clone stands
// in for while its tasks run there, so that the project's history has them.
func (r *Runner) project() string {
	if r.snapshotTree != "" {
		return r.snapshotTree
	}
	return r.Root
}
