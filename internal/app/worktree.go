package app

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/lintcache"
	"github.com/aiseeq/graft/v2/internal/lock"
)

// WorktreeCleanOptions are the arguments of graft worktree clean.
type WorktreeCleanOptions struct {
	// Apply removes; without it the clean only reports.
	Apply bool
	// Trunk is the ref work must be in, by default the HEAD branch of the
	// deploy remote or of the only remote.
	Trunk string
	// MinAge leaves alone a worktree or branch used more recently.
	MinAge time.Duration
	In     []string
}

// worktree is one entry of git worktree list.
type worktree struct {
	path, head, branch string
	locked, prunable   bool
	// lockReason is what git worktree lock --reason gave, if anything.
	lockReason string
}

// cleaner holds what one graft worktree clean works with.
type cleaner struct {
	app   *App
	repo  *gitx.Repo
	cfg   *config.Config
	opts  WorktreeCleanOptions
	trunk string
	// mainPath is the main worktree, once a copy has been compared with it.
	mainPath string
	// exclude are the pathspecs of the version files: every graft commit
	// bumps them, so they differ in work that is merged.
	exclude []string
	// subjects are the trunk's commit subjects.
	subjects map[string]bool
	// cwds maps a directory or an open file to the processes working in
	// it or holding it.
	cwds    map[string][]string
	remotes []string
	now     time.Time
	counts  map[string]int
}

type worktreeCleanResult struct {
	counts  map[string]int
	skipped bool
}

// WorktreeClean removes the worktrees and branches whose work is in the trunk
// by content, and lists the others: in work, unmerged, or to check.
func (a *App) WorktreeClean(o WorktreeCleanOptions) error {
	if len(o.In) != 0 {
		return a.worktreeCleanIn(o)
	}
	_, err := a.worktreeCleanSingle(o)
	return err
}

func (a *App) worktreeCleanSingle(o WorktreeCleanOptions) (result worktreeCleanResult, retErr error) {
	repo, cfg, err := a.open()
	if err != nil {
		return result, err
	}
	lk, err := lock.TryAcquire(filepath.Join(repo.CommonDir, "graft-worktree-clean.lock"), "graft worktree clean")
	if err != nil {
		return result, err
	}
	if lk == nil {
		a.printf("skipped   %s: another graft worktree clean runs in it", repo.Root)
		return worktreeCleanResult{skipped: true}, nil
	}
	defer func() {
		if releaseErr := lk.Release(); releaseErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("releasing the worktree clean lock: %w", releaseErr))
		}
	}()
	return a.worktreeCleanLocked(repo, cfg, o)
}

func (a *App) worktreeCleanLocked(repo *gitx.Repo, cfg *config.Config, o WorktreeCleanOptions) (worktreeCleanResult, error) {
	c := &cleaner{app: a, repo: repo, cfg: cfg, opts: o, now: time.Now(), counts: map[string]int{}}
	result := worktreeCleanResult{counts: c.counts}
	var err error
	if c.remotes, err = repo.Remotes(); err != nil {
		return result, err
	}
	for _, r := range c.remotes {
		if _, err := repo.Git("fetch", "--quiet", "--prune", r); err != nil {
			return result, fmt.Errorf("git fetch %s failed, what the trunk has is unknown: %w", r, err)
		}
	}
	if c.trunk, err = trunkRef(repo, cfg, o.Trunk, c.remotes); err != nil {
		return result, err
	}
	if c.cfg, err = c.readTrunkConfig(); err != nil {
		return result, err
	}
	for _, f := range c.cfg.VersionFiles() {
		c.exclude = append(c.exclude, ":(exclude)"+f)
	}
	if c.cwds, err = processPaths(); err != nil {
		return result, err
	}
	if err := c.readSubjects(); err != nil {
		return result, err
	}
	trees, err := listWorktrees(repo)
	if err != nil {
		return result, err
	}
	held, err := c.cleanWorktrees(trees)
	if err != nil {
		return result, err
	}
	if err := c.branches(held); err != nil {
		return result, err
	}
	if o.Apply {
		gone, err := lintcache.Prune()
		for _, root := range gone {
			a.printf("%-9s the lint cache of %s: its work tree is gone", "removed", root)
		}
		if err != nil {
			return result, err
		}
	}
	c.summary()
	return result, nil
}

func (c *cleaner) cleanWorktrees(trees []worktree) ([]string, error) {
	held := []string{c.trunkBranch()}
	for i, w := range trees {
		if w.branch != "" {
			held = append(held, w.branch)
		}
		// A rebase detaches HEAD: its branch is still the worktree's.
		if !w.prunable {
			rebasing, err := rebaseBranch(c.repo, w.path)
			if err != nil {
				c.line("check", worktreeWhat(w), "not removed: "+strings.Join(strings.Fields(err.Error()), " "))
				continue
			}
			if rebasing != "" {
				held = append(held, rebasing)
			}
		}
		if i == 0 {
			continue
		}
		if err := c.worktree(w); err != nil {
			reason := strings.Join(strings.Fields(err.Error()), " ")
			if strings.HasPrefix(reason, "git add -A:") {
				reason = "worktree snapshot failed: " + strings.TrimPrefix(reason, "git add -A: ")
			} else if !strings.HasPrefix(reason, "not ") {
				reason = "not removed: " + reason
			}
			c.line("check", worktreeWhat(w), reason)
		}
	}
	return held, nil
}

func worktreeWhat(w worktree) string {
	if w.branch == "" {
		return w.path + " (detached)"
	}
	return w.path + " (" + w.branch + ")"
}

