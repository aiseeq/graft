package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/message"
	"github.com/aiseeq/graft/v2/internal/userconfig"
	"github.com/aiseeq/graft/v2/internal/version"
)

type MergeOptions struct {
	Branch, Mode, Message, NoDepsReason string
	NoDeps                              bool
}

// Merge builds and checks a trunk update in a disposable worktree before publishing it.
func (a *App) Merge(ctx context.Context, o MergeOptions) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	if err = requireNormalContext(repo); err != nil {
		return err
	}
	return a.withLock(ctx, repoLock(repo), cfg, "graft merge", func() error { return a.mergeLocked(ctx, repo, cfg, o) })
}

func (a *App) mergeLocked(ctx context.Context, repo *gitx.Repo, cfg *config.Config, o MergeOptions) error {
	if o.Mode == "" {
		o.Mode = "rebase"
	}
	if o.Mode != "rebase" && o.Mode != "merge" {
		return fmt.Errorf("--mode must be rebase or merge")
	}
	if o.Message != "" && o.Mode != "merge" {
		return fmt.Errorf("-m requires --mode merge")
	}
	if o.NoDeps && strings.TrimSpace(o.NoDepsReason) == "" {
		return fmt.Errorf("--no-deps requires --reason <text>")
	}
	if len(cfg.Merge.Deps) > 0 && o.NoDeps {
		a.printf("merge deps skipped: %s", strings.TrimSpace(o.NoDepsReason))
	}
	remotes, err := repo.Remotes()
	if err != nil {
		return err
	}
	if len(remotes) == 0 {
		return errors.New("graft merge requires at least one remote")
	}
	branch, err := repo.Branch()
	if err != nil {
		return err
	}
	clean, err := repo.IsClean()
	if err != nil {
		return err
	}
	if !clean {
		return errors.New("graft merge requires a clean index and work tree, including untracked files")
	}
	trunkRefName, err := trunkRef(repo, cfg, "", remotes)
	if err != nil {
		return err
	}
	trunk, ok, err := worktreeSyncResolvedTrunkBranch(repo, trunkRefName, remotes)
	if err != nil {
		return err
	}
	if !ok || branch != trunk {
		return fmt.Errorf("graft merge must run on the local trunk branch %s", trunk)
	}
	if cfg.Version.TagOnCommit || config.BranchMatches(cfg.Version.Build, trunk) {
		return errors.New("graft merge: version.tag_on_commit and version.build_branches are not supported in v1")
	}
	for _, r := range remotes {
		if _, err := repo.Git("fetch", "--quiet", r); err != nil {
			return fmt.Errorf("git fetch %s: %w", r, err)
		}
	}
	// Resolve trunk and source once, then use only their immutable object ids.
	A, err := repo.Git("rev-parse", "--verify", trunkRefName+"^{commit}")
	if err != nil {
		return err
	}
	A = strings.TrimSpace(A)
	src, err := resolveMergeSource(repo, o.Branch, remotes)
	if err != nil {
		return err
	}
	F, err := repo.Git("rev-parse", "--verify", src+"^{commit}")
	if err != nil {
		return fmt.Errorf("source %q is not a branch", o.Branch)
	}
	F = strings.TrimSpace(F)
	sourceIsTrunk := src == "refs/heads/"+trunk
	for _, remote := range remotes {
		if src == "refs/remotes/"+remote+"/"+trunk {
			sourceIsTrunk = true
		}
	}
	if sourceIsTrunk {
		return errors.New("source is the trunk")
	}
	a.printf("merge snapshot: A %s, F %s", gitx.Short(A), gitx.Short(F))
	local, err := repo.Git("rev-parse", "refs/heads/"+trunk)
	if err != nil {
		return err
	}
	local = strings.TrimSpace(local)
	isAncestor, err := repo.IsAncestor(local, A)
	if err != nil {
		return err
	}
	if !isAncestor {
		return errors.New("local trunk has commits absent from its remote; send or remove them before retrying")
	}
	merged, err := repo.IsAncestor(F, A)
	if err != nil {
		return err
	}
	if merged {
		a.printf("already merged %s", gitx.Short(F))
		return nil
	}
	if o.NoDeps && len(cfg.Merge.Deps) == 0 {
		a.printf("merge deps skipped: %s", strings.TrimSpace(o.NoDepsReason))
	}
	tmp, err := os.MkdirTemp("", "graft-merge-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	wt := filepath.Join(tmp, "tree")
	if _, err = repo.Git("worktree", "add", "--detach", "--quiet", wt, A); err != nil {
		return err
	}
	defer repo.Git("worktree", "remove", "--force", wt)
	if err = a.prepareWorktree(ctx, repo.Root, wt, nil); err != nil {
		return err
	}
	w, err := gitx.Open(wt)
	if err != nil {
		return err
	}
	msg := o.Message
	if msg == "" {
		msg = fmt.Sprintf("Merge %s into %s", o.Branch, trunk)
	}
	if cfg.Ticket != nil && !config.BranchMatches(cfg.Ticket.Skip, o.Branch) {
		msg = message.WithKey(msg, message.BranchKey(o.Branch, cfg.Ticket.Regexp))
	}
	if o.Message != "" {
		uc, e := userconfig.CommitSettings()
		if e != nil {
			return e
		}
		conventional, source := uc.Conventional, "user config commit.conventional"
		if cfg.Commit.Conventional != nil {
			conventional, source = *cfg.Commit.Conventional, ".graft.yaml commit.conventional"
		}
		if conventional {
			if e := message.CheckConventionalSource(msg, source); e != nil {
				return e
			}
		}
	}
	mode := o.Mode
	fastForward := false
	if mode == "rebase" {
		ancestral, e := repo.IsAncestor(A, F)
		if e != nil {
			return e
		}
		if ancestral {
			fastForward = true
			if _, e = w.Git("reset", "--hard", F); e != nil {
				return e
			}
		} else {
			list, e := repo.Git("rev-list", "--reverse", A+".."+F)
			if e != nil {
				return e
			}
			commits := strings.FieldsSeq(list)
			for c := range commits {
				parents, e := repo.Git("rev-list", "--parents", "-n", "1", c)
				if e != nil {
					return e
				}
				if len(strings.Fields(parents)) > 2 {
					return errors.New("source contains a merge commit; retry with --mode merge")
				}
				if _, e = w.GitEnv(markerEnv, "cherry-pick", "--empty=drop", c); e != nil {
					if err := mergeResolveVersion(w, cfg); err != nil {
						return err
					}
					// A commit that changed only the version file is empty
					// once the trunk's version is taken: drop it.
					step := "--continue"
					if _, staged := w.Git("diff", "--cached", "--quiet"); staged == nil {
						step = "--skip"
					}
					if _, e = w.GitEnv(markerEnv, "cherry-pick", step); e != nil {
						return fmt.Errorf("cherry-pick failed; trunk and source are unchanged: %w", e)
					}
				}
			}
		}
	} else {
		args := []string{"merge", "--no-ff", "--no-edit", "-m", msg, F}
		if _, e := w.GitEnv(markerEnv, args...); e != nil {
			paths, _ := w.Git("diff", "--name-only", "--diff-filter=U")
			if strings.TrimSpace(paths) != "" {
				if er := mergeResolveVersion(w, cfg); er != nil {
					return er
				}
				if _, e = w.GitEnv(markerEnv, "commit", "--no-edit"); e != nil {
					return e
				}
			} else {
				return fmt.Errorf("merge failed: %w", e)
			}
		}
	}
	R, err := w.Git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	R = strings.TrimSpace(R)
	if R == A {
		a.printf("already merged by content")
		return nil
	}
	if err = mergeBump(w, cfg, trunk, A); err != nil {
		return err
	}
	if cfg.Version.Mode == config.ModeFile {
		v, e := version.Current(w.Root, cfg.Version)
		if e != nil {
			return e
		}
		a.printf("merge version: %s", v)
	} else {
		a.printf("merge version: %s", cfg.Version.Mode)
	}
	R, err = w.Git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	R = strings.TrimSpace(R)
	if fastForward {
		a.printf("merge mode: fast-forward")
	} else {
		a.printf("merge mode: %s", mode)
	}
	if _, _, err = a.gateForMessage(ctx, w, cfg, trunk, msg); err != nil {
		return fmt.Errorf("merge gate failed; nothing published: %w", err)
	}
	if err = mergeTreeUnchanged(w, R); err != nil {
		return err
	}
	a.printf("merge gate passed")
	if _, err = w.Git("reset", "--soft", A); err != nil {
		return err
	}
	if err = runChecks(ctx, w, cfg, msg, "", a.Stderr); err != nil {
		return fmt.Errorf("merge content checks failed: %w", err)
	}
	// Rebased and fast-forwarded commits carry their own checked messages;
	// only a merge commit is new.
	if cfg.Ticket != nil && mode == "merge" {
		if err = requireTicket(w, cfg, msg, ""); err != nil {
			return err
		}
	}
	if _, err = w.Git("reset", "--soft", R); err != nil {
		return err
	}
	if err = mergeTreeUnchanged(w, R); err != nil {
		return err
	}
	if !o.NoDeps && len(cfg.Merge.Deps) > 0 {
		a.printf("merge deps: %s", strings.Join(cfg.Merge.Deps, ", "))
		if err = a.runner(w, cfg).RunDeps(ctx, cfg.Merge.Deps); err != nil {
			return fmt.Errorf("merge deps failed; nothing published: %w", err)
		}
		if err = mergeTreeUnchanged(w, R); err != nil {
			return err
		}
	} else if !o.NoDeps {
		a.printf("merge deps: skipped (none configured)")
	}
	// Final lease-like local checks happen under the same repository lock.
	cur, err := repo.Git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(cur) != local {
		return errors.New("local trunk moved during checks; nothing published")
	}
	clean, err = repo.IsClean()
	if err != nil {
		return err
	}
	if !clean {
		return errors.New("local trunk became dirty during checks; nothing published")
	}
	primary := primaryRemote(cfg, remotes, trunkRefName)
	if _, err = repo.Git("push", primary, R+":refs/heads/"+trunk); err != nil {
		return fmt.Errorf("push to %s refused, nothing published and the local trunk unchanged; if the remote moved ahead: git fetch, then graft merge again: %w", primary, err)
	}
	a.printf("pushed to %s", primary)
	mirrors := slices.DeleteFunc(slices.Clone(remotes), func(r string) bool { return r == primary })
	mirrorErr := a.push(repo, mirrors, []string{R + ":refs/heads/" + trunk}, false, false)
	if _, err = repo.Git("merge", "--ff-only", R); err != nil {
		return fmt.Errorf("trunk on the remote is already %s; install it with: git merge --ff-only %s: %w", R, R, err)
	}
	a.printf("installed %s %s", trunk, gitx.Short(R))
	a.printf("merged %s %s into %s as %s", o.Branch, gitx.Short(F), trunk, gitx.Short(R))
	return mirrorErr
}

func resolveMergeSource(repo *gitx.Repo, name string, remotes []string) (string, error) {
	if strings.HasPrefix(name, "refs/tags/") || strings.HasPrefix(name, "refs/remotes/") {
		return "", fmt.Errorf("source %q must be a branch name", name)
	}
	local := "refs/heads/" + name
	exists, e := repo.RefExists(local)
	if e != nil {
		return "", e
	}
	candidates := []string{}
	if exists {
		candidates = append(candidates, local)
	}
	for _, r := range remotes {
		if branch, ok := strings.CutPrefix(name, r+"/"); ok {
			ref := "refs/remotes/" + r + "/" + branch
			yes, e := repo.RefExists(ref)
			if e != nil {
				return "", e
			}
			if yes {
				candidates = append(candidates, ref)
			}
		}
	}
	for _, r := range remotes {
		if strings.HasPrefix(name, r+"/") {
			continue
		}
		ref := "refs/remotes/" + r + "/" + name
		yes, e := repo.RefExists(ref)
		if e != nil {
			return "", e
		}
		if yes {
			candidates = append(candidates, ref)
		}
	}
	if len(candidates) != 1 {
		if len(candidates) == 0 {
			if commit, e := repo.RevExists(name); e != nil {
				return "", e
			} else if commit {
				return "", fmt.Errorf("source %q resolves to a commit, not a branch", name)
			}
		}
		return "", fmt.Errorf("source %q is missing or ambiguous; name one branch uniquely", name)
	}
	return candidates[0], nil
}
func primaryRemote(cfg *config.Config, remotes []string, trunkRef string) string {
	if cfg.Deploy != nil && cfg.Deploy.Remote != "" {
		return cfg.Deploy.Remote
	}
	for _, remote := range remotes {
		if strings.HasPrefix(trunkRef, remote+"/") {
			return remote
		}
	}
	return remotes[0]
}
func mergeResolveVersion(repo *gitx.Repo, cfg *config.Config) error {
	out, err := repo.Git("diff", "--name-only", "-z", "--diff-filter=U")
	if err != nil {
		return err
	}
	paths := strings.FieldsFunc(out, func(r rune) bool { return r == 0 })
	allowed := cfg.Version.File
	var bad []string
	for _, p := range paths {
		if p != allowed {
			bad = append(bad, p)
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("merge conflict in %s; trunk and source are unchanged", strings.Join(paths, ", "))
	}
	if len(paths) == 0 {
		return errors.New("merge failed without conflicts")
	}
	_, err = repo.Git("checkout", "--ours", "--", allowed)
	if err == nil {
		_, err = repo.Git("add", "--", allowed)
	}
	return err
}
func mergeBump(repo *gitx.Repo, cfg *config.Config, branch, base string) error {
	if cfg.Version.Mode != config.ModeFile {
		return nil
	}
	current, err := version.Current(repo.Root, cfg.Version)
	if err != nil {
		return err
	}
	baseText, err := repo.Git("show", base+":"+cfg.Version.File)
	if err != nil {
		return err
	}
	baseVersion, err := version.ParseFile(strings.TrimSpace(baseText))
	if err != nil {
		return err
	}
	if mergeVersionGreater(current, baseVersion) {
		return nil
	}
	p, err := planBump(repo, cfg, version.Patch, branch)
	if err != nil {
		return err
	}
	if p.next == nil {
		return nil
	}
	if !mergeVersionGreater(*p.next, current) {
		return nil
	}
	if _, err = p.apply(repo); err != nil {
		return err
	}
	if _, err = repo.Git(append([]string{"add", "--"}, cfg.VersionFiles()...)...); err != nil {
		return err
	}
	_, err = repo.GitEnv(markerEnv, "commit", "-m", "chore: bump version after merge")
	return err
}

func mergeVersionGreater(a, b version.Semver) bool {
	if a.Major != b.Major {
		return a.Major > b.Major
	}
	if a.Minor != b.Minor {
		return a.Minor > b.Minor
	}
	if a.Patch != b.Patch {
		return a.Patch > b.Patch
	}
	return a.Build > b.Build
}
func mergeTreeUnchanged(repo *gitx.Repo, rev string) error {
	d, e := repo.Git("status", "--porcelain", "--untracked-files=all")
	if e != nil {
		return e
	}
	if strings.TrimSpace(d) == "" {
		return nil
	}
	var paths []string
	for line := range strings.SplitSeq(strings.TrimSpace(d), "\n") {
		if len(line) > 3 {
			paths = append(paths, line[3:])
		}
	}
	slices.Sort(paths)
	return fmt.Errorf("merge check changed the temporary tree after %s: %s", gitx.Short(rev), strings.Join(paths, ", "))
}
