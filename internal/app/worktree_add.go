package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/aiseeq/graft/v2/internal/atomicfile"
	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/tasks"
)

// WorktreeAddOptions are the arguments of graft worktree add.
type WorktreeAddOptions struct {
	// Branch is checked out in the worktree, created from Base when new.
	Branch string
	// Base is where a new branch starts, by default the trunk.
	Base string
	// Trunk is the trunk ref where a new branch starts when Base is empty.
	Trunk string
}

// WorktreeAdd makes a worktree under the user's cache, out of the directory
// the repositories live in, and links there the modules that go.work and
// go.mod reach outside the repository, so the worktree builds as the
// repository does.
func (a *App) WorktreeAdd(ctx context.Context, o WorktreeAddOptions) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	trees, err := listWorktrees(repo)
	if err != nil {
		return err
	}
	main := trees[0].path
	exists, err := repo.RefExists("refs/heads/" + o.Branch)
	if err != nil {
		return err
	}
	base := o.Base
	if o.Trunk != "" && base != "" {
		return errors.New("--trunk and <base> cannot be used together")
	}
	if base == "" && !exists {
		remotes, err := repo.Remotes()
		if err != nil {
			return err
		}
		if base, err = trunkRef(repo, cfg, o.Trunk, remotes); err != nil {
			return fmt.Errorf("%w; choose the base with `graft worktree add %s <base>` or `graft worktree add --trunk <ref> %s`", err, o.Branch, o.Branch)
		}
	}
	linkRoot, dir, err := worktreeAddPaths(main, o.Branch)
	if err != nil {
		return err
	}
	from := base
	if exists {
		from = o.Branch
	}
	links, err := neighbours(repo, main, dir, from)
	if err != nil {
		return err
	}
	links, err = normalizeNeighbourLinks(links, dir)
	if err != nil {
		return err
	}
	if err := validateWorktreeNeighbourLinks(links, linkRoot); err != nil {
		return err
	}
	args := []string{"worktree", "add", "--quiet"}
	if exists {
		args = append(args, dir, o.Branch)
	} else {
		args = append(args, "--no-track", "-b", o.Branch, dir, base)
	}
	if _, err := repo.Git(args...); err != nil {
		return err
	}
	if exists {
		a.printf("worktree %s on %s", dir, o.Branch)
	} else {
		a.printf("worktree %s on %s from %s", dir, o.Branch, base)
	}
	linkSetups, err := a.linkWorktree(main, dir)
	if err != nil {
		return err
	}
	if err := linkNeighbours(a, links, linkRoot, true); err != nil {
		return err
	}
	return a.prepareWorktree(ctx, main, dir, linkSetups)
}

func worktreeAddPaths(main, branch string) (linkRoot, dir string, err error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", "", fmt.Errorf("locating the cache: %w", err)
	}
	linkRoot = graftWorktreeRoot(cache)
	if err := os.MkdirAll(linkRoot, 0o755); err != nil {
		return "", "", fmt.Errorf("creating shared worktree link directory %s: %w", linkRoot, err)
	}
	linkRoot, err = filepath.EvalSymlinks(linkRoot)
	if err != nil {
		return "", "", fmt.Errorf("resolving shared worktree link directory: %w", err)
	}
	projectCatalog := filepath.Join(linkRoot, filepath.Base(main))
	dir = filepath.Join(projectCatalog, strings.ReplaceAll(branch, "/", "-"))
	if err := validateWorktreeProjectCatalog(linkRoot, dir); err != nil {
		return "", "", err
	}
	return linkRoot, dir, nil
}

// graftWorktreeRoot is shared by worktree creation and stats recognition of
// deleted worktrees, keeping the latter aligned with the actual cache path.
func graftWorktreeRoot(cache string) string {
	return filepath.Join(cache, "worktrees")
}

// prepareWorktree copies worktree.copy from the main worktree into the one
// at dir and runs worktree.setup there, as the config at dir has them: the
// branch's own.
func (a *App) prepareWorktree(ctx context.Context, main, dir string, linkSetups []string) error {
	repo, err := gitx.Open(dir)
	if err != nil {
		return err
	}
	cfg, err := config.Load(repo.Root)
	if err != nil {
		return err
	}
	if err := a.requireGraft(cfg); err != nil {
		return err
	}
	for _, p := range cfg.Worktree.Copy {
		if err := copyLocal(main, dir, p); err != nil {
			return fmt.Errorf("worktree.copy %s: %w", p, err)
		}
		a.printf("copied %s from %s", p, main)
	}
	setups := append(linkSetups, cfg.Worktree.Setup...)
	return a.runWorktreeSetups(ctx, repo, cfg, setups, false)
}

