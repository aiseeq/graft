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

type mergeState struct {
	a                   *App
	ctx                 context.Context
	repo                *gitx.Repo
	cfg                 *config.Config
	opts                MergeOptions
	allRemotes, pushTo  []string
	branch, trunk       string
	trunkRef, sourceRef string
	A, F, local, R      string
	tmp, wt             string
	cleanupCfg          *config.Config
	w                   *gitx.Repo
	message, ticketKey  string
	sourceVersion       string
	trunkVersion        string
	mode                string
	fastForward         bool
	alreadyMerged       bool
	primary             string
	mirrorErr           error
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
	st := &mergeState{a: a, ctx: ctx, repo: repo, cfg: cfg, opts: o}
	if err := st.validateRequest(); err != nil {
		return err
	}
	if err := st.checkBeforeStart(); err != nil {
		return err
	}
	if err := st.takeSnapshot(); err != nil {
		return err
	}
	if st.alreadyMerged {
		return nil
	}
	defer st.cleanup()
	if err := st.prepareWorktree(); err != nil {
		return err
	}
	if err := st.buildCandidate(); err != nil {
		return err
	}
	if st.alreadyMerged {
		return nil
	}
	if err := st.checkCandidate(); err != nil {
		return err
	}
	if err := st.recheckBeforePush(); err != nil {
		return err
	}
	if err := st.push(); err != nil {
		return err
	}
	if err := st.install(); err != nil {
		return err
	}
	return st.mirrorErr
}

func (s *mergeState) validateRequest() error {
	if s.opts.Mode == "" {
		s.opts.Mode = "rebase"
	}
	if s.opts.Mode != "rebase" && s.opts.Mode != "merge" {
		return fmt.Errorf("--mode must be rebase or merge")
	}
	if s.opts.Message != "" && s.opts.Mode != "merge" {
		return fmt.Errorf("-m requires --mode merge")
	}
	if s.opts.NoDeps && strings.TrimSpace(s.opts.NoDepsReason) == "" {
		return fmt.Errorf("--no-deps requires --reason <text>")
	}
	return nil
}

func (s *mergeState) checkBeforeStart() error {
	var err error
	s.allRemotes, err = s.repo.Remotes()
	if err != nil {
		return err
	}
	if len(s.allRemotes) == 0 {
		return errors.New("graft merge requires at least one remote")
	}
	s.pushTo, err = pushRemotes(s.repo, s.cfg)
	if err != nil {
		return err
	}
	if len(s.pushTo) == 0 {
		return errors.New("graft merge requires at least one push remote")
	}
	s.branch, err = s.repo.Branch()
	if err != nil {
		return err
	}
	clean, err := s.repo.IsClean()
	if err != nil {
		return err
	}
	if !clean {
		return errors.New("graft merge requires a clean index and work tree, including untracked files")
	}
	s.trunkRef, err = trunkRef(s.repo, s.cfg, "", s.allRemotes)
	if err != nil {
		return err
	}
	for _, r := range s.pushTo {
		if _, fetchErr := s.repo.Git("fetch", "--quiet", r); fetchErr != nil {
			s.a.warn("initial fetch %s failed while resolving trunk: %v", r, fetchErr)
		}
	}
	return nil
}

func (s *mergeState) takeSnapshot() error {
	stable, err := s.resolveTrunkSnapshot()
	if err != nil {
		return err
	}
	if !stable {
		return errors.New("trunk config or ref kept changing while resolving it; retry graft merge")
	}
	if err := s.validateTrunk(); err != nil {
		return err
	}
	if err := s.resolveSource(); err != nil {
		return err
	}
	return s.checkSnapshotHistory()
}

