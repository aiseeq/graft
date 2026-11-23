package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/execx"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/userconfig"
)

// WorktreeSyncOptions are the arguments of graft worktree sync.
type WorktreeSyncOptions struct {
	// Trunk is the ref to sync onto, by default as for graft worktree clean.
	Trunk string
	// Push publishes the rebased branch to its upstream with a lease.
	Push bool
}

func worktreeSyncPushSetup(repo *gitx.Repo, cfg *config.Config, remotes []string, trunkArg string) (trunk, trunkBranch, branch, remote, pushRef, trackingRef, lease string, err error) {
	branch, remote, pushRef, trackingRef, err = worktreeSyncUpstream(repo, cfg, remotes)
	if err != nil {
		return "", "", "", "", "", "", "", err
	}
	trunk, trunkBranch, err = worktreeSyncPushTrunkRef(repo, cfg, remotes, trunkArg)
	if err != nil {
		return "", "", "", "", "", "", "", err
	}
	lease, err = repo.Git("rev-parse", "--verify", trackingRef)
	if err != nil {
		fetchRefspec := "+refs/heads/" + branch + ":refs/remotes/" + remote + "/" + branch
		err = fmt.Errorf("upstream tracking ref %s for branch %s is unavailable before fetch; add its fetch refspec with `git config --add remote.%s.fetch %s`, then run `git fetch %s` and retry: %w", trackingRef, branch, remote, fetchRefspec, remote, err)
		return "", "", "", "", "", "", "", err
	}
	lease = strings.TrimSpace(lease)
	return
}

func worktreeSyncPushTrunkRef(repo *gitx.Repo, cfg *config.Config, remotes []string, trunkArg string) (trunk, branch string, err error) {
	trunk, err = trunkRef(repo, cfg, trunkArg, remotes)
	if err != nil {
		return "", "", err
	}
	var isBranch bool
	branch, isBranch, err = worktreeSyncResolvedTrunkBranch(repo, trunk, remotes)
	if err != nil {
		return "", "", err
	}
	if !isBranch {
		err = fmt.Errorf("--push needs the trunk to be a branch (got %s): graft compares the worktree branch with it", trunk)
	}
	return
}

// WorktreeSync moves the work of the current worktree onto the fresh trunk:
// it fetches every remote, rebases the branch's own commits onto the trunk
// and brings the uncommitted changes back uncommitted (git rebase
// --autostash), so syncing needs no temporary commit past the hooks. Nothing
// is committed. With Push, the branch is pushed to its upstream with a lease.
// Changes that conflict with the trunk are left with their conflict markers
// and kept in the stash too.
func (a *App) WorktreeSync(ctx context.Context, o WorktreeSyncOptions) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	remotes, err := repo.Remotes()
	if err != nil {
		return err
	}
	var trunk, trunkBranch, branch, pushRemote, pushRef, trackingRef, lease string
	if o.Push {
		trunk, trunkBranch, branch, pushRemote, pushRef, trackingRef, lease, err = worktreeSyncPushSetup(repo, cfg, remotes, o.Trunk)
		if err != nil {
			return err
		}
	}
	for _, r := range remotes {
		if _, err := repo.Git("fetch", "--quiet", "--prune", r); err != nil {
			return fmt.Errorf("git fetch %s failed, the trunk may be stale: %w", r, err)
		}
	}
	if o.Push {
		fetchedUpstream, err := repo.Git("rev-parse", "--verify", trackingRef)
		if err != nil {
			return fmt.Errorf("upstream %s for branch %s is not available after fetch; fetch it and set the branch upstream before retrying: %w", trackingRef, branch, err)
		}
		fetchedUpstream = strings.TrimSpace(fetchedUpstream)
		if err := worktreeSyncCheckFetchedUpstream(repo, branch, trackingRef, lease, fetchedUpstream); err != nil {
			return err
		}
	}
	if !o.Push {
		trunk, err = trunkRef(repo, cfg, o.Trunk, remotes)
		if err != nil {
			return err
		}
	} else if err := worktreeSyncPushTrunk(repo, trunk, trunkBranch, branch, pushRemote, pushRef); err != nil {
		return err
	}
	if err := worktreeSyncRebase(ctx, repo, trunk, a.Stderr); err != nil {
		return err
	}
	return worktreeSyncFinish(a, repo, o, trunk, pushRemote, pushRef, trackingRef, lease)
}

func worktreeSyncFinish(a *App, repo *gitx.Repo, o WorktreeSyncOptions, trunk, pushRemote, pushRef, trackingRef, lease string) error {
	if !o.Push {
		if err := worktreeSyncStaleUpstreamHint(a, repo); err != nil {
			return err
		}
	} else {
		if _, err := repo.Git("push", "--force-with-lease="+pushRef+":"+lease, pushRemote, "HEAD:"+pushRef); err != nil {
			return fmt.Errorf("git push --force-with-lease to %s/%s failed; inspect the remote branch and include any new commits with git rebase %s or git merge before syncing again: %w", pushRemote, strings.TrimPrefix(pushRef, "refs/heads/"), trackingRef, err)
		}
		a.printf("pushed to %s/%s with force-with-lease", pushRemote, strings.TrimPrefix(pushRef, "refs/heads/"))
	}
	head, err := repo.Git("rev-parse", "--short", "HEAD")
	if err != nil {
		return err
	}
	if o.Push {
		a.printf("on %s (%s), the uncommitted changes back in the worktree; nothing committed", trunk, strings.TrimSpace(head))
	} else {
		a.printf("on %s (%s), the uncommitted changes back in the worktree; nothing committed or pushed", trunk, strings.TrimSpace(head))
	}
	return nil
}