// runWorktreeSetups runs each required setup once in the new worktree.
// Capture the tail only for glint: worktree add must keep the terminal direct.
func (a *App) runWorktreeSetups(ctx context.Context, repo *gitx.Repo, cfg *config.Config, setups []string, capture bool) error {
	seen := map[string]bool{}
	for _, t := range setups {
		if seen[t] {
			continue
		}
		seen[t] = true
		a.printf("setup: %s", t)
		var output tasks.TailCapture
		runner := a.runner(repo, cfg)
		if capture {
			runner.Stdout = io.MultiWriter(runner.Stdout, &output)
			runner.Stderr = io.MultiWriter(runner.Stderr, &output)
		}
		if err := runner.Run(ctx, t, nil); err != nil {
			if tail := output.Reason(); tail != "" {
				return fmt.Errorf("worktree.setup %s in %s: %w: %s", t, repo.Root, err, tail)
			}
			return fmt.Errorf("worktree.setup %s in %s: %w", t, repo.Root, err)
		}
	}
	return nil
}

// linkWorktree links the groups of worktree.link, as the config at dir has
// them, to the main worktree's directories, and returns the setup tasks of
// the groups it did not link, each once. A group whose key matches but whose
// setup runs for another group is not linked: the setup installs its paths
// here too.
func (a *App) linkWorktree(main, dir string) ([]string, error) {
	repo, err := gitx.Open(dir)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(repo.Root)
	if err != nil {
		return nil, err
	}
	if len(cfg.Worktree.Link) == 0 {
		return nil, nil
	}
	trackedOut, err := repo.Git("ls-files", "-z")
	if err != nil {
		return nil, err
	}
	var tracked []string
	for p := range strings.SplitSeq(trackedOut, "\x00") {
		if p != "" {
			tracked = append(tracked, filepath.ToSlash(p))
		}
	}
	var setups []string
	need := func(setup string) {
		if !slices.Contains(setups, setup) {
			setups = append(setups, setup)
		}
	}
	matches := make([]bool, len(cfg.Worktree.Link))
	for i, group := range cfg.Worktree.Link {
		if matches[i], err = groupMatches(main, dir, group, tracked); err != nil {
			return nil, err
		}
		if !matches[i] {
			need(group.Setup)
		}
	}
	for i, group := range cfg.Worktree.Link {
		switch {
		case !matches[i]:
		case slices.Contains(setups, group.Setup):
			a.printf("not linking %s: graft %s installs it with another group", strings.Join(group.Paths, ", "), group.Setup)
		default:
			if err := a.linkGroup(main, dir, group); err != nil {
				if !errors.Is(err, errWorktreeLinkUnavailable) {
					return nil, err
				}
				a.warn("%v; running graft %s", err, group.Setup)
				need(group.Setup)
			}
		}
	}
	return setups, nil
}

// errWorktreeLinkUnavailable is a directory link the system did not make,
// such as on Windows without the right to make symlinks: the group's setup
// installs instead.
var errWorktreeLinkUnavailable = errors.New("directory link unavailable")

// groupMatches tells whether the group's key files in the new worktree at
// dir have the main worktree's content and the main worktree has every path
// of the group as a directory.
func groupMatches(main, dir string, group config.WorktreeLink, tracked []string) (bool, error) {
	for _, p := range group.Paths {
		for _, t := range tracked {
			if t == p || strings.HasPrefix(t, p+"/") {
				return false, fmt.Errorf("worktree.link path %s is tracked (%s); link is for untracked directories", p, t)
			}
		}
	}
	for _, key := range group.Key {
		mainData, err := os.ReadFile(filepath.Join(main, filepath.FromSlash(key)))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("worktree.link key %s in main worktree: %w", key, err)
		}
		newData, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(key)))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("worktree.link key %s in new worktree: %w", key, err)
		}
		if !bytes.Equal(mainData, newData) {
			return false, nil
		}
	}
	for _, p := range group.Paths {
		info, err := os.Stat(filepath.Join(main, filepath.FromSlash(p)))
		if errors.Is(err, os.ErrNotExist) || (err == nil && !info.IsDir()) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("worktree.link path %s in main worktree: %w", p, err)
		}
	}
	return true, nil
}

