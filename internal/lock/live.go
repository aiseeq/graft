package lock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/aiseeq/graft/v2/internal/fsx"
	"github.com/gofrs/flock"
)

// liveAttempts bounds how often createLive tries a new name when a reader
// took the file for a dead one before its owner locked it.
const liveAttempts = 10

// beforeLock runs between writing a live file and locking it: a test takes
// it for dead there.
var beforeLock = func(string) {}

// createLive writes data to a new file named by pattern (os.CreateTemp, in
// dir) that stays live while the returned lock is held: its owner holds it
// shared, and gone reports a file no process holds. A lock, unlike a pid,
// means the same to a process in another PID namespace, a sandbox sharing
// the directory, and dies with its owner.
func createLive(dir, pattern string, data []byte) (*flock.Flock, string, error) {
	for range liveAttempts {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, "", fmt.Errorf("creating a live file: %w", err)
		}
		name := f.Name()
		_, writeErr := f.Write(data)
		if err := errors.Join(writeErr, f.Close()); err != nil {
			return nil, "", errors.Join(err, fsx.Remove(name))
		}
		beforeLock(name)
		fl := flock.New(name, flock.SetFlag(os.O_RDONLY))
		ok, err := fl.TryRLock()
		if errors.Is(err, os.ErrNotExist) {
			// A reader took it for dead before it was opened again.
			continue
		}
		if err != nil {
			return nil, "", errors.Join(fmt.Errorf("locking %s: %w", name, err), fsx.Remove(name))
		}
		if !ok {
			// A reader holds it, about to remove it as dead.
			continue
		}
		held, err := fl.Stat()
		if err != nil {
			return nil, "", errors.Join(err, fl.Unlock(), fsx.Remove(name))
		}
		if now, err := os.Stat(name); err == nil && os.SameFile(held, now) {
			return fl, name, nil
		}
		// A reader took it for dead before the lock was taken.
		if err := fl.Unlock(); err != nil {
			return nil, "", fmt.Errorf("unlocking %s: %w", name, err)
		}
	}
	return nil, "", fmt.Errorf("could not keep a live file %s in %s: readers took it for dead %d times", pattern, dir, liveAttempts)
}

// Reserve creates a live file with data and keeps it until release is called
// or the owner process exits.
func Reserve(dir, pattern string, data []byte) (release func() error, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating reserve directory: %w", err)
	}
	fl, name, err := createLive(dir, pattern, data)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() { releaseErr = releaseLive(fl, name) })
		return releaseErr
	}, nil
}

// LiveFile is the path and contents of one live file.
type LiveFile struct {
	Path string
	Data []byte
}

// LiveData reads live files matching glob and removes dead ones.
// A live file that cannot be read fails the scan with its path.
func LiveData(dir, glob string) ([]LiveFile, error) {
	pattern := filepath.Join(dir, glob)
	names, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("listing live files %s: %w", pattern, err)
	}
	var rows []LiveFile
	for _, name := range names {
		dead, err := gone(name)
		if err != nil {
			return nil, err
		}
		if dead {
			continue
		}
		data, err := os.ReadFile(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading live file %s: %w", name, err)
		}
		if !json.Valid(data) {
			return nil, fmt.Errorf("decoding live file %s: invalid JSON", name)
		}
		rows = append(rows, LiveFile{Path: name, Data: data})
	}
	return rows, nil
}

// releaseLive gives a live file up and removes it.
func releaseLive(fl *flock.Flock, name string) error {
	return errors.Join(fl.Unlock(), removeDead(name))
}

// gone removes the live file at name when no process holds it and reports
// whether it did, or found it removed already.
func gone(name string) (bool, error) {
	fl := flock.New(name, flock.SetFlag(os.O_RDONLY))
	ok, err := fl.TryLock()
	if errors.Is(err, os.ErrNotExist) {
		return true, fl.Close()
	}
	if err != nil {
		return false, errors.Join(fmt.Errorf("probing %s: %w", filepath.Base(name), err), fl.Close())
	}
	if !ok {
		return false, fl.Close()
	}
	if runtime.GOOS == "windows" {
		// Windows removes no open file; an owner taking it meanwhile sees it
		// removed and takes another.
		return true, errors.Join(fl.Unlock(), removeDead(name))
	}
	// The lock still held, an owner that locks the file next finds no file.
	return true, errors.Join(removeDead(name), fl.Unlock())
}

// removeDead removes a live file no process holds. On Windows a handle
// another process has open, reading it or probing it, keeps it: unlocked,
// the next reader removes it.
func removeDead(name string) error {
	if err := fsx.Remove(name); err != nil && !busy(err) {
		return err
	}
	return nil
}
