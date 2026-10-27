//go:build windows

package lock

import (
	"errors"

	"golang.org/x/sys/windows"
)

// busy reports an error of a file another handle has open or locked:
// Windows removes no open file, and reads no file another handle holds
// exclusively, which only a reader removing it as dead does.
func busy(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