func (s *mergeState) resolveTrunkSnapshot() (bool, error) {
	for range 3 {
		A, err := s.repo.Git("rev-parse", "--verify", s.trunkRef+"^{commit}")
		if err != nil {
			return false, err
		}
		s.A = strings.TrimSpace(A)
		if err = s.loadTrunkConfig(); err != nil {
			return false, err
		}
		if err = s.fetchConfiguredRemotes(); err != nil {
			return false, err
		}
		newRef, err := trunkRef(s.repo, s.cfg, "", s.allRemotes)
		if err != nil {
			return false, err
		}
		newA, err := s.repo.Git("rev-parse", "--verify", newRef+"^{commit}")
		if err != nil {
			return false, err
		}
		if newRef == s.trunkRef && strings.TrimSpace(newA) == s.A {
			return true, nil
		}
		s.trunkRef = newRef
	}
	return false, nil
}

func (s *mergeState) loadTrunkConfig() error {
	data, err := s.repo.Git("show", s.A+":"+config.FileName)
	if err != nil {
		return fmt.Errorf("reading %s from trunk %s: %w", config.FileName, gitx.Short(s.A), err)
	}
	s.cfg, err = config.Parse([]byte(data))
	if err != nil {
		return fmt.Errorf("trunk %s %s: %w", gitx.Short(s.A), config.FileName, err)
	}
	return s.a.requireGraft(s.cfg)
}

func (s *mergeState) fetchConfiguredRemotes() error {
	var err error
	s.pushTo, err = pushRemotes(s.repo, s.cfg)
	if err != nil {
		return err
	}
	s.primary = primaryRemote(s.cfg, s.allRemotes, s.trunkRef)
	if !slices.Contains(s.pushTo, s.primary) {
		return fmt.Errorf("deploy remote %q, the trunk's primary remote, is absent from push.remotes", s.primary)
	}
	for _, r := range s.pushTo {
		if _, err = s.repo.Git("fetch", "--quiet", r); err != nil {
			return fmt.Errorf("git fetch %s: %w", r, err)
		}
	}
	return nil
}

func (s *mergeState) validateTrunk() error {
	trunk, ok, err := worktreeSyncResolvedTrunkBranch(s.repo, s.trunkRef, s.allRemotes)
	if err != nil {
		return err
	}
	s.trunk = trunk
	if !ok || s.branch != s.trunk {
		return fmt.Errorf("graft merge must run on the local trunk branch %s", s.trunk)
	}
	if s.cfg.Version.TagOnCommit || config.BranchMatches(s.cfg.Version.Build, s.trunk) {
		return errors.New("graft merge: version.tag_on_commit and version.build_branches are not supported in v1")
	}
	return nil
}

func (s *mergeState) resolveSource() error {
	var err error
	s.sourceRef, err = resolveMergeSource(s.repo, s.opts.Branch, s.pushTo)
	if err != nil {
		return err
	}
	F, err := s.repo.Git("rev-parse", "--verify", s.sourceRef+"^{commit}")
	if err != nil {
		return fmt.Errorf("source %q is not a branch", s.opts.Branch)
	}
	s.F = strings.TrimSpace(F)
	if s.sourceIsTrunk() {
		return errors.New("source is the trunk")
	}
	return nil
}

func (s *mergeState) sourceIsTrunk() bool {
	if s.sourceRef == "refs/heads/"+s.trunk {
		return true
	}
	for _, remote := range s.pushTo {
		if s.sourceRef == "refs/remotes/"+remote+"/"+s.trunk {
			return true
		}
	}
	return false
}

func (s *mergeState) checkSnapshotHistory() error {
	s.a.printf("merge snapshot: A %s, F %s", gitx.Short(s.A), gitx.Short(s.F))
	local, err := s.repo.Git("rev-parse", "refs/heads/"+s.trunk)
	if err != nil {
		return err
	}
	s.local = strings.TrimSpace(local)
	isAncestor, err := s.repo.IsAncestor(s.local, s.A)
	if err != nil {
		return err
	}
	if !isAncestor {
		return errors.New("local trunk has commits absent from its remote; send or remove them before retrying")
	}
	merged, err := s.repo.IsAncestor(s.F, s.A)
	if err != nil {
		return err
	}
	if merged {
		s.a.printf("already merged %s", gitx.Short(s.F))
		s.alreadyMerged = true
	}
	return nil
}

