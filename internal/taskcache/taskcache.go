// Package taskcache keeps the tasks that passed, by the key of everything
// their result depends on, so a task need not run again on the same inputs.
// Only successes are kept: a failure always runs again.
package taskcache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// keep is how long a pass is kept.
const keep = 30 * 24 * time.Hour

// Entry is a pass: where and when the task passed and what that run cost,
// which a skip saves.
type Entry struct {
	Project string    `json:"project"`
	Task    string    `json:"task"`
	Time    time.Time `json:"time"`
	WallS   float64   `json:"wall_s"`
	UserS   float64   `json:"user_s"`
	SysS    float64   `json:"sys_s"`
}

// Dir is where the passes are kept, under the user's cache directory.
func Dir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locating the cache: %w", err)
	}
	return filepath.Join(dir, "graft", "passed"), nil
}

// Lookup finds the pass of key.
func Lookup(dir, key string) (Entry, bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, key+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		return Entry{}, false, fmt.Errorf("%s: %w", key, err)
	}
	if time.Since(e.Time) > keep {
		return Entry{}, false, nil
	}
	return e, true, nil
}

// Store keeps the pass of key and removes the passes older than keep.
func Store(dir, key string, e Entry) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, key+".*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	if err := errors.Join(werr, tmp.Close()); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, key+".json")); err != nil {
		return err
	}
	return prune(dir)
}

func prune(dir string) error {
	files, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, f := range files {
		info, err := f.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if time.Since(info.ModTime()) > keep {
			if err := os.Remove(filepath.Join(dir, f.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