// readTrunkConfig is the config of the trunk's commit: what decides the fate of
// other worktrees' data is not an edit or a branch of the tree running this.
// A trunk without the file has no rules.
func (c *cleaner) readTrunkConfig() (*config.Config, error) {
	entries, err := c.repo.Git("ls-tree", c.trunk, "--", config.FileName)
	if err != nil {
		return nil, fmt.Errorf("checking %s in %s: %w", config.FileName, c.trunk, err)
	}
	if strings.TrimSpace(entries) == "" {
		return &config.Config{}, nil
	}
	data, err := c.repo.ShowFile(c.trunk, config.FileName)
	if err != nil {
		return nil, fmt.Errorf("reading %s in %s: %w", config.FileName, c.trunk, err)
	}
	trunkCfg, err := config.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s in %s: %w", config.FileName, c.trunk, err)
	}
	return trunkCfg, nil
}

func (a *App) worktreeCleanIn(o WorktreeCleanOptions) error {
	roots, err := worktreeCleanRoots(o.In)
	if err != nil {
		return err
	}
	if len(roots) == 0 {
		return fmt.Errorf("--in: no repositories found")
	}
	total := 0
	counts := map[string]int{}
	var failed []string
	for _, root := range roots {
		a.printf("worktree clean: %s", root)
		result, err := a.cleanInRepository(root, o)
		total++
		for kind, count := range result.counts {
			counts[kind] += count
		}
		if result.skipped {
			continue
		}
		if err != nil {
			reason := strings.Join(strings.Fields(err.Error()), " ")
			a.printf("failed    %s: %s", root, reason)
			failed = append(failed, root+": "+reason)
		}
	}
	if o.Apply {
		a.printf("repositories %d, removed %d, in work %d, unmerged %d, to check %d, failed %d",
			total, counts["removed"]+counts["pruned"], counts["in work"], counts["unmerged"], counts["check"], len(failed))
	} else {
		a.printf("repositories %d, would remove %d, in work %d, unmerged %d, to check %d, failed %d",
			total, counts["would remove"]+counts["would prune"], counts["in work"], counts["unmerged"], counts["check"], len(failed))
	}
	for _, item := range failed {
		a.printf("failed    %s", item)
	}
	if len(failed) > 0 {
		return &ExitCodeError{Code: 1, Err: fmt.Errorf("%s failed", pluralRepositories(len(failed)))}
	}
	return nil
}