func (s *mergeState) prepareWorktree() error {
	var err error
	s.tmp, err = os.MkdirTemp("", "graft-merge-*")
	if err != nil {
		return fmt.Errorf("creating merge temporary directory: %w", err)
	}
	s.wt = filepath.Join(s.tmp, "tree")
	if _, err = s.repo.Git("worktree", "add", "--detach", "--quiet", s.wt, s.A); err != nil {
		if rmErr := os.RemoveAll(s.tmp); rmErr != nil {
			s.a.warn("removing merge temporary directory %s: %v", s.tmp, rmErr)
		}
		s.tmp, s.wt = "", ""
		return err
	}
	s.cleanupCfg = s.cfg
	s.w, err = gitx.Open(s.wt)
	return err
}

func (s *mergeState) cleanup() {
	if s.wt == "" {
		return
	}
	if err := removeMergeLinks(s.wt, s.cleanupCfg); err != nil {
		s.a.warn("removing temporary worktree links: %v", err)
	}
	if _, err := s.repo.Git("worktree", "remove", "--force", s.wt); err != nil {
		s.a.warn("removing merge worktree %s: %v", s.wt, err)
	}
	if err := os.RemoveAll(s.tmp); err != nil {
		s.a.warn("removing merge temporary directory %s: %v", s.tmp, err)
	}
}

func (s *mergeState) buildCandidate() error {
	if err := s.prepareMergeMessage(); err != nil {
		return err
	}
	if err := s.captureVersions(); err != nil {
		return err
	}
	if err := s.checkConventionalMessage(); err != nil {
		return err
	}
	s.mode = s.opts.Mode
	if s.mode == "rebase" {
		if err := s.buildRebaseCandidate(); err != nil {
			return err
		}
	} else if err := s.buildMergeCandidate(); err != nil {
		return err
	}
	return s.finishCandidate()
}

func (s *mergeState) prepareMergeMessage() error {
	s.message = s.opts.Message
	if s.message == "" {
		s.message = fmt.Sprintf("Merge %s into %s", s.opts.Branch, s.trunk)
	}
	if s.cfg.Ticket == nil || config.BranchMatches(s.cfg.Ticket.Skip, s.opts.Branch) {
		return nil
	}
	s.ticketKey = message.BranchKey(s.opts.Branch, s.cfg.Ticket.Regexp)
	s.message = message.WithKey(s.message, s.ticketKey)
	if s.cfg.Ticket.Required && s.ticketKey != "" && !hasTicketKey(s.cfg, s.message) {
		s.message += "\n\n" + s.ticketKey
	}
	return nil
}

func (s *mergeState) captureVersions() error {
	if s.cfg.Version.Mode != config.ModeFile {
		return nil
	}
	var err error
	s.sourceVersion, err = mergeVersionAt(s.repo, s.F, s.cfg)
	if err != nil {
		return err
	}
	s.trunkVersion, err = mergeVersionAt(s.repo, s.A, s.cfg)
	return err
}

func (s *mergeState) checkConventionalMessage() error {
	if s.opts.Message == "" {
		return nil
	}
	uc, err := userconfig.CommitSettings()
	if err != nil {
		return err
	}
	conventional, source := uc.Conventional, "user config commit.conventional"
	if s.cfg.Commit.Conventional != nil {
		conventional, source = *s.cfg.Commit.Conventional, ".graft.yaml commit.conventional"
	}
	if conventional {
		return message.CheckConventionalSource(s.message, source)
	}
	return nil
}

func (s *mergeState) buildRebaseCandidate() error {
	ancestral, err := s.repo.IsAncestor(s.A, s.F)
	if err != nil {
		return err
	}
	if ancestral {
		s.fastForward = true
		_, err = s.w.Git("reset", "--hard", s.F)
		return err
	}
	list, err := s.repo.Git("rev-list", "--reverse", s.A+".."+s.F)
	if err != nil {
		return err
	}
	for c := range strings.FieldsSeq(list) {
		if err := s.cherryPickSourceCommit(c); err != nil {
			return err
		}
	}
	return nil
}

