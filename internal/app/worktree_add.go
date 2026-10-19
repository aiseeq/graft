package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/aiseeq/graft/v2/internal/gitx"
)

// WorktreeAddOptions are the arguments of graft worktree add.
type WorktreeAddOptions struct {
	// Branch is checked out in the worktree, created from Base when new.
	Branch string
	// Base is where a new branch starts, by default the trunk.
	Base string
}

// WorktreeAdd makes a worktree under the user's cache, out of the directory
// the repositories live in, and links there the modules that go.work and
// go.mod reach outside the repository, so the worktree builds as the
// repository does.
func (a *App) WorktreeAdd(_ context.Context, o WorktreeAddOptions) error {
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
	if base == "" && !exists {
		remotes, err := repo.Remotes()
		if err != nil {
			return err
		}
		if base, err = trunkRef(repo, cfg, "", remotes); err != nil {
			return err
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
	links, err := neighbours(repo, main, dir, from)
	if err != nil {
		return err
	}
	args := []string{"worktree", "add", "--quiet"}
	if exists {
		args = append(args, dir, o.Branch)
	} else {
		args = append(args, "-b", o.Branch, dir, base)
	}
	if _, err := repo.Git(args...); err != nil {
		return err
	}
	if exists {
		a.printf("worktree %s on %s", dir, o.Branch)
	} else {
		a.printf("worktree %s on %s from %s", dir, o.Branch, base)
	}
	for _, l := range links {
		if err := os.MkdirAll(filepath.Dir(l.at), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(l.to, l.at); err != nil {
			return fmt.Errorf("linking %s: %w", l.at, err)
		}
		a.printf("linked %s -> %s", l.at, l.to)
	}
	return nil
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
			return nil, err
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
			return nil, err
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