// linkGroup links every path of group in the new worktree at dir to the
// main worktree's.
func (a *App) linkGroup(main, dir string, group config.WorktreeLink) error {
	var made []string
	for _, p := range group.Paths {
		at := filepath.Join(dir, filepath.FromSlash(p))
		to := filepath.Join(main, filepath.FromSlash(p))
		if _, err := os.Lstat(at); err == nil {
			return errors.Join(fmt.Errorf("worktree.link %s: the new worktree has it already", p), removeCreatedLinks(made))
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.Join(fmt.Errorf("worktree.link %s: %w", p, err), removeCreatedLinks(made))
		}
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			return errors.Join(fmt.Errorf("worktree.link %s parent: %w", p, err), removeCreatedLinks(made))
		}
		if err := os.Symlink(to, at); err != nil {
			if cleanupErr := removeCreatedLinks(made); cleanupErr != nil {
				return errors.Join(fmt.Errorf("creating worktree.link %s: %w", p, err), cleanupErr)
			}
			return errors.Join(
				fmt.Errorf("%w: worktree.link %s", errWorktreeLinkUnavailable, p),
				fmt.Errorf("creating directory link: %w", err),
			)
		}
		made = append(made, at)
		a.printf("linked %s from %s", p, main)
	}
	return nil
}

// removeCreatedLinks removes the links a group made before one failed.
func removeCreatedLinks(paths []string) error {
	var errs []error
	for _, p := range paths {
		if err := os.Remove(p); err != nil {
			errs = append(errs, fmt.Errorf("removing incomplete link %s: %w", p, err))
		}
	}
	return errors.Join(errs...)
}

// copyLocal copies the file or directory at rel from the main worktree into
// the one at dir, preserving permissions and symbolic links.
func copyLocal(main, dir, rel string) error {
	from, to := filepath.Join(main, filepath.FromSlash(rel)), filepath.Join(dir, filepath.FromSlash(rel))
	info, err := os.Lstat(from)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("not in the main worktree %s", main)
	}
	if err != nil {
		return fmt.Errorf("reading the main worktree's: %w", err)
	}
	if _, err := os.Lstat(to); err == nil {
		return errors.New("the checkout has it: worktree.copy is for paths git does not track")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("the new worktree's: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return fmt.Errorf("its directory: %w", err)
	}
	return copyLocalEntry(from, to, info)
}

func copyLocalEntry(from, to string, info os.FileInfo) (retErr error) {
	switch {
	case info.Mode().IsRegular():
		data, err := os.ReadFile(from)
		if err != nil {
			return fmt.Errorf("reading %s: %w", from, err)
		}
		return atomicfile.Write(to, data, info.Mode().Perm())
	case info.IsDir():
		if err := os.Mkdir(to, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", to, err)
		}
		defer func() {
			if retErr != nil {
				if err := os.RemoveAll(to); err != nil {
					retErr = errors.Join(retErr, fmt.Errorf("removing partial copy %s: %w", to, err))
				}
			}
		}()
		entries, err := os.ReadDir(from)
		if err != nil {
			return fmt.Errorf("reading %s: %w", from, err)
		}
		for _, entry := range entries {
			childFrom, childTo := filepath.Join(from, entry.Name()), filepath.Join(to, entry.Name())
			childInfo, err := os.Lstat(childFrom)
			if err != nil {
				return fmt.Errorf("reading %s: %w", childFrom, err)
			}
			if err := copyLocalEntry(childFrom, childTo, childInfo); err != nil {
				return err
			}
		}
		if err := os.Chmod(to, info.Mode().Perm()); err != nil {
			return fmt.Errorf("setting permissions on %s: %w", to, err)
		}
		return nil
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(from)
		if err != nil {
			return fmt.Errorf("reading symbolic link %s: %w", from, err)
		}
		if err := os.Symlink(target, to); err != nil {
			return fmt.Errorf("creating symbolic link %s: %w", to, err)
		}
		return nil
	default:
		return fmt.Errorf("%s is a special file", from)
	}
}

// link is a symlink to make beside a worktree.
type link struct {
	at, to, skipReason string
	exists             bool
}