func (s *mergeState) cherryPickSourceCommit(commit string) error {
	parents, err := s.repo.Git("rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return err
	}
	if len(strings.Fields(parents)) > 2 {
		return errors.New("source contains a merge commit; retry with --mode merge")
	}
	empty, err := mergeCommitEmpty(s.repo, commit)
	if err != nil {
		return err
	}
	if empty {
		s.a.printf("merge: skipped empty commit %s", gitx.Short(commit))
		return nil
	}
	_, err = s.w.GitEnv(markerEnv, "cherry-pick", commit)
	if err == nil {
		return nil
	}
	return s.resolveCherryPickFailure(commit, err)
}

func (s *mergeState) resolveCherryPickFailure(commit string, cherryErr error) error {
	paths, err := mergeUnmergedPaths(s.w)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		same, err := mergeIndexEqualsHead(s.w)
		if err != nil {
			return err
		}
		if same {
			s.a.printf("merge: skipped empty commit %s", gitx.Short(commit))
			if _, err = s.w.GitEnv(markerEnv, "cherry-pick", "--skip"); err != nil {
				return fmt.Errorf("skipping empty cherry-pick: %w", err)
			}
			return nil
		}
		return fmt.Errorf("cherry-pick failed without conflicts: %w", cherryErr)
	}
	if err = mergeResolveVersion(s.w, s.cfg); err != nil {
		return err
	}
	return s.continueCherryPick()
}

func (s *mergeState) continueCherryPick() error {
	step := "--continue"
	same, err := mergeIndexEqualsHead(s.w)
	if err != nil {
		return err
	}
	if same {
		step = "--skip"
	}
	if _, err = s.w.GitEnv(markerEnv, "cherry-pick", step); err != nil {
		return fmt.Errorf("cherry-pick failed; trunk and source are unchanged: %w", err)
	}
	return nil
}

func (s *mergeState) buildMergeCandidate() error {
	args := []string{"merge", "--no-ff", "--no-edit", "-m", s.message, s.F}
	_, err := s.w.GitEnv(markerEnv, args...)
	if err == nil {
		return nil
	}
	paths, diffErr := s.w.Git("diff", "--name-only", "--diff-filter=U")
	if diffErr != nil {
		return fmt.Errorf("reading merge conflict paths: %w", diffErr)
	}
	if strings.TrimSpace(paths) == "" {
		return fmt.Errorf("merge failed: %w", err)
	}
	if err = mergeResolveVersion(s.w, s.cfg); err != nil {
		return err
	}
	_, err = s.w.GitEnv(markerEnv, "commit", "--no-edit")
	return err
}

func (s *mergeState) finishCandidate() error {
	R, err := s.w.Git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	s.R = strings.TrimSpace(R)
	hasChanges, err := s.candidateHasChanges()
	if err != nil {
		return err
	}
	if !hasChanges {
		s.a.printf("already merged by content")
		s.alreadyMerged = true
		return nil
	}
	if err = mergeBump(s.w, s.cfg, s.trunk, s.A, s.ticketKey); err != nil {
		return err
	}
	return s.reportVersion()
}

func (s *mergeState) candidateHasChanges() (bool, error) {
	if s.R == s.A {
		return false, nil
	}
	if s.mode != "merge" {
		return true, nil
	}
	same, err := mergeTreesEqual(s.w, s.A, s.R)
	return !same, err
}

func (s *mergeState) reportVersion() error {
	if s.cfg.Version.Mode != config.ModeFile {
		s.a.printf("merge version: %s", s.cfg.Version.Mode)
		return nil
	}
	v, err := version.Current(s.w.Root, s.cfg.Version)
	if err != nil {
		return err
	}
	s.a.printf("merge version: %s (source %s, trunk %s)", v, s.sourceVersion, s.trunkVersion)
	return nil
}

func (s *mergeState) checkCandidate() error {
	R, err := s.w.Git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	s.R = strings.TrimSpace(R)
	cfgResult, err := config.Load(s.w.Root)
	if err != nil {
		return fmt.Errorf("loading result config from %s: %w", gitx.Short(s.R), err)
	}
	if err = s.a.requireGraft(cfgResult); err != nil {
		return err
	}
	s.cleanupCfg = cfgResult
	linkSetups, err := s.a.linkWorktree(s.repo.Root, s.wt)
	if err != nil {
		return err
	}
	if err = s.a.prepareWorktree(s.ctx, s.repo.Root, s.wt, linkSetups); err != nil {
		return err
	}
	s.cfg = cfgResult
	return s.runResultChecks()
}