func worktreeSyncStaleUpstreamHint(a *App, repo *gitx.Repo) error {
	branch, err := repo.Branch()
	if err != nil {
		return err
	}
	remote, remoteBranch, upstream, err := worktreeBranchUpstream(repo, branch)
	if err != nil {
		return err
	}
	if remote == "" || remoteBranch != branch || upstream != "refs/remotes/"+remote+"/"+branch {
		return nil
	}
	if _, err := repo.Git("merge-base", "--is-ancestor", upstream, "HEAD"); err == nil {
		return nil
	} else if gitx.ExitCode(err) != 1 {
		return err
	}
	owned, err := branchReflogContains(repo, branch, "refs/remotes/"+remote+"/"+branch)
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	a.printf("the remote branch %s holds the old history; the next graft commit will not overwrite it. Run `graft worktree sync --push` to replace it with force-with-lease, or `graft merge %s` from the main worktree", upstream, branch)
	return nil
}

func worktreeSyncRebase(ctx context.Context, repo *gitx.Repo, trunk string, stderr io.Writer) error {
	beforeStashes, err := worktreeSyncStashes(repo)
	if err != nil {
		return err
	}
	rebaseOutput, rebaseErr := repo.Git("rebase", "--autostash", trunk)
	for worktreeSyncRebaseStopped(repo) {
		resolved, note, resolveErr := worktreeSyncResolveLatestConflict(ctx, repo)
		if !resolved {
			err := worktreeSyncRebaseConflictError(trunk, rebaseErr)
			if resolveErr != nil {
				return fmt.Errorf("%w: %s", err, resolveErr)
			}
			return err
		}
		conflictsOut, err := repo.Git("diff", "--name-only", "--diff-filter=U")
		if err != nil {
			return worktreeSyncRebaseConflictError(trunk, rebaseErr)
		}
		conflicts := strings.Fields(conflictsOut)
		moduleDir := filepath.Dir(conflicts[0])
		if err := worktreeSyncStageModuleFiles(repo, moduleDir, conflicts); err != nil {
			return worktreeSyncRebaseConflictError(trunk, rebaseErr)
		}
		fmt.Fprintln(stderr, note)
		if _, err := repo.GitEnv([]string{"GIT_EDITOR=true"}, "rebase", "--continue"); err != nil {
			rebaseErr = err
			if worktreeSyncRebaseStopped(repo) {
				continue
			}
			break
		}
		rebaseErr = nil
	}
	out, diffErr := repo.Git("diff", "--name-only", "--diff-filter=U")
	if diffErr != nil {
		return diffErr
	}
	if conflicts := strings.Fields(out); len(conflicts) > 0 {
		stash, stashErr := worktreeSyncNewAutostash(repo, beforeStashes, worktreeSyncCreatedAutostash(rebaseOutput, rebaseErr))
		if stashErr != nil {
			return fmt.Errorf("the uncommitted changes conflict with %s in %s; could not identify their autostash: %w", trunk, strings.Join(conflicts, ", "), stashErr)
		}
		dropCommand := `git stash drop "$(git stash list --format='%gd %H' | awk '$2 == "` + stash + `" {print $1; exit}')"`
		message := "the uncommitted changes conflict with " + trunk + " in " + strings.Join(conflicts, ", ") +
			"; autostash " + stash + " is kept in the shared stash. Resolve the markers, then remove this entry with: " + dropCommand
		if hint, hintErr := worktreeSyncGoModHint(repo, conflicts); hintErr != nil {
			message += "; could not compare go.mod versions: " + hintErr.Error()
		} else if hint != "" {
			message += "; " + hint
		}
		return errors.New(message)
	}
	if rebaseErr != nil {
		return fmt.Errorf("git rebase --autostash %s failed, the worktree is as it was: %w", trunk, rebaseErr)
	}
	return nil
}

type worktreeSyncModuleChange struct {
	path, onto, commit, version string
	bothSidesChanged            bool
}

func worktreeSyncLatestGoModChanges(baseText, ontoText, commitText string, isLatest func(string) bool) ([]worktreeSyncModuleChange, bool, error) {
	base, err := modfile.Parse("go.mod", []byte(baseText), nil)
	if err != nil {
		return nil, false, fmt.Errorf("parsing base go.mod: %w", err)
	}
	onto, err := modfile.Parse("go.mod", []byte(ontoText), nil)
	if err != nil {
		return nil, false, fmt.Errorf("parsing onto go.mod: %w", err)
	}
	commit, err := modfile.Parse("go.mod", []byte(commitText), nil)
	if err != nil {
		return nil, false, fmt.Errorf("parsing commit go.mod: %w", err)
	}
	ontoChanges, ok, err := worktreeSyncLatestSideChanges(base, onto, isLatest)
	if err != nil {
		return nil, false, fmt.Errorf("checking onto go.mod: %w", err)
	}
	if !ok {
		return nil, false, nil
	}
	commitChanges, ok, err := worktreeSyncLatestSideChanges(base, commit, isLatest)
	if err != nil {
		return nil, false, fmt.Errorf("checking commit go.mod: %w", err)
	}
	if !ok {
		return nil, false, nil
	}
	return worktreeSyncCombineLatestGoModChanges(base, ontoChanges, commitChanges)
}

