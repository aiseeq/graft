//go:build !linux

package proc

// Refused: only Linux refuses programs to sandboxes this way.
func Refused(string) error { return nil }