func worktreeCleanRoots(parents []string) ([]string, error) {
	var roots []string
	for _, parent := range parents {
		entries, err := os.ReadDir(parent)
		if err != nil {
			return nil, fmt.Errorf("--in %s: %w", parent, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			root := filepath.Join(parent, entry.Name())
			if _, err := os.Stat(filepath.Join(root, ".graft.yaml")); errors.Is(err, os.ErrNotExist) {
				continue
			} else if err != nil {
				return nil, fmt.Errorf("%s/.graft.yaml: %w", root, err)
			}
			st, err := os.Stat(filepath.Join(root, ".git"))
			if err == nil && st.IsDir() {
				roots = append(roots, root)
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%s/.git: %w", root, err)
			}
		}
	}
	slices.Sort(roots)
	roots = slices.Compact(roots)
	return roots, nil
}

func (a *App) cleanInRepository(root string, o WorktreeCleanOptions) (worktreeCleanResult, error) {
	child := *a
	child.Dir, child.Stdout = root, a.Stdout
	return child.worktreeCleanSingle(o)
}

// trunkRef is the trunk, which merged work must be in and work is synced
// onto: --trunk, or the HEAD branch of the deploy remote, or of the only
// remote, or without a remote a local branch (localTrunk).
func trunkRef(repo *gitx.Repo, cfg *config.Config, flag string, remotes []string) (string, error) {
	if flag != "" {
		ok, err := repo.RevExists(flag)
		if err != nil {
			return "", fmt.Errorf("--trunk %s: %w", flag, err)
		}
		if !ok {
			return "", fmt.Errorf("--trunk %s: no such commit", flag)
		}
		return flag, nil
	}
	var remote string
	switch {
	case cfg.Deploy != nil && cfg.Deploy.Remote != "":
		remote = cfg.Deploy.Remote
	case len(remotes) == 1:
		remote = remotes[0]
	case len(remotes) == 0:
		return localTrunk(repo)
	default:
		return upstreamTrunk(repo, remotes)
	}
	out, err := repo.Git("symbolic-ref", "--quiet", "--short", "refs/remotes/"+remote+"/HEAD")
	if err != nil {
		return "", fmt.Errorf("%s/HEAD is not set, the trunk is unknown: git remote set-head %s --auto, or pass --trunk: %w", remote, remote, err)
	}
	return strings.TrimSpace(out), nil
}

// upstreamTrunk is the trunk of a repository with several remotes and no
// deploy.remote: the HEAD branch <remote>/<b> of the remote that the local
// branch b tracks. No such remote, or two of them, is ambiguous.
func upstreamTrunk(repo *gitx.Repo, remotes []string) (string, error) {
	var heads, tracked []string
	for _, remote := range remotes {
		head, err := repo.Git("for-each-ref", "--format=%(symref:short)", "refs/remotes/"+remote+"/HEAD")
		if err != nil {
			return "", err
		}
		head = strings.TrimSpace(head)
		branch, ok := strings.CutPrefix(head, remote+"/")
		if !ok {
			continue
		}
		heads = append(heads, head)
		upstream, err := repo.Git("for-each-ref", "--format=%(upstream:short)", "refs/heads/"+branch)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(upstream) == head {
			tracked = append(tracked, head)
		}
	}
	switch len(tracked) {
	case 1:
		return tracked[0], nil
	case 0:
		return "", fmt.Errorf("remotes %s and no deploy.remote, and no local branch tracks a remote's HEAD branch (%s): set deploy.remote, the branch's upstream, or pass --trunk <remote>/<branch>",
			strings.Join(remotes, ", "), orNone(heads))
	default:
		return "", fmt.Errorf("remotes %s and no deploy.remote, and local branches track the HEAD branches of several (%s): set deploy.remote or pass --trunk <remote>/<branch>",
			strings.Join(remotes, ", "), strings.Join(tracked, ", "))
	}
}

func orNone(list []string) string {
	if len(list) == 0 {
		return "none set"
	}
	return strings.Join(list, ", ")
}

// localTrunk is the trunk of a repository without a remote: main or master
// when only one of them is there, else the branch of the main worktree when
// it is one of them or neither is there. Both there and the main worktree on
// another branch, or on none, is ambiguous.
func localTrunk(repo *gitx.Repo) (string, error) {
	var have []string
	for _, b := range []string{"main", "master"} {
		ok, err := repo.RefExists("refs/heads/" + b)
		if err != nil {
			return "", err
		}
		if ok {
			have = append(have, b)
		}
	}
	if len(have) == 1 {
		return have[0], nil
	}
	trees, err := listWorktrees(repo)
	if err != nil {
		return "", err
	}
	var primary string
	if len(trees) > 0 {
		primary = trees[0].branch
	}
	if primary != "" && (len(have) == 0 || slices.Contains(have, primary)) {
		return primary, nil
	}
	why := "the main worktree is on no branch"
	if primary != "" {
		why = "the main worktree is on " + primary
	}
	if len(have) == 2 {
		why = "both main and master are there and " + why
	}
	return "", fmt.Errorf("no remote, and the trunk is ambiguous: %s: pass --trunk <branch>", why)
}

// trunkBranch is the trunk's branch name, its remote left out.
func (c *cleaner) trunkBranch() string {
	for _, r := range c.remotes {
		if b, ok := strings.CutPrefix(c.trunk, r+"/"); ok {
			return b
		}
	}
	return c.trunk
}

func (c *cleaner) readSubjects() error {
	out, err := c.repo.Git("log", "--no-merges", "--format=%s", c.trunk)
	if err != nil {
		return err
	}
	c.subjects = map[string]bool{}
	for s := range strings.SplitSeq(out, "\n") {
		c.subjects[s] = true
	}
	return nil
}

// listWorktrees reads git worktree list --porcelain.
func listWorktrees(repo *gitx.Repo) ([]worktree, error) {
	out, err := repo.Git("worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	var trees []worktree
	var w *worktree
	for field := range strings.SplitSeq(out, "\x00") {
		key, value, _ := strings.Cut(field, " ")
		switch key {
		case "worktree":
			trees = append(trees, worktree{path: filepath.FromSlash(value)})
			w = &trees[len(trees)-1]
		case "HEAD":
			w.head = value
		case "branch":
			w.branch = strings.TrimPrefix(value, "refs/heads/")
		case "locked":
			w.locked, w.lockReason = true, value
		case "prunable":
			w.prunable = true
		}
	}
	return trees, nil
}

// processPaths maps each directory a process works in, and each file it has
// open, to the processes, from /proc: a process may work on a worktree from
// elsewhere (git -C, go -C, an editor). It errs off Linux.
func processPaths() (map[string][]string, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("graft worktree clean reads the processes' working directories from /proc, which %s lacks", runtime.GOOS)
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("list the processes: %w", err)
	}
	paths := map[string][]string{}
	for _, e := range entries {
		if strings.ContainsFunc(e.Name(), func(r rune) bool { return r < '0' || r > '9' }) {
			continue
		}
		proc := filepath.Join("/proc", e.Name())
		dir, err := os.Readlink(filepath.Join(proc, "cwd"))
		if gone(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("a process's working directory: %w", err)
		}
		comm, err := os.ReadFile(filepath.Join(proc, "comm"))
		if gone(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("a process's name: %w", err)
		}
		who := strings.TrimSpace(string(comm)) + ", pid " + e.Name()
		paths[dir] = append(paths[dir], who)
		fds, err := os.ReadDir(filepath.Join(proc, "fd"))
		if gone(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("a process's open files: %w", err)
		}
		for _, fd := range fds {
			file, err := os.Readlink(filepath.Join(proc, "fd", fd.Name()))
			switch {
			case gone(err):
				// Closed while being read.
			case err != nil:
				return nil, fmt.Errorf("a process's open file: %w", err)
			case filepath.IsAbs(file):
				// Sockets, pipes and the like are not paths.
				paths[file] = append(paths[file], who+" (a file open)")
			}
		}
	}
	return paths, nil
}

// gone tells a process that ended while being read, or another user's,
// whose working directory is not this user's worktree.
func gone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission)
}

// busy names the processes working in the directory or below it, or holding
// a file there open.
func (c *cleaner) busy(dir string) []string {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	var procs []string
	for cwd, ps := range c.cwds {
		if cwd == dir || strings.HasPrefix(cwd, dir+string(filepath.Separator)) {
			procs = append(procs, ps...)
		}
	}
	slices.Sort(procs)
	return slices.Compact(procs)
}