type neighbourReach struct{ why, rel string }

var errNeighbourPathSymlink = errors.New("neighbour path component is a symlink")

// linkNeighbours creates the links a worktree needs to reach modules beside
// the repository, as declared by go.work and go.mod. When boundary is set,
// every link must stay inside it.
func linkNeighbours(a *App, links []link, boundary string, skipSymlinkParents bool) error {
	if boundary != "" {
		if err := checkNeighbourBounds(links, boundary); err != nil {
			return err
		}
	}
	for _, l := range links {
		if l.skipReason != "" {
			a.printf("not linking %s: %s", l.at, l.skipReason)
			continue
		}
		if l.exists {
			continue
		}
		if boundary != "" {
			skipped, err := checkNeighbourCreationParent(a, l, boundary, skipSymlinkParents)
			if err != nil {
				return err
			}
			if skipped {
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(l.at), 0o755); err != nil {
			return fmt.Errorf("the link's directory: %w", err)
		}
		if err := os.Symlink(l.to, l.at); err != nil {
			return fmt.Errorf("linking %s: %w", l.at, err)
		}
		a.printf("linked %s -> %s", l.at, l.to)
	}
	return nil
}

func checkNeighbourCreationParent(a *App, l link, root string, skipSymlink bool) (bool, error) {
	err := checkNeighbourParents(root, filepath.Dir(l.at))
	if err == nil {
		return false, nil
	}
	if !skipSymlink || !errors.Is(err, errNeighbourPathSymlink) {
		return false, err
	}
	if err := checkNeighbourParentLink(l); err != nil {
		return false, err
	}
	a.printf("not linking %s: already available through symlink parent", l.at)
	return true, nil
}

func validateWorktreeNeighbourLinks(links []link, root string) error {
	if err := checkNeighbourBounds(links, root); err != nil {
		return err
	}
	if len(links) == 0 {
		return nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("creating shared worktree link directory %s: %w", root, err)
	}
	if err := checkNeighbourParents(root, root); err != nil {
		return err
	}
	for _, l := range links {
		if l.exists || l.skipReason != "" {
			continue
		}
		if err := checkNeighbourParents(root, filepath.Dir(l.at)); err != nil {
			if errors.Is(err, errNeighbourPathSymlink) {
				if err := checkNeighbourParentLink(l); err != nil {
					return err
				}
				continue
			}
			return err
		}
	}
	return nil
}

func validateWorktreeProjectCatalog(root, worktreePath string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolving shared worktree link directory: %w", err)
	}
	worktreePath, err = filepath.Abs(worktreePath)
	if err != nil {
		return fmt.Errorf("resolving worktree path: %w", err)
	}
	rel, err := filepath.Rel(root, worktreePath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("worktree path %s is outside shared link directory %s", worktreePath, root)
	}
	current := root
	if rel == "." {
		return nil
	}
	for component := range strings.SplitSeq(rel, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("checking worktree path component %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, readErr := os.Readlink(current)
			if readErr != nil {
				return fmt.Errorf("reading neighbour link %s: %w", current, readErr)
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(current), target)
			}
			if resolved, evalErr := filepath.EvalSymlinks(current); evalErr == nil {
				target = resolved
			}
			return fmt.Errorf("worktree path %s contains symlink %s -> %s; worktree directories below cache/worktrees must be real directories; to use another disk, move cache/worktrees as a whole", worktreePath, current, target)
		}
		if !info.IsDir() {
			return fmt.Errorf("worktree path component %s is not a directory", current)
		}
	}
	return nil
}

func checkNeighbourParentLink(l link) error {
	atParent, err := filepath.EvalSymlinks(filepath.Dir(l.at))
	if err != nil {
		return fmt.Errorf("resolving neighbour parent %s: %w", filepath.Dir(l.at), err)
	}
	toParent, err := filepath.EvalSymlinks(filepath.Dir(l.to))
	if err != nil {
		return fmt.Errorf("neighbour path %s leads elsewhere than %s: resolving target parent: %w", l.at, l.to, err)
	}
	if filepath.Clean(atParent) != filepath.Clean(toParent) {
		return fmt.Errorf("neighbour path %s leads elsewhere than %s", l.at, l.to)
	}
	return nil
}

