// Package checks inspects what is about to be committed: credentials, binary
// and oversized files, and version files that disagree with each other.
package checks

import (
	"fmt"
	"strings"

	"github.com/aiseeq/graft/internal/gitx"
)

// StagedFile is a file whose staged content differs from HEAD.
type StagedFile struct {
	// Path is slash-separated, relative to the work tree root.
	Path string
	gitx.Blob
}

// maxScanBytes caps the blobs loaded for the secret scan. Anything larger is
// far over any sane size limit and is reported by the large file check.
const maxScanBytes = 32 << 20

// Staged returns the files added or modified in the index relative to HEAD
// (relative to the empty tree on an unborn branch). Removals and submodules
// have no content to inspect and are not listed.
func Staged(repo *gitx.Repo) ([]StagedFile, error) {
	out, err := repo.Git("diff", "--cached", "--name-only", "-z", "--no-renames", "--diff-filter=AMT", "--no-ext-diff")
	if err != nil {
		return nil, err
	}
	changed := map[string]bool{}
	for p := range strings.SplitSeq(out, "\x00") {
		if p != "" {
			changed[p] = true
		}
	}
	if len(changed) == 0 {
		return []StagedFile{}, nil
	}
	index, err := repo.Git("ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	var paths, oids []string
	for entry := range strings.SplitSeq(index, "\x00") {
		// "<mode> <oid> <stage>\t<path>"
		meta, p, ok := strings.Cut(entry, "\t")
		if !ok || !changed[p] {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 {
			return nil, fmt.Errorf("unexpected ls-files entry %q", entry)
		}
		if fields[0] == "160000" { // submodule
			continue
		}
		paths = append(paths, p)
		oids = append(oids, fields[1])
	}
	blobs, err := repo.ReadBlobs(oids, maxScanBytes)
	if err != nil {
		return nil, fmt.Errorf("reading staged files: %w", err)
	}
	files := make([]StagedFile, len(blobs))
	for i, b := range blobs {
		files[i] = StagedFile{Path: paths[i], Blob: b}
	}
	return files, nil
}
