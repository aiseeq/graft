package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

const worktreeArchiveTagMarker = "Archived-by: graft worktree archive"

// WorktreeArchiveOptions describes a branch archived before its local worktree
// and branch are removed.
type WorktreeArchiveOptions struct {
	Branch, Name, Reason, Trunk string
	List                        bool
	Match                       string
}

// WorktreeArchive tags and pushes the branch before removing its worktree.
func (a *App) WorktreeArchive(ctx context.Context, o WorktreeArchiveOptions) error {
	repo, _, err := a.open()
	if err != nil {
		return err
	}
	trees, err := listWorktrees(repo)
	if err != nil {
		return err
	}
	if len(trees) == 0 {
		return fmt.Errorf("git lists no worktrees")
	}
	repo, err = gitx.Open(trees[0].path)
	if err != nil {
		return fmt.Errorf("open main worktree %s: %w", trees[0].path, err)
	}
	cfg, err := mainWorktreeConfig(repo)
	if err != nil {
		return err
	}
	if err := a.requireGraft(cfg); err != nil {
		return err
	}
	if o.List {
		return a.listWorktreeArchives(repo, o.Match)
	}
	plan, remotes, err := prepareWorktreeArchive(ctx, a, repo, cfg, o)
	if err != nil {
		return err
	}
	if !plan.continuing {
		plan.tagOID, err = createWorktreeArchiveTag(repo, plan.tag, plan.reason, o.Branch)
		if err != nil {
			return err
		}
	}
	if err := pushWorktreeArchiveTag(a, repo, remotes, plan, o); err != nil {
		return err
	}
	if err := removeArchivedWorktree(repo, plan); err != nil {
		return err
	}
	a.printf("archived %s with reason: %s", plan.tag, plan.reason)
	if len(remotes) == 0 {
		a.printf("tag is local only; no remotes configured")
		a.printf("no remote branch %s to remove", o.Branch)
	} else {
		a.printf("remote branch %s remains", o.Branch)
	}
	return nil
}

type worktreeArchivePlan struct {
	tag, branch, path, tagOID string
	reason                    string
	continuing                bool
	remoteNeeds               []string
}

func prepareWorktreeArchive(ctx context.Context, a *App, repo *gitx.Repo, cfg *config.Config, o WorktreeArchiveOptions) (worktreeArchivePlan, []string, error) {
	// The reason is stored trimmed and verbatim (createWorktreeArchiveTag), so
	// the continuation check compares it with what the tag holds.
	o.Reason = strings.TrimSpace(o.Reason)
	if o.Branch == "" || o.Reason == "" {
		return worktreeArchivePlan{}, nil, fmt.Errorf("worktree archive requires <branch> and --reason")
	}
	slug := o.Name
	if slug == "" {
		slug = o.Branch
	}
	plan := worktreeArchivePlan{tag: "archive/" + slug, branch: o.Branch, reason: o.Reason}
	if _, err := repo.Git("check-ref-format", "refs/tags/"+plan.tag); err != nil {
		return plan, nil, fmt.Errorf("invalid archive tag %q: %w", plan.tag, err)
	}
	exists, err := repo.RefExists("refs/heads/" + o.Branch)
	if err != nil {
		return plan, nil, err
	}
	if !exists {
		return plan, nil, fmt.Errorf("local branch %q does not exist", o.Branch)
	}
	remotes, err := pushRemotes(repo, cfg)
	if err != nil {
		return plan, nil, err
	}
	trees, err := listWorktrees(repo)
	if err != nil {
		return plan, nil, err
	}
	target := archiveWorktree(trees, o.Branch)
	if target == nil {
		return plan, nil, fmt.Errorf("branch %q has no registered worktree to archive", o.Branch)
	}
	plan.path = target.path
	mainPath := trees[0].path
	trunk, err := trunkRef(repo, cfg, o.Trunk, remotes)
	if err != nil {
		return plan, nil, err
	}
	if isLocalTrunkBranch(trunk, remotes, o.Branch) {
		return plan, nil, fmt.Errorf("refusing to archive trunk branch %q; choose another --trunk or switch the main worktree", o.Branch)
	}
	if filepath.Clean(target.path) == filepath.Clean(mainPath) {
		return plan, nil, fmt.Errorf("branch %q is checked out in the main worktree %s; switch the main worktree to another branch before archiving", o.Branch, mainPath)
	}
	branchRepo, err := gitx.Open(target.path)
	if err != nil {
		return plan, nil, fmt.Errorf("open worktree %s: %w", target.path, err)
	}
	if err := checkWorktreeArchiveData(ctx, a, branchRepo, trunk); err != nil {
		return plan, nil, err
	}
	if err := prepareArchiveRemoteTags(repo, &plan, remotes); err != nil {
		return plan, nil, err
	}
	return plan, remotes, nil
}

