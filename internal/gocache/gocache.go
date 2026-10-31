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

// BatchEntries bounds the metadata work in one cache trim batch.
const BatchEntries = 1000

// BatchBytes bounds the estimated bytes removed in one cache trim batch.
const BatchBytes int64 = 256 << 20

// Result counts the entries a trim takes, with apply or without, and keeps.
type Result struct {
	Removed, Kept, Remaining                int
	RemovedBytes, KeptBytes, RemainingBytes int64
}

// Options controls a trim. BeforeBatch is called before each batch of
// removals; false stops the trim without an error and leaves the rest for a
// later run. A nil callback removes entries without waiting.
type Options struct {
	Apply       bool
	BeforeBatch func() (bool, error)
}

// TrimWithOptions trims a cache with optional admission before each batch.
func TrimWithOptions(dir string, older time.Duration, now time.Time, opts Options) (Result, error) {
	var r Result
	data, err := os.ReadFile(filepath.Join(dir, "README"))
	if err != nil {
		return r, fmt.Errorf("%s is not a Go build cache: %w", dir, err)
	}
	if !strings.HasPrefix(string(data), readme) {
		return r, fmt.Errorf("%s is not a Go build cache: its README does not start with %q", dir, readme)
	}
	cutoff := now.Add(-older)
	if !opts.Apply {
		return trimDryRun(dir, cutoff, r)
	}
	return trimApply(dir, cutoff, r, opts)
}

func trimDryRun(dir string, cutoff time.Time, r Result) (Result, error) {
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
			size, tracked, old, err := entryInfo(sub, e, cutoff)
			if err != nil {
				return r, err
			}
			if tracked {
				r.count(size, old)
			}
		}
	}
	return r, nil
}

type trimBatch struct {
	dir           string
	before        func() (bool, error)
	result        *Result
	entries       int
	bytes         int64
	started, stop bool
}

func trimApply(dir string, cutoff time.Time, r Result, opts Options) (Result, error) {
	batch := trimBatch{dir: dir, before: opts.BeforeBatch, result: &r}
	for i := range 256 {
		if err := batch.trimSubdir(filepath.Join(dir, fmt.Sprintf("%02x", i)), cutoff); err != nil {
			return r, err
		}
	}
	if err := batch.finish(); err != nil {
		return r, err
	}
	return r, nil
}

func (b *trimBatch) trimSubdir(sub string, cutoff time.Time) error {
	entries, err := os.ReadDir(sub)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the cache: %w", err)
	}
	for _, e := range entries {
		if err := b.trimEntry(sub, e, cutoff); err != nil {
			return err
		}
	}
	return nil
}

func (b *trimBatch) trimEntry(sub string, e fs.DirEntry, cutoff time.Time) error {
	size, tracked, old, err := entryInfo(sub, e, cutoff)
	if err != nil || !tracked {
		return err
	}
	if !old {
		b.result.count(size, false)
		return nil
	}
	if b.entries > 0 && (b.entries >= BatchEntries || b.bytes+size > BatchBytes) {
		if err := b.finish(); err != nil {
			return err
		}
	}
	if b.stop {
		b.result.Remaining++
		b.result.RemainingBytes += size
		return nil
	}
	if err := b.start(); err != nil {
		return err
	}
	if b.stop {
		b.result.Remaining++
		b.result.RemainingBytes += size
		return nil
	}
	removed, err := b.result.remove(sub, e, size)
	if removed {
		b.entries++
		b.bytes += size
	}
	return err
}

func (b *trimBatch) start() error {
	if b.started || b.before == nil {
		b.started = true
		return nil
	}
	proceed, err := b.before()
	if err != nil {
		return err
	}
	if !proceed {
		b.stop = true
		return nil
	}
	b.started = true
	return nil
}

func (b *trimBatch) finish() error {
	if b.entries == 0 {
		return nil
	}
	if err := syncCacheFS(b.dir); err != nil {
		return fmt.Errorf("sync the cache after trimming: %w", err)
	}
	b.entries, b.bytes, b.started = 0, 0, false
	return nil
}

// entryInfo classifies a cache entry and gives its estimated size and age.
func entryInfo(sub string, e fs.DirEntry, cutoff time.Time) (int64, bool, bool, error) {
	name, path := e.Name(), filepath.Join(sub, e.Name())
	executable := e.IsDir() && strings.HasSuffix(name, "-d")
	if !strings.HasSuffix(name, "-a") && !strings.HasSuffix(name, "-d") || !e.Type().IsRegular() && !executable {
		return 0, false, false, nil
	}
	info, err := e.Info()
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, false, nil
	}
	if err != nil {
		return 0, false, false, fmt.Errorf("read the cache entry: %w", err)
	}
	size := info.Size()
	if executable {
		size, err = dirSize(path)
		if errors.Is(err, fs.ErrNotExist) {
			return 0, false, false, nil
		}
		if err != nil {
			return 0, false, false, err
		}
	}
	return size, true, info.ModTime().Before(cutoff), nil
}

func (r *Result) count(size int64, old bool) {
	if old {
		r.Removed++
		r.RemovedBytes += size
		return
	}
	r.Kept++
	r.KeptBytes += size
}

func (r *Result) remove(sub string, e fs.DirEntry, size int64) (bool, error) {
	path := filepath.Join(sub, e.Name())
	remove := os.Remove
	if e.IsDir() && strings.HasSuffix(e.Name(), "-d") {
		remove = os.RemoveAll
	}
	err := remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	r.Removed++
	r.RemovedBytes += size
	return true, nil
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