func (s *mergeState) runResultChecks() error {
	if s.opts.NoDeps {
		s.a.printf("merge deps skipped: %s", strings.TrimSpace(s.opts.NoDepsReason))
	}
	if s.fastForward {
		s.a.printf("merge mode: fast-forward")
	} else {
		s.a.printf("merge mode: %s", s.mode)
	}
	if _, _, err := s.a.gateForMessage(s.ctx, s.w, s.cfg, s.trunk, s.message); err != nil {
		return fmt.Errorf("merge gate failed; nothing published: %w", err)
	}
	if err := mergeTreeUnchanged(s.w, s.R, s.cfg, "gate"); err != nil {
		return err
	}
	s.a.printf("merge gate passed")
	return s.runContentChecks()
}

func (s *mergeState) runContentChecks() error {
	if _, err := s.w.Git("reset", "--soft", s.A); err != nil {
		return err
	}
	if err := runChecks(s.ctx, s.w, s.cfg, s.message, "", s.a.Stderr); err != nil {
		return fmt.Errorf("merge content checks failed: %w", err)
	}
	if s.cfg.Ticket != nil && s.mode == "merge" {
		if err := requireTicket(s.w, s.cfg, s.message, ""); err != nil {
			return err
		}
	}
	if _, err := s.w.Git("reset", "--soft", s.R); err != nil {
		return err
	}
	if err := mergeTreeUnchanged(s.w, s.R, s.cfg, "content checks"); err != nil {
		return err
	}
	return s.runMergeDeps()
}

func (s *mergeState) runMergeDeps() error {
	if s.opts.NoDeps || len(s.cfg.Merge.Deps) == 0 {
		if !s.opts.NoDeps {
			s.a.printf("merge deps: skipped (none configured)")
		}
		return nil
	}
	s.a.printf("merge deps: %s", strings.Join(s.cfg.Merge.Deps, ", "))
	if err := s.a.runner(s.w, s.cfg).RunDeps(s.ctx, s.cfg.Merge.Deps); err != nil {
		return fmt.Errorf("merge deps failed; nothing published: %w", err)
	}
	return mergeTreeUnchanged(s.w, s.R, s.cfg, "merge.deps")
}

func (s *mergeState) recheckBeforePush() error {
	cur, err := s.repo.Git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(cur) != s.local {
		return errors.New("local trunk moved during checks; nothing published")
	}
	clean, err := s.repo.IsClean()
	if err != nil {
		return err
	}
	if !clean {
		return errors.New("local trunk became dirty during checks; nothing published")
	}
	return nil
}

func (s *mergeState) push() error {
	s.primary = primaryRemote(s.cfg, s.pushTo, s.trunkRef)
	if _, err := s.repo.Git("push", s.primary, s.R+":refs/heads/"+s.trunk); err != nil {
		return fmt.Errorf("push to %s refused, nothing published and the local trunk unchanged; if the remote moved ahead: git fetch, then graft merge again: %w", s.primary, err)
	}
	s.a.printf("pushed to %s", s.primary)
	mirrors := slices.DeleteFunc(slices.Clone(s.pushTo), func(r string) bool { return r == s.primary })
	if len(mirrors) > 0 {
		s.mirrorErr = s.a.push(s.repo, mirrors, []string{s.R + ":refs/heads/" + s.trunk}, false, false)
	}
	return nil
}

func (s *mergeState) install() error {
	if _, err := s.repo.GitEnv(markerEnv, "merge", "--ff-only", s.R); err != nil {
		return fmt.Errorf("trunk on the remote is already %s; install it with: git merge --ff-only %s: %w", s.R, s.R, err)
	}
	s.a.printf("installed %s %s", s.trunk, gitx.Short(s.R))
	s.a.printf("merged %s %s into %s as %s", s.opts.Branch, gitx.Short(s.F), s.trunk, gitx.Short(s.R))
	return nil
}

