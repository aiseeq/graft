// Package checks inspects what is about to be committed: credentials, binary
// and oversized files, and version files that disagree with each other.
package checks

import (
	"fmt"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/gitx"
)

// StagedFile is a file whose staged content differs from HEAD.
type StagedFile struct {
	// Path is slash-separated, relative to the work tree root.
	Path string
	// Symlink marks a symbolic link; its content is the target.
	Symlink bool
	gitx.Blob
}

// maxScanBytes caps the blobs loaded for the secret scan. Anything larger is
// far over any sane size limit and is reported by the large file check.
const maxScanBytes = 32 << 20

// Staged returns the files added or modified in the index relative to HEAD
// (relative to the empty tree on an unborn branch). Removals and submodules
// have no content to inspect and are not listed.
func Staged(repo *gitx.Repo) ([]StagedFile, error) {
	// Raw diff includes the index mode and object ID as well as the path,
	// so they all describe the same index snapshot without a second ls-files.
	out, err := repo.Git("diff", "--cached", "--raw", "--no-abbrev", "-z", "--no-renames", "--diff-filter=AMT", "--no-ext-diff")
	if err != nil {
		return nil, err
	}
	type entry struct{ path, oid, mode string }
	var entries []entry
	parts := strings.Split(out, "\x00")
	for i := 0; i < len(parts)-1; i += 2 {
		if i+1 >= len(parts)-1 {
			return nil, fmt.Errorf("unexpected git diff raw output %q", out)
		}
		// ":<old mode> <new mode> <old oid> <new oid> <status>\0<path>\0"
		fields := strings.Fields(parts[i])
		if len(fields) != 5 || !strings.HasPrefix(fields[0], ":") || parts[i+1] == "" {
			return nil, fmt.Errorf("unexpected git diff raw entry %q", parts[i])
		}
		if fields[1] == "160000" { // submodule
			continue
		}
		entries = append(entries, entry{parts[i+1], fields[3], fields[1]})
	}
	// ls-files used to determine the order, independently of diff.orderFile.
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.path, b.path) })
	oids := make([]string, len(entries))
	for i, e := range entries {
		oids[i] = e.oid
	}
	blobs, err := repo.ReadBlobs(oids, maxScanBytes)
	if err != nil {
		return nil, fmt.Errorf("reading staged files: %w", err)
	}
	files := make([]StagedFile, len(blobs))
	for i, b := range blobs {
		files[i] = StagedFile{Path: entries[i].path, Symlink: entries[i].mode == "120000", Blob: b}
	}
	return files, nil
}
