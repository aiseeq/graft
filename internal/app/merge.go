package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/envs"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/message"
	"github.com/aiseeq/graft/v2/internal/proc"
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
	resultCfg           *config.Config
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
	frozen              *activeFreeze
	advisoryFailed      []string
}

const mergeOwnerFile = ".graft-merge-owner"

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
	// A replace ref would show the checks one tree and push another: the
	// merge and its tasks see the objects the remote gets.
	if err := os.Setenv("GIT_NO_REPLACE_OBJECTS", "1"); err != nil {
		return fmt.Errorf("setting GIT_NO_REPLACE_OBJECTS: %w", err)
	}
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
	if err := warnOrphanedMergeTrees(s.repo, s.a); err != nil {
		return err
	}
	if err := refuseSubstitutedObjects(s.repo); err != nil {
		return err
	}
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
	if remote := explicitMergeSourceRemote(s.opts.Branch, s.allRemotes); remote != "" {
		if _, err = s.repo.Git("fetch", "--quiet", remote); err != nil {
			return fmt.Errorf("git fetch %s for source: %w", remote, err)
		}
	}
	name := strings.TrimPrefix(s.opts.Branch, "refs/remotes/")
	s.sourceRef, err = resolveMergeSource(s.repo, name, s.allRemotes, s.trunkRef)
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

func explicitMergeSourceRemote(name string, remotes []string) string {
	name = strings.TrimPrefix(name, "refs/remotes/")
	for _, remote := range remotes {
		if branch, ok := strings.CutPrefix(name, remote+"/"); ok && branch != "" {
			return remote
		}
	}
	return ""
}

func (s *mergeState) sourceIsTrunk() bool {
	if s.sourceRef == "refs/heads/"+s.trunk {
		return true
	}
	for _, remote := range s.allRemotes {
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
		if err := s.reportMissingMirrors(s.A); err != nil {
			return err
		}
		s.a.printf("already merged %s", gitx.Short(s.F))
		s.alreadyMerged = true
		if s.local != s.A {
			return s.installMerged()
		}
	}
	return nil
}

// installMerged fast-forwards a local trunk that is behind the remote one
// already holding the source, as after a push whose install did not run.
func (s *mergeState) installMerged() error {
	if err := s.checkoutUnchanged(); err != nil {
		return fmt.Errorf("local %s is behind %s/%s at %s and the %w; on a clean %s install it with: git merge --ff-only %s", s.trunk, s.primary, s.trunk, gitx.Short(s.A), err, s.trunk, s.A)
	}
	if _, err := s.repo.GitEnv(markerEnv, "merge", "--ff-only", s.A); err != nil {
		return fmt.Errorf("local %s is behind %s/%s; install it with: git merge --ff-only %s: %w", s.trunk, s.primary, s.trunk, s.A, err)
	}
	s.a.printf("local %s moved to %s", s.trunk, gitx.Short(s.A))
	s.a.warnInstallFreshness(s.ctx, s.repo, s.cfg)
	return nil
}

func (s *mergeState) prepareWorktree() error {
	var err error
	trees, err := listWorktrees(s.repo)
	if err != nil {
		return err
	}
	if len(trees) == 0 {
		return errors.New("the repository has no main worktree")
	}
	main, err := filepath.Abs(trees[0].path)
	if err != nil {
		return fmt.Errorf("resolving main worktree path: %w", err)
	}
	s.tmp, err = os.MkdirTemp("", "graft-merge-*")
	if err != nil {
		return fmt.Errorf("creating merge temporary directory: %w", err)
	}
	if err = writeMergeOwner(s.tmp, s.repo, os.Getpid()); err != nil {
		removeErr := os.RemoveAll(s.tmp)
		s.tmp = ""
		return errors.Join(err, removeErr)
	}
	discardTemp := func(cause error) error {
		removeErr := os.RemoveAll(s.tmp)
		s.tmp, s.wt = "", ""
		return errors.Join(cause, removeErr)
	}
	depth, err := maxNeighbourDepth(s.repo, main, s.A, s.F)
	if err != nil {
		return discardTemp(err)
	}
	s.wt, err = mergeWorktreePath(s.tmp, main, depth)
	if err != nil {
		return discardTemp(err)
	}
	links, err := neighboursFor(s.repo, main, s.wt, s.A, s.F)
	if err != nil {
		return discardTemp(err)
	}
	if err = checkNeighbourBounds(links, s.tmp); err != nil {
		return discardTemp(err)
	}
	if err := os.MkdirAll(filepath.Dir(s.wt), 0o755); err != nil {
		return discardTemp(fmt.Errorf("creating merge worktree parent: %w", err))
	}
	if _, err = s.repo.Git("worktree", "add", "--detach", "--quiet", s.wt, s.A); err != nil {
		if rmErr := os.RemoveAll(s.tmp); rmErr != nil {
			s.a.warn("removing merge temporary directory %s: %v", s.tmp, rmErr)
		}
		s.tmp, s.wt = "", ""
		return err
	}
	if err = linkNeighbours(s.a, links, s.tmp, false); err != nil {
		return err
	}
	s.cleanupCfg = s.cfg
	s.w, err = gitx.Open(s.wt)
	return err
}

