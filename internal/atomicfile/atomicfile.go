// Package atomicfile replaces files so that readers see the old content or
// the new one, never a half-written file.
package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Write replaces path with data through a uniquely named temporary file next
// to it: concurrent writers of one path never share a temporary file.
func Write(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	discard := func(cause error) error {
		return errors.Join(cause, os.Remove(tmp.Name()))
	}
	if _, err := tmp.Write(data); err != nil {
		return discard(errors.Join(fmt.Errorf("writing %s: %w", tmp.Name(), err), tmp.Close()))
	}
	if err := tmp.Close(); err != nil {
		return discard(fmt.Errorf("writing %s: %w", tmp.Name(), err))
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		return discard(fmt.Errorf("setting the mode of %s: %w", tmp.Name(), err))
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return discard(fmt.Errorf("replacing %s: %w", path, err))
	}
	return nil
}