func worktreeSyncCombineLatestGoModChanges(base *modfile.File, ontoChanges, commitChanges map[string]string) ([]worktreeSyncModuleChange, bool, error) {
	changes := make([]worktreeSyncModuleChange, 0, len(ontoChanges)+len(commitChanges))
	paths := make([]string, 0, len(ontoChanges)+len(commitChanges))
	for path := range ontoChanges {
		paths = append(paths, path)
	}
	for path := range commitChanges {
		if _, exists := ontoChanges[path]; !exists {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	for _, path := range paths {
		ontoVersion, exists := ontoChanges[path]
		if !exists {
			ontoVersion = baseRequireVersion(base, path)
		}
		changes = append(changes, worktreeSyncModuleChange{path: path, onto: ontoVersion, commit: baseRequireVersion(base, path), version: ontoVersion})
	}
	for i := range changes {
		if version, exists := commitChanges[changes[i].path]; exists {
			changes[i].commit = version
		}
		_, ontoChanged := ontoChanges[changes[i].path]
		_, commitChanged := commitChanges[changes[i].path]
		changes[i].bothSidesChanged = ontoChanged && commitChanged
	}
	if len(ontoChanges) == 0 || len(commitChanges) == 0 {
		return nil, false, nil
	}
	var differing int
	for i := range changes {
		if changes[i].bothSidesChanged && changes[i].onto != changes[i].commit {
			differing++
		}
		if semver.Compare(changes[i].onto, changes[i].commit) > 0 {
			changes[i].version = changes[i].onto
		} else {
			changes[i].version = changes[i].commit
		}
	}
	if differing == 0 {
		return nil, false, nil
	}
	return changes, true, nil
}

func worktreeSyncLatestSideChanges(base, side *modfile.File, isLatest func(string) bool) (map[string]string, bool, error) {
	baseVersions := map[string]string{}
	for _, req := range base.Require {
		baseVersions[req.Mod.Path] = req.Mod.Version
	}
	changes := map[string]string{}
	for _, req := range side.Require {
		old, exists := baseVersions[req.Mod.Path]
		if !exists {
			continue
		}
		if req.Mod.Version == old {
			continue
		}
		if !isLatest(req.Mod.Path) || !semver.IsValid(old) || !semver.IsValid(req.Mod.Version) || semver.Compare(req.Mod.Version, old) <= 0 {
			return nil, false, nil
		}
		changes[req.Mod.Path] = req.Mod.Version
	}
	if len(changes) == 0 {
		return nil, false, nil
	}
	sideData, err := side.Format()
	if err != nil {
		return nil, false, fmt.Errorf("formatting go.mod: %w", err)
	}
	check, err := modfile.Parse("go.mod", sideData, nil)
	if err != nil {
		return nil, false, fmt.Errorf("parsing formatted go.mod: %w", err)
	}
	for path := range changes {
		if err := worktreeSyncSetRequireVersion(check, path, baseVersions[path]); err != nil {
			return nil, false, err
		}
	}
	equal, err := worktreeSyncGoModFilesEqual(check, base)
	if err != nil {
		return nil, false, fmt.Errorf("comparing normalized go.mod: %w", err)
	}
	return changes, equal, nil
}

func baseRequireVersion(file *modfile.File, path string) string {
	for _, req := range file.Require {
		if req.Mod.Path == path {
			return req.Mod.Version
		}
	}
	return ""
}

func worktreeSyncMergeLatestGoMod(ontoText string, changes []worktreeSyncModuleChange) ([]byte, []worktreeSyncModuleChange, error) {
	file, err := modfile.Parse("go.mod", []byte(ontoText), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing onto go.mod: %w", err)
	}
	for _, change := range changes {
		if err := worktreeSyncSetRequireVersion(file, change.path, change.version); err != nil {
			return nil, nil, err
		}
	}
	data, err := file.Format()
	return data, changes, err
}

func worktreeSyncRebaseStopped(repo *gitx.Repo) bool {
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(repo.GitDir, dir)); err == nil {
			return true
		}
	}
	return false
}

func worktreeSyncRebaseConflictError(trunk string, rebaseErr error) error {
	return fmt.Errorf("a commit of the branch conflicts with %s: resolve and git rebase --continue, or git rebase --abort; the uncommitted changes come back when the rebase ends: %w", trunk, rebaseErr)
}

func worktreeSyncResolveLatestConflict(ctx context.Context, repo *gitx.Repo) (bool, string, error) {
	conflictsOut, err := repo.Git("diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return false, "", fmt.Errorf("listing rebase conflicts: %w", err)
	}
	conflicts := strings.Fields(conflictsOut)
	moduleDir, ok := worktreeSyncConflictModuleDir(conflicts)
	if !ok {
		return false, "", nil
	}
	baseGoMod, ontoGoMod, commitGoMod, err := worktreeSyncConflictGoMods(repo, moduleDir)
	if err != nil {
		return false, "", err
	}
	return worktreeSyncResolveLatestConflictFrom(ctx, repo, moduleDir, baseGoMod, ontoGoMod, commitGoMod)
}