func (c *cleaner) line(kind, what, why string) {
	c.counts[kind]++
	c.app.printf("%-9s %s: %s", kind, escapeUnprintable(what), escapeUnprintable(why))
}

func (c *cleaner) worktree(w worktree) error {
	what := worktreeWhat(w)
	switch {
	case w.prunable:
		if c.opts.Apply {
			if _, err := c.repo.Git("worktree", "prune"); err != nil {
				return err
			}
			c.line("pruned", what, "its directory is gone")
			return nil
		}
		c.line("would prune", what, "its directory is gone")
		return nil
	case w.locked:
		why := "locked"
		if w.lockReason != "" {
			why += ": " + w.lockReason
		}
		c.line("in work", what, why+" (git worktree unlock to let it go)")
		return nil
	}
	if operation, err := gitOperation(c.repo, w.path); err != nil {
		return err
	} else if operation != "" {
		c.line("in work", what, "a "+operation+" in progress")
		return nil
	}
	if procs := c.busy(w.path); len(procs) > 0 {
		c.line("in work", what, "a process works in it ("+strings.Join(procs, "; ")+")")
		return nil
	}
	used, err := c.lastUse(w.path)
	if err != nil {
		return err
	}
	if age := c.now.Sub(used); age < c.opts.MinAge {
		c.line("in work", what, "used less than "+c.opts.MinAge.String()+" ago")
		return nil
	}
	tree, err := gitx.Open(w.path)
	if err != nil {
		return err
	}
	hidden, err := hiddenChanges(w.path)
	if err != nil {
		return err
	}
	if len(hidden) > 0 {
		c.line("check", what, "git status does not look at "+strings.Join(hidden, ", ")+": git update-index --no-assume-unchanged --no-skip-worktree to see a change")
		return nil
	}
	changed, err := tree.ChangedFiles()
	if err != nil {
		return err
	}
	switch {
	case len(changed) > 0 && w.head == "":
		c.line("in work", what, plural(len(changed), "uncommitted change")+", no commit yet")
		return nil
	case len(changed) > 0:
		return c.dirtyWorktree(w, what, tree, changed)
	}
	v, err := c.verdict(w.head)
	if err != nil {
		return err
	}
	if !v.merged {
		c.line(v.kind, what, v.why)
		return nil
	}
	return c.removeWorktree(w, what, "clean, in "+c.trunk)
}

