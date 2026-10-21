package gitx

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// WorkTreeID identifies what the work tree holds: every file git would
// commit with git add -A, by content. It does not depend on the commit: a
// rebase or a commit of the same changes keeps it. It writes nothing to the
// repository.
func (r *Repo) WorkTreeID() (string, error) {
	return r.ContentID(nil)
}

// ContentID is WorkTreeID with the paths exclude takes left out.
func (r *Repo) ContentID(exclude func(path string) bool) (string, error) {
	entries, err := r.headEntries()
	if err != nil {
		return "", err
	}
	changed, err := r.changedPaths()
	if err != nil {
		return "", err
	}
	if err := r.hashChanged(entries, changed); err != nil {
		return "", err
	}
	paths := make([]string, 0, len(entries))
	for p := range entries {
		if exclude == nil || !exclude(p) {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	h := sha256.New()
	for _, p := range paths {
		fmt.Fprintf(h, "%s\x00%s\n", p, entries[p])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// headEntries are the files of HEAD, path to "mode object".
func (r *Repo) headEntries() (map[string]string, error) {
	entries := map[string]string{}
	ok, err := r.HasHead()
	if err != nil || !ok {
		return entries, err
	}
	out, err := r.Git("ls-tree", "-r", "-z", "--full-tree", "HEAD")
	if err != nil {
		return nil, err
	}
	for rec := range strings.SplitSeq(out, "\x00") {
		meta, path, found := strings.Cut(rec, "\t")
		if !found {
			continue
		}
		f := strings.Fields(meta) // mode type object
		if len(f) != 3 {
			return nil, fmt.Errorf("git ls-tree: unexpected line %q", rec)
		}
		entries[path] = f[0] + " " + f[2]
	}
	return entries, nil
}

// changedPaths are the files that differ from HEAD (all tracked files
// before the first commit) and the untracked files that are not ignored.
func (r *Repo) changedPaths() ([]string, error) {
	ok, err := r.HasHead()
	if err != nil {
		return nil, err
	}
	tracked := []string{"diff", "HEAD", "--name-only", "-z", "--no-renames", "--no-ext-diff"}
	if !ok {
		tracked = []string{"ls-files", "-z"}
	}
	var paths []string
	for _, args := range [][]string{tracked, {"ls-files", "--others", "--exclude-standard", "-z"}} {
		out, err := r.Git(args...)
		if err != nil {
			return nil, err
		}
		for p := range strings.SplitSeq(out, "\x00") {
			if p != "" {
				paths = append(paths, p)
			}
		}
	}
	return paths, nil
}

// hashChanged puts the changed paths into entries as git would store them:
// regular files by their blob id (clean filters applied), symlinks by their
// target, removed files left out.
func (r *Repo) hashChanged(entries map[string]string, paths []string) error {
	var regular []string
	modes := map[string]string{}
	for _, p := range paths {
		full := filepath.Join(r.Root, filepath.FromSlash(p))
		st, err := os.Lstat(full)
		switch {
		case errors.Is(err, os.ErrNotExist):
			delete(entries, p)
		case err != nil:
			return err
		case st.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(full)
			if err != nil {
				return fmt.Errorf("symlink %s: %w", full, err)
			}
			entries[p] = "120000 link:" + target
		case st.IsDir():
			entries[p] = "160000 dir" // a submodule: its own tree is not looked into
		default:
			mode := "100644"
			if st.Mode()&0o111 != 0 {
				mode = "100755"
			}
			modes[p] = mode
			regular = append(regular, p)
		}
	}
	if len(regular) == 0 {
		return nil
	}
	out, err := r.GitInput(strings.NewReader(strings.Join(regular, "\n")+"\n"), nil, "hash-object", "--stdin-paths")
	if err != nil {
		return err
	}
	ids := strings.Fields(out)
	if len(ids) != len(regular) {
		return fmt.Errorf("git hash-object: %d ids for %d files", len(ids), len(regular))
	}
	for i, p := range regular {
		entries[p] = modes[p] + " " + ids[i]
	}
	return nil
}