func checkNeighbourParents(root, parent string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolving neighbour link root: %w", err)
	}
	parent, err = filepath.Abs(parent)
	if err != nil {
		return fmt.Errorf("resolving neighbour parent %s: %w", parent, err)
	}
	rel, err := filepath.Rel(root, parent)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("neighbour parent %s would leave link root %s", parent, root)
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("checking neighbour link root %s: %w", root, err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return fmt.Errorf("neighbour link root %s is not a real directory", root)
	}
	current := root
	if rel == "." {
		return nil
	}
	for component := range strings.SplitSeq(rel, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("checking neighbour path component %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", errNeighbourPathSymlink, current)
		}
		if !info.IsDir() {
			return fmt.Errorf("neighbour path component %s is not a directory", current)
		}
	}
	return nil
}

func checkNeighbourBounds(links []link, boundary string) error {
	root, err := filepath.Abs(boundary)
	if err != nil {
		return fmt.Errorf("resolving neighbour link root: %w", err)
	}
	for _, l := range links {
		if l.skipReason != "" {
			continue
		}
		at, err := filepath.Abs(l.at)
		if err != nil {
			return fmt.Errorf("resolving neighbour path %s: %w", l.at, err)
		}
		rel, err := filepath.Rel(root, at)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("neighbour link %s would leave link root %s", l.at, root)
		}
	}
	return nil
}

// neighboursFor combines the external modules used at each revision, keeping
// one link per location and refusing revisions that map it to different paths.
func neighboursFor(repo *gitx.Repo, main, dir string, revs ...string) ([]link, error) {
	var links []link
	for _, rev := range revs {
		found, err := neighbours(repo, main, dir, rev)
		if err != nil {
			return nil, err
		}
		for _, candidate := range found {
			if i := slices.IndexFunc(links, func(existing link) bool { return existing.at == candidate.at }); i >= 0 {
				if links[i].to != candidate.to {
					return nil, fmt.Errorf("neighbour path %s points to both %s and %s", candidate.at, links[i].to, candidate.to)
				}
				continue
			}
			links = append(links, candidate)
		}
	}
	return normalizeNeighbourLinks(links, dir)
}

func normalizeNeighbourLinks(links []link, tree string) ([]link, error) {
	tree, err := filepath.Abs(tree)
	if err != nil {
		return nil, fmt.Errorf("resolving merge worktree path: %w", err)
	}
	for i := range links {
		if links[i].at, err = filepath.Abs(links[i].at); err != nil {
			return nil, fmt.Errorf("resolving neighbour link: %w", err)
		}
		links[i].at = filepath.Clean(links[i].at)
		if links[i].to, err = filepath.Abs(links[i].to); err != nil {
			return nil, fmt.Errorf("resolving neighbour target: %w", err)
		}
		links[i].to = filepath.Clean(links[i].to)
	}
	slices.SortFunc(links, func(a, b link) int { return strings.Compare(a.at, b.at) })
	kept := make([]link, 0, len(links))
	for _, candidate := range links {
		if pathWithin(tree, candidate.at) {
			candidate.skipReason = "the neighbour path resolves inside the worktree"
			kept = append(kept, candidate)
			continue
		}
		for _, parent := range kept {
			if parent.skipReason != "" {
				continue
			}
			if pathWithin(parent.at, candidate.at) {
				if parent.at == candidate.at && parent.to != candidate.to {
					return nil, fmt.Errorf("neighbour path %s points to both %s and %s", candidate.at, parent.to, candidate.to)
				}
				candidate.skipReason = "already available through neighbour link " + parent.at
				break
			}
		}
		kept = append(kept, candidate)
	}
	return kept, nil
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)))
}

func maxNeighbourDepth(repo *gitx.Repo, main string, revs ...string) (int, error) {
	depth := 0
	for _, rev := range revs {
		rels, err := neighbourRelPaths(repo, main, rev)
		if err != nil {
			return 0, err
		}
		for _, reach := range rels {
			if n := leadingParentComponents(reach.rel); n > depth {
				depth = n
			}
		}
	}
	return depth, nil
}

func leadingParentComponents(rel string) int {
	depth := 0
	for component := range strings.SplitSeq(filepath.Clean(filepath.FromSlash(rel)), string(filepath.Separator)) {
		if component != ".." {
			break
		}
		depth++
	}
	return depth
}