func writeMergeOwner(root string, repo *gitx.Repo, pid int) error {
	data := fmt.Sprintf("%d\n%s\n", pid, repo.CommonDir)
	if err := os.WriteFile(filepath.Join(root, mergeOwnerFile), []byte(data), 0o600); err != nil {
		return fmt.Errorf("recording merge worktree owner: %w", err)
	}
	return nil
}

// refuseSubstitutedObjects refuses a repo whose replace refs or grafts file
// substitute commits: the gate's git would read the substitutes while the
// push sends the originals.
func refuseSubstitutedObjects(repo *gitx.Repo) error {
	// Both bases: graft's git may read GIT_REPLACE_REF_BASE, while the
	// tasks run without it and read refs/replace/.
	bases := []string{"refs/replace/"}
	if base := os.Getenv("GIT_REPLACE_REF_BASE"); base != "" {
		bases = append(bases, base)
	}
	for _, base := range bases {
		refs, err := repo.Git("for-each-ref", "--count=1", "--format=%(refname)", base)
		if err != nil {
			return err
		}
		if ref := strings.TrimSpace(refs); ref != "" {
			return fmt.Errorf("the repo has replace refs (%s): the checks would read the replacements while the push sends the originals; merge without them (git replace -d) or by hand", ref)
		}
	}
	grafts, err := repo.Git("rev-parse", "--path-format=absolute", "--git-path", "info/grafts")
	if err != nil {
		return err
	}
	if _, err := os.Stat(strings.TrimSpace(grafts)); err == nil {
		return fmt.Errorf("the repo has a grafts file (%s): the checks would read grafted history while the push sends the real one; merge without it or by hand", strings.TrimSpace(grafts))
	}
	return nil
}

// warnOrphanedMergeTrees names the temporary trees of this repo left by a
// graft merge that is gone. It removes none: a task the dead merge started
// may still run in the tree, and graft cannot tell when it ends.
func warnOrphanedMergeTrees(repo *gitx.Repo, a *App) error {
	trees, err := listWorktrees(repo)
	if err != nil {
		return fmt.Errorf("listing worktrees to find orphaned merge trees: %w", err)
	}
	tmpRoot, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return fmt.Errorf("resolving merge temporary directory %s: %w", os.TempDir(), err)
	}
	entries, err := os.ReadDir(tmpRoot)
	if err != nil {
		return fmt.Errorf("listing merge temporary directories: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "graft-merge-") {
			continue
		}
		root := filepath.Join(tmpRoot, entry.Name())
		if err := warnOrphanedMergeRoot(repo, a, root, trees); err != nil {
			a.warn("checking merge temporary directory %s: %v", root, err)
		}
	}
	return nil
}

func warnOrphanedMergeRoot(repo *gitx.Repo, a *App, root string, trees []worktree) error {
	registered, err := worktreesInMergeRoot(root, trees)
	if err != nil {
		return err
	}
	pid, commonDir, hasOwner, ownerErr := readMergeOwner(root)
	if ownerErr != nil {
		warnMergeOwnerUnreadable(a, root, registered, ownerErr)
		return nil
	}
	if !hasOwner {
		for _, wt := range registered {
			a.printf("warning: temporary worktree %s has no merge owner marker; once nothing runs in it, remove it with: %s", wt, registeredMergeCleanup(root, wt))
		}
		return nil
	}
	if filepath.Clean(commonDir) != filepath.Clean(repo.CommonDir) && len(registered) == 0 {
		return nil
	}
	alive, probeErr := proc.Alive(pid)
	if probeErr != nil {
		warnMergeOwnerProbe(a, root, registered, probeErr)
		return nil
	}
	if alive {
		return nil
	}
	warnDeadMergeRoot(a, root, registered, pid)
	return nil
}

