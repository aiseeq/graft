//go:build !windows

package lock

// busy reports an error of a file another handle has open or locked, which
// only Windows has.
func busy(error) bool { return false }
