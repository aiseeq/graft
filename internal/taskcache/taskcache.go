// Package taskcache keeps the tasks that passed, by the key of everything
// their result depends on, so a task need not run again on the same inputs.
// Only successes are kept: a failure always runs again.
package taskcache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/fsx"
)

// keep is how long a pass is kept.
const keep = 30 * 24 * time.Hour

// PartNames are the independently hashed components of a task cache key.
var PartNames = []string{"tree", "definition", "inputs", "graft"}

// ChangedParts names the parts whose hashes differ, including a missing part.
func ChangedParts(previous, current map[string]string) []string {
	var changed []string
	for _, part := range PartNames {
		before, beforeOK := previous[part]
		after, afterOK := current[part]
		if !beforeOK || !afterOK || before != after {
			changed = append(changed, part)
		}
	}
	return changed
}

// Entry is a pass: where and when the task passed and what that run cost,
// which a skip saves.
type Entry struct {
	Project string    `json:"project"`
	Task    string    `json:"task"`
	Time    time.Time `json:"time"`
	WallS   float64   `json:"wall_s"`
	UserS   float64   `json:"user_s"`
	SysS    float64   `json:"sys_s"`
	// Parts are hashes of the components of the key, used to explain a miss.
	Parts map[string]string `json:"parts,omitempty"`
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
		return Entry{}, false, fmt.Errorf("a cached pass: %w", err)
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

// Previous finds the last pass of task on project without scanning the pass
// cache. Older entries without Parts cannot explain a miss and are ignored.
func Previous(dir, project, task string) (Entry, bool, error) {
	return previous(dir, project, task, os.ReadFile)
}

func previous(dir, project, task string, readFile func(string) ([]byte, error)) (Entry, bool, error) {
	data, err := readFile(filepath.Join(dir, lastName(project, task)))
	if errors.Is(err, os.ErrNotExist) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("reading last task cache entry: %w", err)
	}
	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		return Entry{}, false, fmt.Errorf("last task cache entry: %w", err)
	}
	if e.Project != project || e.Task != task || len(e.Parts) == 0 || time.Since(e.Time) > keep {
		return Entry{}, false, nil
	}
	return e, true, nil
}

func lastName(project, task string) string {
	h := sha256.Sum256([]byte(project + "\x00" + task))
	return "last-" + hex.EncodeToString(h[:]) + ".json"
}

// Store keeps the pass of key and removes the passes older than keep.
func Store(dir, key string, e Entry) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("the task cache: %w", err)
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("the pass of %s: %w", key, err)
	}
	tmp, err := os.CreateTemp(dir, key+".*.tmp")
	if err != nil {
		return fmt.Errorf("the pass of %s: %w", key, err)
	}
	_, werr := tmp.Write(data)
	if err := errors.Join(werr, tmp.Close()); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, key+".json")); err != nil {
		return fmt.Errorf("the pass of %s: %w", key, err)
	}
	if len(e.Parts) > 0 {
		if err := storeLast(dir, lastName(e.Project, e.Task), data); err != nil {
			return fmt.Errorf("the last pass of %s: %w", e.Task, err)
		}
	}
	return prune(dir)
}

func storeLast(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, strings.TrimSuffix(name, ".json")+"-*.tmp")
	if err != nil {
		return fmt.Errorf("creating the last-pass file: %w", err)
	}
	_, writeErr := tmp.Write(data)
	if err := errors.Join(writeErr, tmp.Close()); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	return nil
}

func prune(dir string) error {
	files, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("prune the task cache: %w", err)
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
			if err := fsx.Remove(filepath.Join(dir, f.Name())); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