func archiveWorktree(trees []worktree, branch string) *worktree {
	for i := range trees {
		if trees[i].branch == branch {
			return &trees[i]
		}
	}
	return nil
}

func isLocalTrunkBranch(trunk string, remotes []string, branch string) bool {
	trunk = strings.TrimPrefix(trunk, "refs/heads/")
	if trunk == branch {
		return true
	}
	trunk = strings.TrimPrefix(trunk, "refs/remotes/")
	for _, remote := range remotes {
		if candidate, ok := strings.CutPrefix(trunk, remote+"/"); ok && candidate == branch {
			return true
		}
	}
	return false
}

func checkWorktreeArchiveData(ctx context.Context, a *App, repo *gitx.Repo, trunk string) error {
	mainCfg, err := mainWorktreeConfig(repo)
	if err != nil {
		return err
	}
	c := &cleaner{ctx: ctx, app: a, repo: repo, cfg: mainCfg, opts: WorktreeCleanOptions{}, counts: map[string]int{}}
	if c.remotes, err = repo.Remotes(); err != nil {
		return err
	}
	c.trunk = trunk
	if c.cfg, err = c.readTrunkConfig(); err != nil {
		return err
	}
	for _, file := range c.cfg.VersionFiles() {
		c.exclude = append(c.exclude, ":(exclude)"+file)
	}
	holds, err := c.worktreeHolds(repo)
	if err != nil {
		return err
	}
	if archiveHasDataOutsideCommits(holds) {
		return archiveHoldsError(repo.Root, holds)
	}
	return nil
}

func archiveHasDataOutsideCommits(holds worktreeHolds) bool {
	return holds.operation != "" || len(holds.changes) != 0 || holds.refCommits != 0 ||
		len(holds.hidden) != 0 || len(holds.nested) != 0 || len(holds.submodules) != 0 || holds.ignored.count != 0
}

func archiveHoldsError(root string, holds worktreeHolds) error {
	var parts []string
	if holds.operation != "" {
		parts = append(parts, operationInProgress(holds.operation))
	}
	if len(holds.changes) > 0 {
		parts = append(parts, plural(len(holds.changes), "uncommitted change"))
	}
	if len(holds.hidden) > 0 {
		parts = append(parts, "files git status does not look at: "+strings.Join(holds.hidden, ", "))
	}
	if holds.refCommits > 0 {
		parts = append(parts, plural(holds.refCommits, "worktree-only commit"))
	}
	if len(holds.nested) > 0 {
		parts = append(parts, "nested worktrees: "+strings.Join(holds.nested, ", "))
	}
	if len(holds.submodules) > 0 {
		parts = append(parts, "submodule data: "+strings.Join(holds.submodules, ", "))
	}
	if holds.ignored.count > 0 {
		parts = append(parts, holds.ignored.describe())
	}
	return fmt.Errorf("refusing to archive worktree %s: %s; branch and worktree remain", root, strings.Join(parts, "; "))
}