func warnMergeOwnerUnreadable(a *App, root string, registered []string, err error) {
	if len(registered) > 0 {
		for _, wt := range registered {
			a.printf("warning: cannot verify merge worktree owner in %s: %v; if no merge process is using it, remove it with: %s", root, err, registeredMergeCleanup(root, wt))
		}
		return
	}
	a.printf("warning: cannot verify merge worktree owner in %s: %v; if no merge process is using it, remove it with: %s", root, err, unregisteredMergeCleanup(root))
}

func warnMergeOwnerProbe(a *App, root string, registered []string, err error) {
	warnMergeRootStatus(a, root, registered,
		"warning: cannot tell whether merge worktree %s is still in use: %v; if orphaned, remove it with: %s",
		"warning: cannot tell whether merge temporary directory %s is still in use: %v; if orphaned, remove it with: %s",
		err,
	)
}

func warnDeadMergeRoot(a *App, root string, registered []string, pid int) {
	warnMergeRootStatus(a, root, registered,
		"warning: temporary worktree %s is left by a graft merge that is gone (pid %d); once nothing runs in it, remove it with: %s",
		"warning: temporary directory %s is left by a graft merge that is gone (pid %d); once nothing runs in it, remove it with: %s",
		pid,
	)
}

func warnMergeRootStatus(a *App, root string, registered []string, registeredFormat, rootFormat string, values ...any) {
	if len(registered) > 0 {
		for _, wt := range registered {
			args := append([]any{wt}, values...)
			args = append(args, registeredMergeCleanup(root, wt))
			a.printf(registeredFormat, args...)
		}
		return
	}
	args := append([]any{root}, values...)
	args = append(args, unregisteredMergeCleanup(root))
	a.printf(rootFormat, args...)
}

func worktreesInMergeRoot(root string, trees []worktree) ([]string, error) {
	var found []string
	for _, tree := range trees {
		path, err := filepath.Abs(tree.path)
		if err != nil {
			return nil, fmt.Errorf("resolving worktree path %s: %w", tree.path, err)
		}
		if filepath.Clean(path) != filepath.Clean(root) && pathWithin(root, path) {
			found = append(found, path)
		}
	}
	slices.Sort(found)
	return found, nil
}

func registeredMergeCleanup(root, wt string) string {
	return "git worktree remove --force " + shellQuote(wt) + " && rm -rf " + shellQuote(root)
}

func unregisteredMergeCleanup(root string) string {
	return "rm -rf " + shellQuote(root)
}

func readMergeOwner(root string) (int, string, bool, error) {
	data, err := os.ReadFile(filepath.Join(root, mergeOwnerFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, "", false, nil
		}
		return 0, "", false, fmt.Errorf("reading owner marker: %w", err)
	}
	parts := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)
	if len(parts) != 2 {
		return 0, "", false, errors.New("malformed owner marker")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, "", false, fmt.Errorf("parsing owner pid: %w", err)
	}
	if pid < 1 {
		return 0, "", false, fmt.Errorf("invalid owner pid %d", pid)
	}
	return pid, strings.TrimSpace(parts[1]), true, nil
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
	if err = s.validateResultPublishConfig(cfgResult); err != nil {
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
	s.resultCfg = cfgResult
	return s.runResultChecks()
}

func (s *mergeState) validateResultPublishConfig(result *config.Config) error {
	resultPush, err := pushRemotes(s.repo, result)
	if err != nil {
		return err
	}
	resultPrimary := primaryRemote(result, s.allRemotes, s.trunkRef)
	if !sameRemoteSet(s.pushTo, resultPush) || s.primary != resultPrimary {
		return fmt.Errorf("the branch changes where trunk is published (%s/%s); merge it by hand", resultPrimary, s.trunk)
	}
	resultTrunkRef, err := trunkRef(s.repo, result, "", s.allRemotes)
	if err != nil {
		return fmt.Errorf("the branch changes where trunk is published (%s); merge it by hand: %w", resultPrimary, err)
	}
	resultPrimary = primaryRemote(result, s.allRemotes, resultTrunkRef)
	if s.trunkRef == resultTrunkRef && s.primary == resultPrimary {
		return nil
	}
	target := resultPrimary + "/" + s.trunk
	if resultTrunkRef != s.trunkRef {
		target = resultTrunkRef
	}
	return fmt.Errorf("the branch changes where trunk is published (%s); merge it by hand", target)
}

func sameRemoteSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, remote := range a {
		if !slices.Contains(b, remote) {
			return false
		}
	}
	return true
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
	frozen, failed, err := s.a.gateForMessage(s.ctx, s.w, s.resultCfg, s.trunk, s.message)
	if err != nil {
		return fmt.Errorf("merge gate failed; nothing published: %w", err)
	}
	s.frozen, s.advisoryFailed = frozen, failed
	if err := mergeTreeUnchanged(s.w, s.R, s.resultCfg, "gate"); err != nil {
		return err
	}
	s.a.printf("merge gate passed")
	return s.runContentChecks()
}

func (s *mergeState) runContentChecks() error {
	if _, err := s.w.Git("reset", "--soft", s.A); err != nil {
		return err
	}
	if err := runChecks(s.ctx, s.w, s.resultCfg, s.message, "", s.a.Stderr); err != nil {
		return fmt.Errorf("merge content checks failed: %w", err)
	}
	if s.resultCfg.Ticket != nil && s.mode == "merge" {
		if err := requireTicket(s.w, s.resultCfg, s.message, ""); err != nil {
			return err
		}
	}
	if _, err := s.w.Git("reset", "--soft", s.R); err != nil {
		return err
	}
	if err := mergeTreeUnchanged(s.w, s.R, s.resultCfg, "content checks"); err != nil {
		return err
	}
	return s.runMergeDeps()
}

func (s *mergeState) runMergeDeps() error {
	if s.opts.NoDeps || len(s.resultCfg.Merge.Deps) == 0 {
		if !s.opts.NoDeps {
			s.a.printf("merge deps: skipped (none configured)")
		}
		return nil
	}
	s.a.printf("merge deps: %s", strings.Join(s.resultCfg.Merge.Deps, ", "))
	if err := s.a.runner(s.w, s.resultCfg).RunDeps(s.ctx, s.resultCfg.Merge.Deps); err != nil {
		return fmt.Errorf("merge deps failed; nothing published: %w", err)
	}
	return mergeTreeUnchanged(s.w, s.R, s.resultCfg, "merge.deps")
}

func (s *mergeState) recheckBeforePush() error {
	if err := s.checkoutUnchanged(); err != nil {
		return fmt.Errorf("%w during checks; nothing published", err)
	}
	return s.checkMirrorsCanAccept(s.R)
}

// checkoutUnchanged tells whether the checkout is still the clean trunk at
// the commit the merge started from.
func (s *mergeState) checkoutUnchanged() error {
	branch, err := s.repo.Git("symbolic-ref", "--quiet", "HEAD")
	if err != nil || strings.TrimSpace(branch) != "refs/heads/"+s.trunk {
		return errors.New("checkout branch changed")
	}
	cur, err := s.repo.Git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(cur) != s.local {
		return errors.New("local trunk moved")
	}
	clean, err := s.repo.IsClean()
	if err != nil {
		return err
	}
	if !clean {
		return errors.New("local trunk became dirty")
	}
	return nil
}

func (s *mergeState) checkMirrorsCanAccept(rev string) error {
	for _, remote := range s.pushTo {
		if _, err := s.repo.Git("fetch", "--quiet", remote); err != nil {
			return fmt.Errorf("cannot verify remote %s before publishing: %w", remote, err)
		}
		ref := "refs/remotes/" + remote + "/" + s.trunk
		remoteHead, err := s.repo.Git("rev-parse", "--verify", ref+"^{commit}")
		if err != nil {
			return fmt.Errorf("remote %s has no trunk %s before publishing: %w", remote, s.trunk, err)
		}
		ancestor, err := s.repo.IsAncestor(strings.TrimSpace(remoteHead), rev)
		if err != nil {
			return err
		}
		if !ancestor {
			return fmt.Errorf("remote %s trunk is not an ancestor of merge result %s; nothing published; if the remote moved ahead: git fetch, then graft merge again", remote, gitx.Short(rev))
		}
	}
	return nil
}