func resolveMergeSource(repo *gitx.Repo, name string, remotes []string) (string, error) {
	name, forceLocal, err := normalizeMergeSourceName(name)
	if err != nil {
		return "", err
	}
	if !forceLocal {
		if ref, ok, err := explicitMergeRemoteSource(repo, name, remotes); err != nil || ok {
			return ref, err
		}
	}
	if ref, ok, err := localMergeSource(repo, name); err != nil || ok {
		return ref, err
	}
	return remoteMergeSource(repo, name, remotes)
}

func normalizeMergeSourceName(name string) (string, bool, error) {
	forceLocal := strings.HasPrefix(name, "refs/heads/")
	if forceLocal {
		name = strings.TrimPrefix(name, "refs/heads/")
		if name == "" {
			return "", true, errors.New("source must name a branch")
		}
	}
	if strings.HasPrefix(name, "refs/tags/") || strings.HasPrefix(name, "refs/remotes/") {
		return "", forceLocal, fmt.Errorf("source %q must be a branch name", name)
	}
	return name, forceLocal, nil
}

func explicitMergeRemoteSource(repo *gitx.Repo, name string, remotes []string) (string, bool, error) {
	for _, remote := range remotes {
		branch, ok := strings.CutPrefix(name, remote+"/")
		if !ok {
			continue
		}
		ref := "refs/remotes/" + remote + "/" + branch
		exists, err := repo.RefExists(ref)
		if err != nil || exists {
			return ref, exists, err
		}
	}
	return "", false, nil
}

func localMergeSource(repo *gitx.Repo, name string) (string, bool, error) {
	local := "refs/heads/" + name
	exists, err := repo.RefExists(local)
	if err != nil || !exists {
		return "", false, err
	}
	if err = validateLocalMergeUpstream(repo, name, local); err != nil {
		return "", false, err
	}
	return local, true, nil
}

func validateLocalMergeUpstream(repo *gitx.Repo, name, local string) error {
	localOID, err := repo.Git("rev-parse", "--verify", local+"^{commit}")
	if err != nil {
		return err
	}
	upstream, err := repo.Git("for-each-ref", "--format=%(upstream)", local)
	if err != nil || strings.TrimSpace(upstream) == "" {
		return err
	}
	upstream = strings.TrimSpace(upstream)
	present, err := repo.RefExists(upstream)
	if err != nil || !present {
		return err
	}
	upstreamOID, err := repo.Git("rev-parse", "--verify", upstream+"^{commit}")
	if err != nil {
		return err
	}
	contains, err := repo.IsAncestor(strings.TrimSpace(upstreamOID), strings.TrimSpace(localOID))
	if err != nil {
		return err
	}
	if !contains {
		return fmt.Errorf("local source branch %s is behind or diverged from %s; pull or push first", name, upstream)
	}
	return nil
}

func remoteMergeSource(repo *gitx.Repo, name string, remotes []string) (string, error) {
	var candidates []string
	for _, remote := range remotes {
		ref := "refs/remotes/" + remote + "/" + name
		exists, err := repo.RefExists(ref)
		if err != nil {
			return "", err
		}
		if exists {
			candidates = append(candidates, ref)
		}
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	return "", missingMergeSource(repo, name, len(candidates))
}

func missingMergeSource(repo *gitx.Repo, name string, candidates int) error {
	if candidates != 0 {
		return fmt.Errorf("source %q is missing or ambiguous; name one branch uniquely", name)
	}
	commit, err := repo.RevExists(name)
	if err != nil {
		return err
	}
	if commit {
		return fmt.Errorf("source %q resolves to a commit, not a branch", name)
	}
	return fmt.Errorf("source %q is missing or ambiguous; name one branch uniquely", name)
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
	if ours.Less(theirs) {
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
	if baseVersion.Less(current) {
		return nil
	}
	p, err := planBump(repo, cfg, version.Patch, branch)
	if err != nil {
		return err
	}
	if p.next == nil {
		return nil
	}
	if !current.Less(*p.next) {
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