func worktreeSyncResolveLatestConflictFrom(ctx context.Context, repo *gitx.Repo, moduleDir, baseGoMod, ontoGoMod, commitGoMod string) (bool, string, error) {
	settings, err := userconfig.ModulesSettings()
	if err != nil {
		return false, "", fmt.Errorf("reading modules.latest: %w", err)
	}
	changes, ok, err := worktreeSyncLatestGoModChanges(baseGoMod, ontoGoMod, commitGoMod, settings.IsLatest)
	if err != nil || !ok {
		return false, "", err
	}
	return worktreeSyncApplyLatestConflict(ctx, repo, moduleDir, ontoGoMod, changes)
}

func worktreeSyncConflictModuleDir(conflicts []string) (string, bool) {
	if len(conflicts) == 0 {
		return "", false
	}
	moduleDir := ""
	for _, path := range conflicts {
		dir := filepath.Dir(path)
		if filepath.Base(path) != "go.mod" && filepath.Base(path) != "go.sum" || moduleDir != "" && dir != moduleDir {
			return "", false
		}
		moduleDir = dir
	}
	return moduleDir, true
}

func worktreeSyncStageModuleFiles(repo *gitx.Repo, moduleDir string, conflicts []string) error {
	var paths []string
	for _, name := range []string{"go.mod", "go.sum"} {
		path := filepath.ToSlash(filepath.Join(moduleDir, name))
		_, statErr := os.Stat(filepath.Join(repo.Root, filepath.FromSlash(path)))
		if statErr == nil || slices.Contains(conflicts, path) {
			paths = append(paths, path)
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("checking %s: %w", path, statErr)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	_, err := repo.Git(append([]string{"add", "--"}, paths...)...)
	return err
}

func worktreeSyncConflictGoMods(repo *gitx.Repo, moduleDir string) (baseGoMod, ontoGoMod, commitGoMod string, err error) {
	base, err := repo.Git("rev-parse", "REBASE_HEAD^")
	if err != nil {
		return "", "", "", fmt.Errorf("finding parent of rebased commit: %w", err)
	}
	path := filepath.ToSlash(filepath.Join(moduleDir, "go.mod"))
	baseGoMod, err = repo.Git("show", strings.TrimSpace(base)+":"+path)
	if err != nil {
		return "", "", "", fmt.Errorf("reading base go.mod: %w", err)
	}
	ontoGoMod, err = repo.Git("show", "HEAD:"+path)
	if err != nil {
		return "", "", "", fmt.Errorf("reading onto go.mod: %w", err)
	}
	commitGoMod, err = repo.Git("show", "REBASE_HEAD:"+path)
	if err != nil {
		return "", "", "", fmt.Errorf("reading commit go.mod: %w", err)
	}
	return baseGoMod, ontoGoMod, commitGoMod, nil
}

func worktreeSyncApplyLatestConflict(ctx context.Context, repo *gitx.Repo, moduleDir, ontoGoMod string, changes []worktreeSyncModuleChange) (bool, string, error) {
	vendorManifest := filepath.Join(repo.Root, filepath.FromSlash(filepath.Join(moduleDir, "vendor", "modules.txt")))
	if _, err := os.Stat(vendorManifest); err == nil {
		return false, "", nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, "", fmt.Errorf("checking vendor/modules.txt: %w", err)
	}
	paths := []string{filepath.Join(repo.Root, filepath.FromSlash(filepath.Join(moduleDir, "go.mod"))), filepath.Join(repo.Root, filepath.FromSlash(filepath.Join(moduleDir, "go.sum")))}
	backups, err := worktreeSyncBackupFiles(paths)
	if err != nil {
		return false, "", err
	}
	rollback := func(cause error) (bool, string, error) {
		if restoreErr := worktreeSyncRestoreFiles(backups); restoreErr != nil {
			return false, "", fmt.Errorf("%w; restoring conflicted files: %v", cause, restoreErr)
		}
		return false, "", cause
	}
	merged, reports, err := worktreeSyncMergeLatestGoMod(ontoGoMod, changes)
	if err != nil {
		return rollback(fmt.Errorf("merging raised requirements: %w", err))
	}
	goModPath := paths[0]
	if err := os.WriteFile(goModPath, merged, 0o666); err != nil {
		return rollback(fmt.Errorf("writing merged go.mod: %w", err))
	}
	if err := worktreeSyncResetGoSumToOnto(repo, moduleDir, paths[1]); err != nil {
		return rollback(err)
	}
	if err := worktreeSyncGoModDownloadAndTidy(ctx, filepath.Dir(goModPath), reports); err != nil {
		return rollback(err)
	}

	tidied, err := os.ReadFile(goModPath)
	if err != nil {
		return rollback(fmt.Errorf("reading tidied go.mod: %w", err))
	}
	if err := worktreeSyncCheckTidyGoMod(merged, tidied, reports); err != nil {
		return rollback(err)
	}
	var differing []worktreeSyncModuleChange
	for _, item := range reports {
		if item.bothSidesChanged && item.onto != item.commit {
			differing = append(differing, item)
		}
	}
	var modules []string
	for _, item := range differing {
		modules = append(modules, fmt.Sprintf("%s (%s vs %s), took %s", item.path, item.onto, item.commit, item.version))
	}
	note := "graft: worktree sync resolved " + strings.Join(modules, "; ")
	return true, note, nil
}

type worktreeSyncFileBackup struct {
	path   string
	data   []byte
	mode   os.FileMode
	exists bool
}

func worktreeSyncBackupFiles(paths []string) ([]worktreeSyncFileBackup, error) {
	backups := make([]worktreeSyncFileBackup, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			backups = append(backups, worktreeSyncFileBackup{path: path})
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		backups = append(backups, worktreeSyncFileBackup{path: path, data: data, mode: info.Mode().Perm(), exists: true})
	}
	return backups, nil
}

func worktreeSyncRestoreFiles(backups []worktreeSyncFileBackup) error {
	var errs []error
	for _, backup := range backups {
		if !backup.exists {
			if err := os.Remove(backup.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("removing %s: %w", backup.path, err))
			}
			continue
		}
		if err := os.WriteFile(backup.path, backup.data, backup.mode); err != nil {
			errs = append(errs, fmt.Errorf("writing %s: %w", backup.path, err))
		}
	}
	return errors.Join(errs...)
}

func worktreeSyncResetGoSumToOnto(repo *gitx.Repo, moduleDir, sumPath string) error {
	relative := filepath.ToSlash(filepath.Join(moduleDir, "go.sum"))
	if _, err := repo.Git("cat-file", "-e", "HEAD:"+relative); err != nil {
		if gitx.ExitCode(err) == 128 {
			entries, listErr := repo.Git("ls-tree", "-r", "--name-only", "HEAD", "--", relative)
			if listErr != nil {
				return fmt.Errorf("checking onto go.sum: %w", listErr)
			}
			if strings.TrimSpace(entries) == "" {
				if err := os.Remove(sumPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("removing go.sum absent from onto: %w", err)
				}
				return nil
			}
		}
		return fmt.Errorf("checking onto go.sum: %w", err)
	}
	data, err := repo.Git("show", "HEAD:"+relative)
	if err != nil {
		return fmt.Errorf("reading onto go.sum: %w", err)
	}
	if err := os.WriteFile(sumPath, []byte(data), 0o666); err != nil {
		return fmt.Errorf("writing onto go.sum: %w", err)
	}
	return nil
}

func worktreeSyncGoModDownloadAndTidy(ctx context.Context, dir string, reports []worktreeSyncModuleChange) error {
	goPath, err := exec.LookPath("go")
	if err != nil {
		return fmt.Errorf("looking up go: %w", err)
	}
	drop, err := gitx.LocalEnvVars()
	if err != nil {
		return fmt.Errorf("listing git environment variables: %w", err)
	}
	env := gitx.Without(os.Environ(), drop)
	env = append(env, "GOWORK=off")
	for _, item := range reports {
		out, stderr, err := execx.Run(ctx, dir, goPath, env, "mod", "download", item.path+"@"+item.version)
		if err != nil {
			reason := strings.Join(strings.Fields(stderr+"\n"+out), " ")
			if reason == "" {
				reason = err.Error()
			}
			return fmt.Errorf("go mod download %s@%s failed: %s", item.path, item.version, reason)
		}
	}
	out, stderr, err := execx.Run(ctx, dir, goPath, env, "mod", "tidy")
	if err != nil {
		reason := strings.Join(strings.Fields(stderr+"\n"+out), " ")
		if reason == "" {
			reason = err.Error()
		}
		return fmt.Errorf("go mod tidy failed: %s", reason)
	}
	return nil
}

func worktreeSyncCheckTidyGoMod(beforeText, afterText []byte, allowed []worktreeSyncModuleChange) error {
	before, err := modfile.Parse("go.mod", beforeText, nil)
	if err != nil {
		return fmt.Errorf("parsing pre-tidy go.mod: %w", err)
	}
	after, err := modfile.Parse("go.mod", afterText, nil)
	if err != nil {
		return fmt.Errorf("parsing tidied go.mod: %w", err)
	}
	for _, change := range allowed {
		var beforeReq, afterReq *modfile.Require
		for _, req := range before.Require {
			if req.Mod.Path == change.path {
				beforeReq = req
			}
		}
		for _, req := range after.Require {
			if req.Mod.Path == change.path {
				afterReq = req
			}
		}
		if beforeReq == nil || afterReq == nil {
			return fmt.Errorf("go mod tidy removed require %s", change.path)
		}
		if err := before.DropRequire(change.path); err != nil {
			return fmt.Errorf("excluding raised require from pre-tidy comparison: %w", err)
		}
		if err := after.DropRequire(change.path); err != nil {
			return fmt.Errorf("excluding raised require from tidy comparison: %w", err)
		}
	}
	beforeData, err := before.Format()
	if err != nil {
		return fmt.Errorf("formatting pre-tidy go.mod: %w", err)
	}
	afterData, err := after.Format()
	if err != nil {
		return fmt.Errorf("formatting tidied go.mod: %w", err)
	}
	beforeData = bytes.ReplaceAll(beforeData, []byte("\n\nrequire (\n\n)\n"), []byte("\n"))
	afterData = bytes.ReplaceAll(afterData, []byte("\n\nrequire (\n\n)\n"), []byte("\n"))
	equal := bytes.Equal(beforeData, afterData)
	if !equal {
		return errors.New("go mod tidy changed go.mod outside the raised require lines")
	}
	return nil
}

func worktreeSyncStashes(repo *gitx.Repo) (map[string]bool, error) {
	out, err := repo.Git("stash", "list", "--format=%H %gs")
	if err != nil {
		return nil, err
	}
	stashes := make(map[string]bool)
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if sha, _, ok := strings.Cut(line, " "); ok {
			stashes[sha] = true
		}
	}
	return stashes, nil
}

