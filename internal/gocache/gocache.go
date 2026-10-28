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

// Result counts the entries a trim takes, with apply or without, and keeps.
type Result struct {
	Removed, Kept           int
	RemovedBytes, KeptBytes int64
}

// Trim removes, with apply, the entries of the Go build cache in dir not
// used for longer than older before now, and counts them either way. An
// entry is a file <hash>-a or <hash>-d, or a directory <hash>-d that holds
// an executable go run or go tool built, in one of the 256 subdirectories
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
			return r, fmt.Errorf("read the cache: %w", err)
		}
		for _, e := range entries {
			if err := r.entry(sub, e, cutoff, apply); err != nil {
				return r, err
			}
		}
	}
	return r, nil
}

// entry counts one directory entry of a cache subdirectory, removing it with
// apply when it is a cache entry unused since cutoff.
func (r *Result) entry(sub string, e fs.DirEntry, cutoff time.Time, apply bool) error {
	name, path := e.Name(), filepath.Join(sub, e.Name())
	executable := e.IsDir() && strings.HasSuffix(name, "-d")
	if !strings.HasSuffix(name, "-a") && !strings.HasSuffix(name, "-d") || !e.Type().IsRegular() && !executable {
		return nil
	}
	// go marks an executable used on its directory.
	info, err := e.Info()
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the cache entry: %w", err)
	}
	size := info.Size()
	if executable {
		if size, err = dirSize(path); errors.Is(err, fs.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
	}
	if !info.ModTime().Before(cutoff) {
		r.Kept++
		r.KeptBytes += size
		return nil
	}
	if apply {
		remove := os.Remove
		if executable {
			remove = os.RemoveAll
		}
		err := remove(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	r.Removed++
	r.RemovedBytes += size
	return nil
}

// dirSize sums the sizes of the regular files under dir.
func dirSize(dir string) (int64, error) {
	var n int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		n += info.Size()
		return nil
	})
	return n, err
}