func prepareArchiveRemoteTags(repo *gitx.Repo, plan *worktreeArchivePlan, remotes []string) error {
	localTag := "refs/tags/" + plan.tag
	exists, err := repo.RefExists(localTag)
	if err != nil {
		return err
	}
	if exists {
		kind, err := repo.Git("cat-file", "-t", localTag)
		if err != nil {
			return err
		}
		if strings.TrimSpace(kind) != "tag" {
			return fmt.Errorf("archive tag %q already exists locally and is not annotated", plan.tag)
		}
		plan.tagOID, err = revParse(repo, localTag)
		if err != nil {
			return err
		}
		peeled, err := revParse(repo, localTag+"^{}")
		if err != nil {
			return err
		}
		branchTip, err := revParse(repo, "refs/heads/"+plan.branch)
		if err != nil {
			return err
		}
		if peeled != branchTip {
			return fmt.Errorf("archive tag %q already exists locally but does not point to the current tip of %q; choose a different --name or delete the tag manually", plan.tag, plan.branch)
		}
		tagData, err := repo.Git("cat-file", "tag", localTag)
		if err != nil {
			return err
		}
		_, message, ok := strings.Cut(tagData, "\n\n")
		if !ok {
			return archiveTagContinuationError(plan.tag)
		}
		message = strings.TrimSuffix(message, "\n")
		reason, marked := strings.CutSuffix(message, "\n\n"+worktreeArchiveTagMarker)
		if !marked || reason != plan.reason {
			return archiveTagContinuationError(plan.tag)
		}
		if peeled != branchTip {
			return fmt.Errorf("archive tag %q already exists locally but does not point to the current tip of %q; choose a different --name or delete the tag manually", plan.tag, plan.branch)
		}
		plan.continuing = true
	}
	has, needs, err := archiveRemoteTags(repo, remotes, plan.tag, plan.tagOID)
	if err != nil {
		return err
	}
	if !plan.continuing && len(has) > 0 {
		return fmt.Errorf("archive tag %q already exists on remote(s): %s", plan.tag, strings.Join(has, ", "))
	}
	plan.remoteNeeds = needs
	return nil
}

func archiveTagContinuationError(tag string) error {
	return fmt.Errorf("archive tag %q already exists and was not created by this archive or has a different --reason; choose a different --name or delete the tag manually", tag)
}

func archiveRemoteTags(repo *gitx.Repo, remotes []string, tag, expectedOID string) ([]string, []string, error) {
	var has, needs []string
	for _, remote := range remotes {
		urls, err := repo.Git("remote", "get-url", "--push", "--all", remote)
		if err != nil {
			return nil, nil, fmt.Errorf("read push URLs for %s: %w", remote, err)
		}
		var remoteHas, remoteNeeds bool
		for url := range strings.SplitSeq(strings.TrimSpace(urls), "\n") {
			if url == "" {
				continue
			}
			out, err := repo.Git("ls-remote", "--refs", url, "refs/tags/"+tag)
			if err != nil {
				return nil, nil, fmt.Errorf("check archive tag %q on %s: %w", tag, remote, err)
			}
			if strings.TrimSpace(out) == "" {
				remoteNeeds = true
				continue
			}
			fields := strings.Fields(out)
			if len(fields) < 2 || fields[1] != "refs/tags/"+tag {
				return nil, nil, fmt.Errorf("unexpected ls-remote result for %s archive tag %q: %q", remote, tag, out)
			}
			if expectedOID != "" && fields[0] != expectedOID {
				return nil, nil, fmt.Errorf("remote %s already has archive tag %q at a different object", remote, tag)
			}
			remoteHas = true
		}
		if !remoteHas && !remoteNeeds {
			return nil, nil, fmt.Errorf("remote %s has no push URL", remote)
		}
		if remoteHas {
			has = append(has, remote)
		}
		if remoteNeeds {
			needs = append(needs, remote)
		}
	}
	return has, needs, nil
}