func worktreeSyncNewAutostash(repo *gitx.Repo, before map[string]bool, created string) (string, error) {
	out, err := repo.Git("stash", "list", "--format=%H %gs")
	if err != nil {
		return "", err
	}
	var found string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		sha, subject, ok := strings.Cut(line, " ")
		if ok && subject == "autostash" && !before[sha] && (created == "" || strings.HasPrefix(sha, created)) {
			if found != "" {
				return "", errors.New("more than one new autostash entry appeared")
			}
			found = sha
		}
	}
	if found == "" {
		return "", errors.New("no new autostash entry was found")
	}
	return found, nil
}

func worktreeSyncCreatedAutostash(output string, rebaseErr error) string {
	if gitErr, ok := errors.AsType[*gitx.Error](rebaseErr); ok {
		output += "\n" + gitErr.Stderr
	}
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		short, ok := strings.CutPrefix(line, "Created autostash: ")
		if ok {
			if fields := strings.Fields(short); len(fields) > 0 {
				return fields[0]
			}
		}
	}
	return ""
}

func worktreeSyncGoModHint(repo *gitx.Repo, conflicts []string) (string, error) {
	if !slices.Contains(conflicts, "go.mod") {
		return "", nil
	}
	for _, path := range conflicts {
		if path != "go.mod" && path != "go.sum" {
			return "", nil
		}
	}
	ours, err := repo.Git("show", ":2:go.mod")
	if err != nil {
		return "", fmt.Errorf("reading stage 2: %w", err)
	}
	theirs, err := repo.Git("show", ":3:go.mod")
	if err != nil {
		return "", fmt.Errorf("reading stage 3: %w", err)
	}
	return worktreeSyncGoModVersionHint(ours, theirs)
}

