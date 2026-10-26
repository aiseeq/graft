package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/lintcache"
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
	opts  WorktreeCleanOptions
	trunk string
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

// WorktreeClean removes the worktrees and branches whose work is in the trunk
// by content, and lists the others: in work, unmerged, or to check.
func (a *App) WorktreeClean(_ context.Context, o WorktreeCleanOptions) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	c := &cleaner{app: a, repo: repo, opts: o, now: time.Now(), counts: map[string]int{}}
	if c.remotes, err = repo.Remotes(); err != nil {
		return err
	}
	for _, r := range c.remotes {
		if _, err := repo.Git("fetch", "--quiet", "--prune", r); err != nil {
			return fmt.Errorf("git fetch %s failed, what the trunk has is unknown: %w", r, err)
		}
	}
	if c.trunk, err = trunkRef(repo, cfg, o.Trunk, c.remotes); err != nil {
		return err
	}
	for _, f := range cfg.VersionFiles() {
		c.exclude = append(c.exclude, ":(exclude)"+f)
	}
	if c.cwds, err = processPaths(); err != nil {
		return err
	}
	if err := c.readSubjects(); err != nil {
		return err
	}
	trees, err := listWorktrees(repo)
	if err != nil {
		return err
	}
	held := []string{c.trunkBranch()}
	// The first entry is the main worktree: never removed.
	for i, w := range trees {
		if w.branch != "" {
			held = append(held, w.branch)
		}
		if i == 0 {
			continue
		}
		if err := c.worktree(w); err != nil {
			return err
		}
	}
	if err := c.branches(held); err != nil {
		return err
	}
	if o.Apply {
		// The lint caches of work trees gone, here or removed otherwise.
		gone, err := lintcache.Prune()
		for _, root := range gone {
			a.printf("%-9s the lint cache of %s: its work tree is gone", "removed", root)
		}
		if err != nil {
			return err
		}
	}
	c.summary()
	return nil
}

// trunkRef is the trunk, which merged work must be in and work is synced
// onto: --trunk, or the HEAD branch of the deploy remote, or of the only
// remote, or without a remote a local branch (localTrunk).
func trunkRef(repo *gitx.Repo, cfg *config.Config, flag string, remotes []string) (string, error) {
	if flag != "" {
		if ok, err := repo.RevExists(flag); err != nil || !ok {
			return "", fmt.Errorf("--trunk %s: no such commit: %w", flag, err)
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
		return "", fmt.Errorf("remotes %s and no deploy.remote: pass --trunk <remote>/<branch>", strings.Join(remotes, ", "))
	}
	out, err := repo.Git("symbolic-ref", "--quiet", "--short", "refs/remotes/"+remote+"/HEAD")
	if err != nil {
		return "", fmt.Errorf("%s/HEAD is not set, the trunk is unknown: git remote set-head %s --auto, or pass --trunk: %w", remote, remote, err)
	}
	return strings.TrimSpace(out), nil
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
	c.app.printf("%-9s %s: %s", kind, what, why)
}

func (c *cleaner) worktree(w worktree) error {
	what := w.path + " (" + w.branch + ")"
	if w.branch == "" {
		what = w.path + " (detached)"
	}
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
	if !c.opts.Apply {
		c.line("would remove", what, why)
		return nil
	}
	args := append(append([]string{"worktree", "remove"}, flags...), w.path)
	if _, err := c.repo.Git(args...); err != nil {
		return err
	}
	if w.branch != "" {
		if err := c.deleteBranch(w.branch); err != nil {
			return err
		}
	}
	c.line("removed", what, why)
	return nil
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
