// Package fsx holds file system steps the packages of graft share.
package fsx

import (
	"errors"
	"os"
)

// Remove removes path; a path already gone is no error.
func Remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
