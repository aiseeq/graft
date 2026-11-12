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
	repo, err := gitx.Open(a.Dir)
	if err != nil {
		return err
	}
	cfg, err := config.Load(repo.Root)
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
	allRemotes, err := repo.Remotes()
	if err != nil {
		return err
	}
	if len(allRemotes) == 0 {
		return errors.New("graft merge requires at least one remote")
	}
	pushTo, err := pushRemotes(repo, cfg)
	if err != nil {
		return err
	}
	if len(pushTo) == 0 {
		return errors.New("graft merge requires at least one push remote")
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
	trunkRefName, err := trunkRef(repo, cfg, "", allRemotes)
	if err != nil {
		return err
	}
	for _, r := range pushTo {
		if _, err := repo.Git("fetch", "--quiet", r); err != nil {
			return fmt.Errorf("git fetch %s: %w", r, err)
		}
	}
	// The fetched trunk's config governs the entire operation. If its config
	// selects another remote, fetch that configured push set and resolve again.
	var A string
	stableConfig := false
	for range 3 {
		A, err = repo.Git("rev-parse", "--verify", trunkRefName+"^{commit}")
		if err != nil {
			return err
		}
		A = strings.TrimSpace(A)
		data, e := repo.Git("show", A+":"+config.FileName)
		if e != nil {
			return fmt.Errorf("reading %s from trunk %s: %w", config.FileName, gitx.Short(A), e)
		}
		cfg, e = config.Parse([]byte(data))
		if e != nil {
			return fmt.Errorf("trunk %s %s: %w", gitx.Short(A), config.FileName, e)
		}
		if e = a.requireGraft(cfg); e != nil {
			return e
		}
		pushTo, e = pushRemotes(repo, cfg)
		if e != nil {
			return e
		}
		primary := primaryRemote(cfg, allRemotes, trunkRefName)
		if !slices.Contains(pushTo, primary) {
			return fmt.Errorf("deploy remote %q, the trunk's primary remote, is absent from push.remotes", primary)
		}
		for _, r := range pushTo {
			if _, e = repo.Git("fetch", "--quiet", r); e != nil {
				return fmt.Errorf("git fetch %s: %w", r, e)
			}
		}
		newRef, e := trunkRef(repo, cfg, "", allRemotes)
		if e != nil {
			return e
		}
		newA, e := repo.Git("rev-parse", "--verify", newRef+"^{commit}")
		if e != nil {
			return e
		}
		newA = strings.TrimSpace(newA)
		if newRef == trunkRefName && newA == A {
			stableConfig = true
			break
		}
		trunkRefName = newRef
	}
	if !stableConfig {
		return errors.New("trunk config or ref kept changing while resolving it; retry graft merge")
	}
	trunk, ok, err := worktreeSyncResolvedTrunkBranch(repo, trunkRefName, allRemotes)
	if err != nil {
		return err
	}
	if !ok || branch != trunk {
		return fmt.Errorf("graft merge must run on the local trunk branch %s", trunk)
	}
	if cfg.Version.TagOnCommit || config.BranchMatches(cfg.Version.Build, trunk) {
		return errors.New("graft merge: version.tag_on_commit and version.build_branches are not supported in v1")
	}
	// Resolve source only after fetch, using immutable refs selected below.
	src, err := resolveMergeSource(repo, o.Branch, pushTo)
	if err != nil {
		return err
	}
	F, err := repo.Git("rev-parse", "--verify", src+"^{commit}")
	if err != nil {
		return fmt.Errorf("source %q is not a branch", o.Branch)
	}
	F = strings.TrimSpace(F)
	sourceIsTrunk := src == "refs/heads/"+trunk
	for _, remote := range pushTo {
		if src == "refs/remotes/"+remote+"/"+trunk {
			sourceIsTrunk = true
		}
	}
	if sourceIsTrunk {
		return errors.New("source is the trunk")
	}
	if primary := primaryRemote(cfg, allRemotes, trunkRefName); !slices.Contains(pushTo, primary) {
		return fmt.Errorf("trunk primary remote %q is not in push.remotes", primary)
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
	wt := filepath.Join(tmp, "tree")
	if _, err = repo.Git("worktree", "add", "--detach", "--quiet", wt, A); err != nil {
		if rmErr := os.RemoveAll(tmp); rmErr != nil {
			a.warn("removing merge temporary directory %s: %v", tmp, rmErr)
		}
		return err
	}
	cleanupCfg := cfg
	defer func() {
		if err := removeMergeLinks(wt, cleanupCfg); err != nil {
			a.warn("removing temporary worktree links: %v", err)
		}
		if _, err := repo.Git("worktree", "remove", "--force", wt); err != nil {
			a.warn("removing merge worktree %s: %v", wt, err)
		}
		if err := os.RemoveAll(tmp); err != nil {
			a.warn("removing merge temporary directory %s: %v", tmp, err)
		}
	}()
	w, err := gitx.Open(wt)
	if err != nil {
		return err
	}
	msg := o.Message
	if msg == "" {
		msg = fmt.Sprintf("Merge %s into %s", o.Branch, trunk)
	}
	ticketKey := ""
	if cfg.Ticket != nil && !config.BranchMatches(cfg.Ticket.Skip, o.Branch) {
		ticketKey = message.BranchKey(o.Branch, cfg.Ticket.Regexp)
		msg = message.WithKey(msg, ticketKey)
		if cfg.Ticket.Required && ticketKey != "" && !hasTicketKey(cfg, msg) {
			msg += "\n\n" + ticketKey
		}
	}
	sourceVersion, trunkVersion := "", ""
	if cfg.Version.Mode == config.ModeFile {
		sourceVersion, err = mergeVersionAt(repo, F, cfg)
		if err != nil {
			return err
		}
		trunkVersion, err = mergeVersionAt(repo, A, cfg)
		if err != nil {
			return err
		}
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
				// Git before 2.45 has no --empty=drop. Skip source commits
				// that were empty at their original parent before cherry-picking.
				if empty, er := mergeCommitEmpty(repo, c); er != nil {
					return er
				} else if empty {
					a.printf("merge: skipped empty commit %s", gitx.Short(c))
					continue
				}
				if _, e = w.GitEnv(markerEnv, "cherry-pick", c); e != nil {
					paths, pe := mergeUnmergedPaths(w)
					if pe != nil {
						return pe
					}
					if len(paths) == 0 {
						if same, se := mergeIndexEqualsHead(w); se != nil {
							return se
						} else if same {
							a.printf("merge: skipped empty commit %s", gitx.Short(c))
							if _, e = w.GitEnv(markerEnv, "cherry-pick", "--skip"); e != nil {
								return fmt.Errorf("skipping empty cherry-pick: %w", e)
							}
							continue
						}
						return fmt.Errorf("cherry-pick failed without conflicts: %w", e)
					}
					if err := mergeResolveVersion(w, cfg); err != nil {
						return err
					}
					step := "--continue"
					if same, se := mergeIndexEqualsHead(w); se != nil {
						return se
					} else if same {
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
	if mode == "merge" {
		same, e := mergeTreesEqual(w, A, R)
		if e != nil {
			return e
		}
		if same {
			a.printf("already merged by content")
			return nil
		}
	}
	if err = mergeBump(w, cfg, trunk, A, ticketKey); err != nil {
		return err
	}
	if cfg.Version.Mode == config.ModeFile {
		v, e := version.Current(w.Root, cfg.Version)
		if e != nil {
			return e
		}
		a.printf("merge version: %s (source %s, trunk %s)", v, sourceVersion, trunkVersion)
	} else {
		a.printf("merge version: %s", cfg.Version.Mode)
	}
	R, err = w.Git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	R = strings.TrimSpace(R)
	cfgResult, err := config.Load(w.Root)
	if err != nil {
		return fmt.Errorf("loading result config from %s: %w", gitx.Short(R), err)
	}
	if err = a.requireGraft(cfgResult); err != nil {
		return err
	}
	cleanupCfg = cfgResult
	linkSetups, err := a.linkWorktree(repo.Root, wt)
	if err != nil {
		return err
	}
	if err = a.prepareWorktree(ctx, repo.Root, wt, linkSetups); err != nil {
		return err
	}
	cfg = cfgResult
	if o.NoDeps && len(cfg.Merge.Deps) > 0 {
		a.printf("merge deps skipped: %s", strings.TrimSpace(o.NoDepsReason))
	}
	if fastForward {
		a.printf("merge mode: fast-forward")
	} else {
		a.printf("merge mode: %s", mode)
	}
	if _, _, err = a.gateForMessage(ctx, w, cfg, trunk, msg); err != nil {
		return fmt.Errorf("merge gate failed; nothing published: %w", err)
	}
	if err = mergeTreeUnchanged(w, R, cfg, "gate"); err != nil {
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
	if err = mergeTreeUnchanged(w, R, cfg, "content checks"); err != nil {
		return err
	}
	if !o.NoDeps && len(cfg.Merge.Deps) > 0 {
		a.printf("merge deps: %s", strings.Join(cfg.Merge.Deps, ", "))
		if err = a.runner(w, cfg).RunDeps(ctx, cfg.Merge.Deps); err != nil {
			return fmt.Errorf("merge deps failed; nothing published: %w", err)
		}
		if err = mergeTreeUnchanged(w, R, cfg, "merge.deps"); err != nil {
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
	primary := primaryRemote(cfg, pushTo, trunkRefName)
	if _, err = repo.Git("push", primary, R+":refs/heads/"+trunk); err != nil {
		return fmt.Errorf("push to %s refused, nothing published and the local trunk unchanged; if the remote moved ahead: git fetch, then graft merge again: %w", primary, err)
	}
	a.printf("pushed to %s", primary)
	mirrors := slices.DeleteFunc(slices.Clone(pushTo), func(r string) bool { return r == primary })
	var mirrorErr error
	if len(mirrors) > 0 {
		mirrorErr = a.push(repo, mirrors, []string{R + ":refs/heads/" + trunk}, false, false)
	}
	if _, err = repo.GitEnv(markerEnv, "merge", "--ff-only", R); err != nil {
		return fmt.Errorf("trunk on the remote is already %s; install it with: git merge --ff-only %s: %w", R, R, err)
	}
	a.printf("installed %s %s", trunk, gitx.Short(R))
	a.printf("merged %s %s into %s as %s", o.Branch, gitx.Short(F), trunk, gitx.Short(R))
	return mirrorErr
}

func resolveMergeSource(repo *gitx.Repo, name string, remotes []string) (string, error) {
	forceLocal := strings.HasPrefix(name, "refs/heads/")
	if forceLocal {
		name = strings.TrimPrefix(name, "refs/heads/")
		if name == "" {
			return "", errors.New("source must name a branch")
		}
	}
	if strings.HasPrefix(name, "refs/tags/") || strings.HasPrefix(name, "refs/remotes/") {
		return "", fmt.Errorf("source %q must be a branch name", name)
	}
	// A remote/name spelling is explicit and takes precedence over a local
	// branch whose literal name happens to include the remote prefix.
	for _, r := range remotes {
		if branch, ok := strings.CutPrefix(name, r+"/"); ok && !forceLocal {
			ref := "refs/remotes/" + r + "/" + branch
			yes, e := repo.RefExists(ref)
			if e != nil {
				return "", e
			}
			if yes {
				return ref, nil
			}
		}
	}
	local := "refs/heads/" + name
	exists, e := repo.RefExists(local)
	if e != nil {
		return "", e
	}
	if exists {
		localOID, e := repo.Git("rev-parse", "--verify", local+"^{commit}")
		if e != nil {
			return "", e
		}
		upstream, e := repo.Git("for-each-ref", "--format=%(upstream)", local)
		if e != nil {
			return "", e
		}
		upstream = strings.TrimSpace(upstream)
		if upstream != "" {
			upstreamRef := upstream
			if present, er := repo.RefExists(upstreamRef); er != nil {
				return "", er
			} else if present {
				upstreamOID, er := repo.Git("rev-parse", "--verify", upstreamRef+"^{commit}")
				if er != nil {
					return "", er
				}
				contains, er := repo.IsAncestor(strings.TrimSpace(upstreamOID), strings.TrimSpace(localOID))
				if er != nil {
					return "", er
				}
				if !contains {
					return "", fmt.Errorf("local source branch %s is behind or diverged from %s; pull or push first", name, upstream)
				}
			}
		}
		return local, nil
	}
	var candidates []string
	for _, r := range remotes {
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
	paths, err := mergeUnmergedPaths(repo)
	if err != nil {
		return err
	}
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
	oursText, err := repo.Git("show", ":2:"+allowed)
	if err != nil {
		return err
	}
	theirsText, err := repo.Git("show", ":3:"+allowed)
	if err != nil {
		return err
	}
	ours, err := version.ParseFile(strings.TrimSpace(oursText))
	if err != nil {
		return err
	}
	theirs, err := version.ParseFile(strings.TrimSpace(theirsText))
	if err != nil {
		return err
	}
	side := "--ours"
	if mergeVersionGreater(theirs, ours) {
		side = "--theirs"
	}
	_, err = repo.Git("checkout", side, "--", allowed)
	if err == nil {
		_, err = repo.Git("add", "--", allowed)
	}
	return err
}
func mergeBump(repo *gitx.Repo, cfg *config.Config, branch, base, ticketKey string) error {
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
	msg := "chore: bump version after merge"
	if cfg.Ticket != nil && cfg.Ticket.Required && ticketKey != "" {
		msg += "\n\n" + ticketKey
	}
	_, err = repo.GitEnv(markerEnv, "commit", "-m", msg)
	return err
}

func mergeVersionAt(repo *gitx.Repo, oid string, cfg *config.Config) (string, error) {
	if cfg.Version.Mode != config.ModeFile {
		return cfg.Version.Mode, nil
	}
	data, err := repo.Git("show", oid+":"+cfg.Version.File)
	if err != nil {
		return "", err
	}
	v, err := version.ParseFile(strings.TrimSpace(data))
	if err != nil {
		return "", err
	}
	return v.String(), nil
}

func mergeCommitEmpty(repo *gitx.Repo, oid string) (bool, error) {
	_, err := repo.Git("diff-tree", "--root", "--quiet", oid, "--")
	if err == nil {
		return true, nil
	}
	if gitx.ExitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

func mergeUnmergedPaths(repo *gitx.Repo) ([]string, error) {
	out, err := repo.Git("diff", "--name-only", "-z", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	return strings.FieldsFunc(out, func(r rune) bool { return r == 0 }), nil
}

func mergeIndexEqualsHead(repo *gitx.Repo) (bool, error) {
	_, err := repo.Git("diff", "--cached", "--quiet")
	if err == nil {
		return true, nil
	}
	if gitx.ExitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

func mergeTreesEqual(repo *gitx.Repo, a, b string) (bool, error) {
	_, err := repo.Git("diff", "--quiet", a, b, "--")
	if err == nil {
		return true, nil
	}
	if gitx.ExitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

func removeMergeLinks(root string, cfg *config.Config) error {
	if cfg == nil {
		return nil
	}
	var errs []error
	for _, group := range cfg.Worktree.Link {
		for _, p := range group.Paths {
			path := filepath.Join(root, filepath.FromSlash(p))
			info, err := os.Lstat(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("stat %s: %w", p, err))
				continue
			}
			if info.Mode()&os.ModeSymlink != 0 {
				if err := os.Remove(path); err != nil {
					errs = append(errs, fmt.Errorf("remove %s: %w", p, err))
				}
			}
		}
	}
	return errors.Join(errs...)
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
func mergeTreeUnchanged(repo *gitx.Repo, rev string, cfg *config.Config, stage string) error {
	paths, err := repo.ChangedFiles()
	if err != nil {
		return err
	}
	var changed []string
	for _, p := range paths {
		linked := false
		for _, group := range cfg.Worktree.Link {
			for _, lp := range group.Paths {
				if filepath.ToSlash(lp) == filepath.ToSlash(p) {
					info, e := os.Lstat(filepath.Join(repo.Root, filepath.FromSlash(lp)))
					linked = e == nil && info.Mode()&os.ModeSymlink != 0
				}
			}
		}
		if !linked {
			changed = append(changed, p)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	slices.Sort(changed)
	return fmt.Errorf("merge %s changed the temporary tree after %s: %s", stage, gitx.Short(rev), strings.Join(changed, ", "))
}