// worktreeGitDir is the git dir of the worktree at dir.
func worktreeGitDir(repo *gitx.Repo, dir string) (string, error) {
	out, err := repo.Git("-C", dir, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// rebaseBranch is the branch a rebase in the worktree at dir works on, ""
// without a rebase or when it started on a detached HEAD.
func rebaseBranch(repo *gitx.Repo, dir string) (string, error) {
	gitDir, err := worktreeGitDir(repo, dir)
	if err != nil {
		return "", err
	}
	for _, state := range []string{"rebase-merge", "rebase-apply"} {
		data, err := os.ReadFile(filepath.Join(gitDir, state, "head-name"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("reading the rebase state of %s: %w", dir, err)
		}
		branch, _ := strings.CutPrefix(strings.TrimSpace(string(data)), "refs/heads/")
		if branch == "detached HEAD" {
			return "", nil
		}
		return branch, nil
	}
	return "", nil
}

func pluralRepositories(n int) string {
	if n == 1 {
		return "1 repository"
	}
	return fmt.Sprintf("%d repositories", n)
}

func gitOperation(repo *gitx.Repo, dir string) (string, error) {
	gitDir, err := worktreeGitDir(repo, dir)
	if err != nil {
		return "", err
	}
	for _, item := range []struct{ marker, name string }{
		{"rebase-merge", "rebase"}, {"rebase-apply", "rebase"},
		{"MERGE_HEAD", "merge"}, {"CHERRY_PICK_HEAD", "cherry-pick"}, {"REVERT_HEAD", "revert"},
	} {
		if _, err := os.Stat(filepath.Join(gitDir, item.marker)); err == nil {
			return item.name, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return "", nil
}

// dirtyWorktree handles a worktree with uncommitted changes. It goes like a
// clean one when its work tree, changes and untracked files included, is in
// the trunk by content: work merged by other commits, the copy left behind.
// Otherwise it stays in work, with the files the trunk lacks.
func (c *cleaner) dirtyWorktree(w worktree, what string, tree *gitx.Repo, changed []string) error {
	n := plural(len(changed), "uncommitted change")
	// HEAD moving is no measure of an edit in place.
	for _, f := range changed {
		st, err := os.Stat(filepath.Join(w.path, f))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("an uncommitted change: %w", err)
		}
		if c.now.Sub(st.ModTime()) < c.opts.MinAge {
			c.line("in work", what, n+", changed less than "+c.opts.MinAge.String()+" ago")
			return nil
		}
	}
	snap, err := tree.WorkTreeTree()
	if err != nil {
		return err
	}
	commit, err := c.repo.GitEnv(snapshotIdentity, "commit-tree", snap, "-p", w.head, "-m", "graft worktree clean: uncommitted changes")
	if err != nil {
		return err
	}
	v, err := c.verdict(strings.TrimSpace(commit))
	if err != nil {
		return err
	}
	if !v.merged {
		files, err := c.lacks(v)
		if err != nil {
			return err
		}
		c.line("in work", what, n+", not in "+c.trunk+": "+listSome(files, 5))
		return nil
	}
	why := n + ", all in " + c.trunk
	if !c.opts.Apply {
		if decision := c.checkIgnoredData(w, why); decision.kind != "" {
			c.line(decision.kind, what, decision.why)
			return nil
		}
		c.line("would remove", what, why)
		return nil
	}
	// The work tree as it was checked, or another look.
	again, err := tree.WorkTreeTree()
	if err != nil {
		return err
	}
	if again != snap {
		c.line("in work", what, n+", changed while checked")
		return nil
	}
	return c.removeWorktree(w, what, why, "--force")
}

// snapshotIdentity lets commit-tree write the commit of uncommitted changes
// without a configured user: the commit is only merged in memory and dropped.
var snapshotIdentity = []string{
	"GIT_AUTHOR_NAME=graft", "GIT_AUTHOR_EMAIL=graft@localhost",
	"GIT_COMMITTER_NAME=graft", "GIT_COMMITTER_EMAIL=graft@localhost",
}

// lacks lists the files where merging the work into the trunk gives other
// content than the trunk's, or conflicts, the version files aside.
func (c *cleaner) lacks(v verdict) ([]string, error) {
	out, err := c.repo.Git(append([]string{"diff", "--name-only", "-z", "--no-renames", c.trunk, v.tree, "--", "."}, c.exclude...)...)
	if err != nil {
		return nil, err
	}
	files := slices.DeleteFunc(strings.Split(out, "\x00"), func(f string) bool { return f == "" })
	files = append(files, v.conflicts...)
	slices.Sort(files)
	return slices.Compact(files), nil
}

// listSome joins the first max names and counts the rest.
func listSome(names []string, max int) string {
	if len(names) <= max {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:max], ", ") + fmt.Sprintf(" and %d more", len(names)-max)
}

// removeWorktree removes a worktree whose work is in the trunk, and its
// branch.
func (c *cleaner) removeWorktree(w worktree, what, why string, flags ...string) error {
	if decision := c.checkIgnoredData(w, why); decision.kind != "" {
		c.line(decision.kind, what, decision.why)
		return nil
	}
	if !c.opts.Apply {
		c.line("would remove", what, why)
		return nil
	}
	args := append(append([]string{"worktree", "remove"}, flags...), w.path)
	if _, err := c.repo.Git(args...); err != nil {
		if _, statErr := os.Stat(w.path); statErr == nil {
			if trees, listErr := listWorktrees(c.repo); listErr == nil {
				registered := slices.ContainsFunc(trees, func(tree worktree) bool { return filepath.Clean(tree.path) == filepath.Clean(w.path) })
				if !registered {
					return fmt.Errorf("not fully removed, and git no longer lists it: %s: %w", w.path, err)
				}
			}
		}
		return err
	}
	if w.branch != "" {
		if err := c.deleteBranch(w.branch); err != nil {
			c.line("check", what, "removed, its branch stays: "+strings.Join(strings.Fields(err.Error()), " "))
			return nil
		}
	}
	c.line("removed", what, why)
	return nil
}

type ignoredDecision struct {
	kind string
	why  string
}

func (c *cleaner) checkIgnoredData(w worktree, why string) ignoredDecision {
	submodules, err := c.heldSubmodules(w.path)
	if err != nil {
		return ignoredDecision{kind: "check", why: "submodules not read: " + err.Error()}
	}
	if len(submodules) > 0 {
		return ignoredDecision{kind: "check", why: "holds submodule " + strings.Join(submodules, ", ") + ": look inside and remove it by hand"}
	}
	ignored, err := c.ignoredData(w.path)
	if err != nil {
		return ignoredDecision{kind: "check", why: "ignored files not read: " + err.Error()}
	}
	if ignored.count > 0 {
		if c.now.Sub(ignored.newest) < c.opts.MinAge {
			return ignoredDecision{kind: "in work", why: "ignored data changed less than " + c.opts.MinAge.String() + " ago"}
		}
		return ignoredDecision{kind: "check", why: why + ", but ignored data would go with it: " + ignored.describe() + "; move it, or name it in worktree.disposable"}
	}
	return ignoredDecision{}
}

// hiddenChanges names the tracked files git status does not look at, marked
// assume-unchanged or skip-worktree, that are on disk: what a sparse checkout
// leaves out is marked too, and is not.
func hiddenChanges(root string) ([]string, error) {
	out, err := gitx.InOther(root, nil, "ls-files", "-v", "-z")
	if err != nil {
		return nil, err
	}
	var found []string
	for item := range strings.SplitSeq(out, "\x00") {
		if len(item) < 3 || (item[0] != 'S' && (item[0] < 'a' || item[0] > 'z')) {
			continue
		}
		name := item[2:]
		_, statErr := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return nil, fmt.Errorf("stat %s: %w", displayName(name), statErr)
		}
		found = append(found, displayName(name))
	}
	return found, nil
}

// heldSubmodules names the submodules whose folder holds anything: git status
// and ls-files do not look inside one, initialized or not.
func (c *cleaner) heldSubmodules(root string) ([]string, error) {
	out, err := gitx.InOther(root, nil, "ls-files", "-s", "-z")
	if err != nil {
		return nil, err
	}
	var found []string
	for item := range strings.SplitSeq(out, "\x00") {
		meta, name, ok := strings.Cut(item, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) == 0 || fields[0] != "160000" {
			continue
		}
		dir := filepath.Join(root, filepath.FromSlash(name))
		st, statErr := os.Lstat(dir)
		if errors.Is(statErr, os.ErrNotExist) || (statErr == nil && !st.IsDir()) {
			continue
		}
		if statErr != nil {
			return nil, fmt.Errorf("stat submodule folder: %w", statErr)
		}
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			return nil, fmt.Errorf("read submodule folder: %w", readErr)
		}
		if len(entries) > 0 {
			found = append(found, displayName(name))
		}
	}
	slices.Sort(found)
	if len(found) > 0 {
		return found, nil
	}
	// A deinitialized submodule leaves an empty folder, and its commits in the
	// worktree's git dir: git worktree remove --force takes them along.
	gitDir, err := worktreeGitDir(c.repo, root)
	if err != nil {
		return nil, err
	}
	modules := filepath.Join(gitDir, "modules")
	entries, err := os.ReadDir(modules)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(entries) > 0 {
		found = append(found, "data in "+displayName(modules))
	}
	return found, nil
}

type ignoredRoot struct {
	name      string
	count     int
	size      int64
	directory bool
	// cut is true when the walk stopped at the limit inside this root.
	cut     bool
	omitted bool
}

// namedIgnoredRoots is how many roots a line names; the rest are only counted.
const namedIgnoredRoots = 5

type ignoredSummary struct {
	roots []ignoredRoot
	// more counts the roots after the limit that are not kept in roots.
	more   int
	count  int
	newest time.Time
}

var disposableDirectories = map[string]bool{
	"node_modules": true, "__pycache__": true, ".pytest_cache": true,
	".mypy_cache": true, ".ruff_cache": true, ".next": true,
	".nuxt": true, ".turbo": true, ".parcel-cache": true,
	".svelte-kit": true, ".gradle": true,
}

func (c *cleaner) ignoredData(root string) (ignoredSummary, error) {
	out, err := gitx.InOtherStrict(root, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	if err != nil {
		return ignoredSummary{}, err
	}
	var summary ignoredSummary
	limited := false
	// The last root counted: git lists what is inside a folder right after it.
	last := ""
	var linkPaths []string
	for _, group := range c.cfg.Worktree.Link {
		linkPaths = append(linkPaths, group.Paths...)
	}
	templated := slices.Concat(c.cfg.Worktree.Copy, linkPaths)
	for listed := range strings.SplitSeq(out, "\x00") {
		isDirectory := strings.HasSuffix(listed, "/")
		listed = strings.TrimSuffix(listed, "/")
		if listed == "" {
			continue
		}
		if last != "" && strings.HasPrefix(listed, last+"/") {
			continue
		}
		if limited {
			disposable, dispErr := c.cfg.Worktree.DisposablePath(listed)
			if dispErr != nil {
				return ignoredSummary{}, dispErr
			}
			if disposable || disposableDirectoryPath(listed) || coveredByWorktreePath(listed, templated) {
				continue
			}
			last = listed
			if len(summary.roots) < namedIgnoredRoots {
				summary.roots = append(summary.roots, ignoredRoot{name: listed, directory: isDirectory, omitted: true})
			} else {
				summary.more++
			}
			continue
		}
		rootItem, err := c.walkIgnoredRoot(root, listed, isDirectory, linkPaths, &summary)
		if errors.Is(err, errIgnoredLimit) {
			rootItem.cut = true
			summary.roots = append(summary.roots, rootItem)
			limited = true
			last = listed
			continue
		}
		if err != nil {
			return ignoredSummary{}, err
		}
		if rootItem.count > 0 {
			summary.roots = append(summary.roots, rootItem)
			last = listed
		}
	}
	return summary, nil
}

func (c *cleaner) walkIgnoredRoot(root, listed string, directory bool, linkPaths []string, summary *ignoredSummary) (ignoredRoot, error) {
	rootItem := ignoredRoot{name: listed, directory: directory}
	rootPath := filepath.Join(root, filepath.FromSlash(listed))
	err := filepath.WalkDir(rootPath, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		disposable, err := c.cfg.Worktree.DisposablePath(rel)
		if err != nil {
			return err
		}
		if disposableDirectoryPath(rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if disposable || coveredByWorktreePath(rel, linkPaths) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if coveredByWorktreePath(rel, c.cfg.Worktree.Copy) {
			same, err := c.sameAsCopySource(name, rel)
			if err != nil {
				return err
			}
			if same {
				return nil
			}
		}
		st, err := entry.Info()
		if err != nil {
			return err
		}
		rootItem.count++
		rootItem.size += st.Size()
		summary.count++
		if st.ModTime().After(summary.newest) {
			summary.newest = st.ModTime()
		}
		if summary.count >= 1000 {
			return errIgnoredLimit
		}
		return nil
	})
	return rootItem, err
}

var errIgnoredLimit = errors.New("ignored data limit reached")

// sameAsCopySource says whether a file under worktree.copy still is what the
// main worktree holds at that path: what a copy has since gained is the tree's
// own data, and a file the source lacks is the only copy.
func (c *cleaner) sameAsCopySource(file, rel string) (bool, error) {
	if c.mainPath == "" {
		main, err := mainWorktreePath(c.repo)
		if err != nil {
			return false, err
		}
		c.mainPath = main
	}
	source := filepath.Join(c.mainPath, filepath.FromSlash(rel))
	st, err := os.Stat(source)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat the source of %s: %w", displayName(rel), err)
	}
	if !st.Mode().IsRegular() {
		return false, nil
	}
	same, err := sameContent(file, source)
	if err != nil {
		return false, fmt.Errorf("comparing %s with its source: %w", displayName(rel), err)
	}
	return same, nil
}

// sameContent compares two regular files without holding them in memory.
func sameContent(a, b string) (same bool, err error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, fmt.Errorf("opening the copy: %w", err)
	}
	defer func() { err = errors.Join(err, fa.Close()) }()
	fb, err := os.Open(b)
	if err != nil {
		return false, fmt.Errorf("opening the source: %w", err)
	}
	defer func() { err = errors.Join(err, fb.Close()) }()
	bufA, bufB := make([]byte, 64*1024), make([]byte, 64*1024)
	for {
		na, errA := io.ReadFull(fa, bufA)
		nb, errB := io.ReadFull(fb, bufB)
		if !bytes.Equal(bufA[:na], bufB[:nb]) {
			return false, nil
		}
		endA := errors.Is(errA, io.EOF) || errors.Is(errA, io.ErrUnexpectedEOF)
		endB := errors.Is(errB, io.EOF) || errors.Is(errB, io.ErrUnexpectedEOF)
		switch {
		case errA != nil && !endA:
			return false, errA
		case errB != nil && !endB:
			return false, errB
		case endA || endB:
			return endA && endB, nil
		}
	}
}