func worktreeSyncGoModVersionHint(ours, theirs string) (string, error) {
	oursFile, err := modfile.ParseLax("go.mod", []byte(ours), nil)
	if err != nil {
		return "", fmt.Errorf("parsing stage 2: %w", err)
	}
	theirsFile, err := modfile.ParseLax("go.mod", []byte(theirs), nil)
	if err != nil {
		return "", fmt.Errorf("parsing stage 3: %w", err)
	}
	path, oursVersion, theirsVersion, ok := worktreeSyncRequireVersionChange(oursFile, theirsFile)
	if !ok || !semver.IsValid(oursVersion) || !semver.IsValid(theirsVersion) {
		return "", nil
	}
	if err := worktreeSyncSetRequireVersion(oursFile, path, theirsVersion); err != nil {
		return "", err
	}
	equal, err := worktreeSyncGoModFilesEqual(oursFile, theirsFile)
	if err != nil {
		return "", err
	}
	if !equal {
		return "", nil
	}
	newer := oursVersion
	if semver.Compare("v"+strings.TrimPrefix(theirsVersion, "v"), "v"+strings.TrimPrefix(oursVersion, "v")) > 0 {
		newer = theirsVersion
	}
	return fmt.Sprintf("only %s differs by version (%s vs %s); keep the newer %s, then run go mod tidy", path, oursVersion, theirsVersion, newer), nil
}

func worktreeSyncRequireVersionChange(ours, theirs *modfile.File) (path, oursVersion, theirsVersion string, ok bool) {
	if len(ours.Require) != len(theirs.Require) {
		return "", "", "", false
	}
	versions := map[string]string{}
	for _, req := range ours.Require {
		versions[req.Mod.Path] = req.Mod.Version
	}
	for _, req := range theirs.Require {
		oursSideVersion, exists := versions[req.Mod.Path]
		if !exists || oursSideVersion == req.Mod.Version {
			continue
		}
		if path != "" {
			return "", "", "", false
		}
		path, oursVersion, theirsVersion = req.Mod.Path, oursSideVersion, req.Mod.Version
	}
	if path == "" {
		return "", "", "", false
	}
	for _, req := range ours.Require {
		if !slices.ContainsFunc(theirs.Require, func(other *modfile.Require) bool {
			return other.Mod.Path == req.Mod.Path
		}) {
			return "", "", "", false
		}
	}
	return path, oursVersion, theirsVersion, true
}

func worktreeSyncSetRequireVersion(file *modfile.File, path, version string) error {
	for _, req := range file.Require {
		if req.Mod.Path != path {
			continue
		}
		req.Mod.Version = version
		if req.Syntax == nil {
			return errors.New("require directive has no syntax")
		}
		token := 1
		if !req.Syntax.InBlock {
			token = 2
		}
		if len(req.Syntax.Token) <= token {
			return fmt.Errorf("require %s has no version token", path)
		}
		req.Syntax.Token[token] = version
		return nil
	}
	return fmt.Errorf("require %s was not found", path)
}

func worktreeSyncGoModFilesEqual(ours, theirs *modfile.File) (bool, error) {
	oursFormatted, err := ours.Format()
	if err != nil {
		return false, fmt.Errorf("formatting normalized stage 2: %w", err)
	}
	theirsFormatted, err := theirs.Format()
	if err != nil {
		return false, fmt.Errorf("formatting stage 3: %w", err)
	}
	return bytes.Equal(oursFormatted, theirsFormatted), nil
}

