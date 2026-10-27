package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

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
			return nil, "", err
		}
		name := f.Name()
		_, writeErr := f.Write(data)
		if err := errors.Join(writeErr, f.Close()); err != nil {
			return nil, "", errors.Join(err, removeMissing(name))
		}
		beforeLock(name)
		fl := flock.New(name, flock.SetFlag(os.O_RDONLY))
		ok, err := fl.TryRLock()
		if errors.Is(err, os.ErrNotExist) {
			// Removed as dead before it was opened again.
			continue
		}
		if err != nil {
			return nil, "", errors.Join(fmt.Errorf("locking %s: %w", name, err), removeMissing(name))
		}
		if !ok {
			// A reader holds it, about to remove it as dead.
			continue
		}
		held, err := fl.Stat()
		if err != nil {
			return nil, "", errors.Join(err, fl.Unlock(), removeMissing(name))
		}
		if now, err := os.Stat(name); err == nil && os.SameFile(held, now) {
			return fl, name, nil
		}
		// Removed as dead before the lock was taken.
		if err := fl.Unlock(); err != nil {
			return nil, "", err
		}
	}
	return nil, "", fmt.Errorf("could not keep a live file %s in %s: readers took it for dead %d times", pattern, dir, liveAttempts)
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
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("probing %s: %w", filepath.Base(name), err)
	}
	if !ok {
		return false, nil
	}
	if runtime.GOOS == "windows" {
		// Windows removes no open file; an owner taking it meanwhile sees it
		// removed and takes another.
		return true, errors.Join(fl.Unlock(), removeDead(name))
	}
	// Removed under the lock, so an owner that locks it next sees it gone.
	return true, errors.Join(removeDead(name), fl.Unlock())
}

// removeDead removes a live file no process holds. On Windows a handle
// another process has open, reading it or probing it, keeps it: unlocked,
// the next reader removes it.
func removeDead(name string) error {
	if err := removeMissing(name); err != nil && !busy(err) {
		return err
	}
	return nil
}

func removeMissing(name string) error {
	if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