// mainWorktreePath is the main worktree of repo's repository: git lists it
// first, a bare repository's folder included.
func mainWorktreePath(repo *gitx.Repo) (string, error) {
	trees, err := listWorktrees(repo)
	if err != nil {
		return "", err
	}
	if len(trees) == 0 {
		return "", fmt.Errorf("%s: git lists no worktrees", repo.Root)
	}
	return trees[0].path, nil
}

func disposableDirectoryPath(name string) bool {
	for component := range strings.SplitSeq(name, "/") {
		if disposableDirectories[component] {
			return true
		}
	}
	return false
}

func coveredByWorktreePath(name string, paths []string) bool {
	for _, p := range paths {
		p = strings.TrimSuffix(p, "/")
		if name == p || strings.HasPrefix(name, p+"/") {
			return true
		}
	}
	return false
}

func (s ignoredSummary) describe() string {
	parts := make([]string, 0, min(len(s.roots), namedIgnoredRoots)+1)
	for _, root := range s.roots[:min(len(s.roots), namedIgnoredRoots)] {
		name := root.name
		if root.directory {
			name += "/"
		}
		if root.omitted {
			parts = append(parts, displayName(name))
			continue
		}
		size := humanBytes(root.size)
		if root.cut {
			size = strings.Replace(size, " ", "+ ", 1)
		}
		parts = append(parts, fmt.Sprintf("%s (%s, %s)", displayName(name), root.countText(), size))
	}
	if rest := s.more + max(len(s.roots)-namedIgnoredRoots, 0); rest > 0 {
		parts = append(parts, fmt.Sprintf("and %d more", rest))
	}
	return strings.Join(parts, ", ")
}

