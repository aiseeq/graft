package gitx

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// WorkTreeID identifies what the work tree holds: HEAD, the changes to
// tracked files and the untracked files that are not ignored. Two runs with
// the same ID ran on the same code. It writes nothing to the repository.
func (r *Repo) WorkTreeID() (string, error) {
	base, err := r.diffBase()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	io.WriteString(h, base)
	diff, err := r.Git("-c", "core.quotePath=false", "diff", base, "--binary", "--no-renames", "--no-ext-diff", "--no-textconv", "--no-color")
	if err != nil {
		return "", err
	}
	io.WriteString(h, diff)
	untracked, err := r.Git("ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	for name := range strings.SplitSeq(untracked, "\x00") {
		if name == "" {
			continue
		}
		if err := hashFile(h, filepath.Join(r.Root, filepath.FromSlash(name)), name); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// diffBase is HEAD's commit, or the empty tree before the first commit.
func (r *Repo) diffBase() (string, error) {
	ok, err := r.HasHead()
	if err != nil {
		return "", err
	}
	var out string
	if ok {
		out, err = r.Git("rev-parse", "--verify", "HEAD")
	} else {
		out, err = r.GitInput(strings.NewReader(""), nil, "hash-object", "-t", "tree", "--stdin")
	}
	return strings.TrimSpace(out), err
}

// hashFile adds the file's name and content to h; a symlink adds its target.
func hashFile(h io.Writer, path, name string) error {
	io.WriteString(h, "\x00"+name+"\x00")
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // removed since git listed it
	}
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		_, err = io.WriteString(h, "link:"+target)
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(h, f)
	return err
}