func (s *mergeState) reportMissingMirrors(rev string) error {
	var checkErrs []error
	for _, remote := range s.pushTo {
		urls, err := remotePushURLs(s.repo, remote)
		if err != nil {
			checkErrs = append(checkErrs, err)
			continue
		}
		for _, url := range urls {
			remoteHead, err := s.repo.Git("ls-remote", url, "refs/heads/"+s.trunk)
			if err != nil {
				checkErrs = append(checkErrs, fmt.Errorf("cannot check %s: ls-remote failed: %w", url, err))
				continue
			}
			fields := strings.Fields(remoteHead)
			if len(fields) < 2 || fields[1] != "refs/heads/"+s.trunk {
				msg := fmt.Sprintf("push URL %s for remote %s has no trunk %s; catch it up with: git push --no-follow-tags -- %s %s:refs/heads/%s", url, remote, s.trunk, envs.Quote(url), gitx.Short(rev), s.trunk)
				if remote == s.primary {
					s.a.warn("%s", msg)
					continue
				}
				checkErrs = append(checkErrs, errors.New(msg))
				continue
			}
			if err := fetchCommitFromPushURL(s.repo, url, "refs/heads/"+s.trunk, fields[0]); err != nil {
				checkErrs = append(checkErrs, fmt.Errorf("cannot check %s: %w", url, err))
				continue
			}
			contains, err := s.repo.IsAncestor(rev, fields[0])
			if err != nil {
				checkErrs = append(checkErrs, fmt.Errorf("cannot check %s: compare trunk ancestry: %w", url, err))
				continue
			}
			if !contains {
				cmd := fmt.Sprintf("git push --no-follow-tags -- %s %s:refs/heads/%s", envs.Quote(url), gitx.Short(rev), s.trunk)
				msg := fmt.Sprintf("push URL %s for remote %s is missing %s; catch it up with: %s", url, remote, gitx.Short(rev), cmd)
				if remote == s.primary {
					s.a.warn("%s", msg)
					continue
				}
				checkErrs = append(checkErrs, errors.New(msg))
			}
		}
	}
	return errors.Join(checkErrs...)
}

func fetchCommitFromPushURL(repo *gitx.Repo, url, ref, oid string) error {
	if _, err := repo.Git("cat-file", "-e", oid+"^{commit}"); err == nil {
		return nil
	}
	_, oidErr := repo.Git("fetch", "--quiet", url, oid)
	if _, err := repo.Git("cat-file", "-e", oid+"^{commit}"); err == nil {
		return nil
	}
	_, refErr := repo.Git("fetch", "--quiet", url, ref)
	if _, err := repo.Git("cat-file", "-e", oid+"^{commit}"); err == nil {
		return nil
	}
	fetchErr := errors.Join(oidErr, refErr)
	if fetchErr == nil {
		fetchErr = errors.New("fetched commit is still unavailable locally")
	}
	return fmt.Errorf("could not fetch trunk commit %s by object ID or ref %s: %w", oid, ref, fetchErr)
}

func (s *mergeState) push() error {
	recurse, err := s.repo.Git("config", "--get", "push.recurseSubmodules")
	if err != nil && gitx.ExitCode(err) != 1 {
		return fmt.Errorf("cannot read push.recurseSubmodules before publishing: %w", err)
	}
	var gitConfig []string
	if err == nil && strings.TrimSpace(recurse) == "on-demand" {
		gitConfig = []string{"-c", "push.recurseSubmodules=check"}
	}
	// --no-follow-tags: push.followTags must not publish tags beside trunk.
	args := append(slices.Clone(gitConfig), pushArgs(s.primary, false, true)...)
	if _, err := s.repo.Git(append(args, s.R+":refs/heads/"+s.trunk)...); err != nil {
		return fmt.Errorf("push to %s failed: nothing pushed to the mirrors and the local trunk unchanged, but a remote with several push URLs may hold %s on some of them, as git's output below says; if the remote moved ahead: git fetch, then graft merge again: %w", s.primary, gitx.Short(s.R), err)
	}
	s.a.printf("pushed to %s", s.primary)
	mirrors := slices.DeleteFunc(slices.Clone(s.pushTo), func(r string) bool { return r == s.primary })
	if len(mirrors) > 0 {
		s.mirrorErr = s.a.pushWithGitConfig(s.repo, mirrors, []string{s.R + ":refs/heads/" + s.trunk}, gitConfig, false, true)
	}
	if err := s.a.recordCommitAdvisoryAfterPush(s.w, s.resultCfg, s.frozen, s.advisoryFailed); err != nil {
		s.mirrorErr = errors.Join(s.mirrorErr, err)
	}
	return nil
}