// unprintable tells a string that must not reach a report line as it is: a
// line feed forges a line, a bidi mark reorders it, and so on.
func unprintable(s string) bool {
	return !utf8.ValidString(s) || strings.ContainsFunc(s, func(r rune) bool { return !unicode.IsPrint(r) })
}

func displayName(name string) string {
	if unprintable(name) {
		return strconv.Quote(name)
	}
	return name
}

// escapeUnprintable keeps a report line one line, whatever an error text holds.
func escapeUnprintable(s string) string {
	if !unprintable(s) {
		return s
	}
	quoted := strconv.Quote(s)
	return quoted[1 : len(quoted)-1]
}

func (r ignoredRoot) countText() string {
	if r.cut {
		return fmt.Sprintf("%d+ files", r.count)
	}
	return plural(r.count, "file")
}

func humanBytes(size int64) string {
	if size >= 1<<20 {
		return fmt.Sprintf("%.1f MiB", float64(size)/(1<<20))
	}
	if size >= 1<<10 {
		return fmt.Sprintf("%.1f KiB", float64(size)/(1<<10))
	}
	return fmt.Sprintf("%d B", size)
}

// lastUse is when the worktree's HEAD last moved: its reflog, or HEAD itself
// where reflogs are off. The index is no measure, git status rewrites it.
func (c *cleaner) lastUse(dir string) (time.Time, error) {
	out, err := c.repo.Git("-C", dir, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return time.Time{}, err
	}
	gitDir := strings.TrimSpace(out)
	var last time.Time
	for _, f := range []string{filepath.Join(gitDir, "logs", "HEAD"), filepath.Join(gitDir, "HEAD")} {
		st, err := os.Stat(f)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return time.Time{}, fmt.Errorf("the worktree's last use: %w", err)
		}
		if st.ModTime().After(last) {
			last = st.ModTime()
		}
	}
	if last.IsZero() {
		return time.Time{}, fmt.Errorf("%s: no HEAD in %s", dir, gitDir)
	}
	return last, nil
}

// verdict is whether work is in the trunk.
type verdict struct {
	merged bool
	// kind and why describe work that is not: unmerged, or check when its
	// commits are all in the trunk by subject.
	kind, why string
	// tree is the merge of the work into the trunk, conflicts the files
	// it conflicts in.
	tree      string
	conflicts []string
}

