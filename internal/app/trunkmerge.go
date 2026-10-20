package app

import (
	"fmt"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/gitx"
)

// mergeIntoTrunk records the deployed branch on the trunk once the target
// runs it (merge_into_trunk): a merge that keeps the trunk's tree, as git
// merge -s ours does, so that the next deploy from the trunk finds what the
// target runs among its ancestors. It merges only when every commit of the
// branch the trunk lacks is a revert, work the release withheld that the
// trunk keeps, or a change the trunk already has by content: anything else,
// a fix made on the branch, the merge would drop from the trunk without a
// word. The release is live by then, so a failure is returned for the exit
// status.
func (a *App) mergeIntoTrunk(r *deployRun) error {
	if !r.target.MergeIntoTrunk || r.dryRun {
		return nil
	}
	remote := r.cfg.Deploy.Remote
	fail := func(err error) error {
		return fmt.Errorf("%s is not merged into the trunk: %w", r.branch, err)
	}
	if _, err := r.repo.Git("fetch", "--quiet", remote); err != nil {
		return fail(fmt.Errorf("git fetch %s: %w", remote, err))
	}
	remotes, err := r.repo.Remotes()
	if err != nil {
		return fail(err)
	}
	trunk, err := trunkRef(r.repo, r.cfg, "", remotes)
	if err != nil {
		return fail(err)
	}
	branch := strings.TrimPrefix(trunk, remote+"/")
	if r.branch == branch {
		return nil
	}
	if in, err := r.repo.IsAncestor(r.head, trunk); err != nil || in {
		return err
	}
	missing, err := notInTrunk(r.repo, trunk, r.head, r.cfg.VersionFiles())
	if err != nil {
		return fail(err)
	}
	if len(missing) > 0 {
		how := "git merge -s ours --no-edit " + r.branch
		if r.hotfix {
			how = "git merge --no-ff " + r.branch
		}
		return fmt.Errorf("%s is not merged into %s: %s lacks these of its commits by content, and a merge keeping %s's tree would drop them:\n  %s\nport them to %s, then: git switch %s && git pull --ff-only %s %s && %s && git push %s %s",
			r.branch, branch, branch, branch, strings.Join(missing, "\n  "), branch, branch, remote, branch, how, remote, branch)
	}
	old, err := r.repo.Git("rev-parse", trunk)
	if err != nil {
		return fail(err)
	}
	old = strings.TrimSpace(old)
	msg := fmt.Sprintf("chore(release): %s, deployed to %s, merged into %s keeping its tree, which has all the branch's work", r.branch, r.name, branch)
	merge, err := r.repo.Git("commit-tree", old+"^{tree}", "-p", old, "-p", r.head, "-m", msg)
	if err != nil {
		return fail(err)
	}
	merge = strings.TrimSpace(merge)
	pushTo, err := pushRemotes(r.repo, r.cfg)
	if err != nil {
		return fail(err)
	}
	// The trunk's remote first: a push refused there, the trunk having
	// moved meanwhile, leaves the mirrors alone.
	ref := merge + ":refs/heads/" + branch
	if _, err := r.repo.Git("push", "--quiet", remote, ref); err != nil {
		return fail(fmt.Errorf("git push %s: %w; merge by hand: git switch %s && git pull --ff-only %s %s && git merge -s ours --no-edit %s && git push %s %s",
			remote, err, branch, remote, branch, r.branch, remote, branch))
	}
	a.printf("pushed to %s", remote)
	if err := a.push(r.repo, slices.DeleteFunc(pushTo, func(x string) bool { return x == remote }), []string{ref}, false); err != nil {
		return err
	}
	// The local trunk follows when it is where the remote was: the tree is
	// the same, so a worktree on it stays clean.
	local, err := r.repo.Git("rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err == nil && strings.TrimSpace(local) == old {
		if _, err := r.repo.Git("update-ref", "refs/heads/"+branch, merge, old); err != nil {
			a.warn("the local %s stays at %s: %v; git pull --ff-only", branch, gitx.Short(old), err)
		}
	}
	a.printf("merged %s into %s as %s, keeping %s's tree: the next deploy from %s finds what %s runs", r.branch, branch, gitx.Short(merge), branch, branch, r.name)
	return nil
}

// notInTrunk lists the commits of head the trunk lacks whose change is not
// in the trunk by content: picking one onto the trunk would change the
// trunk's tree, the version files aside, which every build bumps. Reverts
// are left out: they are work the release withheld and the trunk keeps. A
// merge counts by what it brings to its first parent.
func notInTrunk(repo *gitx.Repo, trunk, head string, versionFiles []string) ([]string, error) {
	commits, err := logCommits(repo, "--topo-order", trunk+".."+head)
	if err != nil {
		return nil, err
	}
	exclude := make([]string, 0, len(versionFiles))
	for _, f := range versionFiles {
		exclude = append(exclude, ":(exclude)"+f)
	}
	var missing []string
	for _, c := range commits {
		if isRevert(c.subject) {
			continue
		}
		in, err := pickChangesNothing(repo, trunk, c.sha, versionFiles, exclude)
		if err != nil {
			return nil, err
		}
		if !in {
			missing = append(missing, gitx.Short(c.sha)+" "+c.subject)
		}
	}
	return missing, nil
}

// pickChangesNothing tells whether picking commit onto the trunk, its first
// parent as the base, leaves the trunk's tree as it is outside the version
// files, without conflicts there.
func pickChangesNothing(repo *gitx.Repo, trunk, commit string, versionFiles, exclude []string) (bool, error) {
	out, err := repo.Git("merge-tree", "--write-tree", "--name-only", "--no-messages", "--merge-base="+commit+"^1", trunk, commit)
	// Exit status 1 is a merge with conflicts; the tree is still written.
	if err != nil && gitx.ExitCode(err) != 1 {
		return false, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, f := range lines[1:] {
		if f != "" && !slices.Contains(versionFiles, f) {
			return false, nil
		}
	}
	diff, err := repo.Git(append([]string{"diff", "--name-only", trunk, lines[0], "--", "."}, exclude...)...)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(diff) == "", nil
}