func (s *mergeState) install() error {
	// The push ran the user's pre-push hook, which may switch or move the
	// checkout: fast-forward only the trunk the merge started from.
	if err := s.checkoutUnchanged(); err != nil {
		return fmt.Errorf("trunk on the remote is already %s, but the %w after the push, so the local %s is left as it was; on a clean %s install it with: git merge --ff-only %s", s.R, err, s.trunk, s.trunk, s.R)
	}
	if _, err := s.repo.GitEnv(markerEnv, "merge", "--ff-only", s.R); err != nil {
		return fmt.Errorf("trunk on the remote is already %s; install it with: git merge --ff-only %s: %w", s.R, s.R, err)
	}
	s.a.printf("local %s moved to %s", s.trunk, gitx.Short(s.R))
	s.a.printf("merged %s %s into %s as %s", s.opts.Branch, gitx.Short(s.F), s.trunk, gitx.Short(s.R))
	// The merge runs no install task: name an installed binary the new
	// trunk left behind, as graft commit does.
	s.a.warnInstallFreshness(s.ctx, s.repo, s.resultCfg)
	return nil
}

func resolveMergeSource(repo *gitx.Repo, name string, remotes []string, trunkRef string) (string, error) {
	name, forceLocal, err := normalizeMergeSourceName(name)
	if err != nil {
		return "", err
	}
	if !forceLocal {
		if ref, ok, err := explicitMergeRemoteSource(repo, name, remotes); err != nil || ok {
			return ref, err
		}
	}
	if ref, ok, err := localMergeSource(repo, name, trunkRef); err != nil || ok {
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

func localMergeSource(repo *gitx.Repo, name, trunkRef string) (string, bool, error) {
	local := "refs/heads/" + name
	exists, err := repo.RefExists(local)
	if err != nil || !exists {
		return "", false, err
	}
	if err = validateLocalMergeUpstream(repo, name, local, trunkRef); err != nil {
		return "", false, err
	}
	return local, true, nil
}

func validateLocalMergeUpstream(repo *gitx.Repo, name, local, trunkRef string) error {
	localOID, err := repo.Git("rev-parse", "--verify", local+"^{commit}")
	if err != nil {
		return err
	}
	upstream, err := repo.Git("for-each-ref", "--format=%(upstream)", local)
	if err != nil || strings.TrimSpace(upstream) == "" {
		return err
	}
	upstream = strings.TrimSpace(upstream)
	trunkFullRef, err := repo.Git("rev-parse", "--symbolic-full-name", trunkRef)
	if err != nil {
		return err
	}
	if upstream == strings.TrimSpace(trunkFullRef) {
		return nil
	}
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
	targetsByPath := make(map[string][]version.Target, len(cfg.Version.Sync))
	for _, target := range version.Targets(cfg.Version)[1:] {
		targetsByPath[target.Path] = append(targetsByPath[target.Path], target)
	}
	if len(paths) == 0 {
		return errors.New("merge failed without conflicts")
	}
	if !slices.Contains(paths, allowed) {
		return fmt.Errorf("merge conflict in %s; trunk and source are unchanged", strings.Join(paths, ", "))
	}
	for _, p := range paths {
		if p != allowed && len(targetsByPath[p]) == 0 {
			return fmt.Errorf("merge conflict in %s; trunk and source are unchanged", strings.Join(paths, ", "))
		}
	}
	versionStages, err := mergeVersionConflictStages(repo, allowed, len(paths) > 1)
	if err != nil {
		return err
	}
	ours, theirs := versionStages[1], versionStages[2]
	chosen := ours
	side := "--ours"
	if ours.Less(theirs) {
		chosen = theirs
		side = "--theirs"
	}
	mirrors, err := mergePrepareVersionMirrors(repo, paths, allowed, targetsByPath, versionStages, chosen.String())
	if err != nil {
		return err
	}
	return mergeInstallVersionResolution(repo, allowed, side, mirrors)
}

func mergeVersionConflictStages(repo *gitx.Repo, path string, includeBase bool) ([3]version.Semver, error) {
	var stages [3]version.Semver
	for i := 1; i <= len(stages); i++ {
		if i == 1 && !includeBase {
			continue
		}
		text, err := repo.Git("show", fmt.Sprintf(":%d:%s", i, path))
		if err != nil {
			return stages, err
		}
		stages[i-1], err = version.ParseFile(strings.TrimSpace(text))
		if err != nil {
			return stages, err
		}
	}
	return stages, nil
}

type mergeVersionMirrorResult struct {
	path    string
	content []byte
}

func mergePrepareVersionMirrors(repo *gitx.Repo, paths []string, versionFile string, targetsByPath map[string][]version.Target, versionStages [3]version.Semver, selected string) ([]mergeVersionMirrorResult, error) {
	var mirrors []mergeVersionMirrorResult
	for _, p := range paths {
		if p == versionFile {
			continue
		}
		content, err := mergeVersionMirror(repo, targetsByPath[p], p, versionStages, selected)
		if err != nil {
			return nil, err
		}
		mirrors = append(mirrors, mergeVersionMirrorResult{path: p, content: content})
	}
	return mirrors, nil
}

func mergeInstallVersionResolution(repo *gitx.Repo, versionFile, side string, mirrors []mergeVersionMirrorResult) error {
	_, err := repo.Git("checkout", side, "--", versionFile)
	if err == nil {
		_, err = repo.Git("add", "--", versionFile)
	}
	if err != nil {
		return err
	}
	for _, mirror := range mirrors {
		fullPath := filepath.Join(repo.Root, filepath.FromSlash(mirror.path))
		info, err := os.Stat(fullPath)
		if err != nil {
			return fmt.Errorf("stat merge mirror %s: %w", mirror.path, err)
		}
		if err := os.WriteFile(fullPath, mirror.content, info.Mode().Perm()); err != nil {
			return fmt.Errorf("write merged mirror %s: %w", mirror.path, err)
		}
		if _, err := repo.Git("add", "--", mirror.path); err != nil {
			return err
		}
	}
	return nil
}

func mergeVersionMirror(repo *gitx.Repo, targets []version.Target, path string, versionStages [3]version.Semver, selected string) ([]byte, error) {
	stages := [3][]byte{}
	for i := range stages {
		text, err := repo.Git("show", fmt.Sprintf(":%d:%s", i+1, path))
		if err != nil {
			return nil, fmt.Errorf("merge conflict in %s; trunk and source are unchanged", path)
		}
		original := []byte(text)
		for _, target := range targets {
			value, err := target.Extract(original)
			if err != nil {
				return nil, fmt.Errorf("merge conflict in %s; trunk and source are unchanged: %w", path, err)
			}
			got, err := version.ParseFile(value)
			if err != nil {
				return nil, fmt.Errorf("merge conflict in %s; trunk and source are unchanged", path)
			}
			if got != versionStages[i] {
				return nil, fmt.Errorf("merge conflict in %s: mirror version %s does not match VERSION stage %s; trunk and source are unchanged", path, got, versionStages[i])
			}
		}
		stages[i] = original
		for _, target := range targets {
			stages[i], err = target.Replace(stages[i], selected)
			if err != nil {
				return nil, fmt.Errorf("merge conflict in %s; trunk and source are unchanged: %w", path, err)
			}
		}
	}
	dir, err := os.MkdirTemp("", "graft-merge-version-")
	if err != nil {
		return nil, fmt.Errorf("create merge version temp directory: %w", err)
	}
	defer os.RemoveAll(dir)
	files := []string{"base", "ours", "theirs"}
	for i, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), stages[i], 0o600); err != nil {
			return nil, fmt.Errorf("write merge version input %s: %w", name, err)
		}
	}
	merged, err := repo.Git("merge-file", "-p", "--", filepath.Join(dir, "ours"), filepath.Join(dir, "base"), filepath.Join(dir, "theirs"))
	if err != nil {
		return nil, fmt.Errorf("merge conflict in %s; trunk and source are unchanged", path)
	}
	resolved := []byte(merged)
	for _, target := range targets {
		resolved, err = target.Replace(resolved, selected)
		if err != nil {
			return nil, fmt.Errorf("merge conflict in %s; trunk and source are unchanged: %w", path, err)
		}
	}
	return resolved, nil
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
	head, err := repo.Git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(head) != rev {
		return fmt.Errorf("merge %s moved temporary HEAD from %s to %s", stage, gitx.Short(rev), gitx.Short(strings.TrimSpace(head)))
	}
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