// verdict tells whether merging rev into the trunk gives the trunk's tree,
// the version files aside. Ancestry and patch ids are no measure: a rewritten
// commit has another hash, and a commit with the same subject may carry other
// content.
func (c *cleaner) verdict(rev string) (verdict, error) {
	out, err := c.repo.Git("merge-tree", "--write-tree", "--name-only", "--no-messages", c.trunk, rev)
	// Exit status 1 is a merge with conflicts; the tree is still written.
	if err != nil && gitx.ExitCode(err) != 1 {
		return verdict{}, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	tree := lines[0]
	var conflicts []string
	for _, f := range lines[1:] {
		if f != "" && !slices.Contains(c.exclude, ":(exclude)"+f) {
			conflicts = append(conflicts, f)
		}
	}
	stat, err := c.repo.Git(append([]string{"diff", "--numstat", c.trunk, tree, "--", "."}, c.exclude...)...)
	if err != nil {
		return verdict{}, err
	}
	added, deleted := 0, 0
	for l := range strings.SplitSeq(strings.TrimSpace(stat), "\n") {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		// A binary file counts as one line each way.
		a, errA := strconv.Atoi(f[0])
		d, errD := strconv.Atoi(f[1])
		if errA != nil || errD != nil {
			a, d = 1, 1
		}
		added, deleted = added+a, deleted+d
	}
	if len(conflicts) == 0 && added == 0 && deleted == 0 {
		return verdict{merged: true, tree: tree}, nil
	}
	why := fmt.Sprintf("adds +%d -%d lines to %s", added, deleted, c.trunk)
	if len(conflicts) > 0 {
		why = fmt.Sprintf("conflicts with %s in %s", c.trunk, strings.Join(conflicts, ", "))
	}
	subjects, err := c.repo.Git("log", "--no-merges", "--format=%s", c.trunk+".."+rev)
	if err != nil {
		return verdict{}, err
	}
	own := strings.Split(strings.TrimSpace(subjects), "\n")
	if strings.TrimSpace(subjects) != "" && !slices.ContainsFunc(own, func(s string) bool { return !c.subjects[s] }) {
		return verdict{kind: "check", why: "its commits are all in " + c.trunk + " by subject, but it " + why + ": looks merged, check", tree: tree, conflicts: conflicts}, nil
	}
	return verdict{kind: "unmerged", why: why, tree: tree, conflicts: conflicts}, nil
}

// branches handles the local branches no worktree holds.
func (c *cleaner) branches(held []string) error {
	out, err := c.repo.Git("for-each-ref", "--format=%(refname:short)%00%(objectname)", "refs/heads")
	if err != nil {
		return err
	}
	for l := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		name, sha, ok := strings.Cut(l, "\x00")
		if !ok || slices.Contains(held, name) {
			continue
		}
		what := "branch " + name
		used, err := c.branchUse(name)
		if err != nil {
			return err
		}
		if c.now.Sub(used) < c.opts.MinAge {
			c.line("in work", what, "used less than "+c.opts.MinAge.String()+" ago")
			continue
		}
		v, err := c.verdict(sha)
		if err != nil {
			return err
		}
		switch {
		case !v.merged:
			c.line(v.kind, what, v.why)
		case !c.opts.Apply:
			c.line("would remove", what, "in "+c.trunk)
		default:
			if err := c.deleteBranch(name); err != nil {
				return err
			}
			c.line("removed", what, "in "+c.trunk)
		}
	}
	return nil
}

// branchUse is when the branch last moved: its reflog, or its last commit.
func (c *cleaner) branchUse(name string) (time.Time, error) {
	if st, err := os.Stat(filepath.Join(c.repo.CommonDir, "logs", "refs", "heads", filepath.FromSlash(name))); err == nil {
		return st.ModTime(), nil
	}
	out, err := c.repo.Git("log", "-1", "--format=%ct", "refs/heads/"+name)
	if err != nil {
		return time.Time{}, err
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("branch %s: commit time %q: %w", name, out, err)
	}
	return time.Unix(sec, 0), nil
}

// deleteBranch deletes the local branch and the branch of the same name on
// every remote whose copy is in the trunk too. The remote deletes the branch
// only while it is still at the commit checked: commits pushed there after
// the fetch keep it.
func (c *cleaner) deleteBranch(name string) error {
	for _, r := range c.remotes {
		ref := "refs/remotes/" + r + "/" + name
		ok, err := c.repo.RevExists(ref)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		sha, err := c.repo.Git("rev-parse", ref)
		if err != nil {
			return err
		}
		sha = strings.TrimSpace(sha)
		v, err := c.verdict(sha)
		if err != nil {
			return err
		}
		if !v.merged {
			c.line(v.kind, r+"/"+name, v.why+"; kept on "+r)
			continue
		}
		out, err := c.repo.Git("push", "--porcelain", r, "--force-with-lease=refs/heads/"+name+":"+sha, "--delete", name)
		if err != nil {
			if strings.Contains(out, "\t[rejected] (stale info)") {
				c.line("kept", r+"/"+name, "new commits on "+r+" since the check at "+sha[:12])
				continue
			}
			// Gone from the remote since the fetch: git tells only in
			// stderr, in the user's language, so the remote is asked.
			if _, lsErr := c.repo.Git("ls-remote", "--exit-code", r, "refs/heads/"+name); gitx.ExitCode(lsErr) == 2 {
				c.line("already gone", r+"/"+name, "deleted on "+r+" since the fetch")
				continue
			}
			return err
		}
	}
	_, err := c.repo.Git("branch", "-D", name)
	return err
}

func (c *cleaner) summary() {
	if c.opts.Apply {
		c.app.printf("removed %d, in work %d, unmerged %d, to check %d", c.counts["removed"]+c.counts["pruned"], c.counts["in work"], c.counts["unmerged"], c.counts["check"])
		return
	}
	c.app.printf("dry run: graft worktree clean --apply removes %d; in work %d, unmerged %d, to check %d", c.counts["would remove"]+c.counts["would prune"], c.counts["in work"], c.counts["unmerged"], c.counts["check"])
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
