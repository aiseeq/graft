// Package gocache trims the Go build cache to an age shorter than the go
// command's own: go removes entries unused for five days, a limit it does
// not let the user change.
package gocache

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// readme starts the README the go command writes into its cache: a
// directory without it is not trimmed.
const readme = "This directory holds cached build artifacts from the Go build system."

// MarkInterval is how seldom the go command refreshes the mtime of an entry
// it uses: an entry used within this long may look that much older.
const MarkInterval = time.Hour

// Result counts the entries a trim removed, or would remove, and kept.
type Result struct {
	Removed, Kept           int
	RemovedBytes, KeptBytes int64
}

// Trim removes, with apply, the entries of the Go build cache in dir not
// used for longer than older before now, and counts them either way. An
// entry is a file <hash>-a or <hash>-d in one of the 256 subdirectories
// 00…ff, as the go command's own trim takes them; the fuzz cache and the
// cache's own files stay. An entry gone meanwhile, removed by a go command
// trimming at the same time, is not counted.
func Trim(dir string, older time.Duration, now time.Time, apply bool) (Result, error) {
	var r Result
	data, err := os.ReadFile(filepath.Join(dir, "README"))
	if err != nil {
		return r, fmt.Errorf("%s is not a Go build cache: %w", dir, err)
	}
	if !strings.HasPrefix(string(data), readme) {
		return r, fmt.Errorf("%s is not a Go build cache: its README does not start with %q", dir, readme)
	}
	cutoff := now.Add(-older)
	for i := range 256 {
		sub := filepath.Join(dir, fmt.Sprintf("%02x", i))
		entries, err := os.ReadDir(sub)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return r, err
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, "-a") && !strings.HasSuffix(name, "-d") || !e.Type().IsRegular() {
				continue
			}
			info, err := e.Info()
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return r, err
			}
			if !info.ModTime().Before(cutoff) {
				r.Kept++
				r.KeptBytes += info.Size()
				continue
			}
			if apply {
				err := os.Remove(filepath.Join(sub, name))
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err != nil {
					return r, err
				}
			}
			r.Removed++
			r.RemovedBytes += info.Size()
		}
	}
	return r, nil
}
