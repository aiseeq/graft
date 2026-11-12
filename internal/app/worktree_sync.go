package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
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
		err = fmt.Errorf("upstream tracking ref %s for branch %s is unavailable before fetch; fetch it and set the branch upstream before retrying: %w", trackingRef, branch, err)
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
func (a *App) WorktreeSync(o WorktreeSyncOptions) error {
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
	} else if err := worktreeSyncPushTrunk(repo, trunkBranch, branch, pushRemote); err != nil {
		return err
	}
	if err := worktreeSyncRebase(repo, trunk); err != nil {
		return err
	}
	if o.Push {
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

func worktreeSyncRebase(repo *gitx.Repo, trunk string) error {
	if _, err := repo.Git("rebase", "--autostash", trunk); err != nil {
		for _, dir := range []string{"rebase-merge", "rebase-apply"} {
			if _, serr := os.Stat(filepath.Join(repo.GitDir, dir)); serr == nil {
				return fmt.Errorf("a commit of the branch conflicts with %s: resolve and git rebase --continue, or git rebase --abort; the uncommitted changes come back when the rebase ends: %w", trunk, err)
			}
		}
		return fmt.Errorf("git rebase --autostash %s failed, the worktree is as it was: %w", trunk, err)
	}
	out, err := repo.Git("diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return err
	}
	if conflicts := strings.Fields(out); len(conflicts) > 0 {
		return errors.New("on " + trunk + " now, but the uncommitted changes conflict with it in " + strings.Join(conflicts, ", ") +
			": resolve the markers, then git stash drop (the changes are kept in the stash too)")
	}
	return nil
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

func worktreeSyncPushTrunk(repo *gitx.Repo, trunkBranch, branch, remote string) error {
	if trunkBranch == branch {
		return fmt.Errorf("worktree branch %s is the trunk branch; --push cannot update it", branch)
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

func worktreeSyncResolvedTrunkBranch(repo *gitx.Repo, trunk string, remotes []string) (string, bool, error) {
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
	isHeadAlias := originalRef == "refs/remotes/"+branchRemote+"/HEAD" && trunk == branchRemote+"/HEAD"
	return branch, trunk == branchRemote+"/"+branch || trunk == ref || isHeadAlias, nil
}

func worktreeSyncIsRemoteHead(repo *gitx.Repo, branch, remote string) (bool, error) {
	out, err := repo.Git("ls-remote", "--symref", remote, "HEAD")
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
		return "", "", "", "", fmt.Errorf("the upstream of %s is not set; set it with git push -u %s %s", branch, suggestRemote, branch)
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