func revParse(repo *gitx.Repo, ref string) (string, error) {
	out, err := repo.Git("rev-parse", "--verify", ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func pushWorktreeArchiveTag(a *App, repo *gitx.Repo, remotes []string, plan worktreeArchivePlan, o WorktreeArchiveOptions) error {
	if len(plan.remoteNeeds) == 0 {
		return nil
	}
	err := a.push(repo, plan.remoteNeeds, []string{"refs/tags/" + plan.tag}, false, true)
	if err == nil {
		return nil
	}
	has, needs, inspectErr := archiveRemoteTags(repo, remotes, plan.tag, plan.tagOID)
	if inspectErr != nil {
		return fmt.Errorf("push archive tag %q failed and remote state is unknown; local tag, branch and worktree are kept: %w", plan.tag, errors.Join(err, inspectErr))
	}
	if len(needs) == 0 {
		return nil
	}
	if len(has) > 0 {
		retry := archiveRetryCommand(o, plan.reason)
		var pushErr *PushError
		if errors.As(err, &pushErr) {
			pushErr.LocalState = fmt.Sprintf("local archive tag kept; tag is on %s and missing on %s; continue with: %s", strings.Join(has, ", "), strings.Join(needs, ", "), retry)
		}
		return fmt.Errorf("archive tag %q partially pushed: present on %s; missing on %s. Branch %q and worktree %s remain; continue with: %s: %w", plan.tag, strings.Join(has, ", "), strings.Join(needs, ", "), plan.branch, plan.path, retry, err)
	}
	_, dropErr := repo.Git("tag", "-d", plan.tag)
	if dropErr != nil {
		return fmt.Errorf("push archive tag %q failed on all remotes; local tag removal failed, branch %q and worktree %s remain: %w", plan.tag, plan.branch, plan.path, errors.Join(err, fmt.Errorf("remove local archive tag: %w", dropErr)))
	}
	var pushErr *PushError
	if errors.As(err, &pushErr) {
		pushErr.LocalState = fmt.Sprintf("no remote accepted the tag; local tag removed; branch %q and worktree %s remain", plan.branch, plan.path)
	}
	return fmt.Errorf("push archive tag %q failed on all remotes; local tag removed; branch %q and worktree %s remain: %w", plan.tag, plan.branch, plan.path, err)
}

func archiveRetryCommand(o WorktreeArchiveOptions, reason string) string {
	args := []string{"graft worktree archive", "--reason", shellQuote(reason)}
	if o.Name != "" {
		args = append(args, "--name", shellQuote(o.Name))
	}
	if o.Trunk != "" {
		args = append(args, "--trunk", shellQuote(o.Trunk))
	}
	args = append(args, shellQuote(o.Branch))
	return strings.Join(args, " ")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func removeArchivedWorktree(repo *gitx.Repo, plan worktreeArchivePlan) error {
	if _, err := repo.Git("worktree", "remove", plan.path); err != nil {
		return fmt.Errorf("tag %q was pushed, but removing worktree %s failed; branch %q remains: %w", plan.tag, plan.path, plan.branch, err)
	}
	if _, err := repo.Git("branch", "-D", plan.branch); err != nil {
		return fmt.Errorf("tag %q was pushed and worktree removed, but deleting branch %q failed: %w", plan.tag, plan.branch, err)
	}
	return nil
}

func createWorktreeArchiveTag(repo *gitx.Repo, tag, reason, branch string) (string, error) {
	exists, err := repo.RefExists("refs/tags/" + tag)
	if err != nil {
		return "", err
	}
	if exists {
		return "", fmt.Errorf("archive tag %q already exists", tag)
	}
	if _, err := repo.Git("tag", "-a", "--cleanup=verbatim", tag, "-m", reason, "-m", worktreeArchiveTagMarker, branch); err != nil {
		return "", fmt.Errorf("create archive tag %q: %w", tag, err)
	}
	return revParse(repo, "refs/tags/"+tag)
}

func (a *App) listWorktreeArchives(repo *gitx.Repo, match string) error {
	var re *regexp.Regexp
	var err error
	if match != "" {
		re, err = regexp.Compile(match)
		if err != nil {
			return fmt.Errorf("archive list expression: %w", err)
		}
	}
	out, err := repo.Git("for-each-ref", "--format=%(refname:short)", "refs/tags/archive/")
	if err != nil {
		return err
	}
	found := false
	for tag := range strings.SplitSeq(strings.TrimSuffix(out, "\n"), "\n") {
		if tag == "" {
			continue
		}
		record, err := repo.Git("cat-file", "tag", "refs/tags/"+tag)
		if err != nil {
			return fmt.Errorf("read archive tag %s: %w", tag, err)
		}
		_, reason, _ := strings.Cut(record, "\n\n")
		reason = strings.Join(strings.Fields(reason), " ")
		if re != nil && !re.MatchString(tag+"\n"+reason) {
			continue
		}
		fmt.Fprintf(a.Stdout, "%s: %s\n", tag, strings.TrimSpace(reason))
		found = true
	}
	if !found {
		a.printf("no archive tags")
	}
	return nil
}
