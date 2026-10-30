package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/aiseeq/graft/v2/internal/atomicfile"
	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
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
	cache, err := os.UserCacheDir()
	if err != nil {
		return fmt.Errorf("locating the cache: %w", err)
	}
	dir := filepath.Join(cache, "worktrees", filepath.Base(main), strings.ReplaceAll(o.Branch, "/", "-"))
	from := base
	if exists {
		from = o.Branch
	}
	links, err := addWorktree(repo, main, dir, o.Branch, from, exists)
	if err != nil {
		return err
	}
	if exists {
		a.printf("worktree %s on %s", dir, o.Branch)
	} else {
		a.printf("worktree %s on %s from %s", dir, o.Branch, base)
	}
	worktreeRepo, err := a.openRepo(dir)
	if err != nil {
		return err
	}
	linkSetups, err := a.linkWorktree(main, dir, worktreeRepo)
	if err != nil {
		return err
	}
	for _, l := range links {
		if err := os.MkdirAll(filepath.Dir(l.at), 0o755); err != nil {
			return fmt.Errorf("the link's directory: %w", err)
		}
		if err := os.Symlink(l.to, l.at); err != nil {
			return fmt.Errorf("linking %s: %w", l.at, err)
		}
		a.printf("linked %s -> %s", l.at, l.to)
	}
	return a.prepareWorktree(ctx, main, dir, linkSetups, worktreeRepo)
}

func addWorktree(repo *gitx.Repo, main, dir, branch, base string, exists bool) ([]link, error) {
	links, err := neighbours(repo, main, dir, base)
	if err != nil {
		return nil, err
	}
	args := []string{"worktree", "add", "--quiet"}
	if exists {
		args = append(args, dir, branch)
	} else {
		args = append(args, "-b", branch, dir, base)
	}
	if _, err := repo.Git(args...); err != nil {
		return nil, err
	}
	return links, nil
}

// prepareWorktree copies worktree.copy from the main worktree into the one
// at dir and runs worktree.setup there, as the config at dir has them: the
// branch's own.
func (a *App) prepareWorktree(ctx context.Context, main, dir string, linkSetups []string, repo *gitx.Repo) error {
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
	seen := map[string]bool{}
	for _, t := range setups {
		if seen[t] {
			continue
		}
		seen[t] = true
		a.printf("setup: %s", t)
		if err := a.runner(repo, cfg).Run(ctx, t, nil); err != nil {
			return fmt.Errorf("worktree.setup %s in %s: %w", t, dir, err)
		}
	}
	return nil
}

// linkWorktree links the groups of worktree.link, as the config at dir has
// them, to the main worktree's directories, and returns the setup tasks of
// the groups it did not link, each once. A group whose key matches but whose
// setup runs for another group is not linked: the setup installs its paths
// here too.
func (a *App) linkWorktree(main, dir string, repo *gitx.Repo) ([]string, error) {
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

// copyLocal copies the file at rel from the main worktree into the one at
// dir, with its permissions.
func copyLocal(main, dir, rel string) error {
	from, to := filepath.Join(main, filepath.FromSlash(rel)), filepath.Join(dir, filepath.FromSlash(rel))
	info, err := os.Stat(from)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("not in the main worktree %s", main)
	}
	if err != nil {
		return fmt.Errorf("reading the main worktree's: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a file", from)
	}
	if _, err := os.Lstat(to); err == nil {
		return errors.New("the checkout has it: worktree.copy is for files git does not track")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("the new worktree's: %w", err)
	}
	data, err := os.ReadFile(from)
	if err != nil {
		return fmt.Errorf("reading the main worktree's: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return fmt.Errorf("its directory: %w", err)
	}
	return atomicfile.Write(to, data, info.Mode().Perm())
}

// link is a symlink to make beside a worktree.
type link struct{ at, to string }

// neighbours lists the links a worktree at dir needs: every module go.work
// uses, or go.mod replaces with, by a relative path that leads out of the
// repository, as it is at rev. A link already there must lead where the
// repository's path does.
func neighbours(repo *gitx.Repo, main, dir, rev string) ([]link, error) {
	type reach struct{ why, rel string }
	var paths []reach
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
			paths = append(paths, reach{"go.work uses " + u.Path, u.Path})
		}
		for _, r := range work.Replace {
			paths = append(paths, reach{"go.work replaces with " + r.New.Path, r.New.Path})
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
			paths = append(paths, reach{"go.mod replaces with " + r.New.Path, r.New.Path})
		}
	}
	var links []link
	for _, p := range paths {
		why, rel := p.why, p.rel
		if filepath.IsAbs(rel) || !strings.HasPrefix(rel, ".") {
			continue
		}
		to := filepath.Join(main, filepath.FromSlash(rel))
		if !strings.HasPrefix(to, main+string(filepath.Separator)) && to != main {
			at := filepath.Join(dir, filepath.FromSlash(rel))
			ok, err := linked(at, to)
			if err != nil {
				return nil, fmt.Errorf("%s, and %s %w", why, at, err)
			}
			if !ok && !containsLink(links, at) {
				links = append(links, link{at: at, to: to})
			}
		}
	}
	return links, nil
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