func worktreeSyncCheckFetchedUpstream(repo *gitx.Repo, branch, trackingRef, before, after string) error {
	branchRef := "refs/heads/" + branch
	included, err := repo.IsAncestor(after, branchRef)
	if err != nil {
		return err
	}
	branchLog, err := repo.Git("reflog", "show", "--format=%H", branchRef)
	if err != nil {
		return err
	}
	var reflogCommits []string
	if !included {
		for commit := range strings.FieldsSeq(branchLog) {
			included, err = repo.IsAncestor(after, commit)
			if err != nil {
				return err
			}
			if included {
				break
			}
			reflogCommits = append(reflogCommits, commit)
		}
	}
	if included {
		return nil
	}
	args := []string{"rev-list", "--abbrev=12", "--abbrev-commit", "--max-count=5", after, "--not", branchRef}
	args = append(args, reflogCommits...)
	commits, err := repo.Git(args...)
	if err != nil {
		return err
	}
	commits = strings.Join(strings.Fields(commits), ", ")
	if commits == "" {
		commits, err = repo.Git("rev-parse", "--short", after)
		if err != nil {
			return err
		}
		commits = strings.TrimSpace(commits)
	}
	beforeShort, err := repo.Git("rev-parse", "--short", before)
	if err != nil {
		return err
	}
	afterShort, err := repo.Git("rev-parse", "--short", after)
	if err != nil {
		return err
	}
	return fmt.Errorf("upstream %s has commits not included in branch %s: %s (tracking tip %s before fetch, %s after fetch); include them with git rebase %s or git merge, then run graft worktree sync --push", trackingRef, branch, commits, strings.TrimSpace(beforeShort), strings.TrimSpace(afterShort), trackingRef)
}

func worktreeSyncPushTrunk(repo *gitx.Repo, trunk, trunkBranch, branch, remote, pushRef string) error {
	if trunkBranch == branch {
		return fmt.Errorf("worktree branch %s is the trunk branch; --push cannot update it", branch)
	}
	trunkRef, err := repo.Git("rev-parse", "--symbolic-full-name", trunk)
	if err != nil {
		return err
	}
	if localTrunkRef := strings.TrimSpace(trunkRef); strings.HasPrefix(localTrunkRef, "refs/heads/") {
		upstream, err := repo.Git("for-each-ref", "--format=%(upstream:remotename) %(upstream:remoteref)", localTrunkRef)
		if err != nil {
			return err
		}
		fields := strings.Fields(upstream)
		if len(fields) == 2 && fields[0] == remote && fields[1] == pushRef {
			return fmt.Errorf("worktree branch %s targets the upstream of trunk %s; --push cannot update %s/%s", branch, trunk, remote, strings.TrimPrefix(pushRef, "refs/heads/"))
		}
	}
	isRemoteHead, err := worktreeSyncIsRemoteHead(repo, branch, remote)
	if err != nil {
		return err
	}
	if isRemoteHead {
		return fmt.Errorf("worktree branch %s is the remote %s default branch; --push cannot update it", branch, remote)
	}
	return nil
}

func worktreeSyncTrunkHeadAlias(repo *gitx.Repo, trunk string, remotes []string) (string, bool, error) {
	for _, remote := range remotes {
		if trunk != remote+"/HEAD" && trunk != "refs/remotes/"+remote+"/HEAD" {
			continue
		}
		target, err := repo.Git("symbolic-ref", "--quiet", "refs/remotes/"+remote+"/HEAD")
		if err == nil {
			branch, ok := strings.CutPrefix(strings.TrimSpace(target), "refs/remotes/"+remote+"/")
			if ok && branch != "" && branch != "HEAD" {
				return branch, true, nil
			}
		} else if gitx.ExitCode(err) != 1 {
			return "", false, err
		}
	}
	return "", false, nil
}

func worktreeSyncResolvedTrunkBranch(repo *gitx.Repo, trunk string, remotes []string) (string, bool, error) {
	if branch, isAlias, err := worktreeSyncTrunkHeadAlias(repo, trunk, remotes); err != nil || isAlias {
		return branch, isAlias, err
	}
	ref, err := repo.Git("rev-parse", "--symbolic-full-name", trunk)
	if err != nil {
		return "", false, err
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false, nil
	}
	originalRef := ref
	if target, err := repo.Git("symbolic-ref", "--quiet", ref); err == nil {
		ref = strings.TrimSpace(target)
	} else if gitx.ExitCode(err) != 1 {
		return "", false, err
	}
	if branch, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return branch, trunk == branch || trunk == ref || trunk == "HEAD", nil
	}
	branch, branchRemote, longestRemote := "", "", 0
	for _, remote := range remotes {
		if candidate, ok := strings.CutPrefix(ref, "refs/remotes/"+remote+"/"); ok && len(remote) > longestRemote {
			branch, branchRemote, longestRemote = candidate, remote, len(remote)
		}
	}
	if branchRemote == "" {
		return "", false, nil
	}
	if branch == "HEAD" && originalRef == "refs/remotes/"+branchRemote+"/HEAD" {
		return "", false, nil
	}
	return branch, trunk == branchRemote+"/"+branch || trunk == ref, nil
}

