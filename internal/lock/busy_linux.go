//go:build linux

package lock

// busy reports no platform-specific file locking error on Linux.
func busy(error) bool { return false }