func mergeWorktreePath(tmp, main string, depth int) (string, error) {
	if depth <= 0 {
		return filepath.Join(tmp, "tree"), nil
	}
	main, err := filepath.Abs(main)
	if err != nil {
		return "", fmt.Errorf("resolving main worktree path: %w", err)
	}
	parts := strings.Split(strings.TrimPrefix(filepath.Clean(main), string(filepath.Separator)), string(filepath.Separator))
	if len(parts) == 1 && parts[0] == "." {
		parts = nil
	}
	name := filepath.Base(main)
	parents := parts
	if len(parents) > 0 {
		parents = parents[:len(parents)-1]
	}
	if depth > len(parents) {
		padding := make([]string, depth-len(parents))
		for i := range padding {
			padding[i] = "tree"
		}
		parents = append(padding, parents...)
	} else {
		parents = parents[len(parents)-depth:]
	}
	components := append(parents, name)
	return filepath.Join(append([]string{tmp}, components...)...), nil
}

// neighbours lists the links a worktree at dir needs: every module go.work
// uses, or go.mod replaces with, by a relative path that leads out of the
// repository, as it is at rev. A link already there must lead where the
// repository's path does.
func neighbours(repo *gitx.Repo, main, dir, rev string) ([]link, error) {
	rels, err := neighbourRelPaths(repo, main, rev)
	if err != nil {
		return nil, err
	}
	var links []link
	for _, reach := range rels {
		rel := reach.rel
		at := filepath.Join(dir, filepath.FromSlash(rel))
		to := filepath.Join(main, filepath.FromSlash(rel))
		ok, err := linked(at, to)
		if err != nil {
			return nil, fmt.Errorf("%s, and %s %w", reach.why, at, err)
		}
		if !containsLink(links, at) {
			links = append(links, link{at: at, to: to, exists: ok})
		}
	}
	return links, nil
}

func neighbourRelPaths(repo *gitx.Repo, main, rev string) ([]neighbourReach, error) {
	var paths []neighbourReach
	data, found, err := fileAt(repo, rev, "go.work")
	if err != nil {
		return nil, err
	}
	if found {
		work, err := modfile.ParseWork("go.work", data, nil)
		if err != nil {
			return nil, fmt.Errorf("go.work at %s: %w", rev, err)
		}
		for _, u := range work.Use {
			paths = append(paths, neighbourReach{"go.work uses " + u.Path, u.Path})
		}
		for _, r := range work.Replace {
			paths = append(paths, neighbourReach{"go.work replaces with " + r.New.Path, r.New.Path})
		}
	}
	if data, found, err = fileAt(repo, rev, "go.mod"); err != nil {
		return nil, err
	} else if found {
		mod, err := modfile.ParseLax("go.mod", data, nil)
		if err != nil {
			return nil, fmt.Errorf("go.mod at %s: %w", rev, err)
		}
		for _, r := range mod.Replace {
			paths = append(paths, neighbourReach{"go.mod replaces with " + r.New.Path, r.New.Path})
		}
	}
	var rels []neighbourReach
	for _, p := range paths {
		rel := p.rel
		if filepath.IsAbs(rel) || !strings.HasPrefix(rel, ".") {
			continue
		}
		to := filepath.Join(main, filepath.FromSlash(rel))
		if !pathWithin(main, to) {
			rels = append(rels, p)
		}
	}
	return rels, nil
}

// linked tells whether at already leads to the same place as to; nothing
// there is false, anything else an error.
func linked(at, to string) (bool, error) {
	if _, err := os.Lstat(at); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	want, err := filepath.EvalSymlinks(to)
	if err != nil {
		return false, fmt.Errorf("is there, but %s is not: %w", to, err)
	}
	got, err := filepath.EvalSymlinks(at)
	if err != nil || got != want {
		return false, fmt.Errorf("leads elsewhere than %s: fix or remove it", to)
	}
	return true, nil
}

func containsLink(links []link, at string) bool {
	for _, l := range links {
		if l.at == at {
			return true
		}
	}
	return false
}

// fileAt reads a file of a revision; found is false when it has none.
func fileAt(repo *gitx.Repo, rev, path string) ([]byte, bool, error) {
	ok, err := repo.RefExists(rev + ":" + path)
	if err != nil || !ok {
		return nil, false, err
	}
	data, err := repo.ShowFile(rev, path)
	return data, err == nil, err
}