func remotePushURLs(repo *gitx.Repo, remote string) ([]string, error) {
	urls, err := repo.Git("remote", "get-url", "--push", "--all", remote)
	if err != nil {
		return nil, fmt.Errorf("cannot inspect push URLs for remote %s: %w", remote, err)
	}
	pushURLs := strings.Split(strings.TrimSuffix(urls, "\n"), "\n")
	if len(pushURLs) == 0 || (len(pushURLs) == 1 && pushURLs[0] == "") {
		return nil, fmt.Errorf("remote %s has no push URL to check", remote)
	}
	return pushURLs, nil
}

func worktreeSyncIsRemoteHead(repo *gitx.Repo, branch, remote string) (bool, error) {
	pushURLs, err := remotePushURLs(repo, remote)
	if err != nil {
		return false, err
	}
	if len(pushURLs) > 1 {
		flag := "--delete"
		set, err := repo.Git("config", "--get-all", "remote."+remote+".pushurl")
		if err != nil && gitx.ExitCode(err) != 1 {
			return false, fmt.Errorf("cannot read pushurl of remote %s: %w", remote, err)
		}
		if strings.TrimSpace(set) != "" {
			flag = "--delete --push"
		}
		return false, fmt.Errorf("remote %s has multiple push URLs (%s); refusing --push because each server's default branch must be checked separately; keep one by running `git remote set-url %s %s <url>` for each other URL, or use separate remotes", remote, strings.Join(pushURLs, ", "), flag, remote)
	}
	out, err := repo.Git("ls-remote", "--symref", pushURLs[0], "HEAD")
	if err != nil {
		return false, fmt.Errorf("git ls-remote --symref %s HEAD failed; cannot verify its default branch: %w", remote, err)
	}
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "ref:" && fields[2] == "HEAD" {
			remoteBranch, ok := strings.CutPrefix(fields[1], "refs/heads/")
			if !ok {
				return false, fmt.Errorf("remote %s HEAD points to %s, not a branch", remote, fields[1])
			}
			return remoteBranch == branch, nil
		}
	}
	return false, fmt.Errorf("remote %s does not advertise a branch for HEAD", remote)
}
func worktreeSyncUpstream(repo *gitx.Repo, cfg *config.Config, remotes []string) (branch, remote, remoteBranch, trackingRef string, err error) {
	branchOut, err := repo.Git("symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return "", "", "", "", fmt.Errorf("graft worktree sync --push needs a named branch; set an upstream with git push -u <remote> <branch>: %w", err)
	}
	branch, ok := strings.CutPrefix(strings.TrimSpace(branchOut), "refs/heads/")
	if !ok {
		return "", "", "", "", fmt.Errorf("current HEAD %s is not a local branch", strings.TrimSpace(branchOut))
	}
	suggestRemote := "origin"
	if cfg.Deploy != nil && cfg.Deploy.Remote != "" {
		suggestRemote = cfg.Deploy.Remote
	}
	upstreamOut, err := repo.Git("for-each-ref", "--format=%(upstream:remotename) %(upstream:remoteref) %(upstream)", "refs/heads/"+branch)
	if err != nil {
		return "", "", "", "", err
	}
	fields := strings.Fields(upstreamOut)
	if len(fields) != 3 {
		fetchRefspec := "+refs/heads/" + branch + ":refs/remotes/" + suggestRemote + "/" + branch
		if slices.Contains(remotes, suggestRemote) {
			existingRemoteBranch, checkErr := repo.Git("ls-remote", "--heads", suggestRemote, "refs/heads/"+branch)
			if checkErr != nil {
				return "", "", "", "", fmt.Errorf("the upstream of %s is not set and its remote branch could not be checked: %w", branch, checkErr)
			}
			if strings.TrimSpace(existingRemoteBranch) != "" {
				return "", "", "", "", fmt.Errorf("the upstream of %s is not set, but %s/%s already exists; add its fetch refspec with `git config --add remote.%s.fetch %s`, run `git fetch %s`, then set it with `git branch --set-upstream-to=%s/%s %s`", branch, suggestRemote, branch, suggestRemote, fetchRefspec, suggestRemote, suggestRemote, branch, branch)
			}
		}
		return "", "", "", "", fmt.Errorf("the upstream of %s is not set and %s/%s does not exist yet; publish this new branch with `git push -u %s %s`, then retry", branch, suggestRemote, branch, suggestRemote, branch)
	}
	remote, remoteBranch, trackingRef = fields[0], fields[1], fields[2]
	branchName, isRemoteBranch := strings.CutPrefix(remoteBranch, "refs/heads/")
	upstreamName := remote + "/" + branchName
	if remote == "." {
		return "", "", "", "", fmt.Errorf("the upstream of %s is %s, not a remote branch: graft pushes a worktree branch only to its own name; push it once with git push -u %s %s", branch, upstreamName, suggestRemote, branch)
	}
	if remote != "." && !slices.Contains(remotes, remote) {
		return "", "", "", "", fmt.Errorf("the upstream of %s uses remote %s, which is not configured", branch, remote)
	}
	if !isRemoteBranch || branchName != branch {
		return "", "", "", "", fmt.Errorf("the upstream of %s is %s, not %s/%s: graft pushes a worktree branch only to its own name; push it once with git push -u %s %s", branch, upstreamName, remote, branch, remote, branch)
	}
	if !strings.HasPrefix(trackingRef, "refs/remotes/") {
		return "", "", "", "", fmt.Errorf("branch %s upstream tracking ref %s is not a remote tracking branch", branch, trackingRef)
	}
	return branch, remote, remoteBranch, trackingRef, nil
}
